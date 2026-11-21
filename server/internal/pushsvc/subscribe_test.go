package pushsvc

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yingo-server/OpenDiscord/server/internal/pkg/log"
	"github.com/yingo-server/OpenDiscord/server/internal/user/core/authn"
)

const testToken = "tok-1"

// mapVerifier maps tokens to uids so a test can play distinct users.
type mapVerifier struct{ tokens map[string]int64 }

func (m *mapVerifier) Verify(token string) (*authn.Identity, error) {
	uid, ok := m.tokens[token]
	if !ok {
		return nil, errors.New("bad token")
	}
	return &authn.Identity{UserID: uid}, nil
}

// fakeSender records every delivery instead of hitting the network.
type fakeSender struct {
	sent []Subscription
	err  error
}

func (f *fakeSender) Send(sub Subscription, payload []byte) error {
	f.sent = append(f.sent, sub)
	return f.err
}

// validKeys returns a syntactically valid p256dh/auth pair (65 + 16 bytes).
func validKeys() (string, string) {
	pub := make([]byte, 65)
	pub[0] = 0x04
	for i := 1; i < len(pub); i++ {
		pub[i] = byte(i)
	}
	auth := make([]byte, 16)
	for i := range auth {
		auth[i] = byte(i + 1)
	}
	return base64.RawURLEncoding.EncodeToString(pub), base64.RawURLEncoding.EncodeToString(auth)
}

func newTestService(t *testing.T, sender Sender) *Service {
	t.Helper()
	s, err := New(Deps{
		Verifier: &mapVerifier{tokens: map[string]int64{testToken: 42}},
		Sender:   sender,
		Logger:   log.For("pushsvc-test"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func authed(r *http.Request, token string) *http.Request {
	r.AddCookie(&http.Cookie{Name: "access_token", Value: token})
	return r
}

func subscribeReq(t *testing.T, body any) *http.Request {
	t.Helper()
	var raw []byte
	switch v := body.(type) {
	case string:
		raw = []byte(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		raw = b
	}
	r := httptest.NewRequest(http.MethodPost, "/api/push/subscribe", bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func validBody(endpoint string) map[string]any {
	p256dh, auth := validKeys()
	return map[string]any{
		"subscription": map[string]any{
			"endpoint": endpoint,
			"keys":     map[string]any{"p256dh": p256dh, "auth": auth},
		},
	}
}

// validP256DH returns a base64url encoded 65-byte uncompressed public key.
func validP256DH() string {
	p256dh, _ := validKeys()
	return p256dh
}

func bodyWithKeys(endpoint, p256dh, auth string) map[string]any {
	return map[string]any{
		"subscription": map[string]any{
			"endpoint": endpoint,
			"keys":     map[string]any{"p256dh": p256dh, "auth": auth},
		},
	}
}

func bodyWithoutEndpoint() map[string]any {
	p256dh, auth := validKeys()
	return map[string]any{
		"subscription": map[string]any{
			"keys": map[string]any{"p256dh": p256dh, "auth": auth},
		},
	}
}

func bodyWithoutKeys() map[string]any {
	return map[string]any{
		"subscription": map[string]any{
			"endpoint": "https://push.example/a",
		},
	}
}

// --- auth ------------------------------------------------------------------

func TestSubscribeRequiresAuth(t *testing.T) {
	s := newTestService(t, &fakeSender{})
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, subscribeReq(t, validBody("https://push.example/a")))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%s)", rr.Code, rr.Body.String())
	}
}

func TestSubscribeInvalidTokenIs401(t *testing.T) {
	s := newTestService(t, &fakeSender{})
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, authed(subscribeReq(t, validBody("https://push.example/a")), "nope"))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

// --- happy path ------------------------------------------------------------

func TestSubscribeAccepted(t *testing.T) {
	s := newTestService(t, &fakeSender{})
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, authed(subscribeReq(t, validBody("https://push.example/a")), testToken))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body=%s)", rr.Code, rr.Body.String())
	}
	subs := s.Subscriptions(42)
	if len(subs) != 1 {
		t.Fatalf("stored %d subscriptions, want 1", len(subs))
	}
	if subs[0].Endpoint != "https://push.example/a" {
		t.Fatalf("endpoint = %q", subs[0].Endpoint)
	}
	if subs[0].P256DH == "" || subs[0].Auth == "" {
		t.Fatalf("keys not stored: %+v", subs[0])
	}
}

func TestSubscribeIsIdempotent(t *testing.T) {
	s := newTestService(t, &fakeSender{})
	for i := 0; i < 3; i++ {
		rr := httptest.NewRecorder()
		s.Routes().ServeHTTP(rr, authed(subscribeReq(t, validBody("https://push.example/a")), testToken))
		if rr.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rr.Code)
		}
	}
	if got := len(s.Subscriptions(42)); got != 1 {
		t.Fatalf("stored %d subscriptions, want 1 (idempotent)", got)
	}
}

// --- bad body --------------------------------------------------------------

