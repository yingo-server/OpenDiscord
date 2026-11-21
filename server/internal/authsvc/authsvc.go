// Package authsvc is the user authentication service (`/api/auth/*`).
//
// It is mounted on the same HTTP listener as the user gateway (WS acceptor),
// so the browser sees one origin: the SPA posts to /api/auth/*, gets an
// access_token cookie in return, and the WS upgrade on /ws is authenticated by
// that cookie at handshake time (docs/security-spec.md §2.3).
//
// Responsibilities (and nothing else):
//   - phone OTP send/verify, email magic link + poll, account password login,
//     guest device login, refresh rotation with replay detection, logout
//   - Ed25519 access(15m)/refresh(30d) issuance via core/authn (claims:
//     sub/jti/did/typ/ver/exp/iat — no PII, security-spec §2.1)
//   - persistence of the auth domain extension tables (identity_otp,
//     identity_refresh, identity_credentials) plus identity lookups through
//     store.IdentityStore; identifiers are stored ONLY as HMAC index values
//
// Explicit non-goals: no WS/connection state, no session table, no profile
// management, no mail/SMS transport (interfaces are injected; see Mailer).
package authsvc

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/yingo-server/UFOT/server/internal/data/store"
	"github.com/yingo-server/UFOT/server/internal/pkg/log"
	"github.com/yingo-server/UFOT/server/internal/user/core/authn"
)

// Identity types written into identity_identities.id_type.
const (
	IDTypePhone    = 1
	IDTypeEmail    = 2
	IDTypeUsername = 3
	IDTypeGuest    = 4

	// IDTypeIP is only used to derive HMAC indexes for source IPs (rate
	// limiting). It is never written to identity_identities.
	IDTypeIP = 90
)

// Defaults from docs/security-spec.md §2.1 / §2.2 / §2.5 and §1.5.2.
const (
	DefaultAccessTTL       = 15 * time.Minute
	DefaultRefreshTTL      = 30 * 24 * time.Hour
	DefaultOTPTTL          = 5 * time.Minute
	DefaultOTPMaxAttempts  = 5
	DefaultResendInterval  = 60 * time.Second
	DefaultPhoneDailyCap   = 10
	DefaultPhoneHourlyCap  = 5
	DefaultIPHourlyCap     = 5
	DefaultFreezeDistinct  = 3
	DefaultVerifyLock      = 15 * time.Minute
	DefaultLockThreshold   = 5
	DefaultMaxBodyBytes    = 8 << 10
	devOTPCode             = "123456" // only ever used when APP_ENV=dev + Config.DevOTP
	magicLinkTokenBytes    = 32
	challengeIDBytes       = 16
	maxAccountLen          = 254
	minPasswordLen         = 10 // security-spec §2.4
	minHMACKeyBytes        = 16
	cookieAccessTokenName  = "access_token"
	headerAuthorization    = "Authorization"
	bearerPrefix           = "Bearer "
	defaultClockSkewSecond = 0
)

// Mailer delivers a magic link. The transport (SMTP / provider API) is NOT
// wired in this batch: the interface exists so the assembly can inject one
// later without touching the auth flow.
type Mailer interface {
	SendMagicLink(ctx context.Context, email, link string) error
}

// CaptchaVerifier validates a captcha token. Default is nil = fail-closed:
// when a captcha is required and no verifier is configured, the request is
// rejected with code CAPTCHA_REQUIRED instead of silently accepted.
type CaptchaVerifier interface {
	Verify(ctx context.Context, token, remoteIP string) error
}

// Config is the service policy/parameter set (all tunables live here, none in
// the handlers).
type Config struct {
	PublicOrigin      string        // e.g. https://chat.example.com (exact-match CORS)
	AccessTTL         time.Duration // default 15m
	RefreshTTL        time.Duration // default 30d
	OTPTTL            time.Duration // default 5m
	OTPMaxAttempts    int           // default 5
	ResendInterval    time.Duration // default 60s
	PhoneDailyCap     int           // default 10/day
	PhoneHourlyCap    int           // default 5/hour
	IPHourlyCap       int           // default 5/hour per source IP
	FreezeDistinctIPs int           // default 3 distinct IPs/24h -> freeze OTP send
	FreezeWindow      time.Duration
	VerifyLock        time.Duration // wrong-code lock, default 15m
	LockThreshold     int           // login failures per escalation step, default 5
	LockDurations     []time.Duration

	// DevOTP enables the fixed OTP 123456. Refused unless APP_ENV=dev.
	DevOTP bool
	// SecureCookies controls the Secure flag of the access_token cookie.
	SecureCookies bool

	MaxBodyBytes int
	Now          func() time.Time
	Logger       *log.Logger
}

