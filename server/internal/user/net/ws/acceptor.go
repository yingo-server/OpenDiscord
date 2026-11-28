// Package ws 是 usergw 的 WS 接入层（docs/user-frontend-design.md §1.1 acceptor）：
// HTTP 服务器 + GET /ws Upgrade + 每连接读写协程 + 心跳。
//
// 鉴权在 upgrade 之前完成（docs/security-spec.md §2.3 决策）：
// HTTP 握手阶段从 Cookie 取 access_token → 注入的 Authn 验签 → 失败直接 401，
// 不建立 WS 连接、不占用 conn 槽位。
//
// 明确不做：不读业务帧内容（只把 bytes 交给 codec）、不限流、不查 session 表。
package ws

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/yingo-server/UFOT/server/internal/pkg/log"
	"github.com/yingo-server/UFOT/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
	"github.com/yingo-server/UFOT/server/internal/user/net/frame"
)

// Identity 是握手鉴权通过后的最小身份（core/authn 的 Identity 由 app 层适配）。
type Identity struct {
	UserID   int64
	DeviceID string
}

// Authn 是注入的验签函数（core/authn.Verifier.Verify 由 app 层适配）。
type Authn func(token string) (*Identity, error)

// AuthnFunc 是 Authn 的别名（对外暴露的装配签名使用）。
type AuthnFunc = Authn

// Config acceptor 参数。
type Config struct {
	ListenAddr   string
	// 可选 TLS（后端直接对公网提供 https 的小型部署；ACME 部署请用反代终结）。
	TLSCertFile string
	TLSKeyFile  string
	PublicOrigin string // 同源校验白名单，如 https://chat.example.com
	// AllowAnyOrigin 放行任意合法 Origin（独立前端部署的默认姿态）。
	AllowAnyOrigin bool
	// CORSOrigins 与 HTTP 面的 cors_origins 列表共用（allow_list 模式下
	// HTTP 能过的 Origin，WS 握手也必须能过——两套判据不一致会让
	// "XHR 正常但 WS 连不上"这类问题无法排查）。
	CORSOrigins       []string
	CookieName        string // 默认 access_token
	HeartbeatInterval time.Duration
	ReadTimeout       time.Duration
	MaxFrameBytes     int
	WriteChanCap      int
	// Upgrade 限流（security-spec E-3/E-4）。零值取 upgradelimit.go 的默认：
	// UpgradeRatePerMin=10, UpgradeBurst=20, UpgradeFailureThreshold=8,
	// UpgradeFailureWindow=5min, UpgradeBanFor=15min。可由 policy 覆盖。
	UpgradeRatePerMin       float64
	UpgradeBurst            float64
	UpgradeFailureThreshold int
	UpgradeFailureWindow    time.Duration
	UpgradeBanFor           time.Duration
	// ExtraRoutes is the non-/ws handler (auth API, health, SPA). The app layer
	// passes it from app.Config so the whole user-facing surface stays on ONE
	// HTTP listener/port (Caddy then proxies a single upstream).
	ExtraRoutes http.Handler
}

// FrameEvent 上行帧事件：acceptor 解码后交给 router（唯一的生产者）。
type FrameEvent struct {
	Conn   *Conn
	Env    *chatpb.Envelope
	RecvAt time.Time
}

// Conn 一条已认证的 WS 连接。
type Conn struct {
	id         uint64
	identity   Identity
	ws         *websocket.Conn
	send       chan []byte
	done       chan struct{}
	closeOnce  sync.Once
	closeErr   error
	createdAt  time.Time
	remoteAddr string

	// jsonMode 记录本连接的线格式：浏览器客户端默认发 JSON 文本帧
	// （浏览器的 JSON 编码），原生客户端发 protobuf 二进制帧。回帧
	// 必须与对端一致，否则客户端解析失败——格式是传输属性，不是信任边界。
	jsonMode atomic.Bool

	// flushed 在 writeLoop 把队列里剩余帧写完并关闭 socket 后关闭，
	// 供 CloseFlush 等待。见 CloseFlush 注释。
	flushed chan struct{}

	// 统计（atomic）：写缓冲满丢弃数。
	dropped atomic.Uint64
}

