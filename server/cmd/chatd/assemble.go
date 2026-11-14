package main

// assemble.go: the embedded assembly. Everything here is wiring — construct,
// attach, inject. No business rules, no branching on message semantics.
//
// Layer graph (docs/architecture.md, embedded form):
//
//	HTTP/WS :listen ──► usergw(acceptor→router→forwarder) ──► userPeer ┐
//	     └─ /api/auth/* ──► authsvc (same port)                        │
//	                                                                   ▼
//	                       embedded.Bridge (fan-in ⇄ SendTo fan-out) ──┐
//	                                                                   │
//	                     dataPeer ◄── pipe.Run ◄───────────────────────┘
//	                          └──► SQLite (hot DB, WAL)
//	admin :admin_listen ──► AdminAPI(/admin/inject) + /api/admin/* (TODO)
//
// and on SIGTERM: usergw.Drain → pipe.Drain → pipeline.Drain → close.

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/yingo-server/OpenDiscord/server/internal/admin"
	adminstore "github.com/yingo-server/OpenDiscord/server/internal/admin/store"
	"github.com/yingo-server/OpenDiscord/server/internal/authsvc"
	"github.com/yingo-server/OpenDiscord/server/internal/bridge"
	"github.com/yingo-server/OpenDiscord/server/internal/bridge/adminapi"
	"github.com/yingo-server/OpenDiscord/server/internal/bus/delivery"
	"github.com/yingo-server/OpenDiscord/server/internal/data/pipe"
	"github.com/yingo-server/OpenDiscord/server/internal/data/policy"
	"github.com/yingo-server/OpenDiscord/server/internal/data/store"
	"github.com/yingo-server/OpenDiscord/server/internal/data/store/pg"
	"github.com/yingo-server/OpenDiscord/server/internal/data/store/sqlite"
	"github.com/yingo-server/OpenDiscord/server/internal/mediasvc"
	"github.com/yingo-server/OpenDiscord/server/internal/pkg/featureflags"
	"github.com/yingo-server/OpenDiscord/server/internal/pkg/log"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/kinds"
	"github.com/yingo-server/OpenDiscord/server/internal/transport/embedded"
	"github.com/yingo-server/OpenDiscord/server/internal/user/app"
	"github.com/yingo-server/OpenDiscord/server/internal/user/core/authn"
	"github.com/yingo-server/OpenDiscord/server/internal/user/net/frame"
	"github.com/yingo-server/OpenDiscord/server/internal/user/net/ws"
	"github.com/yingo-server/OpenDiscord/server/web"
)

type assembly struct {
	cfg *config
	lg  *log.Logger

	hot     *sqlite.DB
	adminDB *adminstore.DB
	cold    *pg.ColdDB
	policy  *policy.Store
	flags   *featureflags.Flags

	bridge   *embedded.Bridge
	userPeer *layerPeer
	dataPeer *layerPeer

	pipeline  *bridge.Pipeline
	pipe      *pipe.Pipe
	user      *app.Server
	auth      *authsvc.Service
	media     *mediasvc.Service
	verifier  *authn.Verifier
	admin     *adminPlane
	adminREST *admin.Server
	jobs      *jobPlane
	adminKey  []byte
	v3        *WireV3 // P3c：v3 总线句柄（构造但不接线，§9 中间态）
	// v3Hooks 是 P3d-2 接线的双投递保护判定（唯一路径真相源），由
	// AttachV3Deliver 在 newAssembly 末尾构造；runCtx 是 run() 的根 ctx。
	v3Hooks *v3ConnLifecycle
	runCtx  context.Context
	// adaptiveStore 是 CORE-1 控制循环的 effective[cid] 表；仅在
	// adaptive_control_enabled=true 时由 startAdaptiveControl 创建并注入。
	adaptiveStore *pipe.AdaptiveStoreHandle

	started time.Time
}

