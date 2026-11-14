// Package admin assembles the management plane: middleware → handler →
// service → bridgeproxy, with the audit shim decorating service mutations.
//
// Isolation red line (docs/security-spec.md §3.3): this package never imports
// the user plane's authn/JWT middleware. Admin sessions are opaque random
// tokens in a separate adm schema; no user JWT signing key is referenced.
package admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yingo-server/OpenDiscord/server/internal/admin/auditshim"
	"github.com/yingo-server/OpenDiscord/server/internal/admin/bridgeproxy"
	"github.com/yingo-server/OpenDiscord/server/internal/admin/handler"
	"github.com/yingo-server/OpenDiscord/server/internal/admin/middleware"
	"github.com/yingo-server/OpenDiscord/server/internal/admin/service"
	adminstore "github.com/yingo-server/OpenDiscord/server/internal/admin/store"
	"github.com/yingo-server/OpenDiscord/server/internal/data/store"
	"github.com/yingo-server/OpenDiscord/server/internal/pkg/log"
	"github.com/yingo-server/OpenDiscord/server/internal/user/core/authn"
)

// Config is the admin server's operational configuration. Every tunable that
// policy might change lives here rather than in a component.
type Config struct {
	ListenAddr       string // default 127.0.0.1:7781 (loopback only)
	AllowNonLoopback bool   // explicit opt-in; default false
	CookieName       string
	CSRFCookieName   string
	CSRFHeaderName   string
	SecureCookies    bool
	TrustProxy       bool
	SessionTTL       time.Duration // default 15m
	PasswordMinLen   int           // default 14
	RequireTOTP      bool

	BridgeURL        string
	BridgeTimeout    time.Duration
	BridgeRetries    int
	BreakerThreshold int
	BreakerOpen      time.Duration

	// TurnURL is the TURN URI used by the admin diagnostics self-check.
	TurnURL string

	Now func() time.Time
}

// DefaultConfig returns safe defaults for a Caddy-fronted loopback listener.
func DefaultConfig() Config {
	return Config{
		ListenAddr:       "127.0.0.1:7781",
		CookieName:       "oc_admin_session",
		CSRFCookieName:   "oc_admin_csrf",
		CSRFHeaderName:   "X-CSRF-Token",
		SecureCookies:    true,
		TrustProxy:       true,
		SessionTTL:       15 * time.Minute,
		PasswordMinLen:   14,
		RequireTOTP:      true,
		BridgeTimeout:    5 * time.Second,
		BridgeRetries:    2,
		BreakerThreshold: 5,
		BreakerOpen:      30 * time.Second,
		Now:              time.Now,
	}
}

// Dispatcher is the bridge dispatch seam the service layer depends on.
// It is an alias so assembly code (cmd/chatd, admin/prod) can name the
// dependency without importing the service package directly.
type Dispatcher = service.Dispatcher

// Deps is the injected dependency set. DB is admin.db; AuditJournal is the
// cold append-only journal; Authorizer authenticates outbound AdminAPI calls;
// Policy is the flag/policy store.
//
// Authorizer is defined HERE (dependency inversion, docs/bridge-design.md
// §4.1 rule: admin must not import bridge/*): bridge/adminapi.HMACAuthorizer
// satisfies it structurally — the bridge package never knows about this
// interface, and this package never imports the bridge.
type Deps struct {
	DB           *sql.DB
	AuditJournal store.AuditJournal
	Authorizer   bridgeproxy.Authorizer
	Bridge       *bridgeproxy.Proxy
	Policy       service.PolicyStore

	// Dispatch, when non-nil, replaces Bridge as the dispatcher every service
	// mutation goes through. Production assembly supplies either a *Proxy
	// (BridgeURL mode) or any other implementation (embedded/loopback mode);
	// at least one of Bridge/Dispatch is required.
	Dispatch service.Dispatcher

	// Read ports (implemented by the data layer in production; fakes in
	// tests). Nil ports degrade to 503 not_configured instead of crashing.
	Users       service.UserRepository
	Reports     service.ReportRepository
	Messages    service.MessageRepository
	Flags       service.FlagStore
	Reach       service.ReachCounter
	AuditReader service.AuditReader
	Archive     service.ArchiveRepository
}

// Server is the assembled admin HTTP service.
type Server struct {
	cfg    Config
	logger *log.Logger
	http   *http.Server
	api    *handler.API
	bridge *bridgeproxy.Proxy
	addr   string
	addrMu sync.RWMutex
}

