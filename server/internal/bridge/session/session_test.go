package session

import (
	"testing"
	"time"

	"github.com/yingo-server/UFOT/server/internal/proto"
	chatpb "github.com/yingo-server/UFOT/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
	"github.com/yingo-server/UFOT/server/internal/transport"
)

type fakePeer struct{ layer string }

func (p *fakePeer) Send(f *proto.Frame) error   { return nil }
func (p *fakePeer) Frames() <-chan *proto.Frame { return nil }
func (p *fakePeer) Close() error                { return nil }
func (p *fakePeer) Layer() string               { return p.layer }

func TestRegisterUnregister(t *testing.T) {
	m := New()
	id := m.Register(&fakePeer{"user"}, "user", []kinds.Kind{kinds.KindMsgSend})
	if m.Count() != 1 {
		t.Fatal("count want 1")
	}
	s, ok := m.Get(id)
	if !ok || s.Layer != "user" || len(s.Caps) != 1 {
		t.Fatalf("session=%+v ok=%v", s, ok)
	}
	m.Unregister(id)
	if m.Count() != 0 {
		t.Fatal("count want 0")
	}
}

func TestControlPlaneBypass(t *testing.T) {
	m := New()
	id := m.Register(&fakePeer{"user"}, "user", nil)
	for _, k := range []uint32{kinds.KindInternalHello, kinds.KindInternalHeartbeat, kinds.KindInternalFrameAck} {
		f := &proto.Frame{Kind: k, Env: &chatpb.Envelope{Kind: k}}
		if !m.ConsumeControl(id, f) {
			t.Fatalf("kind %d should be consumed by control plane", k)
		}
	}
	// 数据面帧不得旁路——返回 false。
	if m.ConsumeControl(id, &proto.Frame{Kind: 101, Env: &chatpb.Envelope{Kind: 101}}) {
		t.Fatal("data-plane frame must not be consumed by session")
	}
}

func TestSubscribeAndLeaseExpiry(t *testing.T) {
	m := New()
	base := time.Now()
	cur := base
	m.SetClock(func() time.Time { return cur })
	var topics []string
	m.OnSubscribe = func(id uint64, topic string) { topics = append(topics, topic) }
	m.OnUnsubscribe = func(id uint64, topic string) { topics = append(topics, "-"+topic) }
	m.Register(&fakePeer{"user"}, "user", nil)
	m.ConsumeControl(1, &proto.Frame{Kind: kinds.KindInternalSubscribe,
		Env: &chatpb.Envelope{Kind: kinds.KindInternalSubscribe, RequestId: "conv.1"}})
	if len(topics) != 1 || topics[0] != "conv.1" {
		t.Fatalf("topics=%v", topics)
	}
	// TTL 过期。
	cur = cur.Add(DefaultSubTTL + time.Second)
	m.Sweep()
	if len(topics) != 2 || topics[1] != "-conv.1" {
		t.Fatalf("unsub=%v", topics)
	}
}

func TestHeartbeatDeath(t *testing.T) {
	m := New()
	base := time.Now()
	cur := base
	m.SetClock(func() time.Time { return cur })
	dead := 0
	m.OnDead = func(id uint64) { dead++ }
	m.Register(&fakePeer{"data"}, "data", nil)
	m.Sweep() // 未超时。
	if dead != 0 {
		t.Fatal("premature death")
	}
	cur = cur.Add(DefaultHeartbeatTTL + time.Second)
	if m.Sweep() != 1 || dead != 1 {
		t.Fatalf("dead=%d sweep", dead)
	}
	if m.Count() != 0 {
		t.Fatal("dead session must be removed")
	}
}

func TestUnregisterFiresUnsub(t *testing.T) {
	m := New()
	cur := time.Now()
	m.SetClock(func() time.Time { return cur })
	var unsubbed []string
	m.OnUnsubscribe = func(id uint64, topic string) { unsubbed = append(unsubbed, topic) }
	id := m.Register(&fakePeer{"user"}, "user", nil)
	m.ConsumeControl(id, &proto.Frame{Kind: kinds.KindInternalSubscribe,
		Env: &chatpb.Envelope{Kind: kinds.KindInternalSubscribe, RequestId: "sys.>"}})
	m.Unregister(id)
	if len(unsubbed) != 1 || unsubbed[0] != "sys.>" {
		t.Fatalf("unsubbed=%v", unsubbed)
	}
}

var _ transport.Peer = (*fakePeer)(nil)
