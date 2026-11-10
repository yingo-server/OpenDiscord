package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yingo-server/OpenDiscord/server/internal/pkg/featureflags"
	"github.com/yingo-server/OpenDiscord/server/internal/pkg/log"
	"github.com/yingo-server/OpenDiscord/server/internal/proto"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/kinds"
	"github.com/yingo-server/OpenDiscord/server/internal/transport"
	"github.com/yingo-server/OpenDiscord/server/internal/user/core/policy"
	"github.com/yingo-server/OpenDiscord/server/internal/user/core/router"
	"github.com/yingo-server/OpenDiscord/server/internal/user/core/session"
	"github.com/yingo-server/OpenDiscord/server/internal/user/net/bridge"
	"github.com/yingo-server/OpenDiscord/server/internal/user/net/frame"
	"github.com/yingo-server/OpenDiscord/server/internal/user/net/limit"
	"github.com/yingo-server/OpenDiscord/server/internal/user/net/ws"
)

// AuthnFunc 是 WS 握手鉴权函数（由 core/authn.Verifier 适配）。
type AuthnFunc = ws.AuthnFunc

// ConnLifecycle 是可选的连接生命周期钩子（docs/v3-bus-design.md §3.1）。
//
// Connected 在连接注册成功后被调用；返回的 func() 是断连清理函数
// （例如取消该连接的后台投递循环），可为 nil。钩子由装配层在 New 之后
// 经 AddConnLifecycle 后置注入；未注入时 OnConnect/OnDisconnect 不产生
// 任何额外行为（无 goroutine、无日志、无存储）。
type ConnLifecycle interface {
	Connected(c *ws.Conn) func()
}

// callConnCleanups 执行 LoadAndDelete 取回的断连清理函数集合。
// 值是 []func()；对每个非 nil 项做 panic 隔离，保证一个钩子的失败不影响
// 其余钩子与连接收尾路径（与 bridge 侧 jobber 的 recover 语义一致）。
func callConnCleanups(v any) {
	cleans, ok := v.([]func())
	if !ok {
		return
	}
	for _, fn := range cleans {
		if fn == nil {
			continue
		}
		func() {
			defer func() { _ = recover() }()
			fn()
		}()
	}
}

// Server 是组装后的用户层网关。
//
// 组装关系：
//
//	Acceptor(ws) ──Upstream──→ Router ──UpstreamFrame──→ Forwarder ──→ transport.Peer(Bridge)
//	    │                        │                              │
//	    └──OnConnect/OnDisconnect→ Session ←──Lookup/Broadcast──┘（下行）
type Server struct {
	cfg    Config
	pol    *policy.Policy
	lim    *limit.Limiter
	sess   *session.Session
	acc    *ws.Acceptor
	fwd    *bridge.Forwarder
	rtr    *router.Router
	logger *log.Logger
	flags  *featureflags.Flags
	peer   transport.Peer

	// lifecycle 保存 []ConnLifecycle 快照（atomic.Value，后置注入并发安全）。
	// cleanups 按 c.ID() 保存 Connected 返回的断连清理函数。两者零值时
	// 不触发任何额外工作：未注入 hooks 的路径逐字节等同主干。
	lifecycle atomic.Value
	// lifecycleMu 串行化 AddConnLifecycle 的读-拷贝-写（atomic.Value 无 CAS）。
	lifecycleMu sync.Mutex
	cleanups    sync.Map

	cancel context.CancelFunc
	done   chan struct{}
}

