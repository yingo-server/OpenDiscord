package bus

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yingo-server/UFOT/server/internal/proto"
	chatpb "github.com/yingo-server/UFOT/server/internal/proto/gen/chatpb"
)

func frame(kind uint32) *proto.Frame {
	return &proto.Frame{Kind: kind, Env: &chatpb.Envelope{Kind: kind}}
}

func TestExactSubscribe(t *testing.T) {
	b := New()
	ch, unsub := b.Sub("conv.123", "user")
	defer unsub()
	b.Pub("conv.123", frame(101))
	b.Pub("conv.999", frame(101))
	got := <-ch
	if got.Kind != 101 {
		t.Fatalf("want kind 101, got %d", got.Kind)
	}
	// conv.999 不应投给 conv.123 订阅者
	select {
	case f := <-ch:
		t.Fatalf("unexpected frame kind=%d", f.Kind)
	default:
	}
}

func TestWildcardSubscribe(t *testing.T) {
	b := New()
	sys, _ := b.Sub("sys.>", "exporter")
	conv, _ := b.Sub("conv.>", "user")
	b.Pub("sys.flags", frame(911))
	b.Pub("conv.7", frame(101))
	if k := <-sys; k.Kind != 911 {
		t.Fatalf("sys subscriber want 911 got %d", k.Kind)
	}
	if k := <-conv; k.Kind != 101 {
		t.Fatalf("conv subscriber want 101 got %d", k.Kind)
	}
	select {
	case f := <-sys:
		t.Fatalf("sys should not get conv topic, got %d", f.Kind)
	default:
	}
}

func TestUnsubscribe(t *testing.T) {
	b := New()
	ch, unsub := b.Sub("user.456", "user")
	unsub()
	b.Pub("user.456", frame(101))
	if s := b.Stats(); s.Subs != 0 {
		t.Fatalf("subs after unsub = %d", s.Subs)
	}
	select {
	case f := <-ch:
		_ = f
	default:
	}
}

func TestSlowConsumerDrop(t *testing.T) {
	b := New()
	ch, _ := b.Sub("conv.1", "user")
	_ = ch
	for i := 0; i < ChanCap+50; i++ {
		b.Pub("conv.1", frame(101))
	}
	st := b.Stats()
	if st.DroppedSlow < 50 {
		t.Fatalf("expected >=50 drops, got %d", st.DroppedSlow)
	}
	if st.Delivered != ChanCap {
		t.Fatalf("expected %d delivered, got %d", ChanCap, st.Delivered)
	}
}

func TestConcurrentPubSub(t *testing.T) {
	b := New()
	const workers, per = 8, 200
	var received int64
	var wg sync.WaitGroup
	ch, unsub := b.Sub("conv.42", "user")
	defer unsub()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range ch {
			atomic.AddInt64(&received, 1)
		}
	}()
	var pwg sync.WaitGroup
	for i := 0; i < workers; i++ {
		pwg.Add(1)
		go func() {
			defer pwg.Done()
			for j := 0; j < per; j++ {
				b.Pub("conv.42", frame(101))
			}
		}()
	}
	pwg.Wait()
	// 非阻塞投递允许慢消费者 drop：最终收到的必须等于 Delivered 计数。
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&received) != int64(b.Stats().Delivered) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := atomic.LoadInt64(&received); got != int64(b.Stats().Delivered) {
		t.Fatalf("received=%d delivered=%d", got, b.Stats().Delivered)
	}
	if s := b.Stats(); s.Published != workers*per {
		t.Fatalf("published=%d want=%d", s.Published, workers*per)
	}
}
