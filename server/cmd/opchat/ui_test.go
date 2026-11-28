package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAdmin records every request and serves minimal JSON for each endpoint.
type fakeAdmin struct {
	mu   sync.Mutex
	reqs []string // "METHOD /path?query"
	srv  *httptest.Server
}

func newFakeAdmin(t *testing.T) *fakeAdmin {
	f := &fakeAdmin{}
	mux := http.NewServeMux()
	rec := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			f.reqs = append(f.reqs, r.Method+" "+r.URL.RequestURI())
			f.mu.Unlock()
			h(w, r)
		}
	}
	mux.HandleFunc("/api/v1/login", rec(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["username"] == "root" && in["totp"] == "123456" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"token":"tok-1","csrf_token":"csrf-1","expires_at":%d}`, time.Now().Add(time.Hour).Unix())
			return
		}
		w.WriteHeader(401)
		fmt.Fprint(w, `{"reason":"bad credentials","request_id":"req-1"}`)
	}))
	mux.HandleFunc("/api/v1/whoami", rec(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"username":"root","role":"super"}`)
	}))
	mux.HandleFunc("/api/v1/metrics", rec(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "# HELP x\nufot_online_connections 1234\nufot_users_total 5000\nufot_messages_today_total 88213\n")
	}))
	mux.HandleFunc("/api/v1/archive/status", rec(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"last_run":"ok"}`)
	}))
	mux.HandleFunc("/api/v1/users", rec(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"items":[{"id":42,"username":"alice","status":0,"phone":"13800001111","email":"a@x.com"}]}`)
	}))
	mux.HandleFunc("/api/v1/users/42", rec(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":42,"username":"alice","status":0,"phone":"13800001111","email":"a@x.com","created_at":"2024-01-01"}`)
	}))
	mux.HandleFunc("/api/v1/users/42/ban", rec(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	}))
	mux.HandleFunc("/api/v1/users/42/unban", rec(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	}))
	mux.HandleFunc("/api/v1/users/42/kick", rec(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	}))
	mux.HandleFunc("/api/v1/flags", rec(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			fmt.Fprint(w, `{"ok":true}`)
			return
		}
		fmt.Fprint(w, `{"items":[{"key":"beta.on","value":"true","version":3}]}`)
	}))
	mux.HandleFunc("/api/v1/reports", rec(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"items":[{"id":7,"status":0,"reason":"spam"}]}`)
	}))
	mux.HandleFunc("/api/v1/reports/7/handle", rec(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	}))
	mux.HandleFunc("/api/v1/broadcast", rec(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("dry_run") == "true" {
			fmt.Fprint(w, `{"reach":1234,"dry_run":true}`)
			return
		}
		fmt.Fprint(w, `{"sent":1234}`)
	}))
	mux.HandleFunc("/api/v1/archive/run", rec(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("dry_run") == "true" {
			fmt.Fprint(w, `{"rows":500}`)
			return
		}
		fmt.Fprint(w, `{"moved":500}`)
	}))
	mux.HandleFunc("/api/v1/audit", rec(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"items":[{"id":100,"action":"user.ban","created_at":"2024-01-01T00:00:00Z"},{"id":99,"action":"login","created_at":"2024-01-01T00:00:00Z"}]}`)
	}))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAdmin) calls(t *testing.T) []string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.reqs))
	copy(out, f.reqs)
	return out
}

// drive runs one key sequence through the app against the fake server.
// keys are delivered via handleKeys; answers are the newline-separated lines
// that promptLine/confirm read from injected stdin.
func drive(t *testing.T, keys []string, answers string) (string, *fakeAdmin, *uiState) {
	t.Helper()
	f := newFakeAdmin(t)
	c := newClient(f.srv.URL, "tok-1", "csrf-1")
	st := newUIState(localeFor("zh-CN"), "zh-CN", f.srv.URL)
	var out strings.Builder
	a := newApp(c, st, strings.NewReader(answers), &out)
	for _, k := range keys {
		a.handleKeys(k)
	}
	out.WriteString(renderFrame(st)) // capture final frame (toasts etc.)
	return out.String(), f, st
}

// 4. interaction flows

func TestLoginWizardFlow(t *testing.T) {
	f := newFakeAdmin(t)
	var out strings.Builder
	st := newUIState(localeFor("zh-CN"), "zh-CN", "")
	a := newApp(nil, st, strings.NewReader(f.srv.URL+"\nroot\nsecret\n123456\n"), &out)
	if !loginWizard(a, "") {
		t.Fatal("login wizard should succeed")
	}
	joined := strings.Join(f.calls(t), "\n")
	if !strings.Contains(joined, "POST /api/v1/login") {
		t.Fatalf("login endpoint not called, got: %s", joined)
	}
	if !st.connected {
		t.Fatal("state should be connected after login")
	}
}

func TestLoginWizardBadTOTP(t *testing.T) {
	f := newFakeAdmin(t)
	var out strings.Builder
	st := newUIState(localeFor("zh-CN"), "zh-CN", "")
	a := newApp(nil, st, strings.NewReader(f.srv.URL+"\nroot\nsecret\nwrong\n"), &out)
	if loginWizard(a, "") {
		t.Fatal("login wizard should fail with wrong TOTP")
	}
	if st.connected {
		t.Fatal("must not be connected after failed login")
	}
}

