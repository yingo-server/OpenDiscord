package handler

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/yingo-server/UFOT/server/internal/admin/auditshim"
	"github.com/yingo-server/UFOT/server/internal/admin/identity"
	"github.com/yingo-server/UFOT/server/internal/admin/middleware"
	"github.com/yingo-server/UFOT/server/internal/admin/service"
)

type adminView struct {
	AdminID     int64    `json:"admin_id"`
	Username    string   `json:"username"`
	Role        string   `json:"role"`
	Permissions []string `json:"permissions,omitempty"`
	ExpiresAt   int64    `json:"expires_at,omitempty"`
}

func viewOf(a *identity.Admin, withPerms bool) adminView {
	v := adminView{AdminID: a.AdminID, Username: a.Username, Role: a.Role.String(), ExpiresAt: a.ExpiresAt}
	if withPerms {
		perms := middleware.PermissionsFor(a.Role)
		v.Permissions = make([]string, 0, len(perms))
		for _, p := range perms {
			v.Permissions = append(v.Permissions, string(p))
		}
		sort.Strings(v.Permissions)
	}
	return v
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	TOTP     string `json:"totp"`
}

type loginResponse struct {
	Token     string    `json:"token"`
	CSRF      string    `json:"csrf_token"`
	ExpiresAt int64     `json:"expires_at"`
	Admin     adminView `json:"admin"`
}

func (a *API) handleLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req loginRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	fp := middleware.Fingerprint(r, a.cfg.TrustProxy)
	subject := "admin:" + sanitizeSubject(req.Username)
	status := a.deps.LoginLimiter.Check(req.Username, fp)
	if status.Locked {
		// Uniform error body; still burn argon2 time and still audit (A-2).
		a.deps.Auth.DummyVerify(req.Password)
		a.auditLogin(r, subject, false, "locked")
		middleware.WriteError(w, r, http.StatusUnauthorized, "invalid_credentials")
		return
	}
	res, err := a.deps.Auth.Login(r.Context(), req.Username, req.Password, req.TOTP)
	if err != nil {
		if errors.Is(err, service.ErrUnauthorized) {
			a.deps.LoginLimiter.Failure(req.Username, fp)
			a.auditLogin(r, subject, false, "bad_credentials")
			middleware.WriteError(w, r, http.StatusUnauthorized, "invalid_credentials")
			return
		}
		a.deps.Logger.Error("admin login error", "request_id", middleware.RequestID(r.Context()), "err", err.Error())
		middleware.WriteError(w, r, http.StatusInternalServerError, "internal_error")
		return
	}
	a.deps.LoginLimiter.Success(req.Username, fp)
	a.auditLogin(r, subject, true, "")
	a.setAuthCookies(w, res.Token, res.CSRF, res.ExpiresAt)
	middleware.WriteJSON(w, r, http.StatusOK, loginResponse{
		Token: res.Token, CSRF: res.CSRF, ExpiresAt: res.ExpiresAt,
		Admin: viewOf(res.Admin, false),
	})
}

func (a *API) handleLogout(w http.ResponseWriter, r *http.Request) {
	token := middleware.TokenFromRequest(r, a.cfg.CookieName)
	if err := a.deps.Auth.Logout(r.Context(), token); err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	a.clearAuthCookies(w)
	middleware.WriteJSON(w, r, http.StatusOK, map[string]bool{"ok": true})
}

func (a *API) handleWhoami(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	admin := middleware.IdentityFrom(r.Context())
	if admin == nil {
		middleware.WriteError(w, r, http.StatusUnauthorized, "unauthenticated")
		return
	}
	middleware.WriteJSON(w, r, http.StatusOK, viewOf(admin, true))
}

func (a *API) auditLogin(r *http.Request, subject string, success bool, reason string) {
	action := "admin.login.success"
	detail := map[string]any{"success": true}
	if !success {
		action = "admin.login.failed"
		detail = map[string]any{"success": false, "reason": reason}
	}
	a.deps.AuditShim.Record(r.Context(), auditshim.Meta{
		Actor: subject, Action: action, Subject: subject, Detail: detail,
	})
}

func (a *API) setAuthCookies(w http.ResponseWriter, token, csrf string, expiresAt int64) {
	maxAge := int(expiresAt - time.Now().Unix())
	if maxAge <= 0 || maxAge > 86400 {
		maxAge = 900
	}
	http.SetCookie(w, &http.Cookie{
		Name: a.cfg.CookieName, Value: token, Path: "/api/v1",
		MaxAge: maxAge, HttpOnly: true, Secure: a.cfg.SecureCookies,
		SameSite: http.SameSiteStrictMode,
	})
	// CSRF cookie is readable by JS by design (double-submit pattern).
	http.SetCookie(w, &http.Cookie{
		Name: a.cfg.CSRFCookieName, Value: csrf, Path: "/",
		MaxAge: maxAge, HttpOnly: false, Secure: a.cfg.SecureCookies,
		SameSite: http.SameSiteStrictMode,
	})
}

func (a *API) clearAuthCookies(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: a.cfg.CookieName, Value: "", Path: "/api/v1", MaxAge: -1,
		HttpOnly: true, Secure: a.cfg.SecureCookies, SameSite: http.SameSiteStrictMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name: a.cfg.CSRFCookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: false, Secure: a.cfg.SecureCookies, SameSite: http.SameSiteStrictMode,
	})
}

func sanitizeSubject(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > 128 {
		s = s[:128]
	}
	if s == "" {
		s = "unknown"
	}
	return s
}