// New 组装 usergw。authn 为握手验签函数（通常是 authn.Verifier.Verify 适配），
// peer 为桥接传输（transport.Peer，可为 ReconnectingPeer 或测试内存实现）。
func New(cfg Config, authn AuthnFunc, peer transport.Peer) (*Server, error) {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = DefaultConfig().ListenAddr
	}
	if authn == nil {
		return nil, errors.New("usergw: authn required")
	}
	if peer == nil {
		return nil, errors.New("usergw: bridge peer required")
	}
	logger := log.For("usergw")

	pol := policy.New(cfg.Policy())
	lim := limit.New(pol.Snapshot().Limits)
	sess := session.New(pol.Snapshot().TombstoneTTL)
	fwd := bridge.NewForwarder(peer, logger, pol.Snapshot().ForwardQueueCap)
	flags := cfg.Flags
	if flags == nil {
		flags = featureflags.New()
	}
	rtr := router.New(sess, lim, pol, fwd, flags, logger)
	upg := pol.Snapshot()
	acc := ws.NewAcceptor(ws.Config{
		ListenAddr:        cfg.ListenAddr,
		PublicOrigin:      cfg.PublicOrigin,
		CookieName:        cfg.CookieName,
		HeartbeatInterval: cfg.HeartbeatInterval,
		ReadTimeout:       cfg.ReadTimeout,
		MaxFrameBytes:     cfg.MaxFrameBytes,
		WriteChanCap:      cfg.WriteChanCap,
		ExtraRoutes:       cfg.ExtraRoutes,

		UpgradeRatePerMin:       upg.UpgradeRatePerMin,
		UpgradeBurst:            upg.UpgradeBurst,
		UpgradeFailureThreshold: upg.UpgradeFailureThreshold,
		UpgradeFailureWindow:    upg.UpgradeFailureWindow,
		UpgradeBanFor:           upg.UpgradeBanFor,
	}, authn, logger)

	// 连接生命周期 → 连接表 + 限流器连接级状态。
	srv := &Server{
		cfg:    cfg,
		pol:    pol,
		lim:    lim,
		sess:   sess,
		acc:    acc,
		fwd:    fwd,
		rtr:    rtr,
		logger: logger,
		flags:  flags,
		peer:   peer,
		done:   make(chan struct{}),
	}
	acc.OnConnect = func(c *ws.Conn) {
		if err := sess.Register(c); err != nil {
			if logger != nil {
				logger.Warn("session register failed", "conn", c.ID(), "err", err.Error())
			}
			_ = c.Close()
			return
		}
		lim.AddConn(c.ID(), c.UserID())
		// 401 PRESENCE_SUBSCRIBE：上线事件发给数据层，数据层向该用户的会话
		// 成员广播（瞬态，不落 per-user 投递）。帧不带 env user_id —— 数据层
		// 从 payload 读 user_id 并当作内部事件处理。
		emitPresenceEvent(peer, uint32(kinds.KindPresenceOnline), c.UserID())
		// 可选钩子（§3.1）：未注入时快照为空，整段跳过——无 goroutine、
		// 无日志、无额外状态，旧路径逐字节不变。
		if hooks := srv.lifecycleHooks(); len(hooks) > 0 {
			var cleans []func()
			for _, h := range hooks {
				if clean := h.Connected(c); clean != nil {
					cleans = append(cleans, clean)
				}
			}
			if len(cleans) > 0 {
				srv.cleanups.Store(c.ID(), cleans)
				// 连接可能在 OnConnect 返回前就被读/写协程关掉（秒断）：
				// 此时 OnDisconnect 的 LoadAndDelete 已错过，这里自行收尾。
				// 两边都用 LoadAndDelete，保证 cleanup 恰好调用一次。
				select {
				case <-c.Done():
					if v, ok := srv.cleanups.LoadAndDelete(c.ID()); ok {
						callConnCleanups(v)
					}
				default:
				}
			}
		}
	}
	acc.OnDisconnect = func(c *ws.Conn) {
		sess.Unregister(c.ID())
		lim.RemoveConn(c.ID())
		// 402 PRESENCE_UNSUBSCRIBE：下线事件，同上。
		emitPresenceEvent(peer, uint32(kinds.KindPresenceOffline), c.UserID())
		// 钩子只增不减：注入过至少一个钩子才可能有 cleanup，未注入时连
		// LoadAndDelete 都不做（旧路径零额外开销）。
		if len(srv.lifecycleHooks()) > 0 {
			if v, ok := srv.cleanups.LoadAndDelete(c.ID()); ok {
				callConnCleanups(v)
			}
		}
	}
	return srv, nil
}

