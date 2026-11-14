// Package handler renders the admin REST surface. One file per resource.
// Handlers contain NO SQL and NO business rules beyond HTTP parsing/rendering;
// authorization is middleware, side effects are service methods.
package handler

import (
	"net/http"
	"sync"

	"github.com/yingo-server/OpenDiscord/server/internal/admin/auditshim"
	"github.com/yingo-server/OpenDiscord/server/internal/admin/diagnostics"
	"github.com/yingo-server/OpenDiscord/server/internal/admin/middleware"
	"github.com/yingo-server/OpenDiscord/server/internal/admin/service"
	"github.com/yingo-server/OpenDiscord/server/internal/pkg/log"
)

// Config controls cookies and proxy trust for the admin HTTP surface.
type Config struct {
	CookieName     string
	CSRFCookieName string
	CSRFHeaderName string
	SecureCookies  bool
	TrustProxy     bool
}

// DefaultConfig returns loopback/proxy-friendly defaults.
func DefaultConfig() Config {
	return Config{
		CookieName:     "oc_admin_session",
		CSRFCookieName: "oc_admin_csrf",
		CSRFHeaderName: "X-CSRF-Token",
		SecureCookies:  false,
		TrustProxy:     false,
	}
}

// Deps is the assembled handler dependency set. Services may be nil only in
// focused tests; production wiring provides all of them.
type Deps struct {
	Auth          *service.AuthService
	Users         *service.UserService
	Reports       *service.ReportService
	Messages      *service.MessageService
	Flags         *service.FlagService
	Broadcast     *service.BroadcastService
	Audit         *service.AuditService
	Archive       *service.ArchiveService
	Metrics       *service.Metrics
	Authenticator *middleware.Authenticator
	CSRF          *middleware.CSRF
	RBAC          *middleware.RBAC
	LoginLimiter  *middleware.LoginLimiter
	OpLimiter     *middleware.OpLimiter
	AuditShim     *auditshim.Shim
	Logger        *log.Logger

	// TurnURL is the self-hosted TURN URI (turn:/turns:) from the chatd
	// config; empty = self-check answers "not configured" (fail-closed).
	TurnURL string
}

// API is the admin HTTP application.
type API struct {
	cfg  Config
	deps Deps
	idem *idempotencyCache

	once    sync.Once
	handler http.Handler
}

// New builds the API. Missing optional middleware gets harmless defaults so
// route tests can isolate one layer at a time.
func New(cfg Config, deps Deps) *API {
	def := DefaultConfig()
	if cfg.CookieName == "" {
		cfg.CookieName = def.CookieName
	}
	if cfg.CSRFCookieName == "" {
		cfg.CSRFCookieName = def.CSRFCookieName
	}
	if cfg.CSRFHeaderName == "" {
		cfg.CSRFHeaderName = def.CSRFHeaderName
	}
	if deps.Logger == nil {
		deps.Logger = log.For("admin.http")
	}
	if deps.Metrics == nil {
		deps.Metrics = service.NewMetrics()
	}
	if deps.LoginLimiter == nil {
		deps.LoginLimiter = middleware.NewLoginLimiter(middleware.DefaultLockPolicy())
	}
	if deps.OpLimiter == nil {
		deps.OpLimiter = middleware.NewOpLimiter(middleware.DefaultOpLimiterConfig())
	}
	if deps.CSRF == nil {
		deps.CSRF = middleware.NewCSRF(cfg.CSRFCookieName, cfg.CSRFHeaderName)
	}
	return &API{cfg: cfg, deps: deps, idem: newIdempotencyCache()}
}

// Route is one row of the admin route/permission table. Exported so tests and
// docs can enumerate the exact surface.
type Route struct {
	Method     string
	Pattern    string
	Permission middleware.Permission
	Public     bool
	HighRisk   bool
	Handler    http.HandlerFunc
}

