package main

// config.go: the chatd configuration (config/chatd.toml, a deliberately tiny
// TOML subset: comments, [sections], key = value with strings/bools/ints).
// No third-party TOML dependency: the file is small, owned by this binary, and
// unknown keys must not crash a running deployment.

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// config is the effective chatd configuration (file + defaults).
type config struct {
	Mode            string // embedded (default) | split (not built here)
	Listen          string // user HTTP+WS listen address
	AdminListen     string // admin HTTP listen address (bridge AdminAPI: /admin/inject)
	AdminRESTListen string // admin REST listen address (/api/v1/*) — loopback only
	PublicOrigin    string // e.g. https://chat.example.com

	HotDB   string // SQLite hot database
	AdminDB string // SQLite admin database (separate file, least privilege)
	ColdDSN string // PostgreSQL DSN (parsed, not connected in this build)
	// RequireAuditStore makes a missing cold store a startup failure. When
	// false (default) a coldDB==nil deployment starts but logs an ERROR: audit
	// events will not persist. When true, startup aborts unless a cold DB with
	// a registered driver is available.
	RequireAuditStore bool
	PolicyPath        string // policy.json
	JWTKeyPath        string // Ed25519 PKCS#8 PEM
	PIIKeyPath        string // phone_lookup_key (hex) — identifier HMAC index key
	MediaDir          string

	LogLevel string

	// ShutdownTimeout bounds the whole SIGTERM drain (usergw → pipe → bridge).
	ShutdownTimeout int // seconds

	AccessTTLSec  int
	RefreshTTLSec int
	DevOTP        bool

	// RateLimitEnabled=false 放大 WS 限流到实际上无限——仅供压测/容量测试
	// 使用（测「带载极限」而非「攻击面」）。生产配置绝不可置 false。
	RateLimitEnabled bool

	// Flags are the boot-time feature-flag defaults written into the hot DB
	// and served to clients (SIGHUP re-publishes them).
	Flags map[string]bool

	// TurnURL 是自建 TURN 地址（turn:/turns:），仅管理面 TURN 自检用
	//（OPS-2：admin diagnostics ProbeTURN；空 = 自检返回"未配置"）。
	TurnURL string

	// ProvidersPath 是供应商 webhook 配置文件路径（OPS-2：authsvc 适配器；
	// 空/缺失 = 通道保持 fail-closed，不发送、不 panic）。
	ProvidersPath string
}

func defaultConfig() *config {
	return &config{
		Mode:            "embedded",
		Listen:          "127.0.0.1:8080",
		AdminListen:     "127.0.0.1:7780",
		AdminRESTListen: "127.0.0.1:7781",
		PublicOrigin:    "",
		HotDB:           "./data/chatd.db",
		AdminDB:         "./data/admin.db",
		ColdDSN:         "",
		PolicyPath:      "./config/policy.json",
		JWTKeyPath:      "./data/keys/jwt_sign_key.pem",
		PIIKeyPath:      "", // derived: sibling of JWTKeyPath → phone_lookup_key
		MediaDir:        "./data/media",
		LogLevel:        "info",
		ShutdownTimeout: 30,
		AccessTTLSec:    900,
		RefreshTTLSec:   30 * 24 * 3600,
		DevOTP:          false,
		// Rate limiting is ON by default: flipping it off is a capacity-test
		// switch, never a production setting (security-spec §1 红线).
		RateLimitEnabled: true,
		Flags:            map[string]bool{},
	}
}

// loadConfig reads path. A missing file is not an error (all defaults), which
// keeps `chatd` runnable from a checkout; a malformed file is (exit 1).
func loadConfig(path string) (*config, error) {
	cfg := defaultConfig()
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	doc, err := parseTOML(string(body))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	for k, v := range doc.top {
		switch k {
		case "mode":
			cfg.Mode = v
		case "listen":
			cfg.Listen = v
		case "admin_listen":
			cfg.AdminListen = v
		case "admin_rest_listen":
			cfg.AdminRESTListen = v
		case "public_origin":
			cfg.PublicOrigin = v
		case "hot_db":
			cfg.HotDB = v
		case "admin_db":
			cfg.AdminDB = v
		case "cold_dsn":
			cfg.ColdDSN = v
		case "require_audit_store":
			cfg.RequireAuditStore = v == "true"
		case "policy_path":
			cfg.PolicyPath = v
		case "jwt_key_path":
			cfg.JWTKeyPath = v
		case "pii_hmac_key_path", "phone_lookup_key_path":
			cfg.PIIKeyPath = v
		case "media_dir":
			cfg.MediaDir = v
		case "log_level":
			cfg.LogLevel = v
		case "rate_limit_enabled":
			cfg.RateLimitEnabled = v == "true"
		case "dev_otp":
			cfg.DevOTP = v == "true"
		case "shutdown_timeout_sec":
			if n, e := strconv.Atoi(v); e == nil && n > 0 {
				cfg.ShutdownTimeout = n
			}
		case "access_ttl_sec":
			if n, e := strconv.Atoi(v); e == nil && n > 0 {
				cfg.AccessTTLSec = n
			}
		case "refresh_ttl_sec":
			if n, e := strconv.Atoi(v); e == nil && n > 0 {
				cfg.RefreshTTLSec = n
			}
		case "providers_path":
			cfg.ProvidersPath = v
		case "turn_url":
			cfg.TurnURL = v
		}
	}
	// [feature_flags] section overrides the compiled defaults.
	if sec, ok := doc.sections["feature_flags"]; ok {
		for k, v := range sec {
			cfg.Flags[k] = v == "true"
		}
	}
	if cfg.PIIKeyPath == "" {
		cfg.PIIKeyPath = defaultPIIKeyPath(cfg.JWTKeyPath)
	}
	return cfg, nil
}

func defaultPIIKeyPath(jwtPath string) string {
	dir := "."
	if i := strings.LastIndexByte(jwtPath, '/'); i >= 0 {
		dir = jwtPath[:i]
	}
	return dir + "/phone_lookup_key"
}

// tomlDoc is the parsed file: top-level scalars + [section] scalars.
type tomlDoc struct {
	top      map[string]string
	sections map[string]map[string]string
}

func parseTOML(body string) (*tomlDoc, error) {
	doc := &tomlDoc{top: map[string]string{}, sections: map[string]map[string]string{}}
	cur := doc.top
	for i, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				return nil, fmt.Errorf("line %d: unterminated section", i+1)
			}
			name := strings.TrimSpace(line[1:end])
			if name == "" {
				return nil, fmt.Errorf("line %d: empty section name", i+1)
			}
			cur = map[string]string{}
			doc.sections[name] = cur
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("line %d: expected key = value", i+1)
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if hash := strings.IndexByte(val, '#'); hash >= 0 && !strings.HasPrefix(val, "\"") {
			val = strings.TrimSpace(val[:hash])
		}
		val = strings.Trim(val, `"'`)
		cur[key] = val
	}
	return doc, nil
}
