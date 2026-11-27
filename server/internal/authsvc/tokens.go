package authsvc

// tokens.go: Ed25519 token issuance, refresh rotation with family replay
// detection, logout, and access-token extraction from the request.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/yingo-server/UFOT/server/internal/data/store"
	"github.com/yingo-server/UFOT/server/internal/user/core/authn"
)

// sessionResponse is the login result. Field names are the client contract
// (see docs/client-contract.md — the session object is the wire form):
//
//	access_token / token / refresh_token / user_id / session_id / expires_at /
//	guest / profile{display_name}
//
// expires_at is UNIX MILLISECONDS because the client's TokenStore compares it
// with Date.now() (`expiresAt < Date.now()`). expires_at_unix carries the same
// instant in seconds for scripts/servers.
type sessionResponse struct {
	UserID        int64           `json:"user_id"`
	DeviceID      string          `json:"device_id"`
	AccessToken   string          `json:"access_token"`
	Token         string          `json:"token"`
	RefreshToken  string          `json:"refresh_token"`
	ExpiresAt     int64           `json:"expires_at"`
	ExpiresAtUnix int64           `json:"expires_at_unix"`
	SessionID     string          `json:"session_id"`
	Guest         bool            `json:"guest"`
	IsNewUser     bool            `json:"is_new_user"`
	Profile       *profilePayload `json:"profile,omitempty"`
}

type profilePayload struct {
	DisplayName string `json:"display_name"`
}

// challengeResponse is the OTP/magic-link challenge shape. The client's
// mapAuthChallenge requires challenge_id; ok/retry_after are kept for plain
// HTTP consumers of the same endpoint.
type challengeResponse struct {
	OK          bool   `json:"ok"`
	ChallengeID string `json:"challenge_id"`
	Channel     string `json:"channel"`
	ExpiresIn   int64  `json:"expires_in"`
	ResendAfter int64  `json:"resend_after"`
	RetryAfter  int64  `json:"retry_after,omitempty"`
	Hint        string `json:"hint,omitempty"`
	Link        string `json:"link,omitempty"` // dev mode only
}

func (s *Service) newSession(uid int64, deviceID string, pair tokenPair, guest, isNew bool, displayName string) *sessionResponse {
	resp := &sessionResponse{
		UserID:        uid,
		DeviceID:      deviceID,
		AccessToken:   pair.Access,
		Token:         pair.Access, // alias kept for older clients
		RefreshToken:  pair.Refresh,
		ExpiresAt:     pair.AccessExp.UnixMilli(),
		ExpiresAtUnix: pair.AccessExp.Unix(),
		SessionID:     deviceID,
		Guest:         guest,
		IsNewUser:     isNew,
	}
	if displayName != "" {
		resp.Profile = &profilePayload{DisplayName: displayName}
	}
	return resp
}

// setAuthCookie stores the access token as an HttpOnly cookie. It is what
// makes the WS handshake work in a browser: the acceptor reads access_token
// from the Cookie header and refuses URL query tokens (security-spec §2.3).
func (s *Service) setAuthCookie(w http.ResponseWriter, token string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieAccessTokenName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl / time.Second),
	})
}