// ID 连接 id（全局单调）。
func (c *Conn) ID() uint64 { return c.id }

// UserID 鉴权用户 id。
func (c *Conn) UserID() int64 { return c.identity.UserID }

// DeviceID 设备 id。
func (c *Conn) DeviceID() string { return c.identity.DeviceID }

// RemoteAddr 对端地址。
func (c *Conn) RemoteAddr() string { return c.remoteAddr }

// CreatedAt 连接建立时间。
func (c *Conn) CreatedAt() time.Time { return c.createdAt }

// Dropped 写缓冲满导致的丢弃计数。
func (c *Conn) Dropped() uint64 { return c.dropped.Load() }

// Write 非阻塞写一帧到发送缓冲；缓冲满或已关闭返回 false（不阻塞读循环）。
func (c *Conn) Write(b []byte) bool {
	select {
	case <-c.done:
		return false
	default:
	}
	select {
	case c.send <- b:
		return true
	default:
		c.dropped.Add(1)
		return false
	}
}

// WriteWait 带超时的阻塞写（优雅退出排空用）。
func (c *Conn) WriteWait(b []byte, timeout time.Duration) bool {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case c.send <- b:
		return true
	case <-c.done:
		return false
	case <-t.C:
		return false
	}
}

// Done 返回关闭信号。
func (c *Conn) Done() <-chan struct{} { return c.done }

// Send 返回写缓冲（仅供排空观测，勿写入）。
func (c *Conn) Send() chan []byte { return c.send }

// Close 关闭连接（幂等）。
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
		c.closeErr = c.ws.Close()
	})
	return c.closeErr
}

// CloseFlush sends a final frame and then closes, waiting (bounded) until the
// write loop has actually written it to the socket.
//
// Why: c.Write only ENQUEUES. Closing right after enqueue raced the write loop
// and the frame was dropped — clients never saw the protocol-error reason for
// a malformed frame. This is the only path that needs the guarantee, so it
// lives here rather than making every write synchronous.
func (c *Conn) CloseFlush(last []byte, code int, reason string, budget time.Duration) {
	select {
	case <-c.done:
		return
	default:
	}
	if last != nil {
		select {
		case c.send <- last:
		case <-c.done:
			return
		}
	}
	// Give the write loop a bounded window to drain, then force-close.
	if c.flushed != nil {
		t := time.NewTimer(budget)
		select {
		case <-c.flushed:
			t.Stop()
		case <-t.C:
		}
	}
	_ = c.CloseWith(code, reason)
}

// CloseWith 以指定 WS close code 关闭。
func (c *Conn) CloseWith(code int, reason string) error {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
		c.closeErr = c.ws.Close()
	})
	return c.closeErr
}

// Acceptor WS 接入器。零值不可用，用 NewAcceptor。
type Acceptor struct {
	cfg    Config
	authn  Authn
	logger *log.Logger
	up     websocket.Upgrader
	srv    *http.Server
	ln     net.Listener

	// Upstream 上行帧流（router 消费）。
	Upstream chan *FrameEvent
	// OnConnect 连接注册回调（app 层接到 session.Register）。
	OnConnect func(*Conn)
	// OnDisconnect 连接注销回调（session.Unregister）。
	OnDisconnect func(*Conn)

	// extra 非 /ws 路径的处理器（装配层注入，nil = 404）。
	extra http.Handler

	// limiter 是 /ws upgrade 的按 IP 限流 + 失败熔断（E-3/E-4）。
	limiter *UpgradeLimiter

	nextID atomic.Uint64
	mu     sync.Mutex
	conns  map[uint64]*Conn
	closed atomic.Bool
}