// Routes returns the authoritative route table.
func (a *API) Routes() []Route {
	return []Route{
		// auth
		{Method: http.MethodPost, Pattern: "/api/v1/login", Public: true, Handler: a.handleLogin},
		{Method: http.MethodPost, Pattern: "/api/v1/logout", Handler: a.handleLogout},
		{Method: http.MethodGet, Pattern: "/api/v1/whoami", Handler: a.handleWhoami},
		// users
		{Method: http.MethodGet, Pattern: "/api/v1/users", Permission: middleware.PermUserRead, Handler: a.handleUserList},
		{Method: http.MethodGet, Pattern: "/api/v1/users/{id}", Permission: middleware.PermUserRead, Handler: a.handleUserShow},
		{Method: http.MethodPost, Pattern: "/api/v1/users/{id}/ban", Permission: middleware.PermUserBan, HighRisk: true, Handler: a.handleUserBan},
		{Method: http.MethodPost, Pattern: "/api/v1/users/{id}/unban", Permission: middleware.PermUserBan, HighRisk: true, Handler: a.handleUserUnban},
		{Method: http.MethodPost, Pattern: "/api/v1/users/{id}/kick", Permission: middleware.PermUserKick, HighRisk: true, Handler: a.handleUserKick},
		// reports
		{Method: http.MethodGet, Pattern: "/api/v1/reports", Permission: middleware.PermReportRead, Handler: a.handleReportList},
		{Method: http.MethodGet, Pattern: "/api/v1/reports/{id}", Permission: middleware.PermReportRead, Handler: a.handleReportShow},
		{Method: http.MethodPost, Pattern: "/api/v1/reports/{id}/handle", Permission: middleware.PermReportHandle, HighRisk: true, Handler: a.handleReportHandle},
		// messages
		{Method: http.MethodGet, Pattern: "/api/v1/messages/search", Permission: middleware.PermMessageSearch, Handler: a.handleMessageSearch},
		{Method: http.MethodPost, Pattern: "/api/v1/messages/{id}/recall", Permission: middleware.PermMessageRecall, HighRisk: true, Handler: a.handleMessageRecall},
		// flags
		{Method: http.MethodGet, Pattern: "/api/v1/flags", Permission: middleware.PermFlagRead, Handler: a.handleFlagList},
		{Method: http.MethodPut, Pattern: "/api/v1/flags", Permission: middleware.PermFlagWrite, HighRisk: true, Handler: a.handleFlagSet},
		// broadcast
		{Method: http.MethodPost, Pattern: "/api/v1/broadcast", Permission: middleware.PermBroadcast, HighRisk: true, Handler: a.handleBroadcast},
		// audit
		{Method: http.MethodGet, Pattern: "/api/v1/audit", Permission: middleware.PermAuditRead, Handler: a.handleAudit},
		// archive
		{Method: http.MethodGet, Pattern: "/api/v1/archive/status", Permission: middleware.PermArchiveRead, Handler: a.handleArchiveStatus},
		{Method: http.MethodPost, Pattern: "/api/v1/archive/run", Permission: middleware.PermArchiveRun, HighRisk: true, Handler: a.handleArchiveRun},
		// metrics
		{Method: http.MethodGet, Pattern: "/api/v1/metrics", Permission: middleware.PermMetricsRead, Handler: a.handleMetrics},
		// diagnostics: TURN reachability self-check (admin-visible only).
		{Method: http.MethodGet, Pattern: "/api/v1/diagnostics/turn", Permission: middleware.PermMetricsRead, Handler: a.handleTURNCheck},
		// liveness (no auth: fronted by Caddy's admin IP allow-list)
		{Method: http.MethodGet, Pattern: "/health", Public: true, Handler: a.handleHealth},
	}
}

// Handler builds (once) the middleware chain:
//
//	request-id/access-log → recover → mux
//	  per route: authn → rbac → csrf → op-rate-limit → idempotency → handler
func (a *API) Handler() http.Handler {
	a.once.Do(func() {
		mux := http.NewServeMux()
		for _, rt := range a.Routes() {
			h := rt.Handler
			if !rt.Public {
				// Wrappers are applied innermost→outermost so execution is:
				// authn → rbac → csrf → op-limit → idempotency → handler.
				if rt.Method != http.MethodGet && rt.Method != http.MethodHead {
					h = a.idem.Wrap(http.HandlerFunc(h)).ServeHTTP
				}
				h = a.deps.OpLimiter.Middleware(rt.HighRisk)(http.HandlerFunc(h)).ServeHTTP
				h = a.deps.CSRF.Middleware(http.HandlerFunc(h)).ServeHTTP
				if a.deps.RBAC != nil {
					h = a.deps.RBAC.Guard(rt.Permission)(http.HandlerFunc(h)).ServeHTTP
				}
				if a.deps.Authenticator != nil {
					h = a.deps.Authenticator.Require(http.HandlerFunc(h)).ServeHTTP
				}
			}
			mux.Handle(rt.Method+" "+rt.Pattern, http.HandlerFunc(h))
		}
		var root http.Handler = mux
		root = middleware.Recover(a.deps.Logger)(root)
		root = middleware.RequestLog(a.deps.Logger, a.deps.Metrics)(root)
		a.handler = root
	})
	return a.handler
}

func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	middleware.WriteJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

// handleTURNCheck runs the one-shot TURN reachability probe (OPS-2). The TURN
// URI comes from the injected TurnURL seam (chatd config → admin.Deps); an
// unconfigured check answers ok=false with "not configured" — it never
// panics and never exposes credentials. Result stays inside the admin plane.
func (a *API) handleTURNCheck(w http.ResponseWriter, r *http.Request) {
	if a.deps.TurnURL == "" {
		middleware.WriteJSON(w, r, http.StatusOK, map[string]any{
			"ok":      false,
			"detail":  "turn not configured (set turn_url in chatd config)",
			"checked": false,
		})
		return
	}
	res := diagnostics.ProbeTURN(r.Context(), a.deps.TurnURL)
	middleware.WriteJSON(w, r, http.StatusOK, map[string]any{
		"ok":          res.OK,
		"transport":   res.Transport,
		"addr":        res.Addr,
		"detail":      res.Detail,
		"duration_ms": res.Duration,
		"checked_at":  res.CheckedAt,
	})
}