// newAssembly builds every component and wires them. Nothing starts running
// here (no listener, no goroutine): run() is the only place that does.
func newAssembly(cfg *config, lg *log.Logger) (*assembly, error) {
	a := &assembly{cfg: cfg, lg: lg, started: time.Now()}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1. hot database (+ migrations).
	if err := os.MkdirAll(filepath.Dir(cfg.HotDB), 0o750); err != nil {
		return nil, fmt.Errorf("chatd: create hot db dir: %w", err)
	}
	hot, err := sqlite.Open(cfg.HotDB)
	if err != nil {
		return nil, fmt.Errorf("chatd: open hot db: %w", err)
	}
	if err := hot.Migrate(ctx); err != nil {
		_ = hot.Close()
		return nil, fmt.Errorf("chatd: migrate hot db: %w", err)
	}
	a.hot = hot

	// 2. admin database (+ migrations), a separate file with its own schema.
	if err := os.MkdirAll(filepath.Dir(cfg.AdminDB), 0o750); err != nil {
		return nil, fmt.Errorf("chatd: create admin db dir: %w", err)
	}
	adb, err := adminstore.Open(cfg.AdminDB)
	if err != nil {
		return nil, fmt.Errorf("chatd: open admin db: %w", err)
	}
	if err := adb.Migrate(ctx); err != nil {
		return nil, fmt.Errorf("chatd: migrate admin db: %w", err)
	}
	a.adminDB = adb

	// 3. policy (mechanism/policy separation: the data layer reads snapshots).
	// A *missing* file degrades to policy.Default() with a warning (first run);
	// an existing but invalid file is a hard config error (exit 1) — a
	// half-understood policy must never be silently replaced.
	switch {
	case cfg.PolicyPath == "":
		lg.Warn("no policy_path configured; using built-in policy defaults")
	default:
		if _, statErr := os.Stat(cfg.PolicyPath); errors.Is(statErr, os.ErrNotExist) {
			lg.Warn("policy file not found; using built-in defaults", "path", cfg.PolicyPath)
			break
		}
		pol, perr := policy.New(cfg.PolicyPath)
		if perr != nil {
			return nil, fmt.Errorf("chatd: load policy: %w", perr)
		}
		a.policy = pol
	}

	// 4. media directory (LUKS mount point in production; unused this batch).
	if cfg.MediaDir != "" {
		if err := os.MkdirAll(cfg.MediaDir, 0o750); err != nil {
			return nil, fmt.Errorf("chatd: create media dir: %w", err)
		}
	}

	// 5. keys + crypto primitives (single owner: this assembly).
	// In production missing keys are fatal (exit 1): a running deployment must
	// never silently mint new secrets. dev keeps the first-boot convenience.
	prod := isProduction(cfg)
	priv, err := loadOrCreateSigningKey(cfg.JWTKeyPath, lg, prod)
	if err != nil {
		return nil, err
	}
	hmacKey, err := loadOrCreateHMACKey(cfg.PIIKeyPath, lg, prod)
	if err != nil {
		return nil, err
	}
	// The admin plane's injector and the REST layer's outbound proxy share one
	// HMAC key: without it any local process could inject management frames.
	adminKey, err := loadOrCreateHMACKey(adminKeyPath(cfg), lg, prod)
	if err != nil {
		return nil, err
	}
	a.adminKey = adminKey
	// Production must never run without rate limiting: the switch exists for
	// capacity measurements, and a production profile with it off would expose
	// the gateway to the abuse surface the limiter exists to stop.
	if isProduction(cfg) && !cfg.RateLimitEnabled {
		return nil, fmt.Errorf("chatd: rate_limit_enabled=false is not allowed in production " +
			"(https + APP_ENV!=dev); it exists only for capacity testing")
	}

	signer := authn.NewSigner(priv).WithKID("k1")
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("chatd: signing key is not Ed25519")
	}
	verifier := authn.NewVerifier(pub)
	a.verifier = verifier
	hasher := authn.NewHasher(authn.DefaultArgon2Params())

	// 6. feature flags: seed missing keys, then load the effective set.
	if err := a.bootstrapFlags(ctx); err != nil {
		return nil, err
	}

	// 7. embedded transport + bridge pipeline.
	a.userPeer = newLayerPeer("user")
	a.dataPeer = newLayerPeer("data")
	br := embedded.NewBridge(nil)
	br.Attach(a.userPeer)
	br.Attach(a.dataPeer)
	a.bridge = br

	pl, err := bridge.Assemble(bridge.Config{
		RouteTable:    buildRoutes(),
		RouteFromData: buildRoutesFromData(),
		Sender:        br.SendTo,
		Signals:       neverFires(), // chatd owns signal handling (cross-layer drain order)
		// Loopback is not an authorization boundary: the injector verifies the
		// same HMAC the REST layer signs (docs/security-spec.md §3.2).
		AdminAuth: adminapi.NewHMACAuthorizer([][]byte{adminKey}),
	}, br.Peer())
	if err != nil {
		return nil, fmt.Errorf("chatd: assemble bridge: %w", err)
	}
	a.pipeline = pl

	// 8. data layer: cold store (optional, graceful) + pipe + wire adapters.
	//
	// Cold store wiring: the DRIVER is injected by the deployment layer. This
	// binary deliberately imports NO postgres driver (architecture red line:
	// driver 由部署层注入). If the DSN is configured but no driver named
	// "postgres" is registered, we log a loud ERROR and continue hot-only —
	// skeleton deployments must start, not panic (docs/deploy.md 附录 A).
	var coldDB *pg.ColdDB
	if cfg.ColdDSN != "" {
		db, err := sql.Open("postgres", cfg.ColdDSN) // sql.Open never dials
		if err != nil {
			// Only a bad driver name / unregistered driver lands here.
			lg.Error("cold store disabled: driver not registered — 需在部署层 import pgx/stdlib 或 lib/pq 注册 driver（本二进制按架构约定不携带 postgres driver）",
				"err", err.Error())
		} else {
			coldDB = &pg.ColdDB{DB: db, DriverName: "postgres"}
			lg.Info("cold store handle opened (lazy connect on first use)", "driver", coldDB.DriverName)
		}
	}
	a.cold = coldDB

	// Audit store fail-fast switch. coldDB == nil means the archive + audit
	// journal have nowhere to persist. require_audit_store decides whether that
	// is a startup failure (fail-fast) or a loud degradation (fail-open with an
	// ERROR so operators see it, matching the audit red line).
	if coldDB == nil {
		if cfg.RequireAuditStore {
			return nil, fmt.Errorf("chatd: audit store required (require_audit_store=true) but cold DB unavailable: configure cold_dsn and register a postgres driver in the deployment layer")
		}
		lg.Error("audit store degraded: cold DB unavailable, audit events will not persist")
	}

	p := pipe.New(a.dataPeer, hot, pipe.ColdDeps{Cold: coldDB, Audit: coldDB}, a.policy, log.For("data"))
	// 拉取模式（v1.2 §3.3）：pipe 读 pull_mode_threshold / watermark_* 三个
	// flag。共享同一个 *featureflags.Flags，SIGHUP 热重载原地生效；默认
	// threshold=0 时行为与主干逐字节一致。
	p.SetFeatureFlags(a.flags)
	p.FrameSource = newFrameSource(func(connID uint64) int64 {
		// Late binding: the gateway exists only after step 9.
		if a.user == nil {
			return 0
		}
		if c := a.user.Session().Lookup(connID); c != nil {
			return c.UserID()
		}
		return 0
	}, log.For("chatd"))
	a.pipe = p
	// The emitter is per-instance now, so it must be installed on the pipe
	// that was just built (it used to be a package-level global set earlier).
	installEmit(p)

	// 9. auth service (same HTTP port as the gateway).
	// Provider webhooks (OPS-2): a missing/empty providers file keeps all
	// Deps nil — the service keeps its existing fail-closed behavior. A
	// malformed file aborts startup (loud, never silently ignored).
	sms, mailer, captcha, perr := authsvc.LoadProviders(cfg.ProvidersPath)
	if perr != nil {
		return nil, fmt.Errorf("chatd: providers config: %w", perr)
	}
	if sms == nil && mailer == nil && captcha == nil && cfg.ProvidersPath != "" {
		lg.Info("providers: no channel URLs configured, sms/mail/captcha stay fail-closed")
	}
	authSvc, err := authsvc.New(authsvc.Deps{
		DB:       hot.DB,
		Identity: hot,
		Sys:      hot,
		Signer:   signer,
		Verifier: verifier,
		Hasher:   hasher,
		HMACKey:  hmacKey,
		SMS:      sms,
		Mailer:   mailer,
		Captcha:  captcha,
	}, authsvc.Config{
		PublicOrigin:  cfg.PublicOrigin,
		AccessTTL:     time.Duration(cfg.AccessTTLSec) * time.Second,
		RefreshTTL:    time.Duration(cfg.RefreshTTLSec) * time.Second,
		DevOTP:        cfg.DevOTP,
		SecureCookies: strings.HasPrefix(cfg.PublicOrigin, "https://"),
		Logger:        log.For("authsvc"),
	})
	if err != nil {
		return nil, fmt.Errorf("chatd: auth service: %w", err)
	}
	a.auth = authSvc

	// 10. user layer gateway: /ws + /api/auth/* + /healthz + SPA on one port.
	userCfg := app.DefaultConfig()
	userCfg.ListenAddr = cfg.Listen
	userCfg.PublicOrigin = cfg.PublicOrigin
	userCfg.BridgeAddr = "embedded"
	userCfg.LogLevel = cfg.LogLevel
	userCfg.EnableRateLimit = cfg.RateLimitEnabled
	userCfg.ExtraRoutes = a.buildHTTP(authSvc.Routes())
	userCfg.Flags = a.flags

	srv, err := app.New(userCfg, func(token string) (*ws.Identity, error) {
		id, verr := verifier.Verify(token)
		if verr != nil {
			return nil, verr
		}
		return &ws.Identity{UserID: id.UserID, DeviceID: id.DeviceID}, nil
	}, a.userPeer)
	if err != nil {
		return nil, fmt.Errorf("chatd: user gateway: %w", err)
	}
	a.user = srv

	// P0-2 wiring: the policy.json rate_limit section is the user-level
	// aggregate budget; converge the limiter's MSG_SEND rule onto it
	// (never looser — see limitwire.go). Applied post-construction via the
	// same atomic-swap path that SIGHUP reload() uses.
	// Only converge policy limits when rate limiting is actually enabled: the
	// disabled path scales every rule to 1e9 in app.Config.Policy(), and
	// re-applying the policy numbers here silently undid that — a flag that
	// says "off" but behaves "on" (measured: 2 MSG_SEND kills per connection
	// in a capacity test that asked for no limits).
	if a.policy != nil && a.cfg.RateLimitEnabled {
		snap := a.user.Policy().Snapshot()
		snap.Limits = limitFromPolicy(a.policy.Current().Policy, snap.Limits)
		a.user.ReloadPolicy(snap)
	}

	// 11. management surfaces: frame injector (:admin_listen) and the RBAC REST
	// layer (:admin_rest_listen) that reaches the bus through the injector.
	plane, err := newAdminPlane(cfg.AdminListen, pl.AdminAPI().Handler(), log.For("admin"))
	if err != nil {
		return nil, err
	}
	a.admin = plane

	rest, err := newAdminREST(cfg, hot, adb, coldDB, a.policy, adminKey, log.For("admin-rest"))
	if err != nil {
		return nil, err
	}
	a.adminREST = rest

	// 12. job plane: archiver + jobber (cron from policy; 980 → jobber).
	a.jobs = newJobPlane(hot, coldDB, a.policy, log.For("jobs"), a.cfg.MediaDir)
	p.SetTrunkee(a.jobs)
	if coldDB == nil {
		lg.Info("job plane wired in degraded mode (no cold store): archive dry-run only, real runs fail with a clear error")
	}

	// 13. v3 bus wire (P3c) + P3d-1 fanout 灰度分流（双写旁路，仅构造/挂载）：
	// 同一热库上构造 mailbox hub + delivery plane；把 hub.Offer 适配成
	// pipe.V3Offer 注入 fanout 产出点。旧直投路径仍是唯一投递路径，连接写
	// 循环一行不动（P3d-2）；flag 默认 false / percent=0 时 adapter 与
	// Hub.Offer 双层 no-op，旧路径行为逐字节一致（§9）。nil 结果只记 WARN，
	// 不阻断启动——旁路设施故障不得影响旧路径。
	if w := newV3Wire(hot.DB, a.flags.Snapshot, delivery.Config{}); w != nil {
		a.v3 = w
		w.AttachV3Split(p)
		// P3d-2 接线（docs/lts-1.4-remaining.md §1）：把 v3 投递循环接进
		// 连接生命周期（OnConnect → Subscribe + deliverLoop；OnDisconnect →
		// cleanup），并把双投递保护判定 / kind 15 ACK sink 注入 user 层
		// Router。句柄不齐时 no-op（旁路设施故障不阻断启动，§9）。
		// v3_bus_enabled=false（默认）时：钩子全部零成本 no-op，旧直投路径
		// 逐字节不变（T-SV-5）。
		AttachV3Deliver(a)
	} else {
		lg.Warn("v3 bus wire unavailable; continuing on legacy delivery path (P3d split not attached)")
	}
	return a, nil
}

