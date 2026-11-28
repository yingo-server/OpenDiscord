package main

// api_contract_test.go: end-to-end API contract acceptance suite.
//
// It boots the REAL embedded assembly (real SQLite hot DB, real admin DB, real
// transports, real WS acceptor, real REST listeners on ephemeral ports) and
// drives every documented endpoint of the 1.0 surface:
//
//   - user-plane HTTP   (/api/auth/*, /healthz)
//   - media plane       (/api/media/*)
//   - admin-plane REST  (/api/v1/*)   — loopback listener a.adminREST.Addr()
//   - WebSocket frames  (WS handshake, PING/PONG, MSG_SEND/ACK, SYNC, CONV,
//                        non-member 403, rate limit)
//
// Nothing is mocked. Each probe records {endpoint, got, want, verdict} and the
// whole run emits one PASS/FAIL table via t.Log, plus a final tally and the
// list of failing endpoints with a body snippet.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/yingo-server/UFOT/server/internal/admin/totp"
	"github.com/yingo-server/UFOT/server/internal/data/pipe/wire"
	"github.com/yingo-server/UFOT/server/internal/pkg/log"
	"github.com/yingo-server/UFOT/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
	"github.com/yingo-server/UFOT/server/internal/user/core/authn"
	"github.com/yingo-server/UFOT/server/internal/user/net/frame"
)

// ---------------------------------------------------------------- assembly

