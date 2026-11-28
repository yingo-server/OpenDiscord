package authsvc

// handlers.go: password login, guest login, password set. The phone / email
// handlers live in otp.go, the refresh / logout handlers in tokens.go.

import (
	"net/http"
	"strings"
	"unicode"
)

type passwordRequest struct {
	Account  string `json:"account"`
	Password string `json:"password"`
	// Captcha fields are part of the client contract (PasswordAuthProvider).
	// They are accepted but not enforced today: the account lock (5 failures
	// -> 15m/1h/24h) already covers credential stuffing. TODO-CAPTCHA in the
	// report.
	Captcha      string `json:"captcha,omitempty"`
	CaptchaToken string `json:"captcha_token,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
}

type guestRequest struct {
	DeviceID string `json:"device_id,omitempty"`
	Nickname string `json:"nickname,omitempty"`
}

type passwordSetRequest struct {
	Password string `json:"password"`
}

// handlePasswordLogin authenticates account + password (account may be a
// phone number, an email or a username).
func (s *Service) handlePasswordLogin(w http.ResponseWriter, r *http.Request) *apiError {
	if !s.methodAllowed("password") {
		return errMethodDisabled("password")
	}
	var req passwordRequest
	if e := s.decode(w, r, &req); e != nil {
		return e
	}
	if strings.TrimSpace(req.Account) == "" || req.Password == "" {
		return errBadRequest("account and password required")
	}
	if len(req.Password) > 1024 {
		return errBadRequest("password too long")
	}
	idType, normalized, e := classifyAccount(req.Account)
	if e != nil {
		return e
	}
	ctx := r.Context()
	index := s.idIndex(idType, normalized)
	ipHash := s.idIndex(IDTypeIP, s.clientIP(r))
	subjects := []string{accountSubject(index), ipSubject(ipHash)}

	if left, err := s.lockRemaining(ctx, subjects...); err != nil {
		return err
	} else if left > 0 {
		s.lg.Warn("login blocked by lock", "type", idType, "sub", subjectShort(index))
		return errRateLimited("ACCOUNT_LOCKED", left)
	}

	fail := func() *apiError {
		retry, err := s.noteFailure(ctx, subjects...)
		if err != nil {
			s.lg.Error("login failure accounting failed", "err", err.Error())
		}
		s.lg.Warn("password login failed", "type", idType, "sub", subjectShort(index))
		if retry > 0 {
			return errRateLimited("ACCOUNT_LOCKED", retry)
		}
		return errUnauthorized("INVALID_CREDENTIALS")
	}

	uid, err := s.ident.LookupByIdentity(ctx, idType, index)
	if err != nil {
		// Unknown account: run the full argon2id "fake" comparison so the
		// timing of a miss matches a wrong password (security-spec §2.4).
		s.hasher.VerifyOrFake(req.Password)
		return fail()
	}
	hash, err := s.st.credential(ctx, uid)
	if err != nil {
		s.hasher.VerifyOrFake(req.Password)
		return fail()
	}
	ok, verr := s.hasher.Verify(req.Password, hash)
	if verr != nil || !ok {
		return fail()
	}
	if e := s.checkBan(ctx, uid); e != nil {
		return e
	}
	s.clearFailures(ctx, subjects...)

	deviceID := req.DeviceID
	if !validDeviceID(deviceID) {
		deviceID = issueDeviceID()
	}
	if err := s.st.upsertDevice(ctx, deviceID, uid, "password", s.cfg.Now().Unix()); err != nil {
		s.lg.Warn("device upsert failed", "err", err.Error())
	}
	s.lg.Info("login ok", "method", "password", "uid", uid)
	display := ""
	if u, gerr := s.ident.GetUser(ctx, uid); gerr == nil && u != nil {
		display = u.DisplayName
	}
	return s.issueSession(w, ctx, uid, deviceID, "", false, false, display)
}

// handleGuest issues a guest session. The device id is server-issued unless
// the caller already holds one from a previous guest login (security-spec
// §2.6: 设备 ID 服务端首次签发，不可客户端自定义).
func (s *Service) handleGuest(w http.ResponseWriter, r *http.Request) *apiError {
	if !s.methodAllowed("guest") {
		return errMethodDisabled("guest")
	}
	var req guestRequest
	if e := s.decode(w, r, &req); e != nil {
		return e
	}
	ctx := r.Context()
	deviceID := req.DeviceID
	if !validDeviceID(deviceID) || !strings.HasPrefix(deviceID, "g_") {
		deviceID = issueDeviceID()
	}
	name := sanitizeNickname(req.Nickname)
	uid, isNew, display, e := s.resolveUser(ctx, IDTypeGuest, deviceID, true, name)
	if e != nil {
		return e
	}
	if e := s.checkBan(ctx, uid); e != nil {
		return e
	}
	if err := s.st.upsertDevice(ctx, deviceID, uid, "guest", s.cfg.Now().Unix()); err != nil {
		s.lg.Warn("device upsert failed", "err", err.Error())
	}
	s.lg.Info("login ok", "method", "guest", "uid", uid, "new", isNew)
	return s.issueSession(w, ctx, uid, deviceID, "", true, isNew, display)
}

// handlePasswordSet sets or replaces the password of the authenticated
// account. Extension endpoint (not part of the client contract) — it exists so
// password login has a provisioning path without a separate registration API.
func (s *Service) handlePasswordSet(w http.ResponseWriter, r *http.Request) *apiError {
	id, e := s.authenticate(r)
	if e != nil {
		return e
	}
	var req passwordSetRequest
	if e := s.decode(w, r, &req); e != nil {
		return e
	}
	if len([]rune(req.Password)) < minPasswordLen {
		return &apiError{Status: http.StatusBadRequest, Code: "WEAK_PASSWORD",
			Message: "password must be at least 10 characters"}
	}
	if len(req.Password) > 1024 {
		return errBadRequest("password too long")
	}
	ctx := r.Context()
	hash, err := s.hasher.Hash(req.Password)
	if err != nil {
		s.lg.Error("hash password failed", "err", err.Error())
		return errInternal()
	}
	if err := s.st.setCredential(ctx, id.UserID, hash, s.cfg.Now().Unix()); err != nil {
		s.lg.Error("store credential failed", "err", err.Error())
		return errInternal()
	}
	// security-spec §2.4: a password change revokes every refresh family.
	if err := s.st.revokeAllForUID(ctx, id.UserID, "password_change", s.cfg.Now().Unix()); err != nil {
		s.lg.Warn("revoke families after password change failed", "err", err.Error())
	}
	s.lg.Info("password set", "uid", id.UserID)
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	return nil
}

// sanitizeNickname keeps guest display names printable and short.
func sanitizeNickname(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	out := make([]rune, 0, 32)
	for _, r := range name {
		if unicode.IsControl(r) {
			continue
		}
		out = append(out, r)
		if len(out) == 32 {
			break
		}
	}
	return strings.TrimSpace(string(out))
}
