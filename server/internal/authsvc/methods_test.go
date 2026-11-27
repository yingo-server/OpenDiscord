package authsvc

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The login-method switches and the public discovery endpoint.
//
// Contract under test:
//   - a disabled method answers 403 METHOD_DISABLED and parses nothing
//   - GET /api/auth/methods reports the live switch set
//   - the discovery endpoint is rate limited per IP (429 RATE_LIMITED)

// methodsEnv builds a service whose switches are exactly the given set.
func methodsEnv(t *testing.T, m MethodFlags) *testEnv {
	t.Helper()
	return newTestEnv(t, func(c *Config) {
		c.Methods = func() MethodFlags { return m }
	})
}

func TestMethodDisabledRejectsLogin(t *testing.T) {
	env := methodsEnv(t, MethodFlags{})

	cases := []struct {
		name string
		path string
		body string
	}{
		{"phone code", "/api/auth/phone/code", `{"phone":"+8613800138000"}`},
		{"phone verify", "/api/auth/phone/verify", `{"phone":"+8613800138000","code":"123456"}`},
		{"email link", "/api/auth/email/link", `{"email":"a@b.c"}`},
		{"email poll", "/api/auth/email/poll", `{"email":"a@b.c","challenge_id":"x"}`},
		{"password", "/api/auth/password", `{"account":"a@b.c","password":"whatever"}`},
		{"guest", "/api/auth/guest", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := env.post(t, tc.path, tc.body, nil)
			if status != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body=%s", status, body)
			}
			var e struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			if err := json.Unmarshal([]byte(body), &e); err != nil {
				t.Fatalf("decode body: %v (%s)", err, body)
			}
			if e.Code != "METHOD_DISABLED" {
				t.Fatalf("code = %q, want METHOD_DISABLED", e.Code)
			}
			if e.Error == "" {
				t.Fatalf("error message must not be empty")
			}
		})
	}
}

func TestMethodEnabledStillWorks(t *testing.T) {
	env := methodsEnv(t, MethodFlags{Guest: true})

	status, body := env.post(t, "/api/auth/guest", `{}`, nil)
	if status != http.StatusOK {
		t.Fatalf("guest status = %d, want 200; body=%s", status, body)
	}
	// A disabled method stays disabled even when a sibling is enabled.
	status, body = env.post(t, "/api/auth/password", `{"account":"a@b.c","password":"x"}`, nil)
	if status != http.StatusForbidden {
		t.Fatalf("password status = %d, want 403; body=%s", status, body)
	}
}

func TestMethodsDiscoveryEndpoint(t *testing.T) {
	env := methodsEnv(t, MethodFlags{Email: true, Guest: true})

	resp, err := http.Get(env.srv.URL + "/api/auth/methods")
	if err != nil {
		t.Fatalf("GET methods: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
	var out struct {
		Methods struct {
			Phone    bool `json:"phone"`
			Email    bool `json:"email"`
			Password bool `json:"password"`
			Guest    bool `json:"guest"`
		} `json:"methods"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Methods.Phone || out.Methods.Password {
		t.Fatalf("disabled methods reported as enabled: %+v", out.Methods)
	}
	if !out.Methods.Email || !out.Methods.Guest {
		t.Fatalf("enabled methods reported as disabled: %+v", out.Methods)
	}
}

func TestMethodsDiscoveryIsRateLimited(t *testing.T) {
	env := methodsEnv(t, MethodFlags{Guest: true})

	// Tighten the budget for the test: the limiter is per-IP and this test
	// client always presents the same source address.
	env.svc.cfg.MethodsProbeBudget = func() int { return 3 }

	var lastStatus int
	for i := 0; i < 5; i++ {
		resp, err := http.Get(env.srv.URL + "/api/auth/methods")
		if err != nil {
			t.Fatalf("GET methods: %v", err)
		}
		lastStatus = resp.StatusCode
		_ = resp.Body.Close()
	}
	if lastStatus != http.StatusTooManyRequests {
		t.Fatalf("last status = %d, want 429 (rate limit must bite)", lastStatus)
	}
}

func TestMethodFlagsAnyEnabled(t *testing.T) {
	if (MethodFlags{}).AnyEnabled() {
		t.Fatalf("empty switch set must report no enabled method")
	}
	if !(MethodFlags{Guest: true}).AnyEnabled() {
		t.Fatalf("guest-only switch set must report an enabled method")
	}
}