// bootstrapFlags seeds sys_feature_flags with the compiled/config defaults for
// keys that are not in the DB yet, then loads the effective set. Existing
// values always win (flags are policy data: an admin change must survive a
// restart).
func (a *assembly) bootstrapFlags(ctx context.Context) error {
	seed := featureflags.Defaults()
	for k, v := range a.cfg.Flags {
		seed[k] = v
	}
	merged := map[string]any{}
	for k, v := range seed {
		merged[k] = v
	}
	existing, err := a.hot.AllFlags(ctx)
	if err != nil {
		return fmt.Errorf("chatd: read feature flags: %w", err)
	}
	have := map[string]bool{}
	for _, f := range existing {
		have[f.Key] = true
		var v any
		if err := json.Unmarshal([]byte(f.Value), &v); err == nil {
			merged[f.Key] = v
		}
	}
	for k, v := range seed {
		if have[k] {
			continue
		}
		body, err := json.Marshal(v)
		if err != nil {
			continue
		}
		if err := a.hot.UpsertFlag(ctx, &store.Flag{Key: k, Value: string(body)}); err != nil {
			return fmt.Errorf("chatd: seed feature flag %s: %w", k, err)
		}
	}
	a.flags = featureflags.NewFromMap(merged)
	return nil
}

// buildHTTP is the single user-facing mux: everything the browser talks to
// lives on one port (Caddy proxies one upstream).
func (a *assembly) buildHTTP(auth http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/auth/", auth)
	// media plane: same port as the gateway (bytes never ride the 64KB WS
	// frame — see server/internal/mediasvc doc comment).
	mediaSvc, merr := mediasvc.New(mediasvc.Deps{
		Dir:      a.cfg.MediaDir,
		Verifier: a.verifier,
		Media:    a.hot,
		Sys:      a.hot,
		Quota:    a.hot, // store.MediaQuota (SUM(size) accounting)
		Conv:     a.hot,
		QueryDB:  a.hot.DB,
		Logger:   log.For("media"),
	})
	if merr != nil {
		// media_dir is a config invariant: panic loudly rather than serve
		// an upload endpoint that writes to nowhere. buildHTTP runs during
		// assembly, so this aborts startup deterministically.
		panic("chatd: media service: " + merr.Error())
	}
	a.media = mediaSvc
	mux.Handle("/api/media/", mediaSvc.Routes())
	mux.HandleFunc("GET /healthz", a.healthz)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "unknown api path", "code": "NOT_FOUND"})
	})
	// SPA: / and every non-file path fall back to index.html (the web package
	// answers 503 when the dist has not been built, never panics).
	mux.Handle("/", web.Handler(true))
	return mux
}