// NewAcceptor 构造接入器。
func NewAcceptor(cfg Config, authn Authn, logger *log.Logger) *Acceptor {
	if cfg.CookieName == "" {
		cfg.CookieName = "access_token"
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 25 * time.Second
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = 60 * time.Second
	}
	if cfg.MaxFrameBytes <= 0 {
		cfg.MaxFrameBytes = frame.MaxFrameBytes
	}
	if cfg.WriteChanCap <= 0 {
		cfg.WriteChanCap = 64
	}
	a := &Acceptor{
		cfg:      cfg,
		authn:    authn,
		logger:   logger,
		limiter:  NewUpgradeLimiter(cfg.UpgradeRatePerMin, cfg.UpgradeBurst, cfg.UpgradeFailureThreshold, cfg.UpgradeFailureWindow, cfg.UpgradeBanFor),
		Upstream: make(chan *FrameEvent, 1024),
		conns:    make(map[uint64]*Conn),
	}
	a.extra = cfg.ExtraRoutes
	a.up = websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin:     a.checkOrigin,
	}
	return a
}

// ExtraRoutes 注入 /ws 之外的 HTTP 路由（认证 API、健康检查、SPA）。
// 必须在 Run/Serve 之前调用；运行期再替换会有竞态，故不再暴露热替换。
func (a *Acceptor) ExtraRoutes(h http.Handler) {
	a.mu.Lock()
	a.extra = h
	a.mu.Unlock()
}

func (a *Acceptor) extraHandler() http.Handler {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.extra
}

