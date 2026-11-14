package authsvc

// providers.go: pluggable HTTP-webhook adapters for SMSSender / Mailer /
// CaptchaVerifier (lts-1.4-plan §5 rows 2 and 7, OPS-2).
//
// Design (docs/lts-1.4-remaining.md §3):
//   - ONE generic transport: POST a JSON body to a configured URL with a 3s
//     timeout; any non-2xx status or transport error is a failure. Vendor
//     SDKs (Aliyun / Tencent / ...) are NOT implemented here — the example
//     config (deploy/providers.example.toml) shows how to point the webhook
//     at a self-hosted relay for those clouds. Zero vendor constants in code.
//   - Configuration is read from a small TOML file ([providers.*] sections)
//     through the deployment's existing config-file mechanism; nothing is
//     hardcoded. When the file is missing or a channel URL is empty the
//     adapter for that channel is simply not produced, so the service keeps
//     its existing fail-closed behavior (no send, no panic — otp.go already
//     nil-checks s.sms / s.mailer / s.captcha).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultWebhookTimeout is the per-call budget for every provider webhook.
const DefaultWebhookTimeout = 3 * time.Second

// ProviderConfig is the decoded providers file.
type ProviderConfig struct {
	SMSURL     string        // [providers.sms] url
	MailURL    string        // [providers.mail] url
	CaptchaURL string        // [providers.captcha] url
	Timeout    time.Duration // optional timeout_ms, default 3s
}

// Configured reports whether any channel URL is present.
func (c ProviderConfig) Configured() bool {
	return c.SMSURL != "" || c.MailURL != "" || c.CaptchaURL != ""
}

// LoadProviderConfig reads path. A missing file is NOT an error: it returns
// ok=false and the caller keeps the nil (fail-closed) adapters. A malformed
// file IS an error (startup must not silently ignore a half-written config).
func LoadProviderConfig(path string) (ProviderConfig, bool, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ProviderConfig{}, false, nil
		}
		return ProviderConfig{}, false, fmt.Errorf("read providers config: %w", err)
	}
	cfg, perr := parseProviderTOML(string(body))
	if perr != nil {
		return ProviderConfig{}, false, fmt.Errorf("%s: %w", path, perr)
	}
	return cfg, cfg.Configured(), nil
}

// LoadProviders builds the adapters for the assembly layer. All three return
// values are nil when the config file is missing or has no URLs — the Deps
// stay nil and authsvc keeps its fail-closed paths unchanged.
func LoadProviders(path string) (SMSSender, Mailer, CaptchaVerifier, error) {
	cfg, ok, err := LoadProviderConfig(path)
	if err != nil || !ok {
		return nil, nil, nil, err
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultWebhookTimeout
	}
	return newWebhookSMS(cfg.SMSURL, timeout),
		newWebhookMailer(cfg.MailURL, timeout),
		newWebhookCaptcha(cfg.CaptchaURL, timeout),
		nil
}

// ---------------------------------------------------------------- webhook transport

// webhookPoster is the single generic transport: POST JSON, expect 2xx.
type webhookPoster struct {
	url     string
	timeout time.Duration
	kind    string
	client  *http.Client
}

func newPoster(url, kind string, timeout time.Duration) *webhookPoster {
	return &webhookPoster{
		url:     url,
		timeout: timeout,
		kind:    kind,
		client:  &http.Client{Timeout: timeout},
	}
}

// post sends one payload. The 3s budget is enforced twice (client timeout and
// an explicit context deadline) so an attacker-controlled endpoint cannot
// stall a login request beyond the configured budget.
func (p *webhookPoster) post(ctx context.Context, payload map[string]any) error {
	if p == nil || p.url == "" {
		// Unreachable via the assembly (adapters only exist when a URL is
		// configured), but fail-closed rather than panic if misused.
		return errors.New("provider webhook not configured")
	}
	cctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	buf, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s payload: %w", p.kind, err)
	}
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, p.url, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("build %s request: %w", p.kind, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s webhook: %w", p.kind, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s webhook: unexpected status %d", p.kind, resp.StatusCode)
	}
	return nil
}

// ---------------------------------------------------------------- adapters

type webhookSMS struct{ p *webhookPoster }

// newWebhookSMS returns nil when url is empty (channel stays fail-closed).
func newWebhookSMS(url string, timeout time.Duration) SMSSender {
	if url == "" {
		return nil
	}
	return &webhookSMS{p: newPoster(url, "sms", timeout)}
}

func (w *webhookSMS) SendOTP(ctx context.Context, phone, code string) error {
	return w.p.post(ctx, map[string]any{"type": "sms", "phone": phone, "code": code})
}

type webhookMailer struct{ p *webhookPoster }

func newWebhookMailer(url string, timeout time.Duration) Mailer {
	if url == "" {
		return nil
	}
	return &webhookMailer{p: newPoster(url, "magic_link", timeout)}
}

func (w *webhookMailer) SendMagicLink(ctx context.Context, email, link string) error {
	return w.p.post(ctx, map[string]any{"type": "magic_link", "email": email, "link": link})
}

type webhookCaptcha struct{ p *webhookPoster }

func newWebhookCaptcha(url string, timeout time.Duration) CaptchaVerifier {
	if url == "" {
		return nil
	}
	return &webhookCaptcha{p: newPoster(url, "captcha", timeout)}
}

func (w *webhookCaptcha) Verify(ctx context.Context, token, remoteIP string) error {
	return w.p.post(ctx, map[string]any{"type": "captcha", "token": token, "remote_ip": remoteIP})
}

// ---------------------------------------------------------------- tiny TOML subset

// parseProviderTOML understands exactly what the example template uses:
// comments, [section] headers and key = "value" strings. It mirrors the shape
// of the chatd config parser (cmd/chatd/config.go) but lives here so authsvc
// owns its own config reading without importing package main.
func parseProviderTOML(body string) (ProviderConfig, error) {
	var cfg ProviderConfig
	section := ""
	for i, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				return cfg, fmt.Errorf("line %d: unterminated section", i+1)
			}
			section = strings.TrimSpace(line[1:end])
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			return cfg, fmt.Errorf("line %d: expected key = value", i+1)
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if hash := strings.IndexByte(val, '#'); hash >= 0 && !strings.HasPrefix(val, "\"") {
			val = strings.TrimSpace(val[:hash])
		}
		val = strings.Trim(val, `"'`)
		switch section + "." + key {
		case "providers.sms.url":
			cfg.SMSURL = val
		case "providers.mail.url":
			cfg.MailURL = val
		case "providers.captcha.url":
			cfg.CaptchaURL = val
		case "providers.timeout_ms":
			var n int
			if _, err := fmt.Sscanf(val, "%d", &n); err == nil && n > 0 {
				cfg.Timeout = time.Duration(n) * time.Millisecond
			}
		}
	}
	return cfg, nil
}
