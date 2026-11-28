package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yingo-server/UFOT/server/internal/admin/handler"
	"github.com/yingo-server/UFOT/server/internal/admin/identity"
	"github.com/yingo-server/UFOT/server/internal/admin/middleware"
	"github.com/yingo-server/UFOT/server/internal/admin/totp"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
)

func pathOf(pattern string) string {
	p := strings.ReplaceAll(pattern, "{id}", "1")
	if p == "/api/v1/messages/search" {
		p += "?q=hello"
	}
	return p
}

func bodyOf(rt handler.Route) string {
	switch rt.Pattern {
	case "/api/v1/login":
		return `{"username":"root","password":"` + rootPass + `","totp":"` + totp.Generate([]byte(totpSeed), testNow) + `"}`
	case "/api/v1/users/{id}/ban":
		return `{"level":1,"reason":"test"}`
	case "/api/v1/reports/{id}/handle":
		return `{"action":"claim"}`
	case "/api/v1/flags":
		return `{"key":"registration.enabled","value":"false"}`
	case "/api/v1/broadcast":
		return `{"text":"hello everyone"}`
	default:
		return ""
	}
}

func newRequest(rt handler.Route, token, csrf string) *http.Request {
	body := bodyOf(rt)
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(rt.Method, pathOf(rt.Pattern), strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(rt.Method, pathOf(rt.Pattern), nil)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
		r.AddCookie(&http.Cookie{Name: "oc_admin_csrf", Value: csrf})
	}
	return r
}

func serve(api *handler.API, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	return rec
}

// TestFullRouteTableUnauthenticated is the 401 sweep over every protected
// route in the authoritative table.
func TestFullRouteTableUnauthenticated(t *testing.T) {
	env := newTestEnv(t)
	protected := 0
	for _, rt := range env.api.Routes() {
		if rt.Public {
			continue
		}
		protected++
		rec := serve(env.api, newRequest(rt, "", ""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without auth: code=%d body=%s", rt.Method, rt.Pattern, rec.Code, rec.Body.String())
		}
		var body middleware.ErrorBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s %s: bad error body: %v", rt.Method, rt.Pattern, err)
		}
		if body.Code != 401 || body.RequestID == "" {
			t.Fatalf("%s %s: error body=%+v", rt.Method, rt.Pattern, body)
		}
	}
	if protected < 15 {
		t.Fatalf("only %d protected routes enumerated", protected)
	}
}

// TestFullRouteTableReviewForbidden checks every route whose permission the
// review role lacks returns 403 + a denied audit event.
func TestFullRouteTableReviewForbidden(t *testing.T) {
	env := newTestEnv(t)
	forbidden := 0
	for _, rt := range env.api.Routes() {
		if rt.Public || rt.Permission == "" {
			continue
		}
		if middleware.Allowed(identity.RoleReview, rt.Permission) {
			continue
		}
		forbidden++
		rec := serve(env.api, newRequest(rt, revToken, revCSRF))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s as review: code=%d body=%s", rt.Method, rt.Pattern, rec.Code, rec.Body.String())
		}
	}
	if forbidden == 0 {
		t.Fatal("no forbidden routes found — RBAC matrix broken")
	}
	var denied int
	for _, ev := range env.journal.snapshot() {
		if ev.Action == "admin.rbac.denied" {
			denied++
		}
	}
	if denied != forbidden {
		t.Fatalf("denied audit events = %d, want %d", denied, forbidden)
	}
}

// TestFullRouteTableSuperOK is the 200 sweep: every protected route succeeds
// for a super admin with valid input.
func TestFullRouteTableSuperOK(t *testing.T) {
	env := newTestEnv(t)
	for _, rt := range env.api.Routes() {
		if rt.Public || rt.Pattern == "/api/v1/logout" {
			continue
		}
		rec := serve(env.api, newRequest(rt, rootToken, rootCSRF))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s as super: code=%d body=%s", rt.Method, rt.Pattern, rec.Code, rec.Body.String())
		}
	}
	// logout needs a fresh session; it is still part of the table.
	login := serve(env.api, newRequest(routeByPattern(t, env.api, "/api/v1/login"), "", ""))
	if login.Code != http.StatusOK {
		t.Fatalf("login: code=%d body=%s", login.Code, login.Body.String())
	}
	var lr struct {
		Token string `json:"token"`
		CSRF  string `json:"csrf_token"`
	}
	if err := json.Unmarshal(login.Body.Bytes(), &lr); err != nil {
		t.Fatal(err)
	}
	logout := serve(env.api, newRequest(routeByPattern(t, env.api, "/api/v1/logout"), lr.Token, lr.CSRF))
	if logout.Code != http.StatusOK {
		t.Fatalf("logout: code=%d body=%s", logout.Code, logout.Body.String())
	}
	// Public health.
	health := serve(env.api, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health: code=%d", health.Code)
	}
}

