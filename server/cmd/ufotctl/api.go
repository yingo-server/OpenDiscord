package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// credentials mirrors adminctl's credentials file ($XDG_CONFIG_HOME/ufot/
// adminctl.json). Sharing it means `adminctl login` once is enough for both
// CLIs — and guarantees the exact same auth/CSRF path (no side channel).
type credentials struct {
	Server    string `json:"server"`
	Token     string `json:"token"`
	CSRF      string `json:"csrf_token"`
	Username  string `json:"username,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
}

// loadAdminctlCredentials never fails: a missing/corrupt file just means "no
// stored credentials" (explicit flags or env can still authenticate).
func loadAdminctlCredentials(e *environment) credentials {
	var c credentials
	b, err := e.readFile(adminctlCredPath(e))
	if err != nil {
		return c
	}
	_ = json.Unmarshal(b, &c)
	return c
}

// statusError is a non-2xx admin API response carrying the unified error body
// {code, reason, request_id} (docs/admin-data-design.md §8).
type statusError struct {
	Status    int
	Reason    string
	RequestID string
	Body      string
}

func (e *statusError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Reason)
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// apiClient is the thin REST client for the loopback admin API.
type apiClient struct {
	base  string
	token string
	csrf  string
	do    func(*http.Request) (*http.Response, error)
}

func newAPIClient(e *environment, g globals) (*apiClient, error) {
	if strings.TrimSpace(g.Token) == "" {
		return nil, deniedf("未配置管理令牌：请先运行 adminctl login（或设置 UFOT_ADMIN_TOKEN / --token）")
	}
	base := strings.TrimRight(strings.TrimSpace(g.AdminURL), "/")
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, usagef("invalid admin URL %q", base)
	}
	return &apiClient{base: base, token: g.Token, csrf: g.CSRF, do: e.httpDo}, nil
}

// doJSON performs one API call and returns the raw body.
func (c *apiClient) doJSON(ctx context.Context, method, path string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if c.csrf != "" {
		// Strict admin CSRF mode: double submit even for Bearer calls.
		req.Header.Set("X-CSRF-Token", c.csrf)
		req.AddCookie(&http.Cookie{Name: "oc_admin_csrf", Value: c.csrf})
	}
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("Idempotency-Key", newUUID())
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("admin API %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		se := &statusError{Status: resp.StatusCode, Body: string(raw)}
		var eb struct {
			Reason    string `json:"reason"`
			RequestID string `json:"request_id"`
		}
		if json.Unmarshal(raw, &eb) == nil {
			se.Reason, se.RequestID = eb.Reason, eb.RequestID
		}
		if se.Reason == "" {
			se.Reason = http.StatusText(resp.StatusCode)
		}
		return nil, se
	}
	return raw, nil
}

// classifyAPIError maps auth failures onto exit code 77, missing targets
// onto exit 65 (admin-data §7「目标不存在 65」), and leaves everything else
// as a plain error (exit 1).
func classifyAPIError(err error) error {
	if err == nil {
		return nil
	}
	var se *statusError
	if errors.As(err, &se) {
		switch se.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return deniedf("%s（令牌无效/无权限：请重新 adminctl login）", se.Error())
		case http.StatusNotFound:
			return notFoundf("%s", se.Error())
		}
	}
	return err
}

// jsonMarshalIndent is the single JSON pretty-printer used by --json.
func jsonMarshalIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

// newUUID returns a random RFC 4122 v4 string (Idempotency-Key).
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("ufotctl-%d", len(b))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