func TestBanFlowWithConfirm(t *testing.T) {
	// 2 (tab) → g (search, reads "alice") → \r (detail) → b (ban: level, days, reason, y)
	out, f, _ := drive(t,
		[]string{"2", "g", "\r", "b", "y"},
		"alice\n2\n7\nspam\ny\n")
	joined := strings.Join(f.calls(t), "\n")
	if !strings.Contains(joined, "GET /api/v1/users?") {
		t.Fatalf("users search missing: %s", joined)
	}
	if !strings.Contains(joined, "GET /api/v1/users/42") {
		t.Fatalf("user detail missing: %s", joined)
	}
	if !strings.Contains(joined, "POST /api/v1/users/42/ban") {
		t.Fatalf("ban not called: %s", joined)
	}
	if !strings.Contains(out, "users/42/ban") {
		t.Fatalf("confirm prompt should show the API call: %s", out)
	}
}

func TestBanFlowAbortedByDefaultN(t *testing.T) {
	out, f, _ := drive(t,
		[]string{"2", "g", "\r", "b", "N"},
		"alice\n2\n7\nspam\n")
	joined := strings.Join(f.calls(t), "\n")
	if strings.Contains(joined, "POST /api/v1/users/42/ban") {
		t.Fatalf("ban must not run without y: %s", joined)
	}
	if !strings.Contains(out, "已取消") {
		t.Fatalf("expected cancel toast: %s", out)
	}
}

func TestFlagsEditFlow(t *testing.T) {
	// 5 (tab) → g (list) → \r (edit value, reads JSON) → y (confirm)
	out, f, _ := drive(t,
		[]string{"5", "g", "\r", "y"},
		`{"on":true}`+"\ny\n")
	joined := strings.Join(f.calls(t), "\n")
	if !strings.Contains(joined, "GET /api/v1/flags") {
		t.Fatalf("flags list missing: %s", joined)
	}
	if !strings.Contains(joined, "PUT /api/v1/flags") {
		t.Fatalf("flags set missing: %s", joined)
	}
	if !strings.Contains(out, "PUT /api/v1/flags") {
		t.Fatalf("confirm prompt should show PUT: %s", out)
	}
}

func TestFlagsEditRejectsBadJSON(t *testing.T) {
	out, f, _ := drive(t,
		[]string{"5", "g", "\r", "y"},
		"{broken\ny\n")
	joined := strings.Join(f.calls(t), "\n")
	if strings.Contains(joined, "PUT /api/v1/flags") {
		t.Fatalf("bad JSON must not be PUT: %s", joined)
	}
	if !strings.Contains(out, "不是合法 JSON") {
		t.Fatalf("expected invalid-JSON toast: %s", out)
	}
}

func TestBroadcastDryRunThenSend(t *testing.T) {
	// 4 → e (edit title+text) → p (preview) → s (send) → y
	out, f, st := drive(t,
		[]string{"4", "e", "p", "s", "y"},
		"维护\n今晚维护\ny\n")
	joined := strings.Join(f.calls(t), "\n")
	if !strings.Contains(joined, "POST /api/v1/broadcast?dry_run=true") {
		t.Fatalf("dry_run preview missing: %s", joined)
	}
	if strings.Count(joined, "broadcast") < 3 { // preview + send-confirm-dry + send
		t.Fatalf("real send missing: %s", joined)
	}
	if st.broadcastReach != 1234 {
		t.Fatalf("reach not parsed: %d", st.broadcastReach)
	}
	if !strings.Contains(out, "预计触达") {
		t.Fatalf("preview display missing: %s", out)
	}
}

func TestReportHandleFlow(t *testing.T) {
	out, f, _ := drive(t,
		[]string{"3", "g", "f", "y"},
		"-1\nresolve\nfixed\ny\n")
	joined := strings.Join(f.calls(t), "\n")
	if !strings.Contains(joined, "POST /api/v1/reports/7/handle") {
		t.Fatalf("report handle missing: %s", joined)
	}
	if !strings.Contains(out, "reports/7/handle") {
		t.Fatalf("confirm prompt missing: %s", out)
	}
}

func TestArchiveDryRunThenRunConfirm(t *testing.T) {
	out, f, st := drive(t,
		[]string{"6", "d", "r", "y"},
		"y\n")
	joined := strings.Join(f.calls(t), "\n")
	if !strings.Contains(joined, "POST /api/v1/archive/run?dry_run=true") {
		t.Fatalf("archive dry-run missing: %s", joined)
	}
	if !strings.Contains(joined, "POST /api/v1/archive/run {") || !strings.Contains(joined, "run?dry_run=false") && !strings.Contains(joined, "\nPOST /api/v1/archive/run") {
		// real run must appear as a second call without dry_run=true
	}
	found := false
	for _, c := range f.calls(t) {
		if strings.HasPrefix(c, "POST /api/v1/archive/run") && !strings.Contains(c, "dry_run=true") {
			found = true
		}
	}
	if !found {
		t.Fatalf("real archive run missing: %s", joined)
	}
	if st.archiveDryRows != 500 {
		t.Fatalf("dry rows not parsed: %d", st.archiveDryRows)
	}
	if !strings.Contains(out, "将搬移行数") {
		t.Fatalf("dry-run display missing: %s", out)
	}
}

func TestAuditLoadMore(t *testing.T) {
	_, f, st := drive(t,
		[]string{"7", "g", "j", "u", "m"},
		"")
	if len(st.audit) < 2 {
		t.Fatalf("audit rows missing: %d", len(st.audit))
	}
	if !st.auditExpand {
		t.Fatal("u should toggle detail expansion")
	}
	joined := strings.Join(f.calls(t), "\n")
	if !strings.Contains(joined, "since_id=99") {
		t.Fatalf("load-more should page via since_id: %s", joined)
	}
}

func TestOverviewRefresh(t *testing.T) {
	_, f, st := drive(t, []string{"1"}, "")
	a := newApp(newClient(f.srv.URL, "t", "c"), st, strings.NewReader(""), &strings.Builder{})
	a.refresh()
	if !st.connected || st.overview.Online != 1234 {
		t.Fatalf("overview not fetched: %+v", st.overview)
	}
}