// New validates dependencies, migrates admin.db, wires services/middleware and
// returns a runnable server.
func New(cfg Config, deps Deps) (*Server, error) {
	cfg = withDefaults(cfg)
	if deps.DB == nil {
		return nil, errors.New("admin: admin.db handle is required")
	}
	if deps.AuditJournal == nil {
		return nil, errors.New("admin: audit journal is required")
	}
	if err := validateListen(cfg.ListenAddr, cfg.AllowNonLoopback); err != nil {
		return nil, err
	}
	logger := log.For("admin")

	adb := adminstore.New(deps.DB)
	migCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := adb.Migrate(migCtx); err != nil {
		return nil, fmt.Errorf("admin: migrate admin.db: %w", err)
	}

	bridge := deps.Bridge
	if bridge == nil && deps.Dispatch == nil {
		if strings.TrimSpace(cfg.BridgeURL) == "" {
			return nil, errors.New("admin: bridge proxy or BridgeURL is required")
		}
		var auth bridgeproxy.Authorizer
		if deps.Authorizer != nil {
			auth = deps.Authorizer
		}
		p, err := bridgeproxy.New(bridgeproxy.Config{
			BaseURL:          cfg.BridgeURL,
			Timeout:          cfg.BridgeTimeout,
			Retries:          cfg.BridgeRetries,
			BreakerThreshold: cfg.BreakerThreshold,
			BreakerOpen:      cfg.BreakerOpen,
			Authorizer:       auth,
		})
		if err != nil {
			return nil, fmt.Errorf("admin: bridge proxy: %w", err)
		}
		bridge = p
	}

	// Every mutation goes through exactly one dispatcher. Deps.Dispatch wins
	// when supplied (embedded / loopback assembly); otherwise the proxy above.
	var dispatcher service.Dispatcher = bridge
	if deps.Dispatch != nil {
		dispatcher = deps.Dispatch
	}

	shim := auditshim.New(deps.AuditJournal)
	authSvc := service.NewAuthService(
		&adminAccountRepo{db: adb},
		&adminSessionRepo{db: adb},
		authn.NewHasher(authn.DefaultArgon2Params()),
		service.AuthConfig{
			SessionTTL:     cfg.SessionTTL,
			PasswordMinLen: cfg.PasswordMinLen,
			RequireTOTP:    cfg.RequireTOTP,
			Now:            cfg.Now,
		},
	)

	flagStore := deps.Flags
	if flagStore == nil && deps.Policy != nil {
		flagStore = service.SysFlagStore{Sys: deps.Policy}
	}

	users := service.NewUserService(deps.Users, dispatcher, shim)
	reports := service.NewReportService(deps.Reports, dispatcher, shim)
	messages := service.NewMessageService(deps.Messages, dispatcher, shim)
	flags := service.NewFlagService(flagStore, dispatcher, shim)
	broadcast := service.NewBroadcastService(deps.Reach, dispatcher, shim)
	auditSvc := service.NewAuditService(deps.AuditReader, shim)
	archive := service.NewArchiveService(deps.Archive, dispatcher, shim)
	metrics := service.NewMetrics()
	if bridge != nil {
		metrics.SetBridgeStats(bridge.Stats)
	}
	metrics.SetAuditFailures(shim.Failures)

	loginLimiter := middleware.NewLoginLimiter(middleware.DefaultLockPolicy())
	opLimiter := middleware.NewOpLimiter(middleware.DefaultOpLimiterConfig())
	metrics.SetLoginStats(loginLimiter.Stats)
	metrics.SetOpLimiterHits(opLimiter.Hits)

	rbac := middleware.NewRBAC(deps.AuditJournal, log.For("admin.rbac"))
	api := handler.New(handler.Config{
		CookieName:     cfg.CookieName,
		CSRFCookieName: cfg.CSRFCookieName,
		CSRFHeaderName: cfg.CSRFHeaderName,
		SecureCookies:  cfg.SecureCookies,
		TrustProxy:     cfg.TrustProxy,
	}, handler.Deps{
		Auth:          authSvc,
		Users:         users,
		Reports:       reports,
		Messages:      messages,
		Flags:         flags,
		Broadcast:     broadcast,
		Audit:         auditSvc,
		Archive:       archive,
		Metrics:       metrics,
		Authenticator: middleware.NewAuthenticator(authSvc, cfg.CookieName),
		CSRF:          middleware.NewCSRF(cfg.CSRFCookieName, cfg.CSRFHeaderName),
		RBAC:          rbac,
		LoginLimiter:  loginLimiter,
		OpLimiter:     opLimiter,
		AuditShim:     shim,
		Logger:        logger,
		TurnURL:       cfg.TurnURL,
	})

	s := &Server{
		cfg: cfg, logger: logger, api: api, bridge: bridge,
		http: &http.Server{
			Addr:              cfg.ListenAddr,
			Handler:           api.Handler(),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
			MaxHeaderBytes:    64 << 10,
		},
	}
	return s, nil
}

// Handler exposes the HTTP handler (tests/embedded assembly).
func (s *Server) Handler() http.Handler { return s.api.Handler() }

// Addr returns the bound address after Run starts listening. Guarded because
// Run resolves :0 in its own goroutine while callers (tests, logging) read it.
func (s *Server) Addr() string {
	s.addrMu.RLock()
	defer s.addrMu.RUnlock()
	return s.addr
}