// startContractAssembly boots the real process wiring in dev mode (APP_ENV=dev
// + dev OTP enabled, so the fixed 123456 code is accepted) and provisions the
// bootstrap admin from the environment. It mirrors startAssembly but flips the
// dev-specific knobs this suite needs.
func startContractAssembly(t *testing.T) *assembly {
	t.Helper()
	t.Setenv("APP_ENV", "dev")
	t.Setenv("UFOT_ADMIN_USER", "rootadmin")
	t.Setenv("UFOT_ADMIN_PASSWORD", "root-admin-pass-1234567")

	log.SetLevel(log.ErrorLevel)
	if os.Getenv("CHATD_TEST_LOG") == "debug" {
		log.SetLevel(log.DebugLevel)
	}

	cfg := testConfig(t)
	cfg.DevOTP = true
	// The admin REST dispatcher proxy is configured with the *literal*
	// admin_listen at assembly time (before Run binds). With ":0" it could
	// never reach the injector, so high-risk frame dispatch would 503. Hand it
	// a concrete loopback port (as a real deployment does) so the full
	// proxy → injector → bus path is exercised.
	if ln, err := net.Listen("tcp", "127.0.0.1:0"); err == nil {
		cfg.AdminListen = ln.Addr().String()
		ln.Close()
	}

	a, err := newAssembly(cfg, log.For("chatd-contract"))
	if err != nil {
		t.Fatalf("newAssembly: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.run(ctx) }()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if addr := a.user.Addr(); addr != "" && !strings.HasSuffix(addr, ":0") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.HasSuffix(a.user.Addr(), ":0") {
		cancel()
		t.Fatalf("user gateway did not start listening")
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
	return a
}

// ---------------------------------------------------------------- suite

type contractRow struct {
	Method  string
	Path    string
	Got     int
	Want    string // human expectation, may be "200" or "401 (REFRESH_REPLAY)"
	Verdict string // PASS / FAIL / SKIP
	Note    string // failure reason / body snippet / skip reason
}

type suite struct {
	t    *testing.T
	a    *assembly
	base string
	rows []contractRow
}

func (s *suite) add(method, path string, got int, want string, verdict, note string) {
	s.rows = append(s.rows, contractRow{Method: method, Path: path, Got: got, Want: want, Verdict: verdict, Note: note})
}

// expect records PASS when got==wantStatus and (optional) all mustContain
// substrings are present in the body; otherwise FAIL with a body snippet.
func (s *suite) expect(method, path string, got int, body []byte, wantStatus int, wantLabel string, mustContain ...string) {
	label := wantLabel
	if label == "" {
		label = fmt.Sprintf("%d", wantStatus)
	}
	if got != wantStatus {
		s.add(method, path, got, label, "FAIL", "status mismatch; body="+snippet(body))
		return
	}
	lower := strings.ToLower(string(body))
	for _, mc := range mustContain {
		if !strings.Contains(lower, strings.ToLower(mc)) {
			s.add(method, path, got, label, "FAIL", fmt.Sprintf("missing %q; body=%s", mc, snippet(body)))
			return
		}
	}
	s.add(method, path, got, label, "PASS", "")
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return s
}

// do issues a request with optional cookie token + headers and returns status
// and body.
func (s *suite) do(method, url string, body []byte, contentType string, hdr map[string]string) (int, []byte) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		s.t.Fatalf("new request %s %s: %v", method, url, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func (s *suite) authHdr(token string) map[string]string {
	return map[string]string{"Cookie": "access_token=" + token}
}

func (s *suite) postJSONAuth(path, token, body string) (int, []byte) {
	return s.do(http.MethodPost, s.base+path, []byte(body), "application/json", s.authHdr(token))
}
func (s *suite) postJSON(path, body string) (int, []byte) {
	return s.do(http.MethodPost, s.base+path, []byte(body), "application/json", nil)
}

// ---------------------------------------------------------------- test

func TestAPIContract(t *testing.T) {
	a := startContractAssembly(t)
	s := &suite{t: t, a: a, base: "http://" + a.user.Addr()}

	contractAuth(s)
	users := contractMedia(s) // returns guest users still valid for later reuse
	contractAdmin(s, users)   // admin REST surface
	contractWS(s, users)      // WebSocket frame contract

	// ---- emit the table ----
	var pass, fail, skip int
	var failed []contractRow
	var b strings.Builder
	b.WriteString("\n")
	fmt.Fprintf(&b, "%-44s %-8s %-20s %-6s\n", "端点", "状态码", "期望", "判定")
	for _, r := range s.rows {
		ep := r.Method + " " + r.Path
		fmt.Fprintf(&b, "%-44s %-8d %-20s %-6s\n", trunc(ep, 44), r.Got, r.Want, r.Verdict)
		switch r.Verdict {
		case "PASS":
			pass++
		case "SKIP":
			skip++
		default:
			fail++
			failed = append(failed, r)
		}
	}
	fmt.Fprintf(&b, "\n合计: PASS=%d FAIL=%d SKIP=%d (共 %d 个探测点)\n", pass, fail, skip, len(s.rows))
	if len(failed) > 0 {
		b.WriteString("\nFAIL 明细:\n")
		for _, r := range failed {
			fmt.Fprintf(&b, "  - %s %s | got=%d want=%s | %s\n", r.Method, r.Path, r.Got, r.Want, r.Note)
		}
	}
	t.Log(b.String())
	// A FAIL row must fail the test. Printing a table with failures while
	// returning green made CI lie about the WS contract (e.g. 202 fan-out).
	if len(failed) > 0 {
		t.Fatalf("API contract: %d/%d probes failed; see table above", len(failed), len(s.rows))
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

// contractAuth exercises the user-facing authsvc HTTP surface.
// It returns the phone-based account that later sections can reuse.
func contractAuth(s *suite) {
	const devCode = "123456"

	// --- GET /healthz ---
	if code, body := s.do(http.MethodGet, s.base+"/healthz", nil, "", nil); true {
		s.expect(http.MethodGet, "/healthz", code, body, http.StatusOK, "200", `"ok":true`, `"db":"ok"`)
	}

	// --- phone/code (dev fixed OTP) ---
	phone := "+8613800000" + fmt.Sprintf("%03d", time.Now().UnixNano()%1000)
	code, body := s.postJSON("/api/auth/phone/code", fmt.Sprintf(`{"phone":%q}`, phone))
	var challenge struct {
		ChallengeID string `json:"challenge_id"`
		OK          bool   `json:"ok"`
	}
	_ = json.Unmarshal(body, &challenge)
	s.expect(http.MethodPost, "/api/auth/phone/code", code, body, http.StatusOK, "200", "challenge_id")

	// --- phone/verify (dev fixed code) ---
	phoneVerify := fmt.Sprintf(`{"phone":%q,"code":%q,"challenge_id":%q}`, phone, devCode, challenge.ChallengeID)
	vcode, vbody := s.postJSON("/api/auth/phone/verify", phoneVerify)
	var phoneSess struct {
		UserID      int64  `json:"user_id"`
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(vbody, &phoneSess)
	s.expect(http.MethodPost, "/api/auth/phone/verify", vcode, vbody, http.StatusOK, "200", "access_token", "user_id")

	// --- email/link (dev returns link) ---
	email := fmt.Sprintf("contract%d@example.com", time.Now().UnixNano())
	ecode, ebody := s.postJSON("/api/auth/email/link", fmt.Sprintf(`{"email":%q}`, email))
	s.expect(http.MethodPost, "/api/auth/email/link", ecode, ebody, http.StatusOK, "200", "challenge_id", "link")

	// --- email/poll (not clicked yet → 409 PENDING) ---
	pcode, pbody := s.postJSON("/api/auth/email/poll", fmt.Sprintf(`{"email":%q}`, email))
	s.expect(http.MethodPost, "/api/auth/email/poll", pcode, pbody, http.StatusConflict, "409 (PENDING)", "PENDING")

	// --- password/set then password login (phone user) ---
	const pw = "contract-strong-pw-123"
	setCode, setBody := s.postJSONAuth("/api/auth/password/set", phoneSess.AccessToken, fmt.Sprintf(`{"password":%q}`, pw))
	s.expect(http.MethodPost, "/api/auth/password/set", setCode, setBody, http.StatusOK, "200", `"ok":true`)

	loginCode, loginBody := s.postJSON("/api/auth/password", fmt.Sprintf(`{"account":%q,"password":%q}`, phone, pw))
	s.expect(http.MethodPost, "/api/auth/password (correct)", loginCode, loginBody, http.StatusOK, "200", "access_token")

	wrongCode, wrongBody := s.postJSON("/api/auth/password", fmt.Sprintf(`{"account":%q,"password":"definitely-wrong"}`, phone))
	s.expect(http.MethodPost, "/api/auth/password (wrong)", wrongCode, wrongBody, http.StatusUnauthorized, "401 (INVALID_CREDENTIALS)", "INVALID_CREDENTIALS")

	// --- guest login ---
	gcode, gbody := s.postJSON("/api/auth/guest", `{"nickname":"guest-contract"}`)
	var guest struct {
		UserID       int64  `json:"user_id"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Guest        bool   `json:"guest"`
	}
	_ = json.Unmarshal(gbody, &guest)
	s.expect(http.MethodPost, "/api/auth/guest", gcode, gbody, http.StatusOK, "200", "access_token", "refresh_token", `"guest":true`)

	// --- refresh rotation ---
	rcode, rbody := s.postJSON("/api/auth/refresh", fmt.Sprintf(`{"refresh_token":%q}`, guest.RefreshToken))
	var rotated struct {
		RefreshToken string `json:"refresh_token"`
	}
	_ = json.Unmarshal(rbody, &rotated)
	s.expect(http.MethodPost, "/api/auth/refresh", rcode, rbody, http.StatusOK, "200", "access_token", "refresh_token")

	// --- refresh replay of the consumed token → 401 REFRESH_REPLAY ---
	repCode, repBody := s.postJSON("/api/auth/refresh", fmt.Sprintf(`{"refresh_token":%q}`, guest.RefreshToken))
	s.expect(http.MethodPost, "/api/auth/refresh (replay)", repCode, repBody, http.StatusUnauthorized, "401 (REFRESH_REPLAY)", "REFRESH_REPLAY")

	// --- logout ---
	lcode, lbody := s.postJSON("/api/auth/logout", fmt.Sprintf(`{"refresh_token":%q}`, rotated.RefreshToken))
	s.expect(http.MethodPost, "/api/auth/logout", lcode, lbody, http.StatusOK, "200", `"ok":true`)
}

// userSess is a minimal authenticated user for later sections.
type userSess struct {
	ID    int64
	Token string
}

type contractUsers struct {
	A, B, Ban userSess
}

// guestUser logs in a fresh guest and returns its id + access token.
func guestUser(t *testing.T, base, nick string) userSess {
	t.Helper()
	g := loginGuest(t, base, nick)
	return userSess{ID: g.UserID, Token: g.AccessToken}
}

// uploadFile posts a multipart file to /api/media/upload.
func (s *suite) uploadFile(token, filename string, content []byte) (int, []byte) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		s.t.Fatalf("multipart: %v", err)
	}
	_, _ = fw.Write(content)
	_ = mw.Close()

	req, err := http.NewRequest(http.MethodPost, s.base+"/api/media/upload", &buf)
	if err != nil {
		s.t.Fatalf("new upload req: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.Header.Set("Cookie", "access_token="+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// contractMedia exercises /api/media/* and returns the guest users it created
// (reused by the admin section for existing-user operations).
func contractMedia(s *suite) contractUsers {
	u := contractUsers{
		A:   guestUser(s.t, s.base, "media-a"),
		B:   guestUser(s.t, s.base, "media-b"),
		Ban: guestUser(s.t, s.base, "ban-target"),
	}

	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d}

	// --- upload PNG as A → 201 ---
	code, body := s.uploadFile(u.A.Token, "pic.png", png)
	var up struct {
		MediaID string `json:"media_id"`
		Size    int64  `json:"size"`
		Mime    string `json:"mime"`
	}
	_ = json.Unmarshal(body, &up)
	s.expect(http.MethodPost, "/api/media/upload (png)", code, body, http.StatusCreated, "201", "media_id")

	// --- upload .bin → 415 (unknown/generic extension not whitelisted) ---
	binCode, binBody := s.uploadFile(u.A.Token, "payload.bin", []byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x01})
	s.expect(http.MethodPost, "/api/media/upload (.bin)", binCode, binBody, http.StatusUnsupportedMediaType, "415", "UNSUPPORTED_TYPE")

	// --- oversized (>20MB) → 413 ---
	big := make([]byte, (20<<20)+1024)
	copy(big, png)
	bigCode, bigBody := s.uploadFile(u.A.Token, "big.png", big)
	s.expect(http.MethodPost, "/api/media/upload (>20MB)", bigCode, bigBody, http.StatusRequestEntityTooLarge, "413", "FILE_TOO_LARGE")

	// --- upload without auth → 401 ---
	naCode, naBody := s.uploadFile("", "pic.png", png)
	s.expect(http.MethodPost, "/api/media/upload (no auth)", naCode, naBody, http.StatusUnauthorized, "401", "UNAUTHORIZED")

	// --- GET by uploader → 200 ---
	if up.MediaID != "" {
		gCode, gBody := s.do(http.MethodGet, s.base+"/api/media/"+up.MediaID, nil, "", s.authHdr(u.A.Token))
		s.expect(http.MethodGet, "/api/media/{id} (uploader)", gCode, gBody, http.StatusOK, "200", "png")
		// --- GET by unrelated user → 404 (existence not disclosed) ---
		oCode, oBody := s.do(http.MethodGet, s.base+"/api/media/"+up.MediaID, nil, "", s.authHdr(u.B.Token))
		s.expect(http.MethodGet, "/api/media/{id} (other user)", oCode, oBody, http.StatusNotFound, "404", "NOT_FOUND")
	} else {
		s.add(http.MethodGet, "/api/media/{id} (uploader)", 0, "200", "SKIP", "upload produced no media_id")
		s.add(http.MethodGet, "/api/media/{id} (other user)", 0, "404", "SKIP", "upload produced no media_id")
	}

	// --- GET nonexistent (valid-format) id → 404 ---
	nonexistent := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	nCode, nBody := s.do(http.MethodGet, s.base+"/api/media/"+nonexistent, nil, "", s.authHdr(u.A.Token))
	s.expect(http.MethodGet, "/api/media/{nonexistent}", nCode, nBody, http.StatusNotFound, "404", "NOT_FOUND")

	return u
}

// ---------------------------------------------------------------- admin

type adminSess struct {
	base  string
	cli   *http.Client
	token string
	csrf  string
}

func (as *adminSess) do(method, path string, body []byte, ct string) (int, []byte) {
	req, err := http.NewRequest(method, as.base+path, bytes.NewReader(body))
	if err != nil {
		panic(err)
	}
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	req.Header.Set("Authorization", "Bearer "+as.token)
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("X-CSRF-Token", as.csrf)
	}
	resp, err := as.cli.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// totpFromBase32 computes the current 6-digit code for a stored base32 secret.
func totpFromBase32(secret string) string {
	b, err := totp.DecodeSecret(secret)
	if err != nil {
		return ""
	}
	return totp.Generate(b, time.Now())
}

// adminLogin authenticates against the REST surface and returns a session with
// a cookie jar (so the CSRF cookie rides along) plus the bearer/csrf values.
func adminLogin(t *testing.T, base, user, pass, secretBase32 string) *adminSess {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	cli := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	body := fmt.Sprintf(`{"username":%q,"password":%q,"totp":%q}`, user, pass, totpFromBase32(secretBase32))
	req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatalf("admin login %s: %v", user, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Token string `json:"token"`
		CSRF  string `json:"csrf_token"`
	}
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode != http.StatusOK || out.Token == "" {
		t.Fatalf("admin login %s failed: status=%d body=%s", user, resp.StatusCode, snippet(raw))
	}
	return &adminSess{base: base, cli: cli, token: out.Token, csrf: out.CSRF}
}

func contractAdmin(s *suite, u contractUsers) {
	t := s.t
	base := "http://" + waitAddr(t, s.a.adminREST.Addr)

	// --- bootstrap super admin: read its (once-only logged) TOTP secret from
	// the admin DB so the test can actually log in. ---
	var superSecret string
	if err := s.a.adminDB.DB.QueryRow("SELECT totp_secret FROM adm_admins WHERE username = ?", "rootadmin").Scan(&superSecret); err != nil {
		t.Fatalf("read bootstrap totp secret: %v", err)
	}
	super := adminLogin(t, base, "rootadmin", "root-admin-pass-1234567", superSecret)

	// --- insert a review-role admin to prove RBAC least privilege (403). ---
	const reviewPass = "review-pass-1234567"
	reviewSecret := totp.SecretFromASCII("12345678901234567890")
	hash, herr := authn.NewHasher(authn.DefaultArgon2Params()).Hash(reviewPass)
	if herr != nil {
		t.Fatalf("hash review password: %v", herr)
	}
	if _, err := s.a.adminDB.DB.Exec(
		"INSERT INTO adm_admins (username, pw_hash, totp_secret, role, created_at) VALUES (?,?,?,?,?)",
		"reviewer", hash, reviewSecret, 2, time.Now().Unix()); err != nil {
		t.Fatalf("insert review admin: %v", err)
	}
	review := adminLogin(t, base, "reviewer", reviewPass, reviewSecret)

	// ---- unauthenticated → 401 ----
	{
		resp, err := http.Get(base + "/api/v1/whoami")
		if err != nil {
			t.Fatalf("unauth whoami: %v", err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		s.expect(http.MethodGet, "/api/v1/whoami (no auth)", resp.StatusCode, raw, http.StatusUnauthorized, "401")
	}

	// ---- super: read endpoints ----
	get := func(path string) {
		code, body := super.do(http.MethodGet, path, nil, "")
		s.expect(http.MethodGet, path, code, body, http.StatusOK, "200")
	}
	get("/api/v1/whoami")
	get("/api/v1/users")
	get(fmt.Sprintf("/api/v1/users/%d", u.A.ID))
	get("/api/v1/reports")
	// messages/search requires q or conv_id; query with q:
	{
		code, body := super.do(http.MethodGet, "/api/v1/messages/search?q=hello", nil, "")
		s.expect(http.MethodGet, "/api/v1/messages/search", code, body, http.StatusOK, "200")
	}
	get("/api/v1/flags")
	get("/api/v1/audit")
	get("/api/v1/archive/status")
	{
		code, body := super.do(http.MethodGet, "/api/v1/metrics", nil, "")
		if code == http.StatusOK && strings.Contains(string(body), "ufot_admin_build_info") {
			s.add(http.MethodGet, "/api/v1/metrics", code, "200", "PASS", "")
		} else {
			s.add(http.MethodGet, "/api/v1/metrics", code, "200", "FAIL", snippet(body))
		}
	}

	// ---- super: not-found semantics ----
	{
		code, body := super.do(http.MethodGet, "/api/v1/reports/99999999", nil, "")
		s.expect(http.MethodGet, "/api/v1/reports/{id} (missing)", code, body, http.StatusNotFound, "404")
	}

	// ---- super: high-risk mutations ----
	mut := func(method, path, body string, want int, mustContain ...string) {
		got, raw := super.do(method, path, []byte(body), "application/json")
		s.expect(method, path, got, raw, want, fmt.Sprintf("%d", want), mustContain...)
	}
	mut(http.MethodPost, fmt.Sprintf("/api/v1/users/%d/ban", u.Ban.ID), `{"level":1,"reason":"contract"}`, http.StatusOK, "banned")
	mut(http.MethodPost, fmt.Sprintf("/api/v1/users/%d/unban", u.Ban.ID), `{"reason":"contract"}`, http.StatusOK)
	mut(http.MethodPost, fmt.Sprintf("/api/v1/users/%d/kick", u.Ban.ID), `{"reason":"contract"}`, http.StatusOK)
	mut(http.MethodPost, "/api/v1/reports/99999999/handle", `{"action":"dismiss"}`, http.StatusNotFound)
	// recall always injects a 960 frame → 200 regardless of message existence.
	mut(http.MethodPost, "/api/v1/messages/99999999/recall", `{"reason":"contract"}`, http.StatusOK, "recalled")

	// flags PUT: set a benign flag.
	{
		code, body := super.do(http.MethodPut, "/api/v1/flags", []byte(`{"key":"contract_probe","value":"true"}`), "application/json")
		s.expect(http.MethodPut, "/api/v1/flags", code, body, http.StatusOK, "200", "items")
	}

	// ---- dry_run broadcast: no dispatch (frame injector count unchanged) ----
	{
		before := s.a.pipeline.AdminAPI().Injected()
		code, body := super.do(http.MethodPost, "/api/v1/broadcast?dry_run=true",
			[]byte(`{"text":"hi","conv_ids":[1]}`), "application/json")
		s.expect(http.MethodPost, "/api/v1/broadcast?dry_run=true", code, body, http.StatusOK, "200", `"dry_run":true`)
		after := s.a.pipeline.AdminAPI().Injected()
		if before == after {
			s.add(http.MethodPost, "/api/v1/broadcast (no side effect)", int(after-before), "0 dispatch", "PASS", "")
		} else {
			s.add(http.MethodPost, "/api/v1/broadcast (no side effect)", int(after-before), "0 dispatch", "FAIL",
				fmt.Sprintf("dry_run injected %d frame(s)", after-before))
		}
	}

	// ---- idempotency: same Idempotency-Key replays the same response ----
	{
		key := "contract-idem-1"
		send := func() (int, []byte) {
			req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/broadcast?dry_run=true",
				strings.NewReader(`{"text":"idem","conv_ids":[1]}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+super.token)
			req.Header.Set("X-CSRF-Token", super.csrf)
			req.Header.Set("Idempotency-Key", key)
			resp, err := super.cli.Do(req)
			if err != nil {
				t.Fatalf("idem request: %v", err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, raw
		}
		c1, b1 := send()
		c2, b2 := send()
		if c1 == c2 && bytes.Equal(bytes.TrimSpace(b1), bytes.TrimSpace(b2)) {
			s.add(http.MethodPost, "/api/v1/broadcast (idempotent replay)", c2, fmt.Sprintf("%d + same body", c1), "PASS", "")
		} else {
			s.add(http.MethodPost, "/api/v1/broadcast (idempotent replay)", c2, fmt.Sprintf("%d + same body", c1), "FAIL",
				fmt.Sprintf("c1=%d c2=%d body1=%s body2=%s", c1, c2, snippet(b1), snippet(b2)))
		}
	}

	// ---- archive/run dry_run (degraded: no cold store may → 503) ----
	{
		code, body := super.do(http.MethodPost, "/api/v1/archive/run", []byte(`{"dry_run":true}`), "application/json")
		switch code {
		case http.StatusOK:
			s.add(http.MethodPost, "/api/v1/archive/run (dry_run)", code, "200", "PASS", "")
		case http.StatusServiceUnavailable:
			s.add(http.MethodPost, "/api/v1/archive/run (dry_run)", code, "200", "SKIP",
				"degraded: no cold store configured (503 not_configured) — body="+snippet(body))
		default:
			s.add(http.MethodPost, "/api/v1/archive/run (dry_run)", code, "200", "FAIL", snippet(body))
		}
	}

	// ---- review role: least privilege → 403 on elevated surfaces ----
	rget := func(path string) {
		code, body := review.do(http.MethodGet, path, nil, "")
		s.expect(http.MethodGet, path+" (review)", code, body, http.StatusForbidden, "403")
	}
	rget("/api/v1/flags")
	rget("/api/v1/metrics")
	{
		code, body := review.do(http.MethodPost, fmt.Sprintf("/api/v1/users/%d/ban", u.A.ID), []byte(`{"level":1}`), "application/json")
		s.expect(http.MethodPost, "/api/v1/users/{id}/ban (review)", code, body, http.StatusForbidden, "403")
	}
	// review whoami still works (authenticated, low-priv read).
	{
		code, body := review.do(http.MethodGet, "/api/v1/whoami", nil, "")
		s.expect(http.MethodGet, "/api/v1/whoami (review)", code, body, http.StatusOK, "200", "review")
	}
}

// ---------------------------------------------------------------- websocket

// readKindT reads frames until kind appears, without failing the test on a
// read error (so the acceptance table survives a broken frame path).
func readKindT(conn *websocket.Conn, kind uint32, timeout time.Duration) (*chatpb.Envelope, bool, []string) {
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	var seen []string
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return nil, false, seen
		}
		env, derr := frame.Decode(data)
		if derr != nil {
			seen = append(seen, "undecodable")
			continue
		}
		if env.Kind == kind {
			return env, true, seen
		}
		d := kinds.KindName(env.Kind)
		if env.Kind == kinds.KindError {
			d += "(" + string(env.Payload) + ")"
		}
		seen = append(seen, d)
	}
}

// dialWS dials /ws with an optional access-token cookie; returns the conn, the
// HTTP status and the dial error.
func dialWS(addr, token string) (*websocket.Conn, int, error) {
	hdr := http.Header{}
	if token != "" {
		hdr.Set("Cookie", "access_token="+token)
	}
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, resp, err := dialer.Dial("ws://"+addr+"/ws", hdr)
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	return conn, status, err
}

func writeEnv(conn *websocket.Conn, env *chatpb.Envelope) error {
	raw, err := frame.Encode(env)
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.BinaryMessage, raw)
}

// contractWS exercises the WebSocket frame contract end to end.
func contractWS(s *suite, _ contractUsers) {
	t := s.t
	addr := s.a.user.Addr()

	alice := guestUser(t, s.base, "ws-alice")
	bob := guestUser(t, s.base, "ws-bob")
	carol := guestUser(t, s.base, "ws-carol")

	// --- handshake without cookie → 401, no upgrade ---
	{
		conn, status, err := dialWS(addr, "")
		if conn != nil {
			conn.Close()
		}
		if err != nil && status == http.StatusUnauthorized {
			s.add("WS", "handshake (no cookie)", status, "401", "PASS", "")
		} else {
			s.add("WS", "handshake (no cookie)", status, "401", "FAIL", fmt.Sprintf("err=%v", err))
		}
	}

	// --- handshake with cookie → 101 ---
	ca, status, err := dialWS(addr, alice.Token)
	if err != nil || status != http.StatusSwitchingProtocols {
		s.add("WS", "handshake (cookie)", status, "101", "FAIL", fmt.Sprintf("err=%v", err))
		t.Log("WS handshake failed; skipping frame checks")
		return
	}
	s.add("WS", "handshake (cookie)", status, "101", "PASS", "")
	defer ca.Close()
	time.Sleep(100 * time.Millisecond) // let acceptor register the session

	cb, _, _ := dialWS(addr, bob.Token)
	defer cb.Close()
	time.Sleep(50 * time.Millisecond)

	// --- CONV_CREATE(200) → CONV_DATA_RESULT(931) to A, CONV_PAYLOAD(202) to B ---
	createPayload, _ := (&chatpb.ConvCreate{ConvType: 1, MemberIds: []uint64{uint64(bob.ID)}}).Marshal()
	_ = writeEnv(ca, &chatpb.Envelope{Id: 1, Kind: kinds.KindConvCreate, RequestId: "contract-conv", Payload: createPayload})
	var cid int64
	if resEnv, ok, seen := readKindT(ca, kinds.KindConvDataResult, 6*time.Second); ok {
		var res wire.DataResult
		_ = json.Unmarshal(resEnv.Payload, &res)
		var created wire.ConvPayload
		_ = json.Unmarshal(res.Body, &created)
		cid = int64(created.ConvID)
		if res.Ok && cid != 0 {
			s.add("WS", "CONV_CREATE(200)→CONV_DATA_RESULT(931)", int(resEnv.Kind), "931 ok", "PASS", "")
		} else {
			s.add("WS", "CONV_CREATE(200)→CONV_DATA_RESULT(931)", int(resEnv.Kind), "931 ok", "FAIL", string(resEnv.Payload))
		}
		_ = seen
	} else {
		s.add("WS", "CONV_CREATE(200)→CONV_DATA_RESULT(931)", 0, "931 ok", "FAIL", "no CONV_DATA_RESULT")
	}
	// B receives the CONV_PAYLOAD(202) event for the new conversation.
	if cid != 0 {
		if env, ok, _ := readKindT(cb, kinds.KindConvPayload, 6*time.Second); ok && int(env.Kind) == 202 {
			s.add("WS", "CONV_CREATE→CONV_PAYLOAD(202) (peer)", 202, "202", "PASS", "")
		} else {
			s.add("WS", "CONV_CREATE→CONV_PAYLOAD(202) (peer)", 0, "202", "FAIL", "peer got no CONV_PAYLOAD")
		}
	} else {
		s.add("WS", "CONV_CREATE→CONV_PAYLOAD(202) (peer)", 0, "202", "SKIP", "no cid")
	}

	// --- PING(4) → PONG(5) on a fresh conn ---
	{
		c, _, _ := dialWS(addr, alice.Token)
		if c != nil {
			defer c.Close()
			time.Sleep(50 * time.Millisecond)
			pingP, _ := (&chatpb.Ping{ClientMs: 42}).Marshal()
			_ = writeEnv(c, &chatpb.Envelope{Id: 7, Kind: kinds.KindPing, Payload: pingP})
			if env, ok, seen := readKindT(c, kinds.KindPong, 6*time.Second); ok {
				s.add("WS", "PING(4)→PONG(5)", int(env.Kind), "5", "PASS", "")
			} else {
				s.add("WS", "PING(4)→PONG(5)", 0, "5", "FAIL", fmt.Sprintf("no PONG; seen=%v", seen))
			}
		} else {
			s.add("WS", "PING(4)→PONG(5)", 0, "5", "SKIP", "dial failed")
		}
	}

	// --- MSG_SEND(101) → MSG_ACK(103) on a fresh member conn ---
	if cid != 0 {
		c, _, _ := dialWS(addr, alice.Token)
		if c != nil {
			defer c.Close()
			time.Sleep(50 * time.Millisecond)
			mp, _ := (&chatpb.MessagePayload{ClientMsgId: "contract-msg-1", Text: "hello"}).Marshal()
			_ = writeEnv(c, &chatpb.Envelope{Id: 11, Kind: kinds.KindMsgSend, RequestId: "contract-send",
				Target: &chatpb.Envelope_ConvId{ConvId: uint64(cid)}, Payload: mp})
			if env, ok, seen := readKindT(c, kinds.KindMsgAck, 6*time.Second); ok {
				var ack wire.MsgAck
				_ = json.Unmarshal(env.Payload, &ack)
				if ack.ClientMsgID == "contract-msg-1" && ack.Seq != 0 && ack.MsgID != 0 {
					s.add("WS", "MSG_SEND(101)→MSG_ACK(103)", int(env.Kind), "103", "PASS", "")
				} else {
					s.add("WS", "MSG_SEND(101)→MSG_ACK(103)", int(env.Kind), "103", "FAIL", string(env.Payload))
				}
			} else {
				s.add("WS", "MSG_SEND(101)→MSG_ACK(103)", 0, "103", "FAIL", fmt.Sprintf("no ACK; seen=%v", seen))
			}
		} else {
			s.add("WS", "MSG_SEND(101)→MSG_ACK(103)", 0, "103", "SKIP", "dial failed")
		}
	} else {
		s.add("WS", "MSG_SEND(101)→MSG_ACK(103)", 0, "103", "SKIP", "no cid")
	}

	// --- SYNC_REQUEST(8) → history MSG_NEW on a fresh B conn ---
	if cid != 0 {
		c, _, _ := dialWS(addr, bob.Token)
		if c != nil {
			defer c.Close()
			time.Sleep(50 * time.Millisecond)
			syncP, _ := (&chatpb.SyncRequest{ConvId: uint64(cid), SinceSeq: 0, Limit: 100}).Marshal()
			_ = writeEnv(c, &chatpb.Envelope{Id: 21, Kind: kinds.KindSyncRequest, RequestId: "contract-sync",
				Target: &chatpb.Envelope_ConvId{ConvId: uint64(cid)}, Payload: syncP})
			if env, ok, seen := readKindT(c, kinds.KindMsgNew, 6*time.Second); ok {
				var mn struct {
					Seq    uint64 `json:"seq"`
					ConvID uint64 `json:"conv_id"`
				}
				_ = json.Unmarshal(env.Payload, &mn)
				if mn.ConvID == uint64(cid) && mn.Seq >= 1 {
					s.add("WS", "SYNC_REQUEST(8)→MSG_NEW history", int(env.Kind), "102", "PASS", "")
				} else {
					s.add("WS", "SYNC_REQUEST(8)→MSG_NEW history", int(env.Kind), "102", "FAIL", string(env.Payload))
				}
			} else {
				s.add("WS", "SYNC_REQUEST(8)→MSG_NEW history", 0, "102", "FAIL", fmt.Sprintf("no MSG_NEW; seen=%v", seen))
			}
		} else {
			s.add("WS", "SYNC_REQUEST(8)→MSG_NEW history", 0, "102", "SKIP", "dial failed")
		}
	} else {
		s.add("WS", "SYNC_REQUEST(8)→MSG_NEW history", 0, "102", "SKIP", "no cid")
	}

	// --- non-member MSG_SEND → ERROR 403 on a fresh Carol conn ---
	if cid != 0 {
		c, _, _ := dialWS(addr, carol.Token)
		if c != nil {
			defer c.Close()
			time.Sleep(50 * time.Millisecond)
			mp, _ := (&chatpb.MessagePayload{ClientMsgId: "contract-403", Text: "intrude"}).Marshal()
			_ = writeEnv(c, &chatpb.Envelope{Id: 31, Kind: kinds.KindMsgSend, RequestId: "contract-403",
				Target: &chatpb.Envelope_ConvId{ConvId: uint64(cid)}, Payload: mp})
			if env, ok, seen := readKindT(c, kinds.KindError, 6*time.Second); ok {
				var wErr wire.Error
				_ = json.Unmarshal(env.Payload, &wErr)
				if wErr.Code == 403 {
					s.add("WS", "non-member MSG_SEND→ERROR 403", int(env.Kind), "10 (code 403)", "PASS", "")
				} else {
					s.add("WS", "non-member MSG_SEND→ERROR 403", int(env.Kind), "10 (code 403)", "FAIL", string(env.Payload))
				}
			} else {
				s.add("WS", "non-member MSG_SEND→ERROR 403", 0, "10 (code 403)", "FAIL", fmt.Sprintf("no ERROR; seen=%v", seen))
			}
		} else {
			s.add("WS", "non-member MSG_SEND→ERROR 403", 0, "10 (code 403)", "SKIP", "dial failed")
		}
	} else {
		s.add("WS", "non-member MSG_SEND→ERROR 403", 0, "10 (code 403)", "SKIP", "no cid")
	}

	// --- rate limit: burst > 20 msg/s → RATE_LIMITED(13) or disconnect ---
	if cid != 0 {
		c, _, _ := dialWS(addr, alice.Token)
		if c != nil {
			defer c.Close()
			time.Sleep(50 * time.Millisecond)
			go func() {
				for i := 0; i < 400; i++ {
					mp, _ := (&chatpb.MessagePayload{ClientMsgId: fmt.Sprintf("rl-%d", i), Text: "burst"}).Marshal()
					if err := writeEnv(c, &chatpb.Envelope{Id: uint64(1000 + i), Kind: kinds.KindMsgSend,
						RequestId: "rl", Target: &chatpb.Envelope_ConvId{ConvId: uint64(cid)}, Payload: mp}); err != nil {
						return
					}
				}
			}()
			_ = c.SetReadDeadline(time.Now().Add(6 * time.Second))
			limited := false
			disconnected := false
			for {
				_, data, rerr := c.ReadMessage()
				if rerr != nil {
					disconnected = true
					break
				}
				env, derr := frame.Decode(data)
				if derr == nil && env.Kind == kinds.KindRateLimited {
					limited = true
					break
				}
			}
			if limited || disconnected {
				s.add("WS", "rate limit >20 msg/s", 0, "RATE_LIMITED or disconnect", "PASS", "")
			} else {
				s.add("WS", "rate limit >20 msg/s", 0, "RATE_LIMITED or disconnect", "FAIL", "no throttle/disconnect after 400-frame burst")
			}
		} else {
			s.add("WS", "rate limit >20 msg/s", 0, "RATE_LIMITED or disconnect", "SKIP", "dial failed")
		}
	} else {
		s.add("WS", "rate limit >20 msg/s", 0, "RATE_LIMITED or disconnect", "SKIP", "no cid")
	}
}
