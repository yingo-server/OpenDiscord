package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/yingo-server/UFOT/server/internal/pkg/log"
	"github.com/yingo-server/UFOT/server/internal/proto"
	"github.com/yingo-server/UFOT/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
	"github.com/yingo-server/UFOT/server/internal/transport"
)

var (
	_ transport.Peer = (*remotePeer)(nil)
	_ transport.Peer = (*ReconnectingPeer)(nil)
)

var (
	// ErrClosed 连接已关闭。
	ErrClosed = errors.New("remote: peer closed")
	// ErrNotConnected ReconnectingPeer 当前未连接。
	ErrNotConnected = errors.New("remote: not connected")
)

// handshakeTimeout 握手完成时限。
const handshakeTimeout = 5 * time.Second

// remotePeer 一条已认证连接的 Peer 实现。
// 写方向：Send → 编码 → 连接写锁；读方向：Frames 来自读 goroutine。
type remotePeer struct {
	layer  string
	conn   net.Conn
	cw     *connWriter
	frames chan *proto.Frame

	closeOnce sync.Once
	closeErr  error
	ctrl      chan *proto.Frame // 控制面帧（旁路）
}

// peerFramesChan 接收流缓冲深度。
const framesChanCap = 256

// newRemotePeer 握手完成后包装一条连接。
func newRemotePeer(layer string, conn net.Conn) *remotePeer {
	p := &remotePeer{
		layer:  layer,
		conn:   conn,
		cw:     newConnWriter(conn),
		frames: make(chan *proto.Frame, framesChanCap),
		ctrl:   make(chan *proto.Frame, controlChanCap),
	}
	return p
}

func (p *remotePeer) Layer() string               { return p.layer }
func (p *remotePeer) Frames() <-chan *proto.Frame { return p.frames }

// Send 编码 BUS_SOCKET 帧并写入连接。连接级错误返回（不崩进程）。
func (p *remotePeer) Send(f *proto.Frame) error {
	buf, err := EncodeFrame(f, 0)
	if err != nil {
		return err
	}
	return p.cw.write(buf)
}

func (p *remotePeer) Close() error {
	p.closeOnce.Do(func() {
		p.closeErr = p.conn.Close()
	})
	return p.closeErr
}

// readLoop 读 goroutine：解码 → frames channel；任何错误即关连接+关 channel。
func (p *remotePeer) readLoop(ctx context.Context) {
	defer close(p.frames)
	defer close(p.ctrl)
	for {
		_ = p.conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		f, err := DecodeFrame(p.conn)
		if err != nil {
			// 控制面帧（ACK/HEARTBEAT）在此被消费丢弃：数据面流水线只见数据帧
			p.Close()
			return
		}
		// 控制面（902-909：HELLO/SUBSCRIBE/FRAME_ACK/HEARTBEAT）不进数据面
		// 流水线：它们经 Control() 交给 SessionManager 旁路消费（一切皆帧，
		// 但控制面与数据面严格分离）。
		if f.Kind >= kinds.KindInternalHello && f.Kind <= controlCeiling {
			select {
			case p.ctrl <- f:
			default: // 控制面队列满：丢弃并计数（心跳等可丢）
			}
			continue
		}
		select {
		case p.frames <- f:
		case <-ctx.Done():
			p.Close()
			return
		}
	}
}

// controlChanCap 控制面帧队列容量（心跳等允许丢弃）。
const controlChanCap = 64

// controlCeiling 是控制面帧号段上界（902..909）。
const controlCeiling = 909

// Control 暴露控制面帧流（SessionManager 旁路消费）。非接口方法：需要它的
// 装配层用类型断言或接口 ControlPeer 取用。
func (p *remotePeer) Control() <-chan *proto.Frame { return p.ctrl }

// Serve 在监听器上循环接受层连接：accept → HMAC 握手 → 启动 Peer 泵。
// ctx 结束即关闭监听器并返回。auth 为 nil 表示跳过握手（仅测试）。
func Serve(ctx context.Context, ln net.Listener, auth *HMACAuth) error {
	if ul, ok := ln.(*net.UnixListener); ok {
		// UDS 文件权限 0600：仅本进程属主可连
		_ = os.Chmod(ul.Addr().String(), 0o600)
		ul.SetUnlinkOnClose(true)
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			return err
		}
		go handleConn(ctx, conn, auth)
	}
}

