package main

// main_test.go: end-to-end smoke test of the embedded assembly.
//
// No mocks: a real SQLite hot DB (temp dir), the real embedded transport, the
// real user gateway, the real data pipe, the real bridge pipeline, a real HTTP
// listener on a random port and a real WebSocket client.
//
// Covered:
//   - GET /healthz  → 200
//   - WS upgrade    → authenticated with a test-signed Ed25519 access token
//   - MSG_SEND      → committed to SQLite and answered with MSG_ACK
//   - guest login   → /api/auth/guest, refresh rotation, replay → 401
//   - SIGTERM path  → shutdown() drains without panicking, run() returns

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/yingo-server/UFOT/server/internal/data/pipe/wire"
	"github.com/yingo-server/UFOT/server/internal/data/policy"
	"github.com/yingo-server/UFOT/server/internal/data/store"
	"github.com/yingo-server/UFOT/server/internal/pkg/log"
	"github.com/yingo-server/UFOT/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
	"github.com/yingo-server/UFOT/server/internal/user/core/authn"
	"github.com/yingo-server/UFOT/server/internal/user/net/frame"
)

const testUID int64 = 4242

func testConfig(t *testing.T) *config {
	t.Helper()
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.Listen = "127.0.0.1:0"
	cfg.AdminListen = "127.0.0.1:0"
	// The REST plane binds its own loopback listener; :0 keeps it ephemeral so
	// a developer machine (or a parallel test) cannot collide on 7781.
	cfg.AdminRESTListen = "127.0.0.1:0"
	cfg.PublicOrigin = ""
	cfg.HotDB = filepath.Join(dir, "hot.db")
	cfg.AdminDB = filepath.Join(dir, "admin.db")
	cfg.PolicyPath = filepath.Join(dir, "policy.json")
	cfg.JWTKeyPath = filepath.Join(dir, "keys", "jwt_sign_key.pem")
	cfg.PIIKeyPath = filepath.Join(dir, "keys", "phone_lookup_key")
	cfg.MediaDir = filepath.Join(dir, "media")
	cfg.LogLevel = "error"
	cfg.DevOTP = false
	cfg.ShutdownTimeout = 5

	pol := policy.Default()
	// The login-method switches default to OFF (fail-closed). These tests
	// log in as guests (and exercise the password path), so the test policy
	// must opt in explicitly — exactly like a real deployment does.
	pol.Auth.Methods.Guest = true
	pol.Auth.Methods.Password = true
	body, err := json.Marshal(pol)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	if err := os.WriteFile(cfg.PolicyPath, body, 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	return cfg
}

// startAssembly builds + runs the real process wiring and returns it with a
// cancel func; cleanup terminates it.
func startAssembly(t *testing.T) (*assembly, context.CancelFunc) {
	t.Helper()
	log.SetLevel(log.ErrorLevel)
	if os.Getenv("CHATD_TEST_LOG") == "debug" {
		log.SetLevel(log.DebugLevel)
	}
	cfg := testConfig(t)
	a, err := newAssembly(cfg, log.For("chatd-test"))
	if err != nil {
		t.Fatalf("newAssembly: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if addr := a.user.Addr(); addr != "" && !strings.HasSuffix(addr, ":0") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.HasSuffix(a.user.Addr(), ":0") {
		cancel()
		t.Fatalf("usergw did not start listening (addr=%q)", a.user.Addr())
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("assembly did not stop")
		}
		a.close()
	})
	return a, cancel
}

func TestHealthz(t *testing.T) {
	a, _ := startAssembly(t)
	resp, err := http.Get("http://" + a.user.Addr() + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode healthz: %v", err)
	}
	if body["ok"] != true {
		t.Fatalf("healthz body = %v", body)
	}
	if body["db"] != "ok" {
		t.Fatalf("healthz db = %v", body["db"])
	}
}

// signedAccessToken uses the very same key file the assembly generated, so the
// gateway's verifier accepts it (a real signed JWT, not an injected bypass).
func signedAccessToken(t *testing.T, cfgPath string) string {
	t.Helper()
	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read signing key: %v", err)
	}
	priv, err := parsePrivateKey(body)
	if err != nil {
		t.Fatalf("parse signing key: %v", err)
	}
	signer := newTestSigner(priv)
	return signer
}

