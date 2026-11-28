package remote

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/yingo-server/UFOT/server/internal/proto"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
	"github.com/yingo-server/UFOT/server/internal/transport"
)

func freeTCPPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func testKey() *HMACAuth {
	return &HMACAuth{Keys: [][]byte{[]byte("test-shared-secret")}}
}

// runServer 起一个带握手的测试服务端，返回服务端 Peer 流。
func runServer(ctx context.Context, ln net.Listener, auth *HMACAuth) <-chan transport.Peer {
	peers := make(chan transport.Peer, 4)
	go func() {
		defer close(peers)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				p, err := serverHandshake(ctx, conn, auth)
				if err != nil {
					return
				}
				select {
				case peers <- p:
				case <-ctx.Done():
				}
			}(conn)
		}
	}()
	return peers
}

// serverHandshake 服务端握手（与 handleConn 相同路径，但返回 Peer 供测试）。
func serverHandshake(ctx context.Context, conn net.Conn, auth *HMACAuth) (transport.Peer, error) {
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	w, err := DecodeControl(conn)
	if err != nil {
		conn.Close()
		return nil, ErrHandshake
	}
	if w.Kind != kinds.KindInternalHello {
		conn.Close()
		return nil, ErrHandshake
	}
	client, err := unmarshalHello(w.Body)
	if err != nil {
		conn.Close()
		return nil, ErrHandshake
	}
	ack, err := auth.ServerAck(client, "bridge")
	if err != nil {
		conn.Close()
		return nil, err
	}
	ackBuf, err := EncodeControl(w.Seq+1, helloAckKind, ack)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if _, err := conn.Write(ackBuf); err != nil {
		conn.Close()
		return nil, ErrHandshake
	}
	_ = conn.SetDeadline(time.Time{})
	p := newRemotePeer(client.Layer, conn)
	go p.readLoop(ctx)
	return p, nil
}