func (a *assembly) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	body := map[string]any{
		"ok":        true,
		"mode":      a.cfg.Mode,
		"uptime_ms": time.Since(a.started).Milliseconds(),
	}
	if a.user != nil {
		body["conns"] = a.user.Session().Count()
	}
	if err := a.hot.PingContext(ctx); err != nil {
		body["ok"] = false
		body["db"] = "unavailable"
		writeJSONStatus(w, http.StatusServiceUnavailable, body)
		return
	}
	body["db"] = "ok"
	writeJSONStatus(w, http.StatusOK, body)
}

// run starts the four long-lived components and blocks until one of them fails
// or ctx is canceled.
func (a *assembly) run(ctx context.Context) error {
	a.runCtx = ctx // v3 deliverLoop 的根 ctx（SIGTERM cancel → 全部收敛）
	if a.v3Hooks != nil {
		a.v3Hooks.runCtx = ctx
	}
	// CORE-1 自适应控制器：adaptive_control_enabled=false（默认）时不启动、
	// 不写 effective[cid]——机制层保持静态 flag 默认（50/2000）。
	startAdaptiveControl(ctx, a)
	errCh := make(chan error, 4)
	go func() { errCh <- a.user.Run(ctx) }()
	go func() { a.pipe.Run(ctx); errCh <- nil }()
	go func() { errCh <- a.pipeline.Run(ctx) }()
	go func() { errCh <- a.admin.Run(ctx) }()
	go func() { errCh <- a.adminREST.Run(ctx) }()

	a.lg.Info("chatd ready",
		"mode", a.cfg.Mode,
		"listen", a.user.Addr(),
		"admin_listen", a.admin.Addr(),
		"admin_rest_listen", a.cfg.AdminRESTListen,
		"hot_db", a.cfg.HotDB,
		"cold", a.cold != nil,
		"flags", a.flags.Version())

	// job plane: scheduler starts with the rest of the process; ctx cancel
	// (SIGTERM) stops scheduling, in-flight archives stop at the batch
	// boundary and resume idempotently on the next start.
	a.jobs.start(ctx)

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		if err != nil {
			return err
		}
		return nil
	}
}

