package ack

import (
	"testing"
	"time"

	"github.com/yingo-server/UFOT/server/internal/proto"
	chatpb "github.com/yingo-server/UFOT/server/internal/proto/gen/chatpb"
)

func mkf(seq uint32) *proto.Frame {
	return &proto.Frame{Seq: seq, Kind: 101, Env: &chatpb.Envelope{Kind: 101}}
}

func TestTrackAndAck(t *testing.T) {
	tr := New()
	tr.Track(1, mkf(1))
	if tr.InFlight() != 1 {
		t.Fatal("inflight want 1")
	}
	tr.OnAck(1)
	if tr.InFlight() != 0 {
		t.Fatal("inflight want 0 after ack")
	}
}

func TestRtoResend(t *testing.T) {
	tr := New()
	base := time.Now()
	tr.clock = func() time.Time { return base }
	var resends int
	tr.Resend = func(seq uint32, f *proto.Frame) { resends++ }
	tr.Track(1, mkf(1))
	// 未超时：不重发。
	tr.Tick()
	if resends != 0 {
		t.Fatalf("resends=%d want 0 before RTO", resends)
	}
	// 超时：重发。
	tr.clock = func() time.Time { return base.Add(RTO * 2) }
	tr.Tick()
	if resends != 1 {
		t.Fatalf("resends=%d want 1", resends)
	}
}

func TestDeadAfterMaxRetries(t *testing.T) {
	tr := New()
	base := time.Now()
	cur := base
	tr.clock = func() time.Time { return cur }
	resends, deads := 0, 0
	tr.Resend = func(seq uint32, f *proto.Frame) { resends++ }
	tr.OnDead = func(seq uint32, f *proto.Frame) { deads++ }
	tr.Track(7, mkf(7))
	for i := 0; i < MaxRetries; i++ {
		cur = cur.Add(RTO * 2)
		tr.Tick()
	}
	if deads != 1 {
		t.Fatalf("deads=%d want 1", deads)
	}
	if resends != MaxRetries-1 {
		t.Fatalf("resends=%d want %d", resends, MaxRetries-1)
	}
	if tr.InFlight() != 0 {
		t.Fatal("dead seq must be removed")
	}
}

func TestDuplicateTrackPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate Track")
		}
	}()
	tr := New()
	tr.Track(1, mkf(1))
	tr.Track(1, mkf(1))
}

func TestUnknownAckPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on unknown ack")
		}
	}()
	tr := New()
	tr.OnAck(99)
}
