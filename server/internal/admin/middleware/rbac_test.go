package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/yingo-server/UFOT/server/internal/admin/identity"
	"github.com/yingo-server/UFOT/server/internal/data/store"
)

func TestCSRFEqualConstantTime(t *testing.T) {
	if !Equal("abc123", "abc123") {
		t.Fatal("equal tokens rejected")
	}
	if Equal("abc123", "abc124") {
		t.Fatal("different tokens accepted")
	}
	if Equal("", "") {
		t.Fatal("empty tokens accepted")
	}
	if Equal("short", "a-much-longer-token") {
		t.Fatal("differently sized tokens accepted")
	}
}

func TestCSRFMiddleware(t *testing.T) {
	cs := NewCSRF("oc_admin_csrf", "X-CSRF-Token")
	h := cs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	do := func(method string, cookie *http.Cookie, header, bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/v1/users/1/ban", nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		if header != "" {
			req.Header.Set("X-CSRF-Token", header)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	good := &http.Cookie{Name: "oc_admin_csrf", Value: "token-abc"}
	if rec := do(http.MethodPost, good, "token-abc", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("matching double submit: code=%d", rec.Code)
	}
	if rec := do(http.MethodPost, good, "token-xyz", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("mismatch: code=%d want 403", rec.Code)
	}
	if rec := do(http.MethodPost, nil, "token-abc", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("missing cookie: code=%d want 403", rec.Code)
	}
	if rec := do(http.MethodGet, nil, "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("GET must not require csrf: code=%d", rec.Code)
	}
	// Strict mode: even Bearer requests must carry the double-submit token
	// (security-spec §3.3: all non-GET admin requests).
	if rec := do(http.MethodPost, good, "", "bearer-token"); rec.Code != http.StatusForbidden {
		t.Fatalf("bearer request without csrf should be rejected: code=%d", rec.Code)
	}

	// Error body shape.
	rec := do(http.MethodPost, good, "bad", "")
	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body: %v", err)
	}
	if body.Code != http.StatusForbidden || body.Reason != "csrf_invalid" || body.RequestID == "" {
		t.Fatalf("unexpected error body: %+v", body)
	}
}

type recordingJournal struct {
	mu     sync.Mutex
	events []store.AuditEvent
	fail   bool
}

func (j *recordingJournal) Append(_ context.Context, ev store.AuditEvent) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.fail {
		return context.DeadlineExceeded
	}
	j.events = append(j.events, ev)
	return nil
}

func (j *recordingJournal) snapshot() []store.AuditEvent {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]store.AuditEvent(nil), j.events...)
}

func TestRBACThreeRoleMatrix(t *testing.T) {
	// super: everything.
	for _, p := range AllPermissions() {
		if !Allowed(identity.RoleSuper, p) {
			t.Fatalf("super denied %s", p)
		}
	}
	// ops: operations yes, policy write no.
	for _, p := range []Permission{
		PermUserRead, PermUserBan, PermUserKick, PermReportRead, PermReportHandle,
		PermMessageSearch, PermMessageRecall, PermFlagRead, PermBroadcast,
		PermAuditRead, PermArchiveRead, PermArchiveRun, PermMetricsRead,
	} {
		if !Allowed(identity.RoleOps, p) {
			t.Fatalf("ops denied %s", p)
		}
	}
	if Allowed(identity.RoleOps, PermFlagWrite) {
		t.Fatal("ops must not write flags")
	}
	// review: content governance only.
	for _, p := range []Permission{PermUserRead, PermReportRead, PermReportHandle, PermMessageSearch, PermMessageRecall, PermAuditRead} {
		if !Allowed(identity.RoleReview, p) {
			t.Fatalf("review denied %s", p)
		}
	}
	for _, p := range []Permission{PermUserBan, PermUserKick, PermFlagWrite, PermBroadcast, PermArchiveRun, PermMetricsRead} {
		if Allowed(identity.RoleReview, p) {
			t.Fatalf("review must not have %s", p)
		}
	}
	if Allowed(identity.Role(99), PermUserRead) {
		t.Fatal("unknown role must deny")
	}
}

func TestRBACGuardDeniesAndAudits(t *testing.T) {
	j := &recordingJournal{}
	rbac := NewRBAC(j, nil)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })

	req := httptest.NewRequest(http.MethodPut, "/api/v1/flags", nil)
	req = req.WithContext(WithIdentity(req.Context(), &identity.Admin{AdminID: 7, Username: "rev", Role: identity.RoleReview}))
	rec := httptest.NewRecorder()
	rbac.Guard(PermFlagWrite)(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("review flag.write: code=%d want 403", rec.Code)
	}
	evs := j.snapshot()
	if len(evs) != 1 {
		t.Fatalf("denied audit events = %d, want 1", len(evs))
	}
	if evs[0].Action != "admin.rbac.denied" || evs[0].Actor != "rev" {
		t.Fatalf("unexpected denied event: %+v", evs[0])
	}
	var detail map[string]string
	if err := json.Unmarshal(evs[0].Detail, &detail); err != nil || detail["permission"] != string(PermFlagWrite) {
		t.Fatalf("denied detail = %s", evs[0].Detail)
	}

	// ops can ban
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/users/1/ban", nil)
	req2 = req2.WithContext(WithIdentity(req2.Context(), &identity.Admin{AdminID: 8, Username: "op", Role: identity.RoleOps}))
	rec2 := httptest.NewRecorder()
	rbac.Guard(PermUserBan)(next).ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNoContent {
		t.Fatalf("ops ban: code=%d", rec2.Code)
	}
}

func TestRoleStringParse(t *testing.T) {
	for _, r := range []identity.Role{identity.RoleSuper, identity.RoleOps, identity.RoleReview} {
		got, ok := identity.ParseRole(r.String())
		if !ok || got != r {
			t.Fatalf("round-trip %v -> %v %v", r, got, ok)
		}
	}
	if _, ok := identity.ParseRole("root"); ok {
		t.Fatal("unknown role parsed")
	}
	if identity.RoleFromInt(999) != identity.RoleReview {
		t.Fatal("unknown stored role must degrade to review")
	}
}