// shutdown implements the cross-layer SIGTERM order:
//
//	usergw.Drain (stop accept, ERROR{SHUTDOWN}, flush forwarder, BYE)
//	→ pipe.Drain (final group commit; acks flushed)
//	→ pipeline.Drain (bridge peers/egress)
//
// The caller cancels the root ctx afterwards so every Run() returns.
func (a *assembly) shutdown() {
	timeout := time.Duration(a.cfg.ShutdownTimeout) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	a.lg.Info("draining", "timeout_s", int(timeout/time.Second))

	if a.user != nil {
		if err := a.user.Drain(ctx); err != nil {
			a.lg.Warn("usergw drain", "err", err.Error())
		}
	}
	// Give the dispatcher a moment to finish frames that were already handed
	// to the pipe; the pipe's Drain is the only place allowed to close writeCh.
	select {
	case <-time.After(50 * time.Millisecond):
	case <-ctx.Done():
	}
	if a.pipe != nil {
		a.pipe.Drain(ctx)
	}
	if a.pipeline != nil {
		a.pipeline.Drain(ctx)
	}
	if a.adminREST != nil {
		_ = a.adminREST.Close()
	}
	if a.admin != nil {
		_ = a.admin.Close()
	}
	if a.bridge != nil {
		_ = a.bridge.Close()
	}
	a.lg.Info("drain complete")
}

