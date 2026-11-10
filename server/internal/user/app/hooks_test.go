package app

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/yingo-server/OpenDiscord/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/kinds"
	"github.com/yingo-server/OpenDiscord/server/internal/user/core/authn"
	"github.com/yingo-server/OpenDiscord/server/internal/user/net/ws"
)

// countingLifecycle 是 ConnLifecycle 的假实现：只数 Connected / cleanup
// 调用次数（atomic，配合 -race 检查并发注入与连接回调的读写安全）。
type countingLifecycle struct {
	connected atomic.Int64
	cleaned   atomic.Int64
}

func (h *countingLifecycle) Connected(*ws.Conn) func() {
	h.connected.Add(1)
	return func() { h.cleaned.Add(1) }
}

// startHookServer 复用 e2e_test.go 的 helper 起一个真实 Server（:0 随机端口）。
func startHookServer(t *testing.T) (*Server, *authn.Signer) {
	t.Helper()
	authnFn, signer, _ := testAuthn(t)
	cfg := DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.EnableRateLimit = false
	srv, err := New(cfg, authnFn, newFakeBridge())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Run(ctx) }()
	t.Cleanup(cancel)
	return srv, signer
}

// waitHookServerQuiet 等一条连接的 OnDisconnect 收尾完成（session 注销 +
// cleanup）。断连回调在 readLoop 的 defer 里异步触发，测试据此避免与后续
// 连接/断言交错。
func waitHookServerQuiet(t *testing.T, srv *Server) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if srv.Session().Count() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("session still has %d conn(s) after disconnect", srv.Session().Count())
}

// dialHookConn 用 signer 现场签发凭证并握手；不出现任何硬编码 token。
func dialHookConn(t *testing.T, srv *Server, signer *authn.Signer, uid int64, dev string) *websocket.Conn {
	t.Helper()
	tok, err := signer.SignAccess(uid, dev, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	hdr := http.Header{}
	hdr.Set("Cookie", DefaultConfig().CookieName+"="+tok)
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	c, resp, err := dialer.Dial("ws://"+waitAddr(t, srv)+"/ws", hdr)
	if err != nil {
		t.Fatalf("dial: %v (resp=%v)", err, resp)
	}
	return c
}

// awaitHookCount 等 OnConnect/OnDisconnect 的异步回调节拍；超过期望值立即
// 失败（重复调用也算错），而不是等到超时。
func awaitHookCount(t *testing.T, label string, cur func() int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got := cur()
		if got >= want {
			if got != want {
				t.Fatalf("%s = %d, want %d", label, got, want)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s = %d, want %d (timeout)", label, cur(), want)
}

// TestConnLifecycleHooks 覆盖四项：未注入零调用（且旧路径 Ping/Pong 不变）、
// Connected 调用次数、cleanup 在 OnDisconnect 触发、并发 AddConnLifecycle
// 安全（配合 -race）；顺带验证 nil 忽略与后置注入不补调存量连接。
func TestConnLifecycleHooks(t *testing.T) {
	srv, signer := startHookServer(t)

	// ---- 1. 未注入任何 hooks：零调用、零额外状态，旧路径逐字节照常 ----
	observer := &countingLifecycle{}
	before := dialHookConn(t, srv, signer, 9101, "dev-before")
	// 用一次 Ping/Pong 确认连接确实走完了 OnConnect（握手返回不保证回调
	// 已执行），同时确认未注入时下行帧形态与主干一致。
	ping, _ := (&chatpb.Ping{ClientMs: 4242}).Marshal()
	sendEnvelope(t, before, &chatpb.Envelope{Kind: kinds.KindPing, Payload: ping})
	env := readEnvelope(t, before)
	if env.Kind != kinds.KindPong {
		t.Fatalf("reply kind = %d, want PONG", env.Kind)
	}
	var pong chatpb.Pong
	_ = pong.Unmarshal(env.Payload)
	if pong.ClientMs != 4242 {
		t.Fatalf("pong = %+v, want ClientMs 4242", pong)
	}
	if n := observer.connected.Load(); n != 0 {
		t.Fatalf("Connected called %d times without injection, want 0", n)
	}
	if n := observer.cleaned.Load(); n != 0 {
		t.Fatalf("cleanup called %d times without injection, want 0", n)
	}
	if n := len(srv.lifecycleHooks()); n != 0 {
		t.Fatalf("lifecycle snapshot len = %d, want 0", n)
	}
	stored := 0
	srv.cleanups.Range(func(_, _ any) bool { stored++; return true })
	if stored != 0 {
		t.Fatalf("cleanups entries = %d, want 0", stored)
	}
	_ = before.Close()
	waitHookServerQuiet(t, srv)

	// ---- 2. 后置注入：Connected 次数 + OnDisconnect 触发 cleanup ----
	srv.AddConnLifecycle(observer)
	c1 := dialHookConn(t, srv, signer, 9102, "dev-c1")
	awaitHookCount(t, "Connected after 1st conn", observer.connected.Load, 1)
	_ = c1.Close()
	awaitHookCount(t, "cleanup after 1st disconnect", observer.cleaned.Load, 1)
	waitHookServerQuiet(t, srv)
	c2 := dialHookConn(t, srv, signer, 9103, "dev-c2")
	awaitHookCount(t, "Connected after 2nd conn", observer.connected.Load, 2)
	_ = c2.Close()
	awaitHookCount(t, "cleanup after 2nd disconnect", observer.cleaned.Load, 2)
	waitHookServerQuiet(t, srv)

	// ---- 3. 并发 AddConnLifecycle：快照不丢不重，nil 忽略 ----
	const addN = 32
	fakes := make([]*countingLifecycle, addN)
	var wg sync.WaitGroup
	for i := range fakes {
		fakes[i] = &countingLifecycle{}
		wg.Add(1)
		go func(h *countingLifecycle) {
			defer wg.Done()
			srv.AddConnLifecycle(h)
		}(fakes[i])
	}
	wg.Wait()
	srv.AddConnLifecycle(nil)
	if got, want := len(srv.lifecycleHooks()), addN+1; got != want {
		t.Fatalf("lifecycle snapshot len = %d, want %d", got, want)
	}

	// 多钩子连接：每个钩子恰好一次 Connected，断连时按 c.ID() 聚合的
	// cleanup 全部被调用（不漏调、不重复）。
	c3 := dialHookConn(t, srv, signer, 9104, "dev-c3")
	awaitHookCount(t, "observer Connected (3rd conn)", observer.connected.Load, 3)
	for _, f := range fakes {
		awaitHookCount(t, "concurrent hook Connected", f.connected.Load, 1)
	}
	_ = c3.Close()
	awaitHookCount(t, "observer cleanup (3rd conn)", observer.cleaned.Load, 3)
	for _, f := range fakes {
		awaitHookCount(t, "concurrent hook cleanup", f.cleaned.Load, 1)
	}
	waitHookServerQuiet(t, srv)
}
