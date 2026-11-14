package router

import (
	"sync"
	"testing"
	"time"

	"github.com/yingo-server/OpenDiscord/server/internal/pkg/featureflags"
	"github.com/yingo-server/OpenDiscord/server/internal/proto"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/kinds"
	"github.com/yingo-server/OpenDiscord/server/internal/transport"
	"github.com/yingo-server/OpenDiscord/server/internal/user/core/policy"
	"github.com/yingo-server/OpenDiscord/server/internal/user/core/session"
	"github.com/yingo-server/OpenDiscord/server/internal/user/net/bridge"
	"github.com/yingo-server/OpenDiscord/server/internal/user/net/frame"
	"github.com/yingo-server/OpenDiscord/server/internal/user/net/limit"
)

// ---------- fakes ----------

type fakeConn struct {
	id     uint64
	uid    int64
	device string
	mu     sync.Mutex
	got    [][]byte
	closed bool
}

func (c *fakeConn) ID() uint64       { return c.id }
func (c *fakeConn) UserID() int64    { return c.uid }
func (c *fakeConn) DeviceID() string { return c.device }
func (c *fakeConn) Write(b []byte) bool {
	c.mu.Lock()
	c.got = append(c.got, append([]byte(nil), b...))
	c.mu.Unlock()
	return true
}
func (c *fakeConn) Close() error { c.mu.Lock(); c.closed = true; c.mu.Unlock(); return nil }
func (c *fakeConn) frames() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.got...)
}
func (c *fakeConn) isClosed() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.closed }

type fakeSession struct {
	mu    sync.Mutex
	conns map[uint64]*fakeConn
}

func newFakeSession() *fakeSession     { return &fakeSession{conns: map[uint64]*fakeConn{}} }
func (s *fakeSession) add(c *fakeConn) { s.mu.Lock(); s.conns[c.id] = c; s.mu.Unlock() }
func (s *fakeSession) Lookup(id uint64) session.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.conns[id]; ok {
		return c
	}
	return nil
}
func (s *fakeSession) GetByUser(uid int64) []session.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []session.Conn
	for _, c := range s.conns {
		if c.uid == uid {
			out = append(out, c)
		}
	}
	return out
}
func (s *fakeSession) Broadcast(uid int64, b []byte) int {
	n := 0
	for _, c := range s.GetByUser(uid) {
		if c.Write(b) {
			n++
		}
	}
	return n
}
func (s *fakeSession) All() []session.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []session.Conn
	for _, c := range s.conns {
		out = append(out, c)
	}
	return out
}

// fakePeer 内存桥接 Peer。
type fakePeer struct {
	sent   chan *proto.Frame
	frames chan *proto.Frame
	closed chan struct{}
	once   sync.Once
}

func newFakePeer() *fakePeer {
	return &fakePeer{sent: make(chan *proto.Frame, 100), frames: make(chan *proto.Frame, 100), closed: make(chan struct{})}
}
func (p *fakePeer) Send(f *proto.Frame) error   { p.sent <- f; return nil }
func (p *fakePeer) Frames() <-chan *proto.Frame { return p.frames }
func (p *fakePeer) Close() error                { p.once.Do(func() { close(p.closed) }); return nil }
func (p *fakePeer) Layer() string               { return "bridge" }

var _ transport.Peer = (*fakePeer)(nil)

func newTestRouter(t *testing.T) (*Router, *fakeSession, *fakePeer) {
	t.Helper()
	sess := newFakeSession()
	cfg := policy.Default()
	cfg.Limits = limit.DefaultConfig()
	// 放大规则，避免测试被限流干扰。
	for k, r := range cfg.Limits.Rules {
		r.Rate, r.Burst, r.Capacity = 1e9, 1e9, 1e9
		cfg.Limits.Rules[k] = r
	}
	pol := policy.New(cfg)
	lim := limit.New(cfg.Limits)
	peer := newFakePeer()
	fwd := bridge.NewForwarder(peer, nil, 100)
	return New(sess, lim, pol, fwd, newOffFlags(), nil), sess, peer
}

// newOffFlags 显式 off 快照（§F.2）：不依赖 Defaults()——1.4 转生产后
// Defaults() 已将 universal_pull / adaptive_control / v3_bus / msg_domain_v2
// 默认开启，需要 off 基线的测试必须显式构造。
func newOffFlags() *featureflags.Flags {
	return featureflags.NewFromMap(map[string]any{
		featureflags.KeyUniversalPullEnabled:   false,
		featureflags.KeyAdaptiveControlEnabled: false,
		featureflags.KeyV3BusEnabled:           false,
		featureflags.KeyV3BusPercent:           0,
		featureflags.KeyMsgDomainV2:            false,
	})
}