// close releases the databases (last step, after every layer stopped).
func (a *assembly) close() {
	if a.jobs != nil {
		a.jobs.stop() // scheduler ctx is derived from the run ctx too; idempotent
	}
	if a.cold != nil {
		_ = a.cold.Close()
	}
	if a.hot != nil {
		_ = a.hot.Close()
	}
	if a.adminDB != nil {
		_ = a.adminDB.Close()
	}
}

// reload implements SIGHUP (whitelist: policy + feature flags + rate limits).
// Anything else (listen address, keys, DB paths) requires a restart.
func (a *assembly) reload(ctx context.Context) {
	if a.policy != nil {
		changed, err := a.policy.Reload()
		if err != nil {
			a.lg.Warn("policy reload failed; keeping previous snapshot", "err", err.Error())
		} else {
			a.lg.Info("policy reloaded", "changed", strings.Join(changed, ","), "version", a.policy.Current().Version)
			// The write_path tunables are read by the flusher each batch;
			// re-apply them so a reloaded batch_size / flush_interval takes
			// effect on the next group commit without a restart.
			if a.pipe != nil {
				a.pipe.SetWritePolicy(a.policy.Current().Policy)
			}
		}
	}
	// The freshly reloaded rate_limit section is pushed into the user
	// gateway's limiter (see limitwire.go for the mapping semantics).
	// The limiter stores rules in an atomic pointer: the swap is lock-free
	// and immediately visible to every live connection's verdict path.
	if a.policy != nil && a.user != nil && a.cfg.RateLimitEnabled {
		snap := a.user.Policy().Snapshot()
		snap.Limits = limitFromPolicy(a.policy.Current().Policy, snap.Limits)
		a.user.ReloadPolicy(snap)
		a.lg.Info("rate limits refreshed from policy",
			"msg_per_user_per_min", a.policy.Current().Policy.RateLimit.MsgPerUserPerMin)
	}
	stored, err := a.hot.AllFlags(ctx)
	if err != nil {
		a.lg.Warn("flag reload failed", "err", err.Error())
		return
	}
	next := map[string]any{}
	for _, f := range stored {
		var v any
		if err := json.Unmarshal([]byte(f.Value), &v); err == nil {
			next[f.Key] = v
		}
	}
	version := a.flags.Apply(next)

	// Re-publish to every live connection (embedded: all users are here).
	if a.user == nil {
		return
	}
	flags := make(map[string]string, len(next))
	for k, v := range next {
		if s, ok := v.(string); ok {
			flags[k] = s
			continue
		}
		if b, merr := json.Marshal(v); merr == nil {
			flags[k] = strings.Trim(string(b), `"`)
		}
	}
	payload, merr := (&chatpb.FeatureFlags{Version: uint32(version), Flags: flags}).Marshal()
	if merr != nil {
		a.lg.Warn("flag frame encode failed", "err", merr.Error())
		return
	}
	body, ferr := frame.Encode(&chatpb.Envelope{Kind: kinds.KindFeatureFlags, Payload: payload})
	if ferr != nil {
		return
	}
	sent := a.user.Broadcast(body)
	a.lg.Info("feature flags republished", "version", version, "conns", sent)
}