func (s *Service) clearAuthCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieAccessTokenName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// accessTokenFromRequest reads the bearer token from the Authorization header
// or the access_token cookie (never from the URL, security-spec §2.3).
func accessTokenFromRequest(r *http.Request) string {
	if h := r.Header.Get(headerAuthorization); len(h) > len(bearerPrefix) &&
		equalFoldASCII(h[:len(bearerPrefix)], bearerPrefix) {
		return h[len(bearerPrefix):]
	}
	if c, err := r.Cookie(cookieAccessTokenName); err == nil && c.Value != "" {
		return c.Value
	}
	return ""
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// authenticate resolves the caller's identity from the access token.
func (s *Service) authenticate(r *http.Request) (*authn.Identity, *apiError) {
	tok := accessTokenFromRequest(r)
	if tok == "" {
		return nil, errUnauthorized("UNAUTHENTICATED")
	}
	id, err := s.verifier.Verify(tok)
	if err != nil {
		return nil, errUnauthorized("INVALID_TOKEN")
	}
	return id, nil
}

// ---------------------------------------------------------------- issuance

type tokenPair struct {
	Access     string
	Refresh    string
	AccessExp  time.Time
	RefreshExp time.Time
}

// claimsUnverified extracts the claims of a token we just signed (signature
// verification is not needed here; it is verified on the refresh path).
func claimsUnverified(token string) (*authn.Claims, error) {
	claims := &authn.Claims{}
	if _, _, err := jwt.NewParser().ParseUnverified(token, claims); err != nil {
		return nil, err
	}
	return claims, nil
}

// issuePair signs access+refresh and records the refresh row in the family.
// familyID == "" starts a new family (a new login on that device).
func (s *Service) issuePair(ctx context.Context, uid int64, deviceID, familyID string) (*tokenPair, *apiError) {
	now := s.cfg.Now()
	access, err := s.signer.SignAccess(uid, deviceID, s.cfg.AccessTTL)
	if err != nil {
		s.lg.Error("sign access failed", "uid", uid, "err", err.Error())
		return nil, errInternal()
	}
	refresh, err := s.signer.SignRefresh(uid, deviceID, s.cfg.RefreshTTL)
	if err != nil {
		s.lg.Error("sign refresh failed", "uid", uid, "err", err.Error())
		return nil, errInternal()
	}
	claims, err := claimsUnverified(refresh)
	if err != nil || claims.Jti == "" {
		s.lg.Error("refresh claims unreadable", "uid", uid)
		return nil, errInternal()
	}
	if familyID == "" {
		familyID = newID(challengeIDBytes)
	}
	row := &refreshRow{
		JTI:       claims.Jti,
		FamilyID:  familyID,
		UID:       uid,
		DeviceID:  deviceID,
		TokenHash: tokenHash(refresh),
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(s.cfg.RefreshTTL).Unix(),
	}
	if err := s.st.insertRefresh(ctx, row); err != nil {
		s.lg.Error("store refresh failed", "uid", uid, "err", err.Error())
		return nil, errInternal()
	}
	return &tokenPair{
		Access:     access,
		Refresh:    refresh,
		AccessExp:  now.Add(s.cfg.AccessTTL),
		RefreshExp: now.Add(s.cfg.RefreshTTL),
	}, nil
}

// issueSession is issuePair + the response envelope + the auth cookie.
func (s *Service) issueSession(w http.ResponseWriter, ctx context.Context, uid int64,
	deviceID, familyID string, guest, isNew bool, displayName string) *apiError {
	pair, e := s.issuePair(ctx, uid, deviceID, familyID)
	if e != nil {
		return e
	}
	s.setAuthCookie(w, pair.Access, s.cfg.AccessTTL)
	s.writeJSON(w, http.StatusOK, s.newSession(uid, deviceID, *pair, guest, isNew, displayName))
	return nil
}

// ---------------------------------------------------------------- refresh

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
	DeviceID     string `json:"device_id,omitempty"`
}