func TestWebSocketMsgSendAck(t *testing.T) {
	a, _ := startAssembly(t)
	ctx := context.Background()

	token := signedAccessToken(t, a.cfg.JWTKeyPath)

	// The sender must be a member: create a conversation owned by testUID
	// through the real store.
	cid, err := a.hot.CreateConv(ctx, &store.ConvMeta{
		CType: 1, Title: "smoke", CreatedBy: testUID, CreatedAt: time.Now().Unix(),
	}, testUID)
	if err != nil {
		t.Fatalf("CreateConv: %v", err)
	}

	hdr := http.Header{}
	hdr.Set("Cookie", "access_token="+token)
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, resp, err := dialer.Dial("ws://"+a.user.Addr()+"/ws", hdr)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("ws dial: %v (status=%d)", err, status)
	}
	defer conn.Close()

	// --- unauthenticated upgrade must be refused before the socket exists ---
	if _, badResp, derr := dialer.Dial("ws://"+a.user.Addr()+"/ws", nil); derr == nil {
		t.Fatal("ws dial without a token succeeded")
	} else if badResp == nil || badResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %v, want 401", badResp)
	}

	// --- MSG_SEND → MSG_ACK ---
	payload, err := (&chatpb.MessagePayload{ClientMsgId: "smoke-1", Text: "hello"}).Marshal()
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	env := &chatpb.Envelope{
		Id: 1, Kind: kinds.KindMsgSend, RequestId: "trace-smoke",
		Target:  &chatpb.Envelope_ConvId{ConvId: uint64(cid)},
		Payload: payload,
	}
	raw, err := frame.Encode(env)
	if err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, raw); err != nil {
		t.Fatalf("ws write: %v", err)
	}

	// One deadline for the whole wait: gorilla treats any read error as fatal,
	// so a per-iteration timeout would poison the connection.
	_ = conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	var ack wire.MsgAck
	var seen []string
	for {
		_, data, rerr := conn.ReadMessage()
		if rerr != nil {
			rows, _ := a.hot.ReadHot(context.Background(), cid, 0, 10, true)
			t.Fatalf("no MSG_ACK: read failed after kinds=%v: %v (forwarder=%+v bridge=%v rows=%+v)",
				seen, rerr, a.user.Forwarder().Stats(), a.pipeline.Snapshot(), rows)
		}
		got, derr := frame.Decode(data)
		if derr != nil {
			seen = append(seen, "undecodable")
			continue
		}
		if got.Kind != kinds.KindMsgAck {
			detail := kinds.KindName(got.Kind)
			if got.Kind == kinds.KindError {
				detail += "(" + string(got.Payload) + ")"
			}
			seen = append(seen, detail) // heartbeats/control
			continue
		}
		if err := json.Unmarshal(got.Payload, &ack); err != nil {
			t.Fatalf("ack payload is not the wire MsgAck: %v (%q)", err, got.Payload)
		}
		break
	}
	if ack.ClientMsgID != "smoke-1" {
		t.Fatalf("ack client_msg_id = %q, want smoke-1", ack.ClientMsgID)
	}
	if ack.Seq == 0 || ack.MsgID == 0 {
		t.Fatalf("ack = %+v, want non-zero msg_id and seq", ack)
	}

	// The message really landed in SQLite.
	rows, err := a.hot.ReadHot(ctx, cid, 0, 10, true)
	if err != nil {
		t.Fatalf("ReadHot: %v", err)
	}
	if len(rows) != 1 || rows[0].SenderID != testUID || rows[0].Seq != int64(ack.Seq) {
		t.Fatalf("stored rows = %+v", rows)
	}
}