// Deps is the injected dependency set. In embedded mode DB, Identity and Sys
// are all satisfied by the same *sqlite.DB handle.
type Deps struct {
	DB       *sql.DB
	Identity store.IdentityStore
	Sys      store.SysStore
	Signer   *authn.Signer
	Verifier *authn.Verifier
	Hasher   *authn.Hasher
	// HMACKey is the identifier index key (security-spec: phone_lookup_key).
	// It must be a dedicated key file, never the JWT key.
	HMACKey []byte
	Mailer  Mailer
	SMS     SMSSender
	Captcha CaptchaVerifier
}

// Service is the assembled authentication HTTP service.
type Service struct {
	cfg      Config
	st       *authStore
	ident    store.IdentityStore
	sys      store.SysStore
	signer   *authn.Signer
	verifier *authn.Verifier
	hasher   *authn.Hasher
	hmacKey  []byte
	mailer   Mailer
	sms      SMSSender
	captcha  CaptchaVerifier
	lg       *log.Logger

	mux *http.ServeMux
}

// New validates the dependency set and builds the router.
func New(deps Deps, cfg Config) (*Service, error) {
	if deps.DB == nil {
		return nil, errors.New("authsvc: DB required")
	}
	if deps.Identity == nil {
		return nil, errors.New("authsvc: identity store required")
	}
	if deps.Sys == nil {
		return nil, errors.New("authsvc: sys store required")
	}
	if deps.Signer == nil || deps.Verifier == nil || deps.Hasher == nil {
		return nil, errors.New("authsvc: signer/verifier/hasher required")
	}
	if len(deps.HMACKey) < minHMACKeyBytes {
		return nil, fmt.Errorf("authsvc: HMAC index key must be >= %d bytes", minHMACKeyBytes)
	}
	cfg = withDefaults(cfg)
	if cfg.DevOTP && os.Getenv("APP_ENV") != "dev" {
		// Startup assertion, security-spec §2.5: the fixed-code path must be
		// impossible to enable in a production configuration.
		return nil, errors.New("authsvc: dev OTP requested but APP_ENV != dev")
	}
	lg := cfg.Logger
	if lg == nil {
		lg = log.For("authsvc")
	}
	if cfg.DevOTP {
		lg.Warn("DEV OTP ENABLED: fixed code, never run this in production")
	}
	s := &Service{
		cfg:      cfg,
		st:       &authStore{db: deps.DB},
		ident:    deps.Identity,
		sys:      deps.Sys,
		signer:   deps.Signer,
		verifier: deps.Verifier,
		hasher:   deps.Hasher,
		hmacKey:  append([]byte(nil), deps.HMACKey...),
		mailer:   deps.Mailer,
		sms:      deps.SMS,
		captcha:  deps.Captcha,
		lg:       lg,
	}
	s.mux = s.routes()
	return s, nil
}

// Routes returns the /api/auth/* handler (mount it on the shared port).
//
// The wrapper answers CORS preflight before the method-scoped mux is consulted
// (a bare "POST /api/auth/..." pattern would answer OPTIONS with 405).
func (s *Service) Routes() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			s.applyCORS(w, r)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s.applyCORS(w, r)
		s.mux.ServeHTTP(w, r)
	})
}

// ServeHTTP implements http.Handler (same as Routes()).
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.Routes().ServeHTTP(w, r) }

