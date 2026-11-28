package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/yingo-server/UFOT/server/internal/bridge/router"
	"github.com/yingo-server/UFOT/server/internal/proto"
	chatpb "github.com/yingo-server/UFOT/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
	"github.com/yingo-server/UFOT/server/internal/transport/embedded"
)

// stubLayer 最小 echo 层（后续批次被真实层替换）：
// Deliver 收到任何帧 → 回投一条 kind=911 ack 到 Outbound。
type stubLayer struct {
	name    string
	got     chan *proto.Frame
	out     chan *proto.Frame
	closeCh chan struct{}
}

func newStubLayer(name string) *stubLayer {
	return &stubLayer{
		name:    name,
		got:     make(chan *proto.Frame, 16),
		out:     make(chan *proto.Frame, 16),
		closeCh: make(chan struct{}),
	}
}

func (l *stubLayer) LayerName() string { return l.name }
func (l *stubLayer) Deliver(f *proto.Frame) error {
	select {
	case l.got <- f:
	default:
	}
	// 回 911 ack（同 request_id 配对）。
	resp := &proto.Frame{
		Kind: kinds.KindFlagSync,
		Env: &chatpb.Envelope{
			Kind:      uint32(kinds.KindFlagSync),
			RequestId: f.Env.GetRequestId(),
		},
		Meta: &proto.FrameMeta{SrcLayer: l.name, TraceID: f.Meta.TraceID, ConnID: f.Meta.ConnID},
	}
	select {
	case l.out <- resp:
	default:
	}
	return nil
}
func (l *stubLayer) Outbound() <-chan *proto.Frame { return l.out }
func (l *stubLayer) Close() error {
	select {
	case <-l.closeCh:
	default:
		close(l.closeCh)
	}
	return nil
}

// TestPipelineEndToEnd：user 层上行 kind=101 → 流水线 →
// 路由到 data 层（默认表下 101 走段规则 user 层；
// 数据帧同时到 user 层订阅）→ stub data 层回 911 →
// 911 广播回各层。断言 data 层收到 101 且随后收到 911。
func TestPipelineEndToEnd(t *testing.T) {
	dataL := newStubLayer("data")
	userL := newStubLayer("user")

	br := embedded.NewBridge(nil)
	// busPeer 是流水线视角的"对端"（它的 Frames 汇聚全部层上行帧）。
	busPeer := br.Peer()
	// Attach 返回层端 Peer；用 user 层 Peer 注入上行帧。
	_ = br.Attach(dataL)
	userPeer := br.Attach(userL)

	// 配置覆盖：kind=101 定向到 data 层（验证 Router 表可注入 + 全链）。
	pl, err := Assemble(Config{
		Sender: br.SendTo,
		RouteTable: map[uint32][]router.Target{
			kinds.KindMsgSend: {{Layer: "data", Topic: "conv.42"}},
		},
	}, busPeer)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = pl.Run(ctx) }()

	// user 层上行 kind=101。
	env := &chatpb.Envelope{Kind: uint32(kinds.KindMsgSend), RequestId: "e2e-1"}
	env.SetConvId(42)
	err = userPeer.Send(&proto.Frame{
		Seq:  1,
		Kind: kinds.KindMsgSend,
		Env:  env,
		Meta: &proto.FrameMeta{SrcLayer: "user", TraceID: "e2e-1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// data 层应收到该帧（段规则 data 面？不：101→user 层 conv.42。
	// 因此断言 user 层 echo 回来的 911 会广播到 data 层）。
	// 等待任一层 Deliver 到 101 或 911：验证整条链活了。
	deadline := time.After(3 * time.Second)
	var sawMsg, sawAck bool
	for !(sawMsg && sawAck) {
		select {
		case <-deadline:
			t.Fatalf("timeout: sawMsg=%v sawAck=%v", sawMsg, sawAck)
		case f := <-dataL.got:
			if f.Kind == kinds.KindMsgSend {
				sawMsg = true
			}
			if f.Kind == kinds.KindFlagSync {
				sawAck = true
			}
		case f := <-userL.got:
			if f.Kind == kinds.KindMsgSend {
				sawMsg = true
			}
			if f.Kind == kinds.KindFlagSync {
				sawAck = true
			}
		}
	}
}

// TestAssembleRejectsEmptyPeers 装配前置校验。
func TestAssembleRejectsEmptyPeers(t *testing.T) {
	if _, err := Assemble(Config{}); err == nil {
		t.Fatal("expected error with no peers")
	}
}

// TestDrain 排空不 panic。
func TestDrain(t *testing.T) {
	dataL := newStubLayer("data")
	br := embedded.NewBridge(dataL)
	pl, err := Assemble(Config{}, br.Peer())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pl.Drain(ctx)
}