// AddConnLifecycle 后置注入连接生命周期钩子（并发安全，nil 忽略）。
// 每次注入以「读快照 → 拷贝 → 追加 → Store」替换，运行期 OnConnect 直接
// Load 不可变快照，无锁；已建立的连接不补调新钩子，只有后续连接生效。
func (s *Server) AddConnLifecycle(h ConnLifecycle) {
	if h == nil {
		return
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	prev, _ := s.lifecycle.Load().([]ConnLifecycle)
	next := make([]ConnLifecycle, len(prev), len(prev)+1)
	copy(next, prev)
	next = append(next, h)
	s.lifecycle.Store(next)
}

// lifecycleHooks 返回当前钩子快照；未注入时返回 nil（零分配）。
func (s *Server) lifecycleHooks() []ConnLifecycle {
	hooks, _ := s.lifecycle.Load().([]ConnLifecycle)
	return hooks
}

// Run 启动全部组件并阻塞到 ctx 取消；ctx 取消后执行 Drain。
func (s *Server) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	defer cancel()

	s.fwd.Run(runCtx)
	go s.rtr.RunUpstream(runCtx, s.acc.Upstream)
	go s.rtr.RunDownstream(runCtx)

	errCh := make(chan error, 1)
	go func() { errCh <- s.acc.Run(runCtx) }()

	select {
	case <-ctx.Done():
		dctx, dcancel := context.WithTimeout(context.Background(), s.pol.Snapshot().DrainTimeout+5*time.Second)
		defer dcancel()
		drainErr := s.Drain(dctx)
		close(s.done)
		if drainErr != nil {
			return fmt.Errorf("usergw: drain: %w", drainErr)
		}
		return nil
	case err := <-errCh:
		close(s.done)
		return err
	}
}

// Done 返回 Run 退出信号。
func (s *Server) Done() <-chan struct{} { return s.done }

// Addr 返回实际监听地址（监听后有效）。
func (s *Server) Addr() string { return s.acc.Addr() }

// Session 暴露连接表（usergw-ctl status 用）。
func (s *Server) Session() *session.Session { return s.sess }

// Limiter 暴露限流器（指标采集用）。
func (s *Server) Limiter() *limit.Limiter { return s.lim }

// Forwarder 暴露 forwarder（队列水位观测）。
func (s *Server) Forwarder() *bridge.Forwarder { return s.fwd }

// Policy 暴露策略快照。
func (s *Server) Policy() *policy.Policy { return s.pol }

// ReloadPolicy 原子替换策略（SIGHUP 热更入口）。
func (s *Server) ReloadPolicy(cfg policy.Config) {
	s.pol.Replace(cfg)
	s.lim.SetConfig(cfg.Limits)
}

