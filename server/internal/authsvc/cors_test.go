package authsvc

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The backend serves an INDEPENDENT client program (the UI is not part of
// this repository anymore). CORS therefore has to work for a foreign origin,
// while the tighter modes stay available for co-hosted deployments.

func TestCORSAllowAnyReflectsForeignOrigin(t *testing.T) {
	env := methodsEnv(t, MethodFlags{Guest: true})
	// The test env leaves CORSMode empty -> allow_any is the default.

	req := httptest.NewRequest(http.MethodOptions, "/api/auth/methods", nil)
	req.Header.Set("Origin", "https://client.example.com")
	rec := httptest.NewRecorder()
	env.svc.Routes().ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://client.example.com" {
		t.Fatalf("Allow-Origin = %q, want the reflected origin", got)
	}
	if rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("Allow-Credentials must be true for a credentialed client")
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got == "" || got == "POST, OPTIONS" {
		t.Fatalf("Allow-Methods = %q, want GET included (the discovery endpoint is GET)", got)
	}
}

func TestCORSAllowAnyRejectsGarbageOrigin(t *testing.T) {
	env := methodsEnv(t, MethodFlags{Guest: true})

	req := httptest.NewRequest(http.MethodOptions, "/api/auth/methods", nil)
	req.Header.Set("Origin", "not-a-url")
	rec := httptest.NewRecorder()
	env.svc.Routes().ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Allow-Origin = %q, want empty for a malformed origin", got)
	}
}

func TestCORSAllowListOnlyAnswersConfiguredOrigins(t *testing.T) {
	env := newTestEnv(t, func(c *Config) {
		c.Methods = func() MethodFlags { return MethodFlags{Guest: true} }
		c.CORSMode = CORSModeAllowList
		c.PublicOrigin = "https://ui.example.com"
		c.CORSOrigins = []string{"https://alt.example.com"}
	})

	cases := []struct {
		origin string
		want   string
	}{
		{"https://ui.example.com", "https://ui.example.com"},
		{"https://alt.example.com", "https://alt.example.com"},
		{"https://evil.example.com", ""},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodOptions, "/api/auth/methods", nil)
		req.Header.Set("Origin", tc.origin)
		rec := httptest.NewRecorder()
		env.svc.Routes().ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != tc.want {
			t.Fatalf("origin %s: Allow-Origin = %q, want %q", tc.origin, got, tc.want)
		}
	}
}

func TestCORSOffSendsNothing(t *testing.T) {
	env := newTestEnv(t, func(c *Config) {
		c.Methods = func() MethodFlags { return MethodFlags{Guest: true} }
		c.CORSMode = CORSModeOff
	})

	req := httptest.NewRequest(http.MethodOptions, "/api/auth/methods", nil)
	req.Header.Set("Origin", "https://client.example.com")
	rec := httptest.NewRecorder()
	env.svc.Routes().ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Allow-Origin = %q, want empty in off mode", got)
	}
}