// ServeHTTP 实现 http.Handler：GET /ws 升级为 WebSocket，其余路径交给
// 装配层注入的 ExtraRoutes（同一个 HTTP server/端口）。
func (a *Acceptor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/ws" {
		if h := a.extraHandler(); h != nil {
			h.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// ---- upgrade 前限流（security-spec E-3/E-4）：被限直接 429，不建连 ----
	// 限流发生在鉴权之前；token 维度的 key 用原始 token 串的 sha256（见
	// upgradelimit.tokenKey），无需解析 JWT。空 token 共享一个桶。
	ip := clientIP(r)
	rawToken := a.tokenFromRequest(r)
	if ok, retry := a.limiter.Allow(ip, rawToken); !ok {
		w.Header().Set("Retry-After", retryAfterSeconds(retry))
		http.Error(w, "too many upgrade attempts", http.StatusTooManyRequests)
		return
	}
	// ---- upgrade 前鉴权（security-spec §2.3）----
	if rawToken == "" {
		a.limiter.RecordFailure(ip, rawToken) // 失败也消耗配额
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	identity, err := a.authn(rawToken)
	if err != nil || identity == nil || identity.UserID <= 0 {
		a.limiter.RecordFailure(ip, rawToken) // 401 额外扣一次 token
		if a.logger != nil {
			a.logger.Warn("ws handshake rejected", "remote", r.RemoteAddr, "err", errString(err))
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	c, err := a.up.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade 已写错误响应。
		return
	}
	conn := &Conn{
		id:         a.nextID.Add(1),
		identity:   *identity,
		ws:         c,
		send:       make(chan []byte, a.cfg.WriteChanCap),
		done:       make(chan struct{}),
		flushed:    make(chan struct{}),
		createdAt:  time.Now(),
		remoteAddr: r.RemoteAddr,
	}
	a.register(conn)
	if a.OnConnect != nil {
		a.OnConnect(conn)
	}
	go a.readLoop(conn)
	go a.writeLoop(conn)
}

// tokenFromRequest 只从 Cookie 取 token（禁止 URL query，security-spec §2.3 红线）。
func (a *Acceptor) tokenFromRequest(r *http.Request) string {
	if ck, err := r.Cookie(a.cfg.CookieName); err == nil && ck.Value != "" {
		return ck.Value
	}
	// Sec-WebSocket-Protocol 承载（非浏览器客户端），格式 "bearer, <token>"。
	if p := r.Header.Get("Sec-WebSocket-Protocol"); p != "" {
		fields := strings.Split(p, ",")
		for i, f := range fields {
			f = strings.TrimSpace(f)
			if strings.EqualFold(f, "bearer") && i+1 < len(fields) {
				return strings.TrimSpace(fields[i+1])
			}
		}
	}
	return ""
}

// checkOrigin 同源校验：无 Origin（非浏览器客户端）放行；有则必须与 Host 或
// 配置的 PublicOrigin 精确匹配（不前缀/不通配）。
// checkOrigin validates the handshake Origin.
//
// Backend-only deployment: the user interface is a separate program on its
// own origin, so the strict same-origin check would reject legitimate
// clients. AllowAnyOrigin therefore accepts any syntactically valid origin;
// when it is off the previous behavior applies (empty Origin, configured
// PublicOrigin, or same host).
func (a *Acceptor) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := parseOrigin(origin)
	if err != nil {
		return false
	}
	if a.cfg.AllowAnyOrigin {
		return true
	}
	if a.cfg.PublicOrigin != "" && strings.EqualFold(origin, a.cfg.PublicOrigin) {
		return true
	}
	for _, o := range a.cfg.CORSOrigins {
		if o != "" && strings.EqualFold(origin, o) {
			return true
		}
	}
	// 同源：scheme + host 与请求 Host 一致。
	return strings.EqualFold(u.host, r.Host)
}

type originParts struct{ scheme, host string }

func parseOrigin(s string) (originParts, error) {
	scheme, rest, ok := strings.Cut(s, "://")
	if !ok || (scheme != "http" && scheme != "https" && scheme != "ws" && scheme != "wss") {
		return originParts{}, errors.New("bad origin")
	}
	host := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		host = rest[:i]
	}
	if host == "" {
		return originParts{}, errors.New("bad origin")
	}
	return originParts{scheme: scheme, host: host}, nil
}

func (a *Acceptor) register(c *Conn) {
	a.mu.Lock()
	a.conns[c.id] = c
	a.mu.Unlock()
}

func (a *Acceptor) unregister(c *Conn) {
	a.mu.Lock()
	delete(a.conns, c.id)
	a.mu.Unlock()
}

// Conns 返回当前连接快照。
func (a *Acceptor) Conns() []*Conn {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*Conn, 0, len(a.conns))
	for _, c := range a.conns {
		out = append(out, c)
	}
	return out
}

// Count 连接数。
func (a *Acceptor) Count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.conns)
}

// readLoop 每连接读协程：ReadMessage → codec → Upstream。
func (a *Acceptor) readLoop(c *Conn) {
	defer a.closeConn(c)
	c.ws.SetReadLimit(int64(a.cfg.MaxFrameBytes))
	_ = c.ws.SetReadDeadline(time.Now().Add(a.cfg.ReadTimeout))
	c.ws.SetPongHandler(func(string) error {
		return c.ws.SetReadDeadline(time.Now().Add(a.cfg.ReadTimeout))
	})
	for {
		mt, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		switch mt {
		case websocket.BinaryMessage:
			// protobuf wire form (native clients)
		case websocket.TextMessage:
			// JSON wire form (browser client). The codec sniffs the format, so
			// both go through the same validation path.
			c.jsonMode.Store(true)
		default:
			continue // ping/pong/close 由库处理，其余类型无意义
		}
		env, err := frame.Decode(data)
		if err != nil {
			// 坏帧：回 ERROR{code=PROTOCOL} 后按 1003 关闭（资源立即回收）。
			c.CloseFlush(buildErrorFrame(4000, "bad frame: "+err.Error()),
				websocket.CloseUnsupportedData, "bad frame", 300*time.Millisecond)
			return
		}
		ev := &FrameEvent{Conn: c, Env: env, RecvAt: time.Now()}
		select {
		case a.Upstream <- ev:
		case <-c.done:
			return
		}
	}
}