// Bridge exposes the proxy for metrics/health probes. It is nil in
// Dispatch-only (embedded/loopback) assembly.
func (s *Server) Bridge() *bridgeproxy.Proxy { return s.bridge }

// Run listens and serves until ctx is cancelled or the listener fails.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("admin: listen %s: %w", s.cfg.ListenAddr, err)
	}
	s.addrMu.Lock()
	s.addr = ln.Addr().String()
	s.addrMu.Unlock()
	errCh := make(chan error, 1)
	go func() { errCh <- s.http.Serve(ln) }()
	s.logger.Info("admin listening", "addr", s.addr)

	select {
	case <-ctx.Done():
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.Drain(dctx); err != nil {
			return fmt.Errorf("admin: drain: %w", err)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Drain gracefully stops accepting and waits for in-flight requests.
func (s *Server) Drain(ctx context.Context) error {
	if s.http == nil {
		return nil
	}
	if err := s.http.Shutdown(ctx); err != nil {
		_ = s.http.Close()
		return err
	}
	s.logger.Info("admin drained")
	return nil
}

// Close releases the listener immediately (emergency stop).
func (s *Server) Close() error {
	if s.http == nil {
		return nil
	}
	return s.http.Close()
}

func withDefaults(c Config) Config {
	d := DefaultConfig()
	if c.ListenAddr == "" {
		c.ListenAddr = d.ListenAddr
	}
	if c.CookieName == "" {
		c.CookieName = d.CookieName
	}
	if c.CSRFCookieName == "" {
		c.CSRFCookieName = d.CSRFCookieName
	}
	if c.CSRFHeaderName == "" {
		c.CSRFHeaderName = d.CSRFHeaderName
	}
	if c.SessionTTL <= 0 {
		c.SessionTTL = d.SessionTTL
	}
	if c.PasswordMinLen <= 0 {
		c.PasswordMinLen = d.PasswordMinLen
	}
	if c.BridgeTimeout <= 0 {
		c.BridgeTimeout = d.BridgeTimeout
	}
	if c.BridgeRetries < 0 {
		c.BridgeRetries = 0
	} else if c.BridgeRetries == 0 {
		c.BridgeRetries = d.BridgeRetries
	}
	if c.BreakerThreshold <= 0 {
		c.BreakerThreshold = d.BreakerThreshold
	}
	if c.BreakerOpen <= 0 {
		c.BreakerOpen = d.BreakerOpen
	}
	if c.Now == nil {
		c.Now = d.Now
	}
	return c
}

func validateListen(addr string, allowNonLoopback bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("admin: invalid listen address %q: %w", addr, err)
	}
	if allowNonLoopback {
		return nil
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		return fmt.Errorf("admin: refusing non-loopback listen %q (set AllowNonLoopback explicitly)", addr)
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil && !ip.IsLoopback() {
		return fmt.Errorf("admin: refusing non-loopback listen %q", addr)
	}
	if host != "localhost" && net.ParseIP(strings.Trim(host, "[]")) == nil {
		return fmt.Errorf("admin: listen host %q is not loopback", host)
	}
	return nil
}

// ------------------------------------------------------ store adapters

type adminAccountRepo struct{ db *adminstore.DB }

func (r *adminAccountRepo) GetAdminByUsername(ctx context.Context, username string) (*service.AdminAccount, error) {
	a, err := r.db.GetAdminByUsername(ctx, username)
	if err != nil {
		return nil, err
	}
	return toServiceAdmin(a), nil
}

func (r *adminAccountRepo) GetAdmin(ctx context.Context, id int64) (*service.AdminAccount, error) {
	a, err := r.db.GetAdmin(ctx, id)
	if err != nil {
		return nil, err
	}
	return toServiceAdmin(a), nil
}

func toServiceAdmin(a *adminstore.Admin) *service.AdminAccount {
	return &service.AdminAccount{
		ID: a.ID, Username: a.Username, PasswordHash: a.PWHash,
		TOTPSecret: a.TOTPSecret, Role: a.Role, CreatedAt: a.CreatedAt,
	}
}

type adminSessionRepo struct{ db *adminstore.DB }

func (r *adminSessionRepo) CreateSession(ctx context.Context, s *service.AdminSession) error {
	return r.db.CreateSession(ctx, &adminstore.Session{SID: s.SID, AdminID: s.AdminID, ExpiresAt: s.ExpiresAt, CSRF: s.CSRF})
}

func (r *adminSessionRepo) GetSession(ctx context.Context, sid string) (*service.AdminSession, error) {
	s, err := r.db.GetSession(ctx, sid)
	if err != nil {
		return nil, err
	}
	return &service.AdminSession{SID: s.SID, AdminID: s.AdminID, ExpiresAt: s.ExpiresAt, CSRF: s.CSRF}, nil
}

func (r *adminSessionRepo) DeleteSession(ctx context.Context, sid string) error {
	return r.db.DeleteSession(ctx, sid)
}
