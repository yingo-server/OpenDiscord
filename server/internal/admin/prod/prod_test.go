package prod

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yingo-server/UFOT/server/internal/admin"
	"github.com/yingo-server/UFOT/server/internal/admin/bridgeproxy"
	"github.com/yingo-server/UFOT/server/internal/admin/identity"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
)

// seedSession writes an admin row + session row directly, returning the bearer
// token. Admin hashing/TOTP is covered by the bootstrap tests; here we only
// need a principal to prove the read ports are wired end to end.
func seedSession(t *testing.T, db *sql.DB, role identity.Role) string {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO adm_admins (admin_id, username, pw_hash, totp_secret, role, created_at)
VALUES (1, 'root', 'x', 'y', ?, 1)`, int(role)); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	token := "test-admin-token-0123456789"
	sum := sha256.Sum256([]byte(token))
	if _, err := db.Exec(`INSERT INTO adm_admin_sessions (sid, admin_id, expires_at, csrf)
VALUES (?, 1, ?, 'csrf-test')`, hex.EncodeToString(sum[:]), time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return token
}

func TestNewAdminServerWiresProductionPorts(t *testing.T) {
	hot := hotDB(t)
	adb := adminDB(t)
	seedUser(t, hot, 1, "alice", 0, 100)
	mustExec(t, hot, `INSERT INTO sys_feature_flags (key, value, version) VALUES ('chat.recall_window', '600', 1)`)

	disp := &fakeDispatch{}
	srv, err := NewAdminServerWithOptions(
		admin.Config{ListenAddr: "127.0.0.1:0"},
		hot, adb.DB, nil, disp, nil, nil,
		Options{SkipBootstrap: true},
	)
	if err != nil {
		t.Fatalf("NewAdminServer: %v", err)
	}
	if srv.Bridge() != nil {
		t.Fatal("dispatch-only assembly must not synthesize a bridge proxy")
	}
	h := srv.Handler()

	// health is public
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("health = %d", rec.Code)
	}

	// unauthenticated reads are 401 (middleware chain assembled)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/users", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated users = %d, want 401", rec.Code)
	}

	token := seedSession(t, adb.DB, identity.RoleSuper)
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// Users read port really queries the hot DB.
	rec = get("/api/v1/users?limit=10")
	if rec.Code != http.StatusOK {
		t.Fatalf("users = %d body=%s", rec.Code, rec.Body.String())
	}
	var page struct {
		Items []struct {
			UID         int64  `json:"uid"`
			DisplayName string `json:"display_name"`
		} `json:"items"`
		Total int64 `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("users json: %v body=%s", err, rec.Body.String())
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].UID != 1 || page.Items[0].DisplayName != "alice" {
		t.Fatalf("users page = %+v", page)
	}

	// Flags port
	rec = get("/api/v1/flags")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "chat.recall_window") {
		t.Fatalf("flags = %d body=%s", rec.Code, rec.Body.String())
	}

	// Archive port degrades to an explicit state, not an error.
	rec = get("/api/v1/archive/status")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "not_configured") {
		t.Fatalf("archive status = %d body=%s", rec.Code, rec.Body.String())
	}

	// Audit port returns an empty cursor page without a cold store.
	rec = get("/api/v1/audit?since_id=0&limit=10")
	if rec.Code != http.StatusOK {
		t.Fatalf("audit = %d body=%s", rec.Code, rec.Body.String())
	}

	// Messages search requires a query but must not 503.
	rec = get("/api/v1/messages/search?conv_id=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("messages = %d body=%s", rec.Code, rec.Body.String())
	}

	// Reports queue exists via the new hot migration.
	rec = get("/api/v1/reports")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"total":0`) {
		t.Fatalf("reports = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestNewAdminServerRequiresDispatcher(t *testing.T) {
	hot := hotDB(t)
	adb := adminDB(t)
	if _, err := NewAdminServer(admin.Config{ListenAddr: "127.0.0.1:0"}, hot, adb.DB, nil, nil, nil, nil); err == nil {
		t.Fatal("nil dispatcher accepted")
	}
	if _, err := NewAdminServer(admin.Config{ListenAddr: "127.0.0.1:0"}, hot, nil, nil, &fakeDispatch{}, nil, nil); err == nil {
		t.Fatal("nil admin.db accepted")
	}
}

func TestNewAdminServerRunsBootstrap(t *testing.T) {
	hot := hotDB(t)
	adb := adminDB(t)
	env := map[string]string{EnvAdminUser: "root", EnvAdminPassword: "a-very-long-admin-passphrase"}
	srv, err := NewAdminServerWithOptions(
		admin.Config{ListenAddr: "127.0.0.1:0"},
		hot, adb.DB, nil, &fakeDispatch{}, nil, discardLogger(t),
		Options{Getenv: func(k string) string { return env[k] }},
	)
	if err != nil {
		t.Fatalf("NewAdminServer: %v", err)
	}
	if srv == nil {
		t.Fatal("nil server")
	}
	var n int64
	if err := adb.DB.QueryRow(`SELECT COUNT(*) FROM adm_admins`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("bootstrap admins = %d, want 1", n)
	}
}

func TestBanRecorderMirrorsIdentityBans(t *testing.T) {
	db := hotDB(t)
	seedUser(t, db, 9, "mallory", 0, 1)
	users := NewUsers(db, UsersConfig{Now: fixedNow})
	fd := &fakeDispatch{}
	rec := &banRecorder{next: fd, users: users, lg: discardLogger(t)}
	ctx := context.Background()

	ban := []byte(`{"op":"ban","uid":9,"level":2,"reason":"spam","until":0,"by":"root"}`)
	if _, err := rec.InjectFrame(ctx, kinds.KindSecurityRequest, bridgeproxy.Target{}, ban); err != nil {
		t.Fatalf("ban frame: %v", err)
	}
	if fd.calls != 1 {
		t.Fatalf("dispatch calls = %d", fd.calls)
	}
	got, err := users.GetUser(ctx, 9)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.Ban == nil || got.Ban.Level != 2 || got.Ban.Reason != "spam" || got.Ban.CreatedBy != "root" {
		t.Fatalf("mirrored ban = %+v", got.Ban)
	}

	// unban appends the lift row
	if _, err := rec.InjectFrame(ctx, kinds.KindSecurityRequest, bridgeproxy.Target{},
		[]byte(`{"op":"unban","uid":9,"reason":"appeal"}`)); err != nil {
		t.Fatalf("unban frame: %v", err)
	}
	got, _ = users.GetUser(ctx, 9)
	if got.Ban != nil {
		t.Fatalf("ban still effective after unban: %+v", got.Ban)
	}

	// non-960 kinds and non-ban ops are untouched
	if _, err := rec.InjectFrame(ctx, kinds.KindAdminBroadcast, bridgeproxy.Target{},
		[]byte(`{"op":"ban","uid":9,"level":1}`)); err != nil {
		t.Fatalf("broadcast frame: %v", err)
	}
	if _, err := rec.InjectFrame(ctx, kinds.KindSecurityRequest, bridgeproxy.Target{},
		[]byte(`{"op":"kick","uid":9}`)); err != nil {
		t.Fatalf("kick frame: %v", err)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM identity_bans WHERE uid = 9`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("ban rows = %d, want 2 (ban + lift only)", rows)
	}

	// bridge failure ⇒ no local journal row (the frame was not accepted)
	fd.err = errors.New("bridge down")
	if _, err := rec.InjectFrame(ctx, kinds.KindSecurityRequest, bridgeproxy.Target{},
		[]byte(`{"op":"ban","uid":9,"level":3}`)); err == nil {
		t.Fatal("dispatch error swallowed")
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM identity_bans WHERE uid = 9`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("ban rows after failed dispatch = %d, want 2", rows)
	}
}

func TestParseSecurityCommand(t *testing.T) {
	cmd, ok := ParseSecurityCommand([]byte(`{"op":"user.ban","uid":7,"level":1,"until":99,"by":"a"}`))
	if !ok || cmd.Op != "ban" || cmd.UID != 7 || cmd.Until != 99 {
		t.Fatalf("parsed = %+v ok=%v", cmd, ok)
	}
	cmd, ok = ParseSecurityCommand([]byte(`{"op":" U NBAN "}`))
	if ok {
		t.Fatalf("garbage op accepted: %+v", cmd)
	}
	if _, ok := ParseSecurityCommand(nil); ok {
		t.Fatal("nil payload accepted")
	}
	if _, ok := ParseSecurityCommand([]byte(`not json`)); ok {
		t.Fatal("invalid json accepted")
	}
	if _, ok := ParseSecurityCommand([]byte(`{"op":"unban"}`)); !ok {
		t.Fatal("unban without uid must parse (target fallback)")
	}
}