func withDefaults(c Config) Config {
	if c.AccessTTL <= 0 {
		c.AccessTTL = DefaultAccessTTL
	}
	if c.RefreshTTL <= 0 {
		c.RefreshTTL = DefaultRefreshTTL
	}
	if c.OTPTTL <= 0 {
		c.OTPTTL = DefaultOTPTTL
	}
	if c.OTPMaxAttempts <= 0 {
		c.OTPMaxAttempts = DefaultOTPMaxAttempts
	}
	if c.ResendInterval <= 0 {
		c.ResendInterval = DefaultResendInterval
	}
	if c.PhoneDailyCap <= 0 {
		c.PhoneDailyCap = DefaultPhoneDailyCap
	}
	if c.PhoneHourlyCap <= 0 {
		c.PhoneHourlyCap = DefaultPhoneHourlyCap
	}
	if c.IPHourlyCap <= 0 {
		c.IPHourlyCap = DefaultIPHourlyCap
	}
	if c.FreezeDistinctIPs <= 0 {
		c.FreezeDistinctIPs = DefaultFreezeDistinct
	}
	if c.FreezeWindow <= 0 {
		c.FreezeWindow = 24 * time.Hour
	}
	if c.VerifyLock <= 0 {
		c.VerifyLock = DefaultVerifyLock
	}
	if c.LockThreshold <= 0 {
		c.LockThreshold = DefaultLockThreshold
	}
	if len(c.LockDurations) == 0 {
		// 5 failures -> 15m, next 5 -> 1h, then 24h (security-spec A-1).
		c.LockDurations = []time.Duration{15 * time.Minute, time.Hour, 24 * time.Hour}
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// ---------------------------------------------------------------- routing

func (s *Service) routes() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("POST /api/auth/phone/code", s.handler(s.handlePhoneCode))
	m.HandleFunc("POST /api/auth/phone/verify", s.handler(s.handlePhoneVerify))
	m.HandleFunc("POST /api/auth/email/link", s.handler(s.handleEmailLink))
	m.HandleFunc("POST /api/auth/email/poll", s.handler(s.handleEmailPoll))
	m.HandleFunc("POST /api/auth/password", s.handler(s.handlePasswordLogin))
	m.HandleFunc("POST /api/auth/guest", s.handler(s.handleGuest))
	m.HandleFunc("POST /api/auth/refresh", s.handler(s.handleRefresh))
	m.HandleFunc("POST /api/auth/logout", s.handler(s.handleLogout))
	// Extension (not part of the client contract): set/replace a password for
	// the authenticated account. Requires an access token.
	m.HandleFunc("POST /api/auth/password/set", s.handler(s.handlePasswordSet))
	return m
}

// ---------------------------------------------------------------- plumbing

// apiError is the error envelope consumed by the client's AuthApi
// ({error, code, retry_after_ms}).
type apiError struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

func errBadRequest(msg string) *apiError {
	return &apiError{Status: http.StatusBadRequest, Code: "BAD_REQUEST", Message: msg}
}

func errUnauthorized(code string) *apiError {
	return &apiError{Status: http.StatusUnauthorized, Code: code, Message: "authentication failed"}
}

func errRateLimited(code string, retry time.Duration) *apiError {
	return &apiError{Status: http.StatusTooManyRequests, Code: code, Message: "too many requests", RetryAfter: retry}
}

func errInternal() *apiError {
	return &apiError{Status: http.StatusInternalServerError, Code: "INTERNAL", Message: "internal error"}
}

type errBody struct {
	Error        string `json:"error"`
	Code         string `json:"code"`
	RetryAfterMS int64  `json:"retry_after_ms,omitempty"`
	RetryAfter   int64  `json:"retry_after,omitempty"`
	Status       string `json:"status,omitempty"`
}

func (s *Service) writeError(w http.ResponseWriter, e *apiError) {
	body := errBody{Error: e.Message, Code: e.Code}
	if e.RetryAfter > 0 {
		body.RetryAfterMS = e.RetryAfter.Milliseconds()
		body.RetryAfter = int64(e.RetryAfter / time.Second)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Service) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// jsonEncode writes an already-encoded body to the response writer.
func jsonEncode(w http.ResponseWriter, v any) error { return json.NewEncoder(w).Encode(v) }

// decode reads one JSON body with a hard size limit.
func (s *Service) decode(w http.ResponseWriter, r *http.Request, dst any) *apiError {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		return errBadRequest("content-type must be application/json")
	}
	r.Body = http.MaxBytesReader(w, r.Body, int64(s.cfg.MaxBodyBytes))
	dec := json.NewDecoder(r.Body)
	// Unknown fields are ignored on purpose: the contract pins the fields the
	// client sends today, but a newer client adding an optional field must not
	// turn a login into a 400. Size, type and semantic checks still apply.
	if err := dec.Decode(dst); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return errBadRequest("body too large")
		}
		if errors.Is(err, io.EOF) {
			return errBadRequest("empty body")
		}
		return errBadRequest("invalid JSON body")
	}
	return nil
}