func TestAuthGuestRefreshReplay(t *testing.T) {
	a, _ := startAssembly(t)
	base := "http://" + a.user.Addr()

	var guest struct {
		UserID       int64  `json:"user_id"`
		DeviceID     string `json:"device_id"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		SessionID    string `json:"session_id"`
		ExpiresAt    int64  `json:"expires_at"`
		Guest        bool   `json:"guest"`
	}
	if code, body := postJSON(t, base+"/api/auth/guest", `{"nickname":"tester"}`, &guest); code != http.StatusOK {
		t.Fatalf("guest login status=%d body=%s", code, body)
	}
	if guest.UserID == 0 || guest.AccessToken == "" || guest.RefreshToken == "" {
		t.Fatalf("guest response = %+v", guest)
	}
	if guest.DeviceID != guest.SessionID || !strings.HasPrefix(guest.DeviceID, "g_") {
		t.Fatalf("guest device id = %q session = %q", guest.DeviceID, guest.SessionID)
	}
	if !guest.Guest {
		t.Fatal("guest flag not set")
	}
	if guest.ExpiresAt <= time.Now().UnixMilli() {
		t.Fatalf("expires_at (%d) is not a future unix-milli timestamp", guest.ExpiresAt)
	}

	// refresh rotates: the new token works, the old one triggers family revoke.
	var rotated struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if code, body := postJSON(t, base+"/api/auth/refresh",
		fmt.Sprintf(`{"refresh_token":%q}`, guest.RefreshToken), &rotated); code != http.StatusOK {
		t.Fatalf("refresh status=%d body=%s", code, body)
	}
	if rotated.RefreshToken == "" || rotated.RefreshToken == guest.RefreshToken {
		t.Fatal("refresh token was not rotated")
	}

	// Replay of the consumed token → 401 REFRESH_REPLAY.
	var replay struct {
		Code string `json:"code"`
	}
	if code, body := postJSON(t, base+"/api/auth/refresh",
		fmt.Sprintf(`{"refresh_token":%q}`, guest.RefreshToken), &replay); code != http.StatusUnauthorized {
		t.Fatalf("replay status=%d body=%s, want 401", code, body)
	}
	if replay.Code != "REFRESH_REPLAY" {
		t.Fatalf("replay code = %q, want REFRESH_REPLAY", replay.Code)
	}

	// The whole family is revoked, so even the rotated token is now dead.
	if code, body := postJSON(t, base+"/api/auth/refresh",
		fmt.Sprintf(`{"refresh_token":%q}`, rotated.RefreshToken), &replay); code != http.StatusUnauthorized {
		t.Fatalf("family revoke check status=%d body=%s, want 401", code, body)
	}
}

func TestGracefulShutdown(t *testing.T) {
	a, cancel := startAssembly(t)
	if err := a.hot.PingContext(context.Background()); err != nil {
		t.Fatalf("hot ping: %v", err)
	}
	// shutdown must be safe to run while the layers are live and idempotent.
	cancel()
	time.Sleep(50 * time.Millisecond)
	a.shutdown()
}

// ---------------------------------------------------------------- helpers

// newTestSigner mints an access token with the production signer, so the test
// exercises the exact claims/code path a real login uses.
func newTestSigner(priv ed25519.PrivateKey) string {
	signer := authn.NewSigner(priv)
	tok, err := signer.SignAccess(testUID, "dev-smoke", 15*time.Minute)
	if err != nil {
		panic("sign test access token: " + err.Error())
	}
	return tok
}

func postJSON(t *testing.T, url, body string, out any) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil && resp.StatusCode == http.StatusOK {
			t.Fatalf("decode %s: %v (%s)", url, err, raw)
		}
	}
	return resp.StatusCode, string(raw)
}

// ensure httptest stays referenced for future HTTP-level cases.
var _ = httptest.NewRequest

func TestReloadAndStatusDump(t *testing.T) {
	a, _ := startAssembly(t)

	// SIGUSR1 path: one JSON line with the expected shape.
	var buf bytes.Buffer
	a.dump(&buf)
	var dump struct {
		Event  string         `json:"event"`
		Status map[string]any `json:"status"`
	}
	if err := json.Unmarshal(buf.Bytes(), &dump); err != nil {
		t.Fatalf("dump is not JSON: %v (%s)", err, buf.String())
	}
	if dump.Event != "status_dump" || dump.Status["conns"] == nil || dump.Status["bridge"] == nil {
		t.Fatalf("dump = %+v", dump)
	}

	if a.policy == nil {
		t.Fatal("policy store missing")
	}
	before := a.policy.Current().Version

	// SIGHUP path: rewrite policy + change a flag, then reload.
	changed := policy.Default()
	changed.HotRetentionDays = 45
	body, err := json.Marshal(changed)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	if err := os.WriteFile(a.cfg.PolicyPath, body, 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	if err := a.hot.UpsertFlag(context.Background(), &store.Flag{Key: "video_enabled", Value: "true"}); err != nil {
		t.Fatalf("upsert flag: %v", err)
	}
	a.reload(context.Background())

	if got := a.policy.Current().Version; got != before+1 {
		t.Fatalf("policy version = %d, want %d", got, before+1)
	}
	if !a.flags.Bool("video_enabled", false) {
		t.Fatal("flag delta was not applied on reload")
	}
}

// waitAddr blocks until the plane has bound a real port (Run resolves :0 in its
// own goroutine, so reading Addr() too early yields ":0").
func waitAddr(t *testing.T, addr func() string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		a := addr()
		if a != "" && !strings.HasSuffix(a, ":0") {
			return a
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("listener never bound: %q", addr())
	return ""
}

// TestRateLimitWiredFromPolicy (P0-2): the policy.json rate_limit section must
// actually reach the user gateway's limiter — both at startup and after a
// SIGHUP-style reload. Semantics: the MSG_SEND rule converges onto
// min(policy-derived user-level budget, current default); see limitwire.go.
func TestRateLimitWiredFromPolicy(t *testing.T) {
	a, _ := startAssembly(t)

	msgRule := func() (rate, burst, capacity float64) {
		r := a.user.Limiter().Config().Rules[uint32(kinds.KindMsgSend)]
		return r.Rate, r.Burst, r.Capacity
	}

	// Startup wiring: policy.Default() has msg_per_user_per_min=60 →
	// rate=1/s, burst=30, capacity=60 — all tighter than the 20/40/100
	// security-spec baseline, so the converged values must equal the
	// policy-derived ones.
	rate, burst, cap := msgRule()
	if rate != 1 || burst != 30 || cap != 60 {
		t.Fatalf("startup MSG_SEND rule = (%v,%v,%v), want policy-derived (1,30,60)", rate, burst, cap)
	}

	// Reload with a stricter policy: 120/min derives rate=2 (< baseline 20),
	// burst=60 (> baseline 40 → clamps to 40), capacity=120 (> baseline 100 →
	// clamps to 100). Each field is independently min(policy, baseline).
	changed := policy.Default()
	changed.RateLimit.MsgPerUserPerMin = 120
	body, err := json.Marshal(changed)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	if err := os.WriteFile(a.cfg.PolicyPath, body, 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	a.reload(context.Background())
	rate, burst, cap = msgRule()
	if rate != 2 || burst != 40 || cap != 100 {
		t.Fatalf("after reload MSG_SEND rule = (%v,%v,%v), want (2,40,100)", rate, burst, cap)
	}

	// Reload with a huge policy value: 12000/min derives 200/6000/12000, but
	// every field is capped at the security-spec baseline (20/40/100) — the
	// limiter must not get looser than the documented baseline from this path.
	changed.RateLimit.MsgPerUserPerMin = 12000
	body, err = json.Marshal(changed)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	if err := os.WriteFile(a.cfg.PolicyPath, body, 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	a.reload(context.Background())
	rate, burst, cap = msgRule()
	if rate != 20 || burst != 40 || cap != 100 {
		t.Fatalf("huge policy must clamp to baseline: MSG_SEND rule = (%v,%v,%v), want (20,40,100)", rate, burst, cap)
	}

	// Non-MSG_SEND kinds keep the security-spec baseline untouched.
	typing := a.user.Limiter().Config().Rules[uint32(kinds.KindTyping)]
	if typing.Rate != 1 || typing.Burst != 3 {
		t.Fatalf("typing rule drifted from baseline: %+v", typing)
	}
}

func TestAdminSurfaceHealthz(t *testing.T) {
	a, _ := startAssembly(t)
	adminAddr := waitAddr(t, a.admin.Addr)
	restAddr := waitAddr(t, a.adminREST.Addr)
	resp, err := http.Get("http://" + adminAddr + "/healthz")
	if err != nil {
		t.Fatalf("admin healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin healthz status = %d", resp.StatusCode)
	}
	// /api/admin/* on the injector port is a misroute: the RBAC REST layer has
	// its own loopback listener (admin_rest_listen).
	resp2, err := http.Post("http://"+adminAddr+"/api/admin/v1/users", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("admin api: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("injector port /api/admin status = %d, want 404", resp2.StatusCode)
	}

	// Unauthenticated request to the real REST surface must be 401, not 404:
	// that proves the RBAC layer is mounted and enforcing.
	url := "http://" + restAddr + "/api/v1/whoami"
	resp3, err := http.Get(url)
	if err != nil {
		t.Fatalf("admin rest whoami: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusUnauthorized {
		t.Fatalf("rest whoami status = %d, want 401", resp3.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// 1.0 acceptance: two real guest users, two real WS connections, a direct
// conversation created through the user-facing CONV_CREATE frame, then a
// message from A that B must receive as MSG_NEW.
// ---------------------------------------------------------------------------

type guestSession struct {
	UserID       int64  `json:"user_id"`
	DeviceID     string `json:"device_id"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Guest        bool   `json:"guest"`
}

func loginGuest(t *testing.T, base, nickname string) guestSession {
	t.Helper()
	var g guestSession
	if code, body := postJSON(t, base+"/api/auth/guest",
		fmt.Sprintf(`{"nickname":%q}`, nickname), &g); code != http.StatusOK {
		t.Fatalf("guest login %q: status=%d body=%s", nickname, code, body)
	}
	if g.UserID == 0 || g.AccessToken == "" {
		t.Fatalf("guest login %q returned %+v", nickname, g)
	}
	return g
}

func dialToken(t *testing.T, addr, token string) *websocket.Conn {
	t.Helper()
	hdr := http.Header{}
	hdr.Set("Cookie", "access_token="+token)
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, resp, err := dialer.Dial("ws://"+addr+"/ws", hdr)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("ws dial: %v (status=%d)", err, status)
	}
	return conn
}

