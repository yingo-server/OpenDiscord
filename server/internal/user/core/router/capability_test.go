// capability_test.go —— 旧客户端能力协商的上行解析测试
// （lts-1.4-acceptance §B.1/B.5）。
//
// 覆盖：
//   - HELLO(1) 载荷带 pull_capable=true → 连接注册表记为有能力；
//   - HELLO 载荷带 capabilities=["pull_capable"] → 同样算有能力；
//   - CLIENT_CAPS(18) JSON 载荷 {"pull_capable":true} → 有能力；
//   - 没上报 / 坏载荷 → 无能力（老客户端，必须 inline）。
package router

import (
	"testing"

	"github.com/yingo-server/OpenDiscord/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/kinds"
)

func TestCapabilityReporting(t *testing.T) {
	cases := []struct {
		name string
		kind uint32
		pl   []byte
		want bool
	}{
		{
			name: "hello protobuf pull_capable=true",
			kind: kinds.KindHello,
			pl:   mustHello(t, &chatpb.Hello{ClientVersion: "1.4", PullCapable: true}),
			want: true,
		},
		{
			name: "hello protobuf pull_capable=false",
			kind: kinds.KindHello,
			pl:   mustHello(t, &chatpb.Hello{ClientVersion: "1.2.1"}),
			want: false,
		},
		{
			name: "hello capability token",
			kind: kinds.KindHello,
			pl:   mustHello(t, &chatpb.Hello{Capabilities: []string{"pull_capable"}}),
			want: true,
		},
		{
			name: "client caps json true",
			kind: kinds.KindClientCaps,
			pl:   []byte(`{"pull_capable":true}`),
			want: true,
		},
		{
			name: "client caps json camelCase",
			kind: kinds.KindClientCaps,
			pl:   []byte(`{"pullCapable":true}`),
			want: true,
		},
		{
			name: "client caps json false",
			kind: kinds.KindClientCaps,
			pl:   []byte(`{"pull_capable":false}`),
			want: false,
		},
		{
			name: "client caps garbage",
			kind: kinds.KindClientCaps,
			pl:   []byte{0xff, 0xff, 0xff},
			want: false,
		},
		{
			name: "empty payload",
			kind: kinds.KindClientCaps,
			pl:   nil,
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, sess, _ := newTestRouter(t)
			c := &fakeConn{id: 42, uid: 7, device: "d1"}
			sess.add(c)
			var gotConn uint64
			var gotCap bool
			called := false
			r.CapSink = func(connID uint64, pullCapable bool) {
				called = true
				gotConn, gotCap = connID, pullCapable
			}
			r.HandleUpstream(&UpEvent{Conn: c, Env: &chatpb.Envelope{Kind: tc.kind, Payload: tc.pl}})
			if !called {
				t.Fatalf("capability sink was not called")
			}
			if gotConn != 42 {
				t.Fatalf("capability recorded for conn %d, want 42", gotConn)
			}
			if gotCap != tc.want {
				t.Fatalf("pull_capable = %v, want %v", gotCap, tc.want)
			}
		})
	}
}

// TestCapabilitySinkNilIsNoop CapSink 未注入（未装配）时不得 panic，
// 且不得产生任何下行帧（零行为变化）。
func TestCapabilitySinkNilIsNoop(t *testing.T) {
	r, sess, _ := newTestRouter(t)
	c := &fakeConn{id: 43, uid: 7}
	sess.add(c)
	pl := mustHello(t, &chatpb.Hello{PullCapable: true})
	r.HandleUpstream(&UpEvent{Conn: c, Env: &chatpb.Envelope{Kind: kinds.KindHello, Payload: pl}})
	if got := len(c.frames()); got != 0 {
		t.Fatalf("HELLO must not produce downstream frames, got %d", got)
	}
}

func mustHello(t *testing.T, h *chatpb.Hello) []byte {
	t.Helper()
	b, err := h.Marshal()
	if err != nil {
		t.Fatalf("hello marshal: %v", err)
	}
	return b
}