// writeLoop 每连接写协程：chan []byte → WriteMessage；定时 Ping 心跳。
func (a *Acceptor) writeLoop(c *Conn) {
	ticker := time.NewTicker(a.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case b := <-c.send:
			_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			mt, out := websocket.BinaryMessage, b
			if c.jsonMode.Load() {
				// Upper layers always hand us protobuf; a JSON peer gets the
				// JSON form of the same envelope. A frame that cannot be
				// re-encoded is a bug — drop it rather than send garbage.
				if env, err := frame.Decode(b); err == nil {
					if jb, jerr := frame.EncodeJSON(env); jerr == nil {
						mt, out = websocket.TextMessage, jb
					}
				}
			}
			if err := c.ws.WriteMessage(mt, out); err != nil {
				a.closeConn(c)
				return
			}
		case <-ticker.C:
			if err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				a.closeConn(c)
				return
			}
		case <-c.done:
			return
		}
	}
}

// closeConn 连接收尾：关闭 socket、注销、回调。
func (a *Acceptor) closeConn(c *Conn) {
	_ = c.Close()
	a.unregister(c)
	if a.OnDisconnect != nil {
		a.OnDisconnect(c)
	}
}

// Run 监听并在 ctx 结束时优雅关闭。
func (a *Acceptor) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", a.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("acceptor: listen %s: %w", a.cfg.ListenAddr, err)
	}
	// 可选 TLS：配置 CertFile+KeyFile 时监听升级为 TLS（ACME 用户请继续用
	// 反向代理终结 TLS——本路径面向“后端直接对公网提供 https”的小型部署）。
	if a.cfg.TLSCertFile != "" && a.cfg.TLSKeyFile != "" {
		cert, cerr := tls.LoadX509KeyPair(a.cfg.TLSCertFile, a.cfg.TLSKeyFile)
		if cerr != nil {
			return fmt.Errorf("acceptor: load tls keypair: %w", cerr)
		}
		ln = tls.NewListener(ln, &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
		})
	}
	if err != nil {
		return fmt.Errorf("ws: listen %s: %w", a.cfg.ListenAddr, err)
	}
	// Serve 在锁内登记 listener；在 Run 里再写一次会与并发的 Serve/Stop 竞争
	// （CI -race 实测）。
	return a.Serve(ctx, ln)
}

// Serve 在已有 listener 上服务（httptest 用）。
func (a *Acceptor) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: a, ReadHeaderTimeout: 10 * time.Second}
	// 登记 listener 与 server 必须同一把锁：Shutdown 可能在另一个 goroutine
	// 里并发读取它们（SIGTERM 排空路径；CI -race 实测）。
	a.mu.Lock()
	a.ln = ln
	a.srv = srv
	a.mu.Unlock()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Shutdown 停止 accept（listener.Close，如同 nginx QUIT）并等待活跃连接处理完。
func (a *Acceptor) Shutdown(ctx context.Context) error {
	a.closed.Store(true)
	a.mu.Lock()
	srv := a.srv
	a.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

// Addr 返回实际监听地址（测试用 :0 后取真实端口）。
func (a *Acceptor) Addr() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ln == nil {
		return a.cfg.ListenAddr
	}
	return a.ln.Addr().String()
}

// CloseAll 关闭全部连接（优雅退出最后一步）。
func (a *Acceptor) CloseAll(code int, reason string) {
	for _, c := range a.Conns() {
		_ = c.CloseWith(code, reason)
	}
}

func buildErrorFrame(code uint32, reason string) []byte {
	payload, _ := (&chatpb.Error{Code: code, Reason: reason}).Marshal()
	env := &chatpb.Envelope{Kind: kinds.KindError, Payload: payload}
	b, _ := frame.Encode(env)
	return b
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
