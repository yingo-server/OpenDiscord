package embedded

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yingo-server/UFOT/server/internal/proto"
)

// mockLayer 模拟一个层。
type mockLayer struct {
	name      string
	outbound  chan *proto.Frame
	delivered []*proto.Frame
	mu        sync.Mutex
}

func newMockLayer(name string) *mockLayer {
	return &mockLayer{name: name, outbound: make(chan *proto.Frame, 8)}
}

func (m *mockLayer) LayerName() string             { return m.name }
func (m *mockLayer) Outbound() <-chan *proto.Frame { return m.outbound }
func (m *mockLayer) Deliver(f *proto.Frame) error {
	m.mu.Lock()
	m.delivered = append(m.delivered, f)
	m.mu.Unlock()
	return nil
}
func (m *mockLayer) Close() error { return nil }
func (m *mockLayer) got() []*proto.Frame {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*proto.Frame(nil), m.delivered...)
}

func mk(seq uint32) *proto.Frame { return &proto.Frame{Seq: seq, Kind: seq} }

// 双向：层上行帧 → 总线 Peer.Frames()；总线 Peer.Send() → 层 Deliver。
func TestEmbeddedBidirectional(t *testing.T) {
	layer := newMockLayer("user")
	br := NewBridge(nil)
	peer := br.Attach(layer)
	bus := br.Peer()

	// 上行：层 → 总线
	layer.outbound <- mk(1)
	select {
	case f := <-bus.Frames():
		if f.Seq != 1 {
			t.Fatalf("upstream seq = %d, want 1", f.Seq)
		}
	case _ = <-errAfter(2):
		t.Fatal("timeout waiting for upstream frame")
	}

	// 下行：总线 → 层
	if err := bus.Send(mk(2)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor(t, func() bool { return len(layer.got()) == 1 })
	if layer.got()[0].Seq != 2 {
		t.Fatalf("downstream seq = %d, want 2", layer.got()[0].Seq)
	}

	// Peer 属性
	if peer.Layer() != "user" || bus.Layer() != "bridge" {
		t.Fatalf("layer names: %q / %q", peer.Layer(), bus.Layer())
	}
	_ = peer.Close()
	_ = bus.Close()
}

// 层端 Peer.Send 走上行方向进总线。
func TestLayerPeerSend(t *testing.T) {
	layer := newMockLayer("data")
	br := NewBridge(layer)
	// Attach 已启动泵；直接用返回的 Peer 上行发送。
	peer := br.lastAttach()
	if err := peer.Send(mk(7)); err != nil {
		t.Fatalf("peer.Send: %v", err)
	}
	select {
	case f := <-br.Peer().Frames():
		if f.Seq != 7 {
			t.Fatalf("seq = %d, want 7", f.Seq)
		}
	case _ = <-errAfter(2):
		t.Fatal("timeout")
	}
}

func TestCloseIdempotentAndSendAfterClose(t *testing.T) {
	layer := newMockLayer("admin")
	br := NewBridge(nil)
	peer := br.Attach(layer)
	if err := peer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := peer.Close(); err != nil {
		t.Fatalf("second Close should be nil, got %v", err)
	}
	err := peer.Send(mk(1))
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Send after close = %v, want ErrClosed", err)
	}
}

func TestMultiLayerFanout(t *testing.T) {
	l1, l2 := newMockLayer("user"), newMockLayer("data")
	br := NewBridge(nil)
	br.Attach(l1)
	br.Attach(l2)
	bus := br.Peer()
	if err := bus.Send(mk(3)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(l1.got()) == 1 && len(l2.got()) == 1 })
}

// ---- helpers ----

func errAfter(sec int) <-chan error {
	ch := make(chan error, 1)
	go func() {
		<-timeAfter(sec)
		ch <- errors.New("timeout")
	}()
	return ch
}

func timeAfter(sec int) <-chan time.Time { return time.After(time.Duration(sec) * time.Second) }

func sleepMs(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		sleepMs(10)
	}
	t.Fatal("condition never met")
}
