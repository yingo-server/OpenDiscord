package egress

import (
	"context"
	"testing"
	"time"

	"github.com/yingo-server/UFOT/server/internal/pipedef"
	"github.com/yingo-server/UFOT/server/internal/proto"
	chatpb "github.com/yingo-server/UFOT/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
)

func target() pipedef.Target { return pipedef.Target{Layer: "data", Topic: "data.101"} }

func mk(kind uint32) *proto.Frame {
	e := &chatpb.Envelope{Kind: kind}
	e.SetConvId(1)
	return &proto.Frame{Seq: 1, Kind: kind, Env: e}
}

// ---- queue 直测（控制水位用小队列） ----

func TestNeverDropSurvivesFullQueue(t *testing.T) {
	q := newQueue("data", Config{Cap: 4, Soft: 0.6, Firm: 0.85})
	for i := 0; i < 4; i++ {
		if err := q.enqueue(mk(kinds.KindInternalHeartbeat)); err != nil {
			t.Fatalf("fill never-drop: %v", err)
		}
	}
	// 心跳在满队列上仍应入队成功？容量 4 已满 → 判死错误。
	if err := q.enqueue(mk(kinds.KindInternalHeartbeat)); err == nil {
		t.Fatal("expected overflow error on totally full queue for never-drop frame")
	}
	// 未满时不可丢帧必须成功。
	q2 := newQueue("data", Config{Cap: 100, Soft: 0.6, Firm: 0.85})
	for i := 0; i < 90; i++ { // 超 firm(85)
		_ = q2.enqueue(mk(kinds.KindInternalHeartbeat))
	}
	if q2.nacked != 0 || q2.dropped != 0 {
		t.Fatalf("never-drop frames must not be dropped/nacked: dropped=%d nacked=%d", q2.dropped, q2.nacked)
	}
}

func TestDropFirstAtSoftWatermark(t *testing.T) {
	q := newQueue("user", Config{Cap: 100, Soft: 0.6, Firm: 0.85})
	for i := 0; i < 60; i++ {
		_ = q.enqueue(mk(kinds.KindMsgSend))
	}
	if err := q.enqueue(mk(kinds.KindTyping)); err != nil {
		t.Fatalf("typing should be silently dropped, got err %v", err)
	}
	if q.dropped != 1 {
		t.Fatalf("dropped=%d want 1", q.dropped)
	}
	if len(q.pending) != 60 {
		t.Fatalf("queue len=%d want 60 (typing not enqueued)", len(q.pending))
	}
}

func TestMessageFrameNACKAtFirm(t *testing.T) {
	rej := make(chan *proto.Reject, 8)
	q := newQueue("data", Config{Cap: 100, Soft: 0.6, Firm: 0.85, Rejects: rej})
	for i := 0; i < 85; i++ {
		if err := q.enqueue(mk(kinds.KindMsgSend)); err != nil {
			t.Fatalf("fill: %v", err)
		}
	}
	if err := q.enqueue(mk(kinds.KindMsgSend)); err == nil {
		t.Fatal("expected NACK overflow error at firm watermark")
	}
	select {
	case r := <-rej:
		if r.Kind != kinds.KindMsgSend {
			t.Fatalf("reject kind=%d", r.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("no NACK on rejects chan")
	}
	if q.nacked != 1 {
		t.Fatalf("nacked=%d want 1", q.nacked)
	}
}

// ---- Egress 公开 API ----

func TestEgressDelivers(t *testing.T) {
	e := New(Config{Cap: 64})
	defer e.Drain()
	if err := e.Enqueue(target(), mk(kinds.KindMsgSend)); err != nil {
		t.Fatal(err)
	}
	select {
	case tf := <-e.Out():
		if tf.T.Layer != "data" || tf.F.Kind != kinds.KindMsgSend {
			t.Fatalf("bad delivery %+v", tf)
		}
	case <-time.After(time.Second):
		t.Fatal("no delivery")
	}
}

func TestEgressNACKPath(t *testing.T) {
	rej := make(chan *proto.Reject, 8)
	e := New(Config{Cap: 10, Soft: 0.6, Firm: 0.85, Rejects: rej})
	defer e.Drain()
	tg := target()
	var lastErr error
	for i := 0; i < 20; i++ {
		if err := e.Enqueue(tg, mk(kinds.KindMsgSend)); err != nil {
			lastErr = err
		}
	}
	if lastErr == nil {
		t.Fatal("expected overflow error eventually")
	}
	select {
	case <-rej:
	case <-time.After(time.Second):
		t.Fatal("NACK not emitted")
	}
}

func TestStats(t *testing.T) {
	e := New(Config{Cap: 100, Soft: 0.6, Firm: 0.85})
	defer e.Drain()
	_ = e.Enqueue(target(), mk(kinds.KindMsgSend))
	sts := e.Stats()
	if len(sts) != 1 || sts[0].Layer != "data" || sts[0].Cap != 100 {
		t.Fatalf("stats=%+v", sts)
	}
}

// Drain 排空出向流（测试辅助暴露在实现里，供 pipeline 复用）。
func TestDrainHelper(t *testing.T) {
	e := New(Config{Cap: 16})
	_ = e.Enqueue(target(), mk(kinds.KindMsgSend))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	e.DrainWithContext(ctx)
}
