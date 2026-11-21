package framerw

import (
	"context"
	"testing"
	"time"

	"github.com/yingo-server/UFOT/server/internal/proto"
	chatpb "github.com/yingo-server/UFOT/server/internal/proto/gen/chatpb"
)

// fakePeer 最小 Peer 实现（测试内定义，不 import 兄弟组件）。
type fakePeer struct {
	layer  string
	in     chan *proto.Frame
	sent   []*proto.Frame
	closed bool
}

func newFakePeer(layer string) *fakePeer {
	return &fakePeer{layer: layer, in: make(chan *proto.Frame, 8)}
}

func (p *fakePeer) Send(f *proto.Frame) error {
	p.sent = append(p.sent, f)
	return nil
}
func (p *fakePeer) Frames() <-chan *proto.Frame { return p.in }
func (p *fakePeer) Close() error                { p.closed = true; return nil }
func (p *fakePeer) Layer() string               { return p.layer }

func TestRunStampsMeta(t *testing.T) {
	p := newFakePeer("user")
	out := make(chan *proto.Frame, 8)
	rw := New(p, out)
	ctx, cancel := context.WithCancel(context.Background())
	go rw.Run(ctx)
	p.in <- &proto.Frame{Seq: 1, Kind: 101, Env: &chatpb.Envelope{Kind: 101, RequestId: "req-abc"}}
	f := <-out
	if f.Meta == nil {
		t.Fatal("meta not stamped")
	}
	if f.Meta.TraceID != "req-abc" {
		t.Fatalf("trace id from request_id: got %q", f.Meta.TraceID)
	}
	if f.Meta.SrcLayer != "user" {
		t.Fatalf("src layer: got %q", f.Meta.SrcLayer)
	}
	if f.Meta.EnqueuedAt.IsZero() {
		t.Fatal("enqueued_at not stamped")
	}
	cancel()
	rw.Close()
}

func TestRunGeneratesTraceID(t *testing.T) {
	p := newFakePeer("data")
	out := make(chan *proto.Frame, 8)
	rw := New(p, out)
	ctx, cancel := context.WithCancel(context.Background())
	go rw.Run(ctx)
	p.in <- &proto.Frame{Seq: 2, Kind: 911, Env: &chatpb.Envelope{Kind: 911}}
	f := <-out
	if f.Meta.TraceID == "" {
		t.Fatal("trace id not generated")
	}
	cancel()
	rw.Close()
}

func TestWrite(t *testing.T) {
	p := newFakePeer("data")
	rw := New(p, nil)
	err := rw.Write(&proto.Frame{Seq: 3, Kind: 101, Env: &chatpb.Envelope{Kind: 101}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.sent) != 1 {
		t.Fatalf("sent=%d want 1", len(p.sent))
	}
}

func TestRunStopsOnPeerClose(t *testing.T) {
	p := newFakePeer("user")
	out := make(chan *proto.Frame, 8)
	rw := New(p, out)
	done := make(chan error, 1)
	go func() { done <- rw.Run(context.Background()) }()
	close(p.in)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("want nil, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("run did not stop on peer close")
	}
}
