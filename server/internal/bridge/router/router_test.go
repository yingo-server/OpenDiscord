package router

import (
	"fmt"
	"testing"

	chatpb "github.com/yingo-server/UFOT/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
)

func env(kind uint32, cid, uid uint64) *chatpb.Envelope {
	e := &chatpb.Envelope{Kind: kind}
	if cid != 0 {
		e.SetConvId(cid)
	}
	if uid != 0 {
		e.SetUserId(uid)
	}
	return e
}

func TestConvRoute(t *testing.T) {
	r := New(nil)
	ts := r.Route(kinds.KindMsgSend, env(kinds.KindMsgSend, 123, 0))
	if len(ts) != 1 || ts[0].Layer != "user" || ts[0].Topic != "conv.123" {
		t.Fatalf("got %v", ts)
	}
}

func TestUserRoute(t *testing.T) {
	r := New(nil)
	ts := r.Route(kinds.KindProfileUpdate, env(kinds.KindProfileUpdate, 0, 456))
	if len(ts) != 1 || ts[0].Topic != "user.456" {
		t.Fatalf("got %v", ts)
	}
}

func TestOneofOnlyOneTarget(t *testing.T) {
	// proto oneof：conv_id 与 user_id 互斥，不能同时路由两人。
	r := New(nil)
	e := &chatpb.Envelope{Kind: kinds.KindMsgSend}
	e.SetConvId(1)
	ts := r.Route(kinds.KindMsgSend, e)
	if len(ts) != 1 || ts[0].Topic != "conv.1" {
		t.Fatalf("got %v", ts)
	}
}

// TestAllKindSegments 全 kind 段路由表断言。
func TestAllKindSegments(t *testing.T) {
	r := New(nil)
	cases := []struct {
		name  string
		kind  uint32
		env   *chatpb.Envelope
		check func([]Target) error
	}{
		{"0-99 system not routed", kinds.KindPing, nil, empty()},
		{"100-199 message → user/conv", kinds.KindMsgNew, env(kinds.KindMsgNew, 7, 0), want("user", "conv.7")},
		{"200-299 conv → user", kinds.KindTyping, env(kinds.KindTyping, 9, 0), want("user", "conv.9")},
		{"300-349 media → user", kinds.KindMediaReady, env(kinds.KindMediaReady, 5, 0), want("user", "conv.5")},
		{"400-449 presence → user", kinds.KindPresenceUpdate, env(kinds.KindPresenceUpdate, 0, 3), want("user", "user.3")},
		{"450-499 user → user", kinds.KindBlock, env(kinds.KindBlock, 0, 8), want("user", "user.8")},
		{"500-599 admin → admin+user broadcast", kinds.KindAdminBroadcast, nil, two(
			want("admin", "sys.admin"), want("user", "sys.broadcast"))},
		{"900-909 internal control not routed", kinds.KindInternalHello, nil, empty()},
		{"910-919 internal system → data", kinds.KindShutdownNotice, env(kinds.KindShutdownNotice, 0, 0), want("data", "data.910")},
		{"920-929 read → data", kinds.KindReadHistory, env(kinds.KindReadHistory, 0, 0), want("data", "data.920")},
		{"930-939 convdata → data", kinds.KindConvDataRequest, nil, want("data", "data.930")},
		{"940-959 identity → data", kinds.KindIdentityRequest, nil, want("data", "data.940")},
		{"960-969 security → data", kinds.KindSecurityRequest, nil, want("data", "data.960")},
		{"970-979 flags → data", kinds.KindFlagGet, nil, want("data", "data.970")},
		{"980-989 jobs → data", kinds.KindJobTrigger, nil, want("data", "data.980")},
		{"990-999 diag → no route (out of data segment 910-989)", kinds.KindDiagLoopback, nil, empty()},
		{"600 reserved → no route", 600, nil, empty()},
	}
	for _, c := range cases {
		if err := c.check(r.Route(c.kind, c.env)); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func TestDefaultTableOverridesSegments(t *testing.T) {
	r := New(DefaultTable())
	// 默认表 910 有广播规则（3 目标），覆盖段规则的 data.910。
	ts := r.Route(kinds.KindShutdownNotice, env(kinds.KindShutdownNotice, 0, 0))
	if len(ts) != 3 {
		t.Fatalf("want 3 targets, got %v", ts)
	}
	// 非默认表 kind 走段规则。
	ts = r.Route(kinds.KindReadHistory, env(kinds.KindReadHistory, 0, 0))
	if len(ts) != 1 || ts[0].Topic != "data.920" {
		t.Fatalf("got %v", ts)
	}
}

func TestCustomTableInject(t *testing.T) {
	r := New(map[uint32][]Target{kinds.KindMsgSend: {{Layer: "data", Topic: "data.x"}}})
	ts := r.Route(kinds.KindMsgSend, env(kinds.KindMsgSend, 123, 0))
	if len(ts) != 1 || ts[0].Topic != "data.x" {
		t.Fatalf("got %v", ts)
	}
}

func TestSetAndTableSnapshot(t *testing.T) {
	r := New(nil)
	r.Set(kinds.KindPing, []Target{{Layer: "admin", Topic: "sys.admin"}})
	if tb := r.Table(); len(tb[kinds.KindPing]) != 1 {
		t.Fatalf("table snapshot: %v", tb)
	}
}

// ---- 断言辅助 ----
func empty() func([]Target) error {
	return func(ts []Target) error {
		if len(ts) != 0 {
			return fmt.Errorf("want no route, got %v", ts)
		}
		return nil
	}
}
func want(layer, topic string) func([]Target) error {
	return func(ts []Target) error {
		for _, x := range ts {
			if x.Layer == layer && x.Topic == topic {
				return nil
			}
		}
		return fmt.Errorf("missing target %s/%s in %v", layer, topic, ts)
	}
}
func two(a, b func([]Target) error) func([]Target) error {
	return func(ts []Target) error {
		if err := a(ts); err != nil {
			return err
		}
		return b(ts)
	}
}
