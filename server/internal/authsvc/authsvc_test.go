package authsvc

// authsvc_test.go: integration tests over a real SQLite hot DB (temp dir), the
// real core/authn signer/verifier/hasher and a real HTTP server. No mocks of the
// storage layer; only the clock is injected so rate-limit windows are testable.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yingo-server/UFOT/server/internal/data/store/sqlite"
	"github.com/yingo-server/UFOT/server/internal/pkg/log"
	"github.com/yingo-server/UFOT/server/internal/user/core/authn"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type testEnv struct {
	svc   *Service
	db    *sqlite.DB
	clock *fakeClock
	srv   *httptest.Server
	pub   ed25519.PublicKey
}

// smallArgon2 keeps the password tests fast (the shipped defaults are the
// security-spec values; tests only need the code path).
func smallArgon2() authn.Argon2Params {
	return authn.Argon2Params{Memory: 8 * 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}
}

func newTestEnv(t *testing.T, mutate func(*Config)) *testEnv {
	t.Helper()
	t.Setenv("APP_ENV", "dev")
	log.SetLevel(log.ErrorLevel)

	dir := t.TempDir()
	db, err := sqlite.Open(filepath.Join(dir, "hot.db"))
	if err != nil {
		t.Fatalf("open hot db: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	clock := &fakeClock{t: time.Now()}
	cfg := Config{
		PublicOrigin: "",
		DevOTP:       true,
		Now:          clock.Now,
		Logger:       log.For("authsvc-test"),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	svc, err := New(Deps{
		DB:       db.DB,
		Identity: db,
		Sys:      db,
		Signer:   authn.NewSigner(priv),
		Verifier: authn.NewVerifier(pub),
		Hasher:   authn.NewHasher(smallArgon2()),
		HMACKey:  []byte("0123456789abcdef0123456789abcdef"),
	}, cfg)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	srv := httptest.NewServer(svc.Routes())
	t.Cleanup(srv.Close)
	return &testEnv{svc: svc, db: db, clock: clock, srv: srv, pub: pub}
}

func (e *testEnv) post(t *testing.T, path, body string, out any) (int, string) {
	t.Helper()
	resp, err := http.Post(e.srv.URL+path, "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	var raw bytes.Buffer
	if _, err := raw.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if out != nil {
		_ = json.Unmarshal(raw.Bytes(), out)
	}
	return resp.StatusCode, raw.String()
}

type sessionBody struct {
	UserID        int64  `json:"user_id"`
	DeviceID      string `json:"device_id"`
	AccessToken   string `json:"access_token"`
	Token         string `json:"token"`
	RefreshToken  string `json:"refresh_token"`
	ExpiresAt     int64  `json:"expires_at"`
	ExpiresAtUnix int64  `json:"expires_at_unix"`
	SessionID     string `json:"session_id"`
	Guest         bool   `json:"guest"`
	IsNewUser     bool   `json:"is_new_user"`
	Profile       *struct {
		DisplayName string `json:"display_name"`
	} `json:"profile"`
}

type challengeBody struct {
	OK          bool   `json:"ok"`
	ChallengeID string `json:"challenge_id"`
	Channel     string `json:"channel"`
	ExpiresIn   int64  `json:"expires_in"`
	ResendAfter int64  `json:"resend_after"`
	Link        string `json:"link"`
}

type errorBody struct {
	Error        string `json:"error"`
	Code         string `json:"code"`
	Status       string `json:"status"`
	RetryAfterMS int64  `json:"retry_after_ms"`
}

func TestPhoneOTPLoginCreatesUser(t *testing.T) {
	e := newTestEnv(t, nil)
	var ch challengeBody
	if code, body := e.post(t, "/api/auth/phone/code", `{"phone":"+8613800138000"}`, &ch); code != 200 {
		t.Fatalf("send code: %d %s", code, body)
	}
	if ch.ChallengeID == "" || ch.Channel != "sms" || ch.ExpiresIn != 300 || ch.ResendAfter != 60 {
		t.Fatalf("challenge = %+v", ch)
	}

	var sess sessionBody
	if code, body := e.post(t, "/api/auth/phone/verify",
		fmt.Sprintf(`{"phone":"+8613800138000","code":"123456","challenge_id":%q,"device_id":"dev-1"}`, ch.ChallengeID), &sess); code != 200 {
		t.Fatalf("verify: %d %s", code, body)
	}
	if sess.UserID == 0 || !sess.IsNewUser || sess.DeviceID != "dev-1" || sess.SessionID != "dev-1" {
		t.Fatalf("session = %+v", sess)
	}
	if sess.ExpiresAt <= time.Now().UnixMilli() || sess.ExpiresAtUnix <= time.Now().Unix() {
		t.Fatalf("expires_at = %d / %d", sess.ExpiresAt, sess.ExpiresAtUnix)
	}

	// The access token verifies with the core/authn verifier (round trip).
	id, err := authn.NewVerifier(e.pub).Verify(sess.AccessToken)
	if err != nil {
		t.Fatalf("access token does not verify: %v", err)
	}
	if id.UserID != sess.UserID || id.DeviceID != "dev-1" {
		t.Fatalf("identity = %+v", id)
	}

	// Single use: replaying the same code fails.
	var e1 errorBody
	if code, body := e.post(t, "/api/auth/phone/verify",
		fmt.Sprintf(`{"phone":"+8613800138000","code":"123456","challenge_id":%q}`, ch.ChallengeID), &e1); code != 401 {
		t.Fatalf("replay verify: %d %s", code, body)
	}

	// Same phone again -> same uid, not new.
	e.clock.Advance(61 * time.Second)
	var ch2 challengeBody
	if code, body := e.post(t, "/api/auth/phone/code", `{"phone":"+8613800138000","captcha_token":"dev"}`, &ch2); code != 200 {
		t.Fatalf("second send: %d %s", code, body)
	}
	var sess2 sessionBody
	if code, body := e.post(t, "/api/auth/phone/verify",
		fmt.Sprintf(`{"phone":"+8613800138000","code":"123456","challenge_id":%q}`, ch2.ChallengeID), &sess2); code != 200 {
		t.Fatalf("second verify: %d %s", code, body)
	}
	if sess2.UserID != sess.UserID || sess2.IsNewUser {
		t.Fatalf("second login = %+v, want same uid %d and is_new_user=false", sess2, sess.UserID)
	}
}

func TestOTPRateLimits(t *testing.T) {
	e := newTestEnv(t, nil)
	const phone = `{"phone":"+8613900139000"}`
	const phoneCaptcha = `{"phone":"+8613900139000","captcha_token":"dev"}`

	var ch challengeBody
	if code, body := e.post(t, "/api/auth/phone/code", phone, &ch); code != 200 {
		t.Fatalf("first send: %d %s", code, body)
	}
	sends := 1

	// 60s interval (docs §1.5.2 红线).
	var eb errorBody
	code, body := e.post(t, "/api/auth/phone/code", phone, &eb)
	if code != http.StatusTooManyRequests || eb.Code != "OTP_RESEND_TOO_SOON" {
		t.Fatalf("resend within 60s: %d %s", code, body)
	}
	if eb.RetryAfterMS <= 0 || eb.RetryAfterMS > 60_000 {
		t.Fatalf("retry_after_ms = %d", eb.RetryAfterMS)
	}

	// 2nd send of the day requires a captcha (fail-closed: no verifier wired).
	e.clock.Advance(61 * time.Second)
	code, body = e.post(t, "/api/auth/phone/code", phone, &eb)
	if code != http.StatusBadRequest || eb.Code != "CAPTCHA_REQUIRED" {
		t.Fatalf("second send without captcha: %d %s", code, body)
	}

	// Dev mode accepts any non-empty captcha token.
	for i := 0; i < 4; i++ { // sends 2..5 within the hour
		if i > 0 {
			e.clock.Advance(61 * time.Second)
		}
		code, body = e.post(t, "/api/auth/phone/code", phoneCaptcha, &ch)
		if code != 200 {
			t.Fatalf("send %d: %d %s", sends+1, code, body)
		}
		sends++
	}
	if sends != 5 {
		t.Fatalf("sends = %d, want 5", sends)
	}

	// 6th within the hour -> hourly cap (5/hour).
	e.clock.Advance(61 * time.Second)
	code, body = e.post(t, "/api/auth/phone/code", phoneCaptcha, &eb)
	if code != http.StatusTooManyRequests || eb.Code != "OTP_HOURLY_LIMIT" {
		t.Fatalf("6th send in an hour: %d %s", code, body)
	}

	// Roll the hour and fill the daily quota (10/day).
	for sends < 10 {
		e.clock.Advance(61 * time.Minute)
		code, body = e.post(t, "/api/auth/phone/code", phoneCaptcha, &ch)
		if code != 200 {
			t.Fatalf("send %d: %d %s", sends+1, code, body)
		}
		sends++
	}
	e.clock.Advance(61 * time.Minute)
	code, body = e.post(t, "/api/auth/phone/code", phoneCaptcha, &eb)
	if code != http.StatusTooManyRequests || eb.Code != "OTP_DAILY_LIMIT" {
		t.Fatalf("11th send in 24h: %d %s", code, body)
	}
}

func TestWrongCodeLocksPhone(t *testing.T) {
	e := newTestEnv(t, nil)
	var ch challengeBody
	if code, body := e.post(t, "/api/auth/phone/code", `{"phone":"+8613700137000"}`, &ch); code != 200 {
		t.Fatalf("send: %d %s", code, body)
	}
	for i := 0; i < 5; i++ {
		var eb errorBody
		code, body := e.post(t, "/api/auth/phone/verify",
			fmt.Sprintf(`{"phone":"+8613700137000","code":"000000","challenge_id":%q}`, ch.ChallengeID), &eb)
		if i < 4 && code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d %s", i+1, code, body)
		}
		if i == 4 {
			if code != http.StatusTooManyRequests || eb.Code != "OTP_LOCKED" {
				t.Fatalf("5th attempt: %d %s", code, body)
			}
			if eb.RetryAfterMS != int64(DefaultVerifyLock/time.Millisecond) {
				t.Fatalf("lock retry_after_ms = %d", eb.RetryAfterMS)
			}
		}
	}
	// Even the right code is refused while the lock is active.
	var eb errorBody
	code, body := e.post(t, "/api/auth/phone/verify",
		fmt.Sprintf(`{"phone":"+8613700137000","code":"123456","challenge_id":%q}`, ch.ChallengeID), &eb)
	if code != http.StatusTooManyRequests || eb.Code != "OTP_VERIFY_LOCKED" {
		t.Fatalf("locked verify: %d %s", code, body)
	}
}

func TestPasswordLoginAndSet(t *testing.T) {
	e := newTestEnv(t, nil)

	// A phone login gives us an account with an access token.
	var ch challengeBody
	if code, body := e.post(t, "/api/auth/phone/code", `{"phone":"+8613600136000"}`, &ch); code != 200 {
		t.Fatalf("send: %d %s", code, body)
	}
	var sess sessionBody
	if code, body := e.post(t, "/api/auth/phone/verify",
		fmt.Sprintf(`{"phone":"+8613600136000","code":"123456","challenge_id":%q}`, ch.ChallengeID), &sess); code != 200 {
		t.Fatalf("verify: %d %s", code, body)
	}

	// Set a password (authenticated extension endpoint).
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/api/auth/password/set",
		bytes.NewReader([]byte(`{"password":"correct horse battery"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sess.AccessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("password set: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("password set status = %d", resp.StatusCode)
	}

	// Too short is rejected.
	if code, body := e.post(t, "/api/auth/password",
		`{"account":"+8613600136000","password":"short"}`, nil); code != 401 {
		t.Fatalf("short password login should be 401, got %d %s", code, body)
	}

	// Correct password logs in.
	var login sessionBody
	if code, body := e.post(t, "/api/auth/password",
		`{"account":"+8613600136000","password":"correct horse battery"}`, &login); code != 200 {
		t.Fatalf("password login: %d %s", code, body)
	}
	if login.UserID != sess.UserID {
		t.Fatalf("login uid = %d, want %d", login.UserID, sess.UserID)
	}

	// Five wrong passwords lock the account (15m stage).
	var eb errorBody
	for i := 0; i < 5; i++ {
		code, body := e.post(t, "/api/auth/password",
			`{"account":"+8613600136000","password":"wrong-password"}`, &eb)
		if i < 4 && code != http.StatusUnauthorized {
			t.Fatalf("wrong password %d: %d %s", i+1, code, body)
		}
		if i == 4 && (code != http.StatusTooManyRequests || eb.Code != "ACCOUNT_LOCKED") {
			t.Fatalf("5th wrong password: %d %s", code, body)
		}
	}
	if eb.RetryAfterMS != int64(15*time.Minute/time.Millisecond) {
		t.Fatalf("lock retry_after_ms = %d, want 15m", eb.RetryAfterMS)
	}
	// The correct password is refused while locked.
	code, body := e.post(t, "/api/auth/password",
		`{"account":"+8613600136000","password":"correct horse battery"}`, &eb)
	if code != http.StatusTooManyRequests {
		t.Fatalf("locked login: %d %s", code, body)
	}
}

func TestEmailMagicLinkDevMode(t *testing.T) {
	e := newTestEnv(t, nil)
	var ch challengeBody
	if code, body := e.post(t, "/api/auth/email/link", `{"email":"Alice@Example.COM"}`, &ch); code != 200 {
		t.Fatalf("email link: %d %s", code, body)
	}
	if ch.Channel != "email" || ch.Link == "" {
		t.Fatalf("challenge = %+v (dev mode must return the link)", ch)
	}
	u, err := url.Parse(ch.Link)
	if err != nil {
		t.Fatalf("parse link: %v", err)
	}
	token := u.Query().Get("token")
	if token == "" || strings.Contains(ch.Link, "Alice") || strings.Contains(ch.Link, "alice") {
		t.Fatalf("link must carry a token and no email: %s", ch.Link)
	}

	// Polling without the token is "pending" (202 + code=PENDING).
	var pend errorBody
	if code, body := e.post(t, "/api/auth/email/poll",
		fmt.Sprintf(`{"email":"alice@example.com","challenge_id":%q}`, ch.ChallengeID), &pend); code != http.StatusConflict || pend.Code != "PENDING" {
		t.Fatalf("pending poll: %d %s", code, body)
	}

	// Polling with the token logs in (and normalizes the email to lowercase).
	var sess sessionBody
	if code, body := e.post(t, "/api/auth/email/poll",
		fmt.Sprintf(`{"email":"alice@example.com","challenge_id":%q,"token":%q}`, ch.ChallengeID, token), &sess); code != 200 {
		t.Fatalf("poll with token: %d %s", code, body)
	}
	if sess.UserID == 0 || !sess.IsNewUser {
		t.Fatalf("session = %+v", sess)
	}
	// Replaying the magic token fails (single use) and stays opaque.
	if code, _ := e.post(t, "/api/auth/email/poll",
		fmt.Sprintf(`{"email":"alice@example.com","challenge_id":%q,"token":%q}`, ch.ChallengeID, token), nil); code == 200 {
		t.Fatal("magic token replay succeeded")
	}
}

func TestGuestAndLogoutRevokesFamily(t *testing.T) {
	e := newTestEnv(t, nil)
	var guest sessionBody
	if code, body := e.post(t, "/api/auth/guest", `{"nickname":"  kai  "}`, &guest); code != 200 {
		t.Fatalf("guest: %d %s", code, body)
	}
	if !guest.Guest || !strings.HasPrefix(guest.DeviceID, "g_") || guest.Profile == nil || guest.Profile.DisplayName != "kai" {
		t.Fatalf("guest session = %+v", guest)
	}

	// Same device id resumes the same guest account.
	var again sessionBody
	if code, body := e.post(t, "/api/auth/guest",
		fmt.Sprintf(`{"device_id":%q}`, guest.DeviceID), &again); code != 200 {
		t.Fatalf("guest resume: %d %s", code, body)
	}
	if again.UserID != guest.UserID || again.IsNewUser {
		t.Fatalf("resume = %+v, want uid %d", again, guest.UserID)
	}

	// Logout revokes the family: the refresh token is dead afterwards.
	if code, body := e.post(t, "/api/auth/logout",
		fmt.Sprintf(`{"refresh_token":%q}`, guest.RefreshToken), nil); code != 200 {
		t.Fatalf("logout: %d %s", code, body)
	}
	var eb errorBody
	if code, body := e.post(t, "/api/auth/refresh",
		fmt.Sprintf(`{"refresh_token":%q}`, guest.RefreshToken), &eb); code != 401 {
		t.Fatalf("refresh after logout: %d %s", code, body)
	}
}

func TestRefreshRotation(t *testing.T) {
	e := newTestEnv(t, nil)
	var guest sessionBody
	if code, body := e.post(t, "/api/auth/guest", `{}`, &guest); code != 200 {
		t.Fatalf("guest: %d %s", code, body)
	}
	var rotated sessionBody
	if code, body := e.post(t, "/api/auth/refresh",
		fmt.Sprintf(`{"refresh_token":%q}`, guest.RefreshToken), &rotated); code != 200 {
		t.Fatalf("refresh: %d %s", code, body)
	}
	if rotated.RefreshToken == guest.RefreshToken || rotated.UserID != guest.UserID {
		t.Fatalf("rotated = %+v", rotated)
	}
	// Old token replay revokes the family; the new token dies too.
	var eb errorBody
	if code, body := e.post(t, "/api/auth/refresh",
		fmt.Sprintf(`{"refresh_token":%q}`, guest.RefreshToken), &eb); code != 401 || eb.Code != "REFRESH_REPLAY" {
		t.Fatalf("replay: %d %s", code, body)
	}
	if code, body := e.post(t, "/api/auth/refresh",
		fmt.Sprintf(`{"refresh_token":%q}`, rotated.RefreshToken), &eb); code != 401 {
		t.Fatalf("family revoke: %d %s", code, body)
	}
}

func TestDevOTPRequiresDevEnv(t *testing.T) {
	// Restore the production assertion: DevOTP must be refused outside dev.
	t.Setenv("APP_ENV", "production")
	dir := t.TempDir()
	db, err := sqlite.Open(filepath.Join(dir, "hot.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, err = New(Deps{
		DB: db.DB, Identity: db, Sys: db,
		Signer: authn.NewSigner(priv), Verifier: authn.NewVerifier(pub),
		Hasher: authn.NewHasher(smallArgon2()), HMACKey: []byte("0123456789abcdef0123456789abcdef"),
	}, Config{DevOTP: true})
	if err == nil {
		t.Fatal("DevOTP accepted with APP_ENV=production")
	}
	if !strings.Contains(err.Error(), "APP_ENV") {
		t.Fatalf("error = %v", err)
	}
}
