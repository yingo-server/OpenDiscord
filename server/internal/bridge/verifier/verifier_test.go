package verifier

import (
	"context"
	"testing"
	"time"

	"github.com/yingo-server/UFOT/server/internal/proto"
	chatpb "github.com/yingo-server/UFOT/server/internal/proto/gen/chatpb"
	kinds "github.com/yingo-server/UFOT/server/internal/proto/kinds"
)

func mkf(layer string, kind uint32) *proto.Frame {
	return &proto.Frame{
		Seq: 1, Kind: kind,
		Env:  &chatpb.Envelope{Kind: kind},
		Meta: &proto.FrameMeta{SrcLayer: layer, TraceID: "t1"},
	}
}

func TestPassesValidKind(t *testing.T) {
	in := make(chan *proto.Frame, 1)
	out := make(chan *proto.Frame, 1)
	rej := make(chan *proto.Reject, 1)
	v := New(in, out, rej, map[string][]uint32{"user": {kinds.KindMsgSend}})
	go v.Run(context.Background())
	in <- mkf("user", kinds.KindMsgSend)
	select {
	case f := <-out:
		if f.Kind != kinds.KindMsgSend {
			t.Fatal("wrong frame")
		}
	case <-time.After(time.Second):
		t.Fatal("no frame out")
	}
}

func TestRejectsUnknownLayer(t *testing.T) {
	in := make(chan *proto.Frame, 1)
	out := make(chan *proto.Frame, 1)
	rej := make(chan *proto.Reject, 1)
	v := New(in, out, rej, map[string][]uint32{"user": {kinds.KindMsgSend}})
	go v.Run(context.Background())
	in <- mkf("ghost", kinds.KindMsgSend)
	select {
	case r := <-rej:
		if r.Code != CodeUnknownLayer {
			t.Fatalf("code=%d want %d", r.Code, CodeUnknownLayer)
		}
	case <-time.After(time.Second):
		t.Fatal("no reject")
	}
	select {
	case <-out:
		t.Fatal("rejected frame should be dropped")
	default:
	}
}

func TestRejectsKindOutsideCaps(t *testing.T) {
	in := make(chan *proto.Frame, 1)
	out := make(chan *proto.Frame, 1)
	rej := make(chan *proto.Reject, 1)
	v := New(in, out, rej, map[string][]uint32{"user": {kinds.KindMsgSend}})
	go v.Run(context.Background())
	in <- mkf("user", kinds.KindAdminBroadcast)
	select {
	case r := <-rej:
		if r.Code != CodeNoCapability {
			t.Fatalf("code=%d want %d", r.Code, CodeNoCapability)
		}
	case <-time.After(time.Second):
		t.Fatal("no reject")
	}
}