// handleConn 单条服务端连接：握手 + 读循环。握手失败立即断开。
func handleConn(ctx context.Context, conn net.Conn, auth *HMACAuth) {
	if auth == nil {
		// 无认证模式：跳过握手，按裸帧处理
		p := newRemotePeer("bridge", conn)
		go p.readLoop(ctx)
		return
	}
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	w, err := DecodeControl(conn)
	if err != nil {
		conn.Close()
		return
	}
	if w.Kind != kinds.KindInternalHello {
		conn.Close()
		return
	}
	client, err := unmarshalHello(w.Body)
	if err != nil {
		conn.Close()
		return
	}
	ack, err := auth.ServerAck(client, "bridge")
	if err != nil {
		log.For("remote").Warn("handshake rejected", "layer", client.Layer, "err", err.Error())
		conn.Close()
		return
	}
	ackBuf, err := EncodeControl(w.Seq+1, helloAckKind, ack)
	if err != nil {
		conn.Close()
		return
	}
	if _, err := conn.Write(ackBuf); err != nil {
		conn.Close()
		return
	}
	_ = conn.SetDeadline(time.Time{}) // 握手完成，恢复常规读超时
	p := newRemotePeer(client.Layer, conn)
	go p.readLoop(ctx)
}

// Listen 在 UDS 或 TCP 地址上监听。路径含 "/" 视为 UDS。
func Listen(addr string) (net.Listener, error) {
	if len(addr) > 0 && addr[0] == '/' {
		_ = os.Remove(addr)
		ln, err := net.Listen("unix", addr)
		if err != nil {
			return nil, err
		}
		if err := os.Chmod(addr, 0o600); err != nil {
			ln.Close()
			return nil, fmt.Errorf("remote: chmod uds %s: %w", addr, err)
		}
		return ln, nil
	}
	return net.Listen("tcp", addr)
}

// Dial 建立到桥接的连接并完成 HMAC 握手，返回已认证 Peer。
func Dial(ctx context.Context, addr string, auth *HMACAuth) (transport.Peer, error) {
	network := "tcp"
	if len(addr) > 0 && addr[0] == '/' {
		network = "unix"
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	if auth == nil {
		p := newRemotePeer("bridge", conn)
		go p.readLoop(ctx)
		return p, nil
	}
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	hello, err := auth.ClientHello("user", nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	helloBuf, err := EncodeControl(1, kinds.KindInternalHello, hello)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if _, err := conn.Write(helloBuf); err != nil {
		conn.Close()
		return nil, err
	}
	w, err := DecodeControl(conn)
	if err != nil {
		conn.Close()
		return nil, ErrHandshake
	}
	if w.Kind != helloAckKind {
		conn.Close()
		return nil, ErrHandshake
	}
	ack, err := unmarshalHello(w.Body)
	if err != nil {
		conn.Close()
		return nil, ErrHandshake
	}
	if err := auth.VerifyAck(ack, "bridge"); err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	p := newRemotePeer("user", conn)
	go p.readLoop(ctx)
	return p, nil
}

// ReconnectingPeer 包装一个会断的 Peer：断线后指数退避重连
// （0.5s→1s→2s→4s→8s 封顶），重连成功后自动重发 HELLO+SUBSCRIBE
// 帧并回调通知上层 resync。
type ReconnectingPeer struct {
	addr  string
	auth  *HMACAuth
	layer string
	subs  []string

	mu      sync.Mutex
	inner   transport.Peer
	closed  bool
	attempt int
	seq     uint32

	// onResync 重连成功后回调（上层触发 RESYNC）。写入只经 SetOnResync：
	// connectLoop 在另一个 goroutine 里读它，直接写字段就是数据竞争
	// （CI -race 实测）。
	onResync func(p transport.Peer)
}

// NewReconnectingPeer 构造并启动首次连接。subs 为重连后重发的订阅 topic。
func NewReconnectingPeer(ctx context.Context, addr string, auth *HMACAuth, layer string, subs []string) *ReconnectingPeer {
	rp := &ReconnectingPeer{addr: addr, auth: auth, layer: layer, subs: subs}
	go rp.connectLoop(ctx)
	return rp
}

func backoff(attempt int) time.Duration {
	d := 500 * time.Millisecond
	for i := 0; i < attempt && d < 8*time.Second; i++ {
		d *= 2
	}
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return d
}

// connectLoop 首连 + 守护重连。
func (rp *ReconnectingPeer) connectLoop(ctx context.Context) {
	for {
		rp.mu.Lock()
		if rp.closed {
			rp.mu.Unlock()
			return
		}
		rp.mu.Unlock()

		p, err := Dial(ctx, rp.addr, rp.auth)
		if err != nil {
			rp.waitBackoff(ctx)
			continue
		}
		rp.mu.Lock()
		if rp.closed {
			rp.mu.Unlock()
			p.Close()
			return
		}
		rp.inner = p
		rp.attempt = 0
		rp.mu.Unlock()

		// 重发 HELLO + SUBSCRIBE（重连恢复语义：订约 = 各层重连重建）
		rp.resendHelloSubscribe(p)

		rp.mu.Lock()
		cb := rp.onResync
		rp.mu.Unlock()
		if cb != nil {
			cb(p)
		}

		// 监视连接存活
		rp.watchConn(ctx, p)
	}
}

func (rp *ReconnectingPeer) waitBackoff(ctx context.Context) {
	rp.mu.Lock()
	d := backoff(rp.attempt)
	rp.attempt++
	rp.mu.Unlock()
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

func (rp *ReconnectingPeer) watchConn(ctx context.Context, p transport.Peer) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-p.Frames():
			if !ok { // 连接断开 → 触发重连循环
				return
			}
		}
	}
}