// callStats returns the in-process call table occupancy for the status dump
// (active calls, users pinned in a call). Nil-safe for early/partial assembly.
func callStats(p *pipe.Pipe) map[string]int {
	if p == nil {
		return map[string]int{"active": 0, "users": 0}
	}
	calls, users := p.CallStats()
	return map[string]int{"active": calls, "users": users}
}

// dump is the SIGUSR1 status snapshot (stdout, one JSON line).
func (a *assembly) dump(w io.Writer) {
	snap := map[string]any{
		"mode":        a.cfg.Mode,
		"uptime_ms":   time.Since(a.started).Milliseconds(),
		"goroutines":  runtime.NumGoroutine(),
		"listen":      a.user.Addr(),
		"admin_addr":  a.admin.Addr(),
		"conns":       a.user.Session().Count(),
		"calls":       callStats(a.pipe),
		"flags_ver":   a.flags.Version(),
		"forwarder":   fmt.Sprintf("%+v", a.user.Forwarder().Stats()),
		"bridge":      a.pipeline.Snapshot(),
		"policy_path": a.cfg.PolicyPath,
	}
	if a.policy != nil {
		snap["policy_version"] = a.policy.Current().Version
	}
	// v3 背压指标（v3-bus-design §5.2 / 任务书 §1 要求 6）：现有 dump 暴露，
	// 不新增 HTTP 端点。PausedConns = credit 耗尽的连接数；Backpressured =
	// 全 conn PAUSED 的 uid 数；Suppressed = 双投递保护跳过的旧直投帧数。
	if a.v3 != nil && a.v3.Plane != nil {
		st := a.v3.Plane.Stats()
		snap["v3_bus"] = map[string]any{
			"paused_conns":   st.PausedConns,
			"backpressured":  st.Backpressured,
			"legacy_skipped": a.v3.v3LegacySkipped(),
		}
	}
	if h := a.v3Hooks; h != nil {
		// 全局开关视图（无 uid 语境）：只看 Enabled()，不判桶。
		snap["v3_bus_enabled"] = h.w != nil && h.w.Enabled != nil && h.w.Enabled()
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"event":  "status_dump",
		"at":     time.Now().UTC().Format(time.RFC3339Nano),
		"status": snap,
	})
}

// neverFires is a signal channel that never receives: the bridge supervisor is
// told "someone else owns signals", so SIGTERM reaches the ordered drain above
// instead of racing a partial bridge-only drain.
func neverFires() <-chan os.Signal { return make(chan os.Signal) }