// ---------- upstream ----------

// TestUpstreamPingLocal 白名单：PING → 本地回 PONG，不转发。
func TestUpstreamPingLocal(t *testing.T) {
	r, sess, peer := newTestRouter(t)
	c := &fakeConn{id: 1, uid: 7, device: "d1"}
	sess.add(c)
	ping, _ := (&chatpb.Ping{ClientMs: 123}).Marshal()
	r.HandleUpstream(&UpEvent{Conn: c, Env: &chatpb.Envelope{Kind: kinds.KindPing, Payload: ping}})

	frames := c.frames()
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1 PONG", len(frames))
	}
	env, err := frame.Decode(frames[0])
	if err != nil || env.Kind != kinds.KindPong {
		t.Fatalf("reply = %+v, %v", env, err)
	}
	var pong chatpb.Pong
	_ = pong.Unmarshal(env.Payload)
	if pong.ClientMs != 123 || pong.ServerMs == 0 {
		t.Fatalf("pong = %+v", pong)
	}
	select {
	case f := <-peer.sent:
		t.Fatalf("PING must not be forwarded, got kind=%d", f.Kind)
	default:
	}
}

// TestUpstreamAuthLocal AUTH → AUTH_RESULT ok（upgrade 前已鉴权）。
func TestUpstreamAuthLocal(t *testing.T) {
	r, sess, _ := newTestRouter(t)
	c := &fakeConn{id: 2, uid: 9}
	sess.add(c)
	r.HandleUpstream(&UpEvent{Conn: c, Env: &chatpb.Envelope{Kind: kinds.KindAuth}})
	frames := c.frames()
	if len(frames) != 1 {
		t.Fatalf("got %d frames", len(frames))
	}
	env, _ := frame.Decode(frames[0])
	if env.Kind != kinds.KindAuthResult {
		t.Fatalf("kind = %d", env.Kind)
	}
	var res chatpb.AuthResult
	_ = res.Unmarshal(env.Payload)
	if !res.Ok || res.UserId != 9 {
		t.Fatalf("auth result = %+v", res)
	}
}

// TestUpstreamForward MSG_SEND → 内部帧进 forwarder（src_layer=user/conn_id/trace_id）。
func TestUpstreamForward(t *testing.T) {
	r, sess, _ := newTestRouter(t)
	c := &fakeConn{id: 3, uid: 11}
	sess.add(c)
	payload, _ := (&chatpb.MessagePayload{Text: "hi", ClientMsgId: "m1"}).Marshal()
	env := &chatpb.Envelope{Kind: kinds.KindMsgSend, RequestId: "trace-x", Target: &chatpb.Envelope_ConvId{ConvId: 55}, Payload: payload}
	r.HandleUpstream(&UpEvent{Conn: c, Env: env})

	select {
	case fr := <-r.fwd.Upstream:
		intf, err := frame.UnmarshalInternal(fr.Body)
		if err != nil {
			t.Fatalf("unmarshal internal: %v", err)
		}
		if intf.SrcLayer != "user" || intf.ConnId != 3 || intf.TraceId != "trace-x" {
			t.Fatalf("internal header = %+v", intf)
		}
		if intf.Envelope == nil || intf.Envelope.Kind != kinds.KindMsgSend || intf.Envelope.GetConvId() != 55 {
			t.Fatalf("inner envelope = %+v", intf.Envelope)
		}
	default:
		t.Fatal("MSG_SEND not forwarded")
	}
}

// ---------- downstream ----------

// TestDownstreamByConnID 下行按 conn_id 定位投递。
func TestDownstreamByConnID(t *testing.T) {
	r, sess, _ := newTestRouter(t)
	c := &fakeConn{id: 42, uid: 1}
	sess.add(c)
	inner := &chatpb.Envelope{Kind: kinds.KindMsgNew, Target: &chatpb.Envelope_ConvId{ConvId: 5}, Payload: []byte("new")}
	body, _ := frame.MarshalInternal(frame.Wrap(inner, "t", 42))
	r.HandleDownstream(&proto.Frame{Kind: kinds.KindMsgNew, Env: &chatpb.Envelope{Kind: kinds.KindMsgNew, Payload: body}, Meta: &proto.FrameMeta{ConnID: 42}})
	frames := c.frames()
	if len(frames) != 1 {
		t.Fatalf("got %d frames", len(frames))
	}
	env, _ := frame.Decode(frames[0])
	if env.Kind != kinds.KindMsgNew {
		t.Fatalf("kind = %d", env.Kind)
	}
}