// resendHelloSubscribe 重连后重发 HELLO+SUBSCRIBE 内部帧。
func (rp *ReconnectingPeer) resendHelloSubscribe(p transport.Peer) {
	rp.mu.Lock()
	rp.seq++
	seq := rp.seq
	rp.mu.Unlock()
	if err := p.Send(makeInternalFrame(seq, kinds.KindInternalHello, rp.layer)); err != nil {
		return
	}
	for _, topic := range rp.subs {
		rp.mu.Lock()
		rp.seq++
		s := rp.seq
		rp.mu.Unlock()
		if err := p.Send(makeSubscribeFrame(s, topic)); err != nil {
			return
		}
	}
}

// Layer 返回层标识。
// SetOnResync installs the resync callback. Safe to call at any time (including
// after the first connection has been established).
func (rp *ReconnectingPeer) SetOnResync(fn func(p transport.Peer)) {
	rp.mu.Lock()
	rp.onResync = fn
	rp.mu.Unlock()
}

func (rp *ReconnectingPeer) Layer() string { return rp.layer }

// Send 发送一帧；未连接时返回 ErrNotConnected。
func (rp *ReconnectingPeer) Send(f *proto.Frame) error {
	rp.mu.Lock()
	inner := rp.inner
	rp.mu.Unlock()
	if inner == nil {
		return ErrNotConnected
	}
	return inner.Send(f)
}

// Frames 返回当前连接的接收流（断线重连后是新流；上层应配合 OnResync）。
func (rp *ReconnectingPeer) Frames() <-chan *proto.Frame {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	if rp.inner == nil {
		ch := make(chan *proto.Frame)
		close(ch)
		return ch
	}
	return rp.inner.Frames()
}

// Close 停止重连并关闭当前连接。
func (rp *ReconnectingPeer) Close() error {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	if rp.closed {
		return nil
	}
	rp.closed = true
	if rp.inner != nil {
		return rp.inner.Close()
	}
	return nil
}

// makeInternalFrame 构造内部 HELLO 帧（Envelope 内嵌 hello JSON）。
func makeInternalFrame(seq uint32, kind kinds.Kind, layer string) *proto.Frame {
	body, _ := json.Marshal(map[string]string{"layer": layer})
	return &proto.Frame{
		Seq:  seq,
		Kind: kind,
		Env:  &chatpb.Envelope{Kind: uint32(kind), Payload: []byte(body)},
		Meta: &proto.FrameMeta{TraceID: ""},
	}
}

// makeSubscribeFrame 构造 SUBSCRIBE 帧（Envelope 内嵌 topic）。
func makeSubscribeFrame(seq uint32, topic string) *proto.Frame {
	body, _ := json.Marshal(map[string]string{"topic": topic})
	return &proto.Frame{
		Seq:  seq,
		Kind: kinds.KindInternalSubscribe,
		Env:  &chatpb.Envelope{Kind: uint32(kinds.KindInternalSubscribe), Payload: []byte(body)},
		Meta: &proto.FrameMeta{},
	}
}