// handler adapts a func(w,r) *apiError to http.HandlerFunc with panic
// recovery, CORS and no-store semantics.
func (s *Service) handler(fn func(http.ResponseWriter, *http.Request) *apiError) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.lg.Error("auth handler panic", "path", r.URL.Path, "panic", fmt.Sprint(rec))
				s.writeError(w, errInternal())
			}
		}()
		s.applyCORS(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if e := fn(w, r); e != nil {
			if e.Status >= 500 {
				s.lg.Error("auth request failed", "path", r.URL.Path, "code", e.Code)
			}
			s.writeError(w, e)
		}
	}
}

// applyCORS allows exactly the configured public origin (never a prefix or a
// wildcard, security-spec §2.6 style strictness), with credentials.
func (s *Service) applyCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	if s.cfg.PublicOrigin != "" && strings.EqualFold(origin, s.cfg.PublicOrigin) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Allow-Credentials", "true")
		h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		h.Set("Vary", "Origin")
	}
}

// clientIP resolves the caller address. X-Forwarded-For is only honored when
// the immediate peer is the loopback Caddy (docs/deploy.md: Caddy fronts the
// process and is the only way in).
func (s *Service) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if isLoopback(host) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first := strings.TrimSpace(strings.Split(xff, ",")[0])
			if ip := net.ParseIP(first); ip != nil {
				return ip.String()
			}
		}
		if ip := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); ip != nil {
			return ip.String()
		}
	}
	return host
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ---------------------------------------------------------------- identity

var (
	phoneRe    = regexp.MustCompile(`^\+?[0-9]{6,15}$`)
	emailRe    = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]{2,}$`)
	usernameRe = regexp.MustCompile(`^[a-z0-9_.-]{3,32}$`)
)

// normalizePhone strips spaces/dashes. We do not invent a country code: the
// caller must send E.164 (+86...) or the bare national number consistently.
func normalizePhone(p string) string {
	p = strings.TrimSpace(p)
	p = strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(p)
	return p
}

func normalizeEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

func normalizeUsername(u string) string { return strings.ToLower(strings.TrimSpace(u)) }

// idIndex is the HMAC index of an identifier: HMAC-SHA256(key,
// "<idType>:<normalized>") hex. This is what identity_identities.id_value
// stores, so the plaintext identifier never reaches the database
// (docs/security-spec.md §5.3).
func (s *Service) idIndex(idType int, normalized string) string {
	mac := hmac.New(sha256.New, s.hmacKey)
	fmt.Fprintf(mac, "%d:%s", idType, normalized)
	return hex.EncodeToString(mac.Sum(nil))
}

// subjectShort is the only form of a subject that may appear in logs
// (security-spec §5.3: no plaintext phone/email ever).
func subjectShort(index string) string {
	if len(index) <= 8 {
		return "****"
	}
	return index[:8] + "…"
}

func shortToken(s string) string {
	if len(s) <= 8 {
		return "****"
	}
	return s[:8] + "…"
}

// hmacEqual compares two hex digests in constant time (the values are public
// hex strings, so length is not secret).
func hmacEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// codeDigest = HMAC-SHA256(otpKey, challenge_id || 0x00 || code).
func (s *Service) codeDigest(challengeID, code string) string {
	mac := hmac.New(sha256.New, s.hmacKey)
	mac.Write([]byte("otp\x00"))
	mac.Write([]byte(challengeID))
	mac.Write([]byte{0})
	mac.Write([]byte(code))
	return hex.EncodeToString(mac.Sum(nil))
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// newID returns a random hex identifier of n bytes.
func newID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is fatal-grade; fall back to a time-based id so
		// we never emit an empty identifier (still unique enough to not collide).
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func newSecret(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// randomCode returns a 6 digit OTP (uniform, crypto/rand).
func randomCode() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return devOTPCode
	}
	n := (uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])) % 1000000
	return fmt.Sprintf("%06d", n)
}

// validDeviceID accepts the client-generated device id (UUID / hex-ish).
func validDeviceID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == ':':
		default:
			return false
		}
	}
	return true
}

// issueDeviceID mints a server-side guest device id (security-spec §2.6:
// guest device ids are issued by the server, never client-chosen).
func issueDeviceID() string { return "g_" + newID(16) }