// Drain 优雅退出七步（docs/user-frontend-design.md §3.1）：
//  1. 停止 accept（listener.Close）
//  2. 向全部在线连接推 ERROR{SHUTDOWN, retry_at=now+5s}
//  3. 等 forwarder 队列排空（≤10s）
//  4. 发内部 BYE 帧后关闭桥接 socket
//  5. 每连接写缓冲排空（≤2s）后 close
//  6. 返回（systemd TimeoutStopSec 兜底 SIGKILL）
func (s *Server) Drain(ctx context.Context) error {
	pol := s.pol.Snapshot()
	if s.cancel != nil {
		s.cancel() // 步骤 0：取消 root ctx（幂等）
	}

	// 步骤 1：停止 accept。
	shCtx, shCancel := context.WithTimeout(ctx, 3*time.Second)
	if err := s.acc.Shutdown(shCtx); err != nil && !errors.Is(err, context.Canceled) {
		// listener 已关但活跃连接仍在，继续排空。
		_ = err
	}
	shCancel()

	// 步骤 2：广播 ERROR{SHUTDOWN, retry_at=now+5s}。
	retryAt := time.Now().Add(5 * time.Second).UnixMilli()
	payload, _ := (&chatpb.Error{Code: 503, Reason: "SHUTDOWN", RetryAfterMs: 5000}).Marshal()
	if b, err := frame.Encode(&chatpb.Envelope{Kind: kinds.KindError, SentAt: uint64(retryAt), Payload: payload}); err == nil {
		s.sess.BroadcastAll(b)
	}

	// 步骤 3：等 forwarder 排空（≤ DrainTimeout=10s）。
	drainCtx, drainCancel := context.WithTimeout(ctx, pol.DrainTimeout)
	flushed := s.fwd.Drain(drainCtx, pol.DrainTimeout)
	drainCancel()
	if !flushed && s.logger != nil {
		s.logger.Warn("forwarder drain timeout", "retry_queue", s.fwd.QueueDepth())
	}

	// 步骤 4：发内部 BYE（SHUTDOWN_NOTICE），桥接据此标记用户「转移中」；
	// 然后关闭桥接 socket。
	bye, _ := (&chatpb.ShutdownNotice{Deadline: uint64(retryAt), Reason: "usergw drain"}).Marshal()
	_ = s.peer.Send(&proto.Frame{
		Kind: kinds.KindShutdownNotice,
		Env:  &chatpb.Envelope{Kind: kinds.KindShutdownNotice, Payload: bye},
		Meta: &proto.FrameMeta{SrcLayer: "user"},
	})
	_ = s.peer.Close()

	// 步骤 5：每连接写缓冲排空（≤ WriteFlushTimeout=2s）后 close。
	flushCtx, flushCancel := context.WithTimeout(ctx, pol.WriteFlushTimeout)
	defer flushCancel()
	for _, c := range s.acc.Conns() {
		select {
		case <-c.Done():
			continue
		case <-flushCtx.Done():
			_ = c.Close()
			continue
		default:
		}
		// 等待写协程把已入队帧写完（Best effort：缓冲为空视为排空）。
		deadline := time.Now().Add(pol.WriteFlushTimeout)
		for time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		_ = c.CloseWith(1001, "server shutdown")
	}

	if s.logger != nil {
		s.logger.Info("drain complete", "conns", s.sess.Count())
	}
	return nil
}

// Broadcast 向全部在线连接投递一帧（SIGHUP flag 重下发等进程内广播）。
// 返回成功入队的连接数。
func (s *Server) Broadcast(b []byte) int { return s.sess.BroadcastAll(b) }

// emitPresenceEvent publishes the user layer's internal connect/disconnect
// notification (401 online / 402 offline) to the bridge. The frame carries NO
// env user_id: the data layer reads user_id from the JSON payload and treats
// the frame as an internal event, fanning it out to the user's conversation
// peers (buildRoutes routes these kinds to "data.presence").
func emitPresenceEvent(peer transport.Peer, kind uint32, uid int64) {
	if peer == nil || uid <= 0 {
		return
	}
	online := kind == uint32(kinds.KindPresenceOnline)
	body, err := json.Marshal(map[string]any{
		"user_id": uid, "online": online, "at": time.Now().UnixMilli(),
	})
	if err != nil {
		return
	}
	_ = peer.Send(&proto.Frame{
		Kind: kind,
		// UserId marks the SUBJECT of this internal event; it is written by the
		// server, never by a client. The data layer requires it to match the
		// authenticated actor so a forged client frame cannot impersonate.
		Env:  &chatpb.Envelope{Kind: kind, Payload: body, Target: &chatpb.Envelope_UserId{UserId: uint64(uid)}},
		Meta: &proto.FrameMeta{SrcLayer: "user", TraceID: "presence"},
	})
}