// readKind reads frames until kind appears. Every other frame is recorded so
// failures can show exactly what the server sent instead.
func readKind(t *testing.T, conn *websocket.Conn, kind uint32, timeout time.Duration) (*chatpb.Envelope, []string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	var seen []string
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("waiting for %s: read failed after kinds=%v: %v",
				kinds.KindName(kind), seen, err)
		}
		env, derr := frame.Decode(data)
		if derr != nil {
			seen = append(seen, "undecodable")
			continue
		}
		if env.Kind == kind {
			return env, seen
		}
		detail := kinds.KindName(env.Kind)
		if env.Kind == kinds.KindError {
			detail += "(" + string(env.Payload) + ")"
		}
		seen = append(seen, detail)
	}
}

func TestE2EConvCreateThenPeerMsgNew(t *testing.T) {
	a, _ := startAssembly(t)
	base := "http://" + a.user.Addr()

	alice := loginGuest(t, base, "alice")
	bob := loginGuest(t, base, "bob")
	if alice.UserID == bob.UserID {
		t.Fatalf("guest uids collided: %d", alice.UserID)
	}

	ca := dialToken(t, a.user.Addr(), alice.AccessToken)
	defer ca.Close()
	cb := dialToken(t, a.user.Addr(), bob.AccessToken)
	defer cb.Close()
	// The acceptor registers the session in OnConnect; give it one scheduler
	// slot before the first routed request so uid resolution never races.
	time.Sleep(100 * time.Millisecond)

	// --- A: CONV_CREATE (direct, member = B) ---
	createPayload, err := (&chatpb.ConvCreate{
		Title: "", ConvType: 1, MemberIds: []uint64{uint64(bob.UserID)},
	}).Marshal()
	if err != nil {
		t.Fatalf("marshal ConvCreate: %v", err)
	}
	createRaw, err := frame.Encode(&chatpb.Envelope{
		Id: 1, Kind: kinds.KindConvCreate, RequestId: "e2e-conv-create", Payload: createPayload,
	})
	if err != nil {
		t.Fatalf("encode CONV_CREATE: %v", err)
	}
	if err := ca.WriteMessage(websocket.BinaryMessage, createRaw); err != nil {
		t.Fatalf("write CONV_CREATE: %v", err)
	}

	resEnv, seenA := readKind(t, ca, kinds.KindConvDataResult, 8*time.Second)
	var res wire.DataResult
	if err := json.Unmarshal(resEnv.Payload, &res); err != nil {
		t.Fatalf("931 payload is not wire.DataResult: %v (%q)", err, resEnv.Payload)
	}
	if !res.Ok {
		t.Fatalf("CONV_CREATE failed: op=%d error=%q (seen=%v)", res.Op, res.Error, seenA)
	}
	var created wire.ConvPayload
	if err := json.Unmarshal(res.Body, &created); err != nil {
		t.Fatalf("931 body is not wire.ConvPayload: %v (%q)", err, res.Body)
	}
	if created.ConvID == 0 {
		t.Fatalf("931 body has no cid: %+v", created)
	}
	if created.ConvType != 1 || len(created.Members) != 2 {
		t.Fatalf("created conv = %+v, want direct with 2 members", created)
	}
	cid := int64(created.ConvID)
	t.Logf("E2E: A received 931 OK cid=%d members=%d (seen before result: %v)", cid, len(created.Members), seenA)

	// --- A: same pair again must return the SAME cid (no duplicate direct chat) ---
	if err := ca.WriteMessage(websocket.BinaryMessage, createRaw); err != nil {
		t.Fatalf("write duplicate CONV_CREATE: %v", err)
	}
	dupEnv, _ := readKind(t, ca, kinds.KindConvDataResult, 8*time.Second)
	var dupRes wire.DataResult
	if err := json.Unmarshal(dupEnv.Payload, &dupRes); err != nil || !dupRes.Ok {
		t.Fatalf("duplicate CONV_CREATE failed: %v %q", err, dupEnv.Payload)
	}
	var dupConv wire.ConvPayload
	if err := json.Unmarshal(dupRes.Body, &dupConv); err != nil {
		t.Fatalf("duplicate 931 body: %v", err)
	}
	if int64(dupConv.ConvID) != cid {
		t.Fatalf("duplicate CONV_CREATE created cid=%d, want existing %d", dupConv.ConvID, cid)
	}
	t.Logf("E2E: duplicate CONV_CREATE returned existing cid=%d", dupConv.ConvID)

	// --- A: MSG_SEND to the new cid ---
	msgPayload, err := (&chatpb.MessagePayload{ClientMsgId: "e2e-msg-1", Text: "hello from alice"}).Marshal()
	if err != nil {
		t.Fatalf("marshal MessagePayload: %v", err)
	}
	msgRaw, err := frame.Encode(&chatpb.Envelope{
		Id: 2, Kind: kinds.KindMsgSend, RequestId: "e2e-msg-send",
		Target: &chatpb.Envelope_ConvId{ConvId: uint64(cid)}, Payload: msgPayload,
	})
	if err != nil {
		t.Fatalf("encode MSG_SEND: %v", err)
	}
	if err := ca.WriteMessage(websocket.BinaryMessage, msgRaw); err != nil {
		t.Fatalf("write MSG_SEND: %v", err)
	}
	// Wait for A's ack first: MSG_NEW is emitted only after the group commit.
	ackEnv, seenAck := readKind(t, ca, kinds.KindMsgAck, 8*time.Second)
	var ack wire.MsgAck
	if err := json.Unmarshal(ackEnv.Payload, &ack); err != nil {
		t.Fatalf("MSG_ACK payload: %v (%q)", err, ackEnv.Payload)
	}
	if ack.ClientMsgID != "e2e-msg-1" || ack.Seq == 0 {
		t.Fatalf("ack = %+v (seen=%v)", ack, seenAck)
	}

	// --- B: must receive MSG_NEW (202 CONV_PAYLOAD may precede it) ---
	newEnv, seenB := readKind(t, cb, kinds.KindMsgNew, 8*time.Second)
	var got struct {
		MsgID    uint64 `json:"msg_id"`
		Seq      uint64 `json:"seq"`
		SenderID uint64 `json:"sender_id"`
		SentAt   int64  `json:"sent_at"`
		ConvID   uint64 `json:"conv_id"`
		Content  struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(newEnv.Payload, &got); err != nil {
		t.Fatalf("MSG_NEW payload: %v (%q)", err, newEnv.Payload)
	}
	if got.ConvID != uint64(cid) || got.SenderID != uint64(alice.UserID) {
		t.Fatalf("MSG_NEW = %+v, want conv=%d sender=%d", got, cid, alice.UserID)
	}
	if got.Content.Text != "hello from alice" {
		t.Fatalf("MSG_NEW text = %q", got.Content.Text)
	}
	if got.Seq == 0 || got.MsgID == 0 {
		t.Fatalf("MSG_NEW missing ids: %+v", got)
	}
	t.Logf("E2E: B received MSG_NEW msg_id=%d seq=%d conv=%d sender=%d text=%q (seen before: %v)",
		got.MsgID, got.Seq, got.ConvID, got.SenderID, got.Content.Text, seenB)
}

// ---------------------------------------------------------------------------
// Task 1: production missing keys must abort startup (deploy/keygen guidance);
// dev keeps first-boot auto-generation.
// ---------------------------------------------------------------------------

func TestProductionMissingKeysFails(t *testing.T) {
	t.Setenv("APP_ENV", "prod")
	cfg := testConfig(t)
	cfg.PublicOrigin = "https://chat.example.com" // required for production mode
	// testConfig wrote only policy.json; key files do not exist.

	a, err := newAssembly(cfg, log.For("chatd-test"))
	if err == nil {
		if a != nil {
			a.close()
		}
		t.Fatal("newAssembly succeeded in production without provisioned keys")
	}
	if !strings.Contains(err.Error(), "keygen") {
		t.Fatalf("error = %v, want a deploy/keygen hint", err)
	}
}

func TestDevMissingKeysAutoGenerated(t *testing.T) {
	t.Setenv("APP_ENV", "dev")
	cfg := testConfig(t)
	cfg.PublicOrigin = "https://chat.example.com"

	a, err := newAssembly(cfg, log.For("chatd-test"))
	if err != nil {
		t.Fatalf("dev newAssembly: %v", err)
	}
	defer a.close()
	if _, err := os.Stat(cfg.JWTKeyPath); err != nil {
		t.Fatalf("signing key was not auto-generated in dev: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Task 2: require_audit_store fail-fast switch.
// ---------------------------------------------------------------------------

func TestAuditStoreRequireFails(t *testing.T) {
	cfg := testConfig(t)
	cfg.RequireAuditStore = true
	cfg.ColdDSN = "" // no cold store ⇒ coldDB == nil

	a, err := newAssembly(cfg, log.For("chatd-test"))
	if err == nil {
		if a != nil {
			a.close()
		}
		t.Fatal("require_audit_store=true must fail when no cold DB is available")
	}
	if !strings.Contains(err.Error(), "cold_dsn") {
		t.Fatalf("error = %v, want a cold_dsn / driver hint", err)
	}
}

func TestAuditStoreDegradedStartsAndLogsError(t *testing.T) {
	// Capture the structured log so we can assert the degraded line is ERROR.
	log.SetLevel(log.ErrorLevel)
	lf, err := os.CreateTemp(t.TempDir(), "chatd-log-*")
	if err != nil {
		t.Fatalf("create log: %v", err)
	}
	defer lf.Close()
	log.SetOutput(lf)
	defer log.SetOutput(os.Stdout)

	cfg := testConfig(t)
	cfg.RequireAuditStore = false
	cfg.ColdDSN = ""

	a, err := newAssembly(cfg, log.For("chatd-test"))
	if err != nil {
		t.Fatalf("require_audit_store=false must still start: %v", err)
	}
	a.close()

	body, rerr := os.ReadFile(lf.Name())
	if rerr != nil {
		t.Fatalf("read log: %v", rerr)
	}
	out := string(body)
	if !strings.Contains(out, "audit store degraded") {
		t.Fatalf("expected 'audit store degraded' in logs, got: %s", out)
	}
	if !strings.Contains(out, `"level":"ERROR"`) {
		t.Fatalf("degraded audit log must be ERROR level, got: %s", out)
	}
}
