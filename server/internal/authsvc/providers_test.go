package authsvc

// providers_test.go: OPS-2 webhook adapter tests (docs/lts-1.4-remaining.md
// §3.5): success / timeout / non-zero status / fail-closed when unconfigured.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWebhookSMSSuccess(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	s := newWebhookSMS(srv.URL, time.Second)
	if s == nil {
		t.Fatal("sms adapter must exist when URL configured")
	}
	if err := s.SendOTP(context.Background(), "+8613800000000", "123456"); err != nil {
		t.Fatalf("SendOTP: %v", err)
	}
	if !strings.HasPrefix(got, "application/json") {
		t.Fatalf("content-type: %q", got)
	}
}

func TestWebhookNon2xxFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	m := newWebhookMailer(srv.URL, time.Second)
	if err := m.SendMagicLink(context.Background(), "a@b.co", "https://x/l"); err == nil {
		t.Fatal("non-2xx must fail the send")
	}
}

func TestWebhookTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release) // unblock even on failure
	s := newWebhookSMS(srv.URL, 50*time.Millisecond)
	start := time.Now()
	err := s.SendOTP(context.Background(), "+8613800000000", "123456")
	if err == nil {
		t.Fatal("slow endpoint must fail")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("timeout budget blown: %v", d)
	}
}

func TestCaptchaVerifyFailClosedOnBackendDeny(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c := newWebhookCaptcha(srv.URL, time.Second)
	if err := c.Verify(context.Background(), "tok", "1.2.3.4"); err == nil {
		t.Fatal("backend deny must surface as Verify error")
	}
}

func TestLoadProvidersUnconfigured(t *testing.T) {
	// Missing file: no adapters, no error — existing fail-closed wiring.
	s, m, c, err := LoadProviders("/nonexistent/providers.toml")
	if err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	if s != nil || m != nil || c != nil {
		t.Fatal("missing file must yield nil adapters")
	}
	// Present file but no URLs: same.
	s, m, c, err = LoadProviders(writeTemp(t, "[providers.sms]\n# url not set\n"))
	if err != nil {
		t.Fatalf("empty config must not error: %v", err)
	}
	if s != nil || m != nil || c != nil {
		t.Fatal("empty config must yield nil adapters")
	}
}

func TestLoadProvidersConfigured(t *testing.T) {
	s, m, c, err := LoadProviders(writeTemp(t, `# providers example (no real keys)
[providers.sms]
url = "http://127.0.0.1:1/sms"

[providers.mail]
url = "http://127.0.0.1:1/mail"

[providers.captcha]
url = "http://127.0.0.1:1/captcha"

[providers]
timeout_ms = 3000
`))
	if err != nil {
		t.Fatalf("LoadProviders: %v", err)
	}
	if s == nil || m == nil || c == nil {
		t.Fatal("all three adapters must be produced when URLs present")
	}
}

func TestParseProviderTOMLMalformed(t *testing.T) {
	if _, err := parseProviderTOML("not a toml line at all"); err == nil {
		t.Fatal("garbage line must be a parse error")
	}
}

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	path := t.TempDir() + "/providers.toml"
	if err := providerTestWriteFile(path, body); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}