func TestTCPHandshakeAndRoundTrip(t *testing.T) {
	addr := freeTCPPort(t)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serverPeers := runServer(ctx, ln, testKey())

	// 客户端拨号（真实 Dial 路径）
	client, err := Dial(ctx, addr, testKey())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	if client.Layer() != "user" {
		t.Errorf("Layer = %q, want user", client.Layer())
	}

	var server transport.Peer
	select {
	case p, ok := <-serverPeers:
		if !ok || p == nil {
			t.Fatal("server peer missing")
		}
		server = p
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for server peer")
	}
	defer server.Close()

	// 客户端 → 服务端
	if err := client.Send(&proto.Frame{Seq: 1, Kind: 100, Meta: &proto.FrameMeta{TraceID: "tcp-1"}}); err != nil {
		t.Fatalf("client Send: %v", err)
	}
	select {
	case f := <-server.Frames():
		if f.Seq != 1 || f.Meta.TraceID != "tcp-1" {
			t.Fatalf("server got seq=%d trace=%q", f.Seq, f.Meta.TraceID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for frame at server")
	}

	// 服务端 → 客户端
	if err := server.Send(&proto.Frame{Seq: 2, Kind: 101, Meta: &proto.FrameMeta{}}); err != nil {
		t.Fatalf("server Send: %v", err)
	}
	select {
	case f := <-client.Frames():
		if f.Seq != 2 {
			t.Fatalf("client got seq=%d", f.Seq)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for frame at client")
	}
}

// 手动管道（net.Pipe）：完整握手 + 双向收发。
func TestPipeHandshakeBidirectional(t *testing.T) {
	ctx := context.Background()
	c1, c2 := net.Pipe()
	auth := testKey()

	serverDone := make(chan transport.Peer, 1)
	go func() {
		p, err := serverHandshake(ctx, c2, auth)
		if err != nil {
			serverDone <- nil
			return
		}
		serverDone <- p
	}()

	// 客户端握手（复用 Dial 的握手逻辑但注入管道连接）。握手必须与
	// readLoop 串行：两条 goroutine 同时读同一个 net.Conn 会互相切碎流。
	hello, _ := auth.ClientHello("user", []string{"sys.>"})
	buf, _ := EncodeControl(1, kinds.KindInternalHello, hello)
	if _, werr := c1.Write(buf); werr != nil {
		t.Fatalf("client write hello: %v", werr)
	}
	// The server answers HELLO with HELLO_ACK on the same socket; a real Dial
	// consumes it before handing the conn to the peer.
	if _, derr := DecodeControl(c1); derr != nil {
		t.Fatalf("client read hello_ack: %v", derr)
	}
	_ = c1.SetDeadline(time.Time{})
	clientPeer := newRemotePeer("user", c1)
	go clientPeer.readLoop(ctx)

	srv := <-serverDone
	if srv == nil {
		t.Fatal("server handshake failed")
	}
	defer srv.Close()
	defer clientPeer.Close()

	// 双向收发：客户端 → 服务端
	if err := clientPeer.Send(&proto.Frame{Seq: 42, Kind: 100, Meta: &proto.FrameMeta{TraceID: "tx-42"}}); err != nil {
		t.Fatalf("client Send: %v", err)
	}
	select {
	case f := <-srv.Frames():
		if f.Seq != 42 || f.Meta.TraceID != "tx-42" {
			t.Fatalf("server got seq=%d trace=%q", f.Seq, f.Meta.TraceID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}
	// 服务端 → 客户端
	if err := srv.Send(&proto.Frame{Seq: 7, Kind: 101, Meta: &proto.FrameMeta{}}); err != nil {
		t.Fatalf("server Send: %v", err)
	}
	select {
	case f2 := <-clientPeer.Frames():
		if f2.Seq != 7 {
			t.Fatalf("client got seq=%d", f2.Seq)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}
}

// 握手失败场景：错误密钥 → 服务端拒绝（连接被断开，客户端读 EOF）。
func TestHandshakeWrongKeyRejected(t *testing.T) {
	serverAuth := testKey()
	badAuth := &HMACAuth{Keys: [][]byte{[]byte("wrong-key")}}

	c1, c2 := net.Pipe()
	defer c1.Close()

	result := make(chan error, 1)
	go func() {
		if _, err := serverHandshake(context.Background(), c2, serverAuth); err != nil {
			result <- err
		} else {
			result <- nil
		}
	}()

	go func() {
		_ = c1.SetDeadline(time.Now().Add(handshakeTimeout))
		hello, _ := badAuth.ClientHello("user", nil)
		buf, _ := EncodeControl(1, kinds.KindInternalHello, hello)
		_, _ = c1.Write(buf)
		// The server answers HELLO with HELLO_ACK on the same socket; a real
		// Dial consumes it before handing the conn to the peer. Mirror that
		// here, otherwise the ack would surface as the first data frame.
		if _, derr := DecodeControl(c1); derr != nil {
			return
		}
		_ = c1.SetDeadline(time.Time{})
	}()

	select {
	case err := <-result:
		if !errors.Is(err, ErrHandshake) {
			t.Fatalf("want ErrHandshake, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}
}

// 时间窗外的握手被拒绝。
func TestHandshakeExpiredTimestampRejected(t *testing.T) {
	auth := testKey()
	hello, _ := auth.ClientHello("user", nil)
	hello.TS = time.Now().Add(-2 * time.Minute).Unix()
	hello.HMAC = computeMAC(auth.Keys[0], hello.Layer, hello.Nonce, hello.TS)
	if _, err := auth.ServerAck(hello, "bridge"); !errors.Is(err, ErrHandshake) {
		t.Fatalf("expired ts should be rejected, got %v", err)
	}
}

// nonce 重放拒绝：同一 HELLO 验证两次，第二次失败。
func TestNonceReplayRejected(t *testing.T) {
	auth := testKey()
	hello, err := auth.ClientHello("user", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.ServerAck(hello, "bridge"); err != nil {
		t.Fatalf("first handshake should pass: %v", err)
	}
	if _, err := auth.ServerAck(hello, "bridge"); !errors.Is(err, ErrHandshake) {
		t.Fatalf("replayed nonce should be rejected, got %v", err)
	}
}

// nonce LRU 淘汰：容量 1 时不同 nonce 依次通过（旧 nonce 被淘汰）。
func TestNonceLRUEviction(t *testing.T) {
	auth := &HMACAuth{Keys: [][]byte{[]byte("k")}, NonceCap: 1}
	for i := 0; i < 3; i++ {
		h, _ := auth.ClientHello("user", nil)
		if _, err := auth.ServerAck(h, "bridge"); err != nil {
			t.Fatalf("iteration %d should pass (LRU evicted old nonce): %v", i, err)
		}
	}
}

// 非法 magic 帧解码失败。
func TestBadMagicRejected(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	go func() { _, _ = c1.Write([]byte("XXXX1234")) }()
	if _, err := DecodeFrame(c2); !errors.Is(err, ErrBadFrame) {
		t.Fatalf("bad magic should be ErrBadFrame, got %v", err)
	}
}

// 帧编解码往返：BUS_SOCKET 帧的 encode→decode 保真。
func TestFrameCodecRoundTrip(t *testing.T) {
	f := &proto.Frame{
		Seq:  99,
		Kind: 100,
		Meta: &proto.FrameMeta{TraceID: "trace-99", ConnID: 1},
	}
	buf, err := EncodeFrame(f, 0)
	if err != nil {
		t.Fatal(err)
	}
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	go func() { _, _ = c1.Write(buf) }()
	got, err := DecodeFrame(c2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Seq != 99 || got.Kind != 100 || got.Meta.TraceID != "trace-99" ||
		got.Meta.ConnID != 1 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

// ReconnectingPeer：断线后重连并触发 resync 回调。
func TestReconnectingPeerResync(t *testing.T) {
	addr := freeTCPPort(t)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	resyncs := 0
	serverPeers := runServer(ctx, ln, testKey())

	rp := NewReconnectingPeer(ctx, addr, testKey(), "user", []string{"sys.>"})
	rp.SetOnResync(func(transport.Peer) {
		mu.Lock()
		resyncs++
		mu.Unlock()
	})
	defer rp.Close()

	// 第一次连接 + resync
	waitDeadline := time.After(5 * time.Second)
	var server transport.Peer
	for server == nil {
		select {
		case p, ok := <-serverPeers:
			if !ok {
				t.Fatal("server peers closed")
			}
			server = p
		case <-waitDeadline:
			t.Fatal("timeout waiting for first connection")
		}
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return resyncs >= 1 })

	// 断开服务端 → 客户端应重连（backoff 0.5s）
	server.Close()
	secondDeadline := time.After(6 * time.Second)
	var server2 transport.Peer
	for server2 == nil {
		select {
		case p, ok := <-serverPeers:
			if !ok {
				t.Fatal("server peers closed")
			}
			server2 = p
		case <-secondDeadline:
			t.Fatal("timeout waiting for reconnection")
		}
	}
	defer server2.Close()
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return resyncs >= 2 })

	// 重连后能正常收发
	if err := rp.Send(&proto.Frame{Seq: 5, Kind: 100, Meta: &proto.FrameMeta{}}); err != nil {
		t.Fatalf("Send after reconnect: %v", err)
	}
	select {
	case f := <-server2.Frames():
		if f.Seq != 5 {
			t.Fatalf("server2 got seq=%d", f.Seq)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never met")
}