// handleRefresh rotates the refresh token. Replay of an already-rotated token
// revokes the entire family (security-spec §2.2, no grace window).
func (s *Service) handleRefresh(w http.ResponseWriter, r *http.Request) *apiError {
	var req refreshRequest
	if e := s.decode(w, r, &req); e != nil {
		return e
	}
	if req.RefreshToken == "" {
		return errBadRequest("refresh_token required")
	}
	id, err := s.verifier.VerifyRefresh(req.RefreshToken)
	if err != nil {
		s.lg.Warn("refresh rejected", "reason", "invalid signature/expired")
		return errUnauthorized("INVALID_REFRESH")
	}
	claims, err := claimsUnverified(req.RefreshToken)
	if err != nil || claims.Jti == "" {
		return errUnauthorized("INVALID_REFRESH")
	}
	row, err := s.st.refresh(r.Context(), claims.Jti)
	if errors.Is(err, store.ErrNotFound) {
		return errUnauthorized("INVALID_REFRESH")
	}
	if err != nil {
		s.lg.Error("refresh lookup failed", "err", err.Error())
		return errInternal()
	}
	if row.Revoked {
		return errUnauthorized("REFRESH_REVOKED")
	}
	if row.UID != id.UserID || row.DeviceID != id.DeviceID {
		// Token/claims mismatch: treat as compromise.
		_ = s.st.revokeFamily(r.Context(), row.FamilyID, "claim_mismatch", s.cfg.Now().Unix())
		s.lg.Warn("refresh claim mismatch; family revoked", "uid", row.UID, "sub", subjectShort(row.JTI))
		return errUnauthorized("REFRESH_REPLAY")
	}
	if row.RotatedTo != "" {
		return s.replayDetected(r.Context(), row)
	}
	if !hmacEqual(row.TokenHash, tokenHash(req.RefreshToken)) {
		return s.replayDetected(r.Context(), row)
	}
	if row.ExpiresAt <= s.cfg.Now().Unix() {
		return errUnauthorized("REFRESH_EXPIRED")
	}

	pair, e := s.issuePair(r.Context(), row.UID, row.DeviceID, row.FamilyID)
	if e != nil {
		return e
	}
	newClaims, err := claimsUnverified(pair.Refresh)
	if err != nil || newClaims.Jti == "" {
		return errInternal()
	}
	ok, err := s.st.rotate(r.Context(), row.JTI, newClaims.Jti, s.cfg.Now().Unix())
	if err != nil {
		s.lg.Error("refresh rotate failed", "err", err.Error())
		return errInternal()
	}
	if !ok {
		// Lost a rotation race: another caller rotated the same token.
		return s.replayDetected(r.Context(), row)
	}
	s.setAuthCookie(w, pair.Access, s.cfg.AccessTTL)
	s.writeJSON(w, http.StatusOK, s.newSession(row.UID, row.DeviceID, *pair, false, false, ""))
	return nil
}

// replayDetected revokes the whole family (all devices' chain sharing this
// family id) and answers 401. Family revocation is the documented response to
// refresh reuse (security-spec §2.2); kicking live WS connections is the
// gateway's job and is not wired to this path yet (see report: TODO-KICK).
func (s *Service) replayDetected(ctx context.Context, row *refreshRow) *apiError {
	if err := s.st.revokeFamily(ctx, row.FamilyID, "refresh_replay", s.cfg.Now().Unix()); err != nil {
		s.lg.Error("revoke family failed", "err", err.Error())
	}
	s.lg.Warn("refresh replay detected; token family revoked",
		"uid", row.UID, "family", shortToken(row.FamilyID), "jti", shortToken(row.JTI))
	return errUnauthorized("REFRESH_REPLAY")
}

// ---------------------------------------------------------------- logout

type logoutRequest struct {
	RefreshToken string `json:"refresh_token,omitempty"`
}

// handleLogout revokes the refresh family (the current device's chain) and
// clears the cookie. Access tokens are not blacklisted by design
// (security-spec §2.7: 15 min residual window + kick on the WS side).
func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) *apiError {
	var req logoutRequest
	// logout may legitimately be called without a body.
	if r.ContentLength > 0 {
		if e := s.decode(w, r, &req); e != nil {
			return e
		}
	}
	ctx := r.Context()
	revoked := false
	if req.RefreshToken != "" {
		if claims, err := claimsUnverified(req.RefreshToken); err == nil && claims.Jti != "" {
			if err := s.st.revokeFamilyOf(ctx, claims.Jti, "logout", s.cfg.Now().Unix()); err != nil {
				s.lg.Warn("logout revoke failed", "err", err.Error())
			} else {
				revoked = true
			}
		}
	}
	s.clearAuthCookie(w)
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "revoked": revoked})
	return nil
}