// TestDownstreamByUserID conn 不匹配时按 user_id 广播。
func TestDownstreamByUserID(t *testing.T) {
	r, sess, _ := newTestRouter(t)
	c1 := &fakeConn{id: 1, uid: 77}
	c2 := &fakeConn{id: 2, uid: 77}
	sess.add(c1)
	sess.add(c2)
	inner := &chatpb.Envelope{Kind: kinds.KindMsgNew, Target: &chatpb.Envelope_UserId{UserId: 77}}
	body, _ := frame.MarshalInternal(frame.Wrap(inner, "t", 999)) // 过期 conn id
	r.HandleDownstream(&proto.Frame{Kind: kinds.KindMsgNew, Env: &chatpb.Envelope{Kind: kinds.KindMsgNew, Payload: body}, Meta: &proto.FrameMeta{ConnID: 999}})
	if len(c1.frames()) != 1 || len(c2.frames()) != 1 {
		t.Fatalf("broadcast = %d/%d, want 1/1", len(c1.frames()), len(c2.frames()))
	}
}

// TestDownstreamKicked KICKED：写 Kicked 帧并断连。
func TestDownstreamKicked(t *testing.T) {
	r, sess, _ := newTestRouter(t)
	var kicked []int64
	r.OnKick = func(uid int64, reason string) { kicked = append(kicked, uid) }
	c1 := &fakeConn{id: 1, uid: 55}
	c2 := &fakeConn{id: 2, uid: 55}
	sess.add(c1)
	sess.add(c2)
	payload, _ := (&chatpb.Kicked{Reason: "BANNED"}).Marshal()
	inner := &chatpb.Envelope{Kind: kinds.KindKicked, Target: &chatpb.Envelope_UserId{UserId: 55}, Payload: payload}
	body, _ := frame.MarshalInternal(frame.Wrap(inner, "t", 0))
	r.HandleDownstream(&proto.Frame{Kind: kinds.KindKicked, Env: &chatpb.Envelope{Kind: kinds.KindKicked, Payload: body}})

	for _, c := range []*fakeConn{c1, c2} {
		frames := c.frames()
		if len(frames) != 1 {
			t.Fatalf("conn %d got %d frames", c.id, len(frames))
		}
		env, _ := frame.Decode(frames[0])
		if env.Kind != kinds.KindKicked {
			t.Fatalf("kind = %d", env.Kind)
		}
		if !c.isClosed() {
			t.Fatalf("conn %d not closed", c.id)
		}
	}
	if len(kicked) != 1 || kicked[0] != 55 {
		t.Fatalf("OnKick = %v", kicked)
	}
}

// TestRateLimitThrottle 超限时回 RATE_LIMITED。
func TestRateLimitThrottle(t *testing.T) {
	sess := newFakeSession()
	cfg := policy.Default()
	cfg.Limits = limit.DefaultConfig()
	cfg.Limits.Rules = map[uint32]limit.Rule{kinds.KindMsgSend: {Rate: 0.001, Burst: 1, Capacity: 1}}
	cfg.Limits.DefaultRule = limit.Rule{Rate: 0.001, Burst: 1, Capacity: 1}
	pol := policy.New(cfg)
	lim := limit.New(cfg.Limits)
	fwd := bridge.NewForwarder(newFakePeer(), nil, 10)
	r := New(sess, lim, pol, fwd, newOffFlags(), nil)
	c := &fakeConn{id: 1, uid: 2}
	sess.add(c)
	env := &chatpb.Envelope{Kind: kinds.KindMsgSend}
	r.HandleUpstream(&UpEvent{Conn: c, Env: env})
	r.HandleUpstream(&UpEvent{Conn: c, Env: env})
	frames := c.frames()
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1 RATE_LIMITED", len(frames))
	}
	e, _ := frame.Decode(frames[0])
	if e.Kind != kinds.KindRateLimited {
		t.Fatalf("kind = %d", e.Kind)
	}
	_ = time.Now()
}