func routeByPattern(t *testing.T, api *handler.API, pattern string) handler.Route {
	t.Helper()
	for _, rt := range api.Routes() {
		if rt.Pattern == pattern {
			return rt
		}
	}
	t.Fatalf("route %s not found", pattern)
	return handler.Route{}
}

func TestLoginSuccessAndWhoami(t *testing.T) {
	env := newTestEnv(t)
	login := serve(env.api, newRequest(routeByPattern(t, env.api, "/api/v1/login"), "", ""))
	if login.Code != http.StatusOK {
		t.Fatalf("login code=%d body=%s", login.Code, login.Body.String())
	}
	var lr struct {
		Token string `json:"token"`
		CSRF  string `json:"csrf_token"`
		Admin struct {
			Username string `json:"username"`
			Role     string `json:"role"`
		} `json:"admin"`
	}
	if err := json.Unmarshal(login.Body.Bytes(), &lr); err != nil {
		t.Fatal(err)
	}
	if lr.Token == "" || lr.CSRF == "" || lr.Admin.Role != "super" {
		t.Fatalf("login response = %+v", lr)
	}
	cookies := login.Result().Cookies()
	var haveSession, haveCSRF bool
	for _, c := range cookies {
		if c.Name == "oc_admin_session" && c.Value == lr.Token && c.HttpOnly {
			haveSession = true
		}
		if c.Name == "oc_admin_csrf" && c.Value == lr.CSRF && !c.HttpOnly {
			haveCSRF = true
		}
	}
	if !haveSession || !haveCSRF {
		t.Fatalf("cookies = %+v", cookies)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+lr.Token)
	rec := serve(env.api, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("whoami: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var who struct {
		Username    string   `json:"username"`
		Role        string   `json:"role"`
		Permissions []string `json:"permissions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &who); err != nil {
		t.Fatal(err)
	}
	if who.Username != "root" || who.Role != "super" || len(who.Permissions) != len(middleware.AllPermissions()) {
		t.Fatalf("whoami = %+v", who)
	}
}

func TestLoginWrongPasswordUniform401(t *testing.T) {
	env := newTestEnv(t)
	rt := routeByPattern(t, env.api, "/api/v1/login")
	body := `{"username":"root","password":"wrong password!!","totp":"000000"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := serve(env.api, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: code=%d", rec.Code)
	}
	var eb middleware.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &eb); err != nil {
		t.Fatal(err)
	}
	if eb.Reason != "invalid_credentials" {
		t.Fatalf("reason=%s (must not reveal account/lock state)", eb.Reason)
	}
	_ = rt
}

func TestLoginLockoutAfterFiveFailures(t *testing.T) {
	env := newTestEnv(t)
	for i := 0; i < 5; i++ {
		body := `{"username":"root","password":"wrong password!!","totp":"000000"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/login", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if rec := serve(env.api, req); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: code=%d", i, rec.Code)
		}
	}
	// Correct credentials are still rejected while locked, with the same body.
	good := newRequest(routeByPattern(t, env.api, "/api/v1/login"), "", "")
	rec := serve(env.api, good)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("locked correct login: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var eb middleware.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &eb); err != nil {
		t.Fatal(err)
	}
	if eb.Reason != "invalid_credentials" {
		t.Fatalf("locked reason=%s", eb.Reason)
	}
	if f, l := env.limiter.Stats(); f < 5 || l == 0 {
		t.Fatalf("limiter stats failures=%d lockouts=%d", f, l)
	}
}

func TestIdempotencyKeyReplaysFirstResponse(t *testing.T) {
	env := newTestEnv(t)
	rt := routeByPattern(t, env.api, "/api/v1/users/{id}/ban")
	req1 := newRequest(rt, rootToken, rootCSRF)
	req1.Header.Set("Idempotency-Key", "idem-abc-123")
	rec1 := serve(env.api, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first: code=%d body=%s", rec1.Code, rec1.Body.String())
	}
	req2 := newRequest(rt, rootToken, rootCSRF)
	req2.Header.Set("Idempotency-Key", "idem-abc-123")
	rec2 := serve(env.api, req2)
	if rec2.Code != rec1.Code {
		t.Fatalf("replay status=%d want %d", rec2.Code, rec1.Code)
	}
	if rec1.Body.String() != rec2.Body.String() {
		t.Fatalf("replay body differs:\nfirst=%s\nsecond=%s", rec1.Body.String(), rec2.Body.String())
	}
	if got := len(env.dispatch.snapshot()); got != 1 {
		t.Fatalf("dispatch calls = %d, want 1 (idempotency violated)", got)
	}
	audits := 0
	for _, ev := range env.journal.snapshot() {
		if ev.Action == "user.ban" {
			audits++
		}
	}
	if audits != 1 {
		t.Fatalf("user.ban audit events = %d, want 1", audits)
	}
}

func TestDryRunHasNoSideEffects(t *testing.T) {
	env := newTestEnv(t)
	broadcast := routeByPattern(t, env.api, "/api/v1/broadcast")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/broadcast?dry_run=true", strings.NewReader(`{"text":"preview me"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rootToken)
	req.Header.Set("X-CSRF-Token", rootCSRF)
	req.AddCookie(&http.Cookie{Name: "oc_admin_csrf", Value: rootCSRF})
	rec := serve(env.api, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("broadcast dry-run: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var pv struct {
		EstimatedReach int64 `json:"estimated_reach"`
		DryRun         bool  `json:"dry_run"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pv); err != nil {
		t.Fatal(err)
	}
	if !pv.DryRun || pv.EstimatedReach != 1000 {
		t.Fatalf("preview = %+v", pv)
	}
	if len(env.dispatch.snapshot()) != 0 || len(env.journal.snapshot()) != 0 {
		t.Fatalf("dry-run produced side effects: dispatch=%d audit=%d",
			len(env.dispatch.snapshot()), len(env.journal.snapshot()))
	}
	_ = broadcast

	// archive dry-run too
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/archive/run?dry_run=true", strings.NewReader(`{}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+rootToken)
	req2.Header.Set("X-CSRF-Token", rootCSRF)
	req2.AddCookie(&http.Cookie{Name: "oc_admin_csrf", Value: rootCSRF})
	rec2 := serve(env.api, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("archive dry-run: code=%d body=%s", rec2.Code, rec2.Body.String())
	}
	if len(env.dispatch.snapshot()) != 0 || len(env.journal.snapshot()) != 0 {
		t.Fatal("archive dry-run produced side effects")
	}
}

func TestArchiveRunConflictWhenRunning(t *testing.T) {
	env := newTestEnv(t)
	env.archive.running = true
	req := httptest.NewRequest(http.MethodPost, "/api/v1/archive/run", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rootToken)
	req.Header.Set("X-CSRF-Token", rootCSRF)
	req.AddCookie(&http.Cookie{Name: "oc_admin_csrf", Value: rootCSRF})
	rec := serve(env.api, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("archive run while running: code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCSRFCookiePathEnforced(t *testing.T) {
	env := newTestEnv(t)
	rt := routeByPattern(t, env.api, "/api/v1/users/{id}/ban")
	// Cookie auth without CSRF header -> 403.
	req := newRequest(rt, "", "")
	req.AddCookie(&http.Cookie{Name: "oc_admin_session", Value: rootToken})
	rec := serve(env.api, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cookie POST without csrf: code=%d body=%s", rec.Code, rec.Body.String())
	}
	// Matching cookie + header -> 200.
	req2 := newRequest(rt, "", rootCSRF)
	req2.AddCookie(&http.Cookie{Name: "oc_admin_session", Value: rootToken})
	req2.AddCookie(&http.Cookie{Name: "oc_admin_csrf", Value: rootCSRF})
	rec2 := serve(env.api, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("cookie POST with csrf: code=%d body=%s", rec2.Code, rec2.Body.String())
	}
}

func TestBanFrameKind(t *testing.T) {
	env := newTestEnv(t)
	rt := routeByPattern(t, env.api, "/api/v1/users/{id}/ban")
	rec := serve(env.api, newRequest(rt, rootToken, rootCSRF))
	if rec.Code != http.StatusOK {
		t.Fatalf("ban: code=%d body=%s", rec.Code, rec.Body.String())
	}
	calls := env.dispatch.snapshot()
	if len(calls) != 1 || calls[0].Kind != kinds.KindSecurityRequest {
		t.Fatalf("calls = %+v, want one 960 frame", calls)
	}
}