func TestSubscribeBadBodyCases(t *testing.T) {
	cases := []struct {
		name string
		body any
	}{
		{"not json", "{"},
		{"empty object", map[string]any{}},
		{"missing endpoint", bodyWithoutEndpoint()},
		{"missing keys", bodyWithoutKeys()},
		{"bad p256dh", bodyWithKeys("https://push.example/a", "!!!", "!!!")},
		{"non https endpoint", bodyWithKeys("http://push.example/a", "!!!", "!!!")},
		{"short auth secret", bodyWithKeys("https://push.example/a", validP256DH(), "AAAA")},
	}
	s := newTestService(t, &fakeSender{})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			s.Routes().ServeHTTP(rr, authed(subscribeReq(t, tc.body), testToken))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
			}
			if got := len(s.Subscriptions(42)); got != 0 {
				t.Fatalf("stored %d subscriptions after a bad body, want 0", got)
			}
		})
	}
}

// --- notify ----------------------------------------------------------------

func TestNotifyWithoutSubscriptionsDoesNotPanic(t *testing.T) {
	s := newTestService(t, &fakeSender{})
	if n := s.Notify(999, []byte(`{"x":1}`)); n != 0 {
		t.Fatalf("sent = %d, want 0", n)
	}
}

func TestNotifyUsesInjectedSender(t *testing.T) {
	sender := &fakeSender{}
	s := newTestService(t, sender)
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, authed(subscribeReq(t, validBody("https://push.example/a")), testToken))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("subscribe status = %d", rr.Code)
	}
	payload := []byte(`{"type":"msg"}`)
	if n := s.Notify(42, payload); n != 1 {
		t.Fatalf("sent = %d, want 1", n)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("sender called %d times, want 1", len(sender.sent))
	}
	if sender.sent[0].Endpoint != "https://push.example/a" {
		t.Fatalf("endpoint = %q", sender.sent[0].Endpoint)
	}
}

func TestNotifyDropsGoneSubscription(t *testing.T) {
	sender := &fakeSender{err: &StatusError{Status: http.StatusGone, Err: errors.New("gone")}}
	s := newTestService(t, sender)
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, authed(subscribeReq(t, validBody("https://push.example/a")), testToken))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("subscribe status = %d", rr.Code)
	}
	s.Notify(42, []byte(`{"type":"msg"}`))
	if got := len(s.Subscriptions(42)); got != 0 {
		t.Fatalf("stored %d subscriptions after 410, want 0", got)
	}
}

func TestNotifyKeepsSubscriptionOnTransientError(t *testing.T) {
	sender := &fakeSender{err: &StatusError{Status: http.StatusServiceUnavailable, Err: errors.New("boom")}}
	s := newTestService(t, sender)
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, authed(subscribeReq(t, validBody("https://push.example/a")), testToken))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("subscribe status = %d", rr.Code)
	}
	s.Notify(42, []byte(`{"type":"msg"}`))
	if got := len(s.Subscriptions(42)); got != 1 {
		t.Fatalf("stored %d subscriptions after a transient error, want 1", got)
	}
}

func TestClearRemovesSubscriptions(t *testing.T) {
	s := newTestService(t, &fakeSender{})
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, authed(subscribeReq(t, validBody("https://push.example/a")), testToken))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("subscribe status = %d", rr.Code)
	}
	s.Clear(42)
	if got := len(s.Subscriptions(42)); got != 0 {
		t.Fatalf("stored %d subscriptions after Clear, want 0", got)
	}
}

// --- sender plumbing -------------------------------------------------------

func TestSenderFromEnvIsNoopWithoutEnv(t *testing.T) {
	t.Setenv("VAPID_PRIVATE_KEY", "")
	t.Setenv("VAPID_SUBJECT", "")
	s := SenderFromEnv(log.For("pushsvc-test"))
	if _, ok := s.(*NoopSender); !ok {
		t.Fatalf("sender = %T, want *NoopSender", s)
	}
}

func TestWebPushSenderBuildsVAPIDJWT(t *testing.T) {
	priv := testVAPIDKey(t)
	sender, err := NewWebPushSender(WebPushConfig{
		PrivateKey: priv,
		Subject:    "mailto:admin@example.com",
		Logger:     log.For("pushsvc-test"),
	})
	if err != nil {
		t.Fatalf("NewWebPushSender: %v", err)
	}
	jwt, err := sender.vapidJWT("https://push.example/a")
	if err != nil {
		t.Fatalf("vapidJWT: %v", err)
	}
	parts := bytes.Split([]byte(jwt), []byte("."))
	if len(parts) != 3 {
		t.Fatalf("jwt parts = %d, want 3", len(parts))
	}
	claims := map[string]any{}
	if err := json.Unmarshal(mustB64(t, parts[1]), &claims); err != nil {
		t.Fatalf("claims: %v", err)
	}
	if claims["aud"] != "https://push.example" {
		t.Fatalf("aud = %v", claims["aud"])
	}
	if claims["sub"] != "mailto:admin@example.com" {
		t.Fatalf("sub = %v", claims["sub"])
	}
}

func mustB64(t *testing.T, part []byte) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(string(part))
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	return b
}

// testVAPIDKey mints a throwaway P-256 key in SEC1 PEM form.
func testVAPIDKey(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}
