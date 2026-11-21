package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/yingo-server/UFOT/server/internal/proto"
	"github.com/yingo-server/UFOT/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
	"github.com/yingo-server/UFOT/server/internal/user/core/authn"
	"github.com/yingo-server/UFOT/server/internal/user/net/frame"
	"github.com/yingo-server/UFOT/server/internal/user/net/ws"
)

// ---------- fake bridge peer（内存 chan） ----------

type fakeBridge struct {
	up    chan *proto.Frame // usergw → bridge（上行）
	down  chan *proto.Frame // bridge → usergw（下行）
	close chan struct{}
	once  sync.Once
}

func newFakeBridge() *fakeBridge {
	return &fakeBridge{up: make(chan *proto.Frame, 64), down: make(chan *proto.Frame, 64), close: make(chan struct{})}
}

func (b *fakeBridge) Send(f *proto.Frame) error {
	select {
	case b.up <- f:
		return nil
	case <-b.close:
		return fmt.Errorf("closed")
	}
}

func (b *fakeBridge) Frames() <-chan *proto.Frame { return b.down }
func (b *fakeBridge) Close() error                { b.once.Do(func() { close(b.close) }); return nil }
func (b *fakeBridge) Layer() string               { return "bridge" }

// ---------- helpers ----------

func testAuthn(t *testing.T) (AuthnFunc, *authn.Signer, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer := authn.NewSigner(priv)
	verifier := authn.NewVerifier(pub)
	authnFn := func(token string) (*ws.Identity, error) {
		id, err := verifier.Verify(token)
		if err != nil {
			return nil, err
		}
		return &ws.Identity{UserID: id.UserID, DeviceID: id.DeviceID}, nil
	}
	return authnFn, signer, pub
}

func waitAddr(t *testing.T, s *Server) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if a := s.Addr(); a != "" && a != "127.0.0.1:0" {
			return a
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server did not start listening")
	return ""
}

func readEnvelope(t *testing.T, c *websocket.Conn) *chatpb.Envelope {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	env, err := frame.Decode(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return env
}

func sendEnvelope(t *testing.T, c *websocket.Conn, env *chatpb.Envelope) {
	t.Helper()
	b, err := frame.Encode(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.WriteMessage(websocket.BinaryMessage, b); err != nil {
		t.Fatal(err)
	}
}

// TestEndToEnd 完整链路：HTTP 鉴权 → WS → Ping/Pong → MSG_SEND 到桥接 → 桥接回投 MSG_NEW。
func TestEndToEnd(t *testing.T) {
	authnFn, signer, _ := testAuthn(t)
	bridgePeer := newFakeBridge()
	cfg := DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.EnableRateLimit = false

	srv, err := New(cfg, authnFn, bridgePeer)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	addr := waitAddr(t, srv)

	// --- HTTP 鉴权失败：无 Cookie → 401，不升级 ---
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	_, resp, err := dialer.Dial("ws://"+addr+"/ws", nil)
	if err == nil {
		t.Fatal("dial without token should fail")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("status = %d, want 401", status)
	}

	// --- 正常连接 ---
	token, err := signer.SignAccess(1001, "dev-1", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	hdr := http.Header{}
	hdr.Set("Cookie", "access_token="+token)
	c, resp, err := dialer.Dial("ws://"+addr+"/ws", hdr)
	if err != nil {
		t.Fatalf("dial: %v (resp=%v)", err, resp)
	}
	defer c.Close()

	// --- Ping → Pong ---
	pingPayload, _ := (&chatpb.Ping{ClientMs: 777}).Marshal()
	sendEnvelope(t, c, &chatpb.Envelope{Kind: kinds.KindPing, Payload: pingPayload})
	env := readEnvelope(t, c)
	if env.Kind != kinds.KindPong {
		t.Fatalf("reply kind = %d, want PONG", env.Kind)
	}
	var pong chatpb.Pong
	_ = pong.Unmarshal(env.Payload)
	if pong.ClientMs != 777 {
		t.Fatalf("pong = %+v", pong)
	}

	// --- MSG_SEND → fake bridge 收到 InternalFrame ---
	msgPayload, _ := (&chatpb.MessagePayload{ClientMsgId: "c-1", Text: "hello"}).Marshal()
	sendEnvelope(t, c, &chatpb.Envelope{
		Id: 10, Kind: kinds.KindMsgSend, RequestId: "trace-e2e",
		Target: &chatpb.Envelope_ConvId{ConvId: 66}, Payload: msgPayload,
	})
	var upstream *proto.Frame
	select {
	case upstream = <-bridgePeer.up:
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not receive MSG_SEND")
	}
	// The connect lifecycle now emits a 401 presence event before user
	// traffic; skip it (and only it) so the assertion below sees MSG_SEND.
	if upstream.Kind == kinds.KindPresenceOnline {
		select {
		case upstream = <-bridgePeer.up:
		case <-time.After(3 * time.Second):
			t.Fatal("bridge did not receive MSG_SEND after presence event")
		}
	}
	if upstream.Kind != kinds.KindMsgSend {
		t.Fatalf("bridge kind = %d", upstream.Kind)
	}
	intf, err := frame.UnmarshalInternal(upstream.Env.Payload)
	if err != nil {
		t.Fatalf("internal frame: %v", err)
	}
	if intf.SrcLayer != "user" || intf.TraceId != "trace-e2e" || intf.ConnId == 0 {
		t.Fatalf("internal header = %+v", intf)
	}
	if intf.Envelope == nil || intf.Envelope.GetConvId() != 66 || intf.Envelope.Kind != kinds.KindMsgSend {
		t.Fatalf("inner envelope = %+v", intf.Envelope)
	}

	// --- fake bridge 回投 MSG_NEW → WS 客户端收到 ---
	newEnv := &chatpb.Envelope{
		Kind:    kinds.KindMsgNew,
		Target:  &chatpb.Envelope_UserId{UserId: 1001},
		Payload: []byte("new-message"),
	}
	body, _ := frame.MarshalInternal(frame.Wrap(newEnv, "trace-e2e", intf.ConnId))
	bridgePeer.down <- &proto.Frame{
		Kind: kinds.KindMsgNew,
		Env:  &chatpb.Envelope{Kind: kinds.KindMsgNew, Payload: body},
		Meta: &proto.FrameMeta{ConnID: intf.ConnId},
	}
	got := readEnvelope(t, c)
	if got.Kind != kinds.KindMsgNew {
		t.Fatalf("downstream kind = %d, want MSG_NEW", got.Kind)
	}
	if string(got.Payload) != "new-message" {
		t.Fatalf("downstream payload = %q", got.Payload)
	}
}

// TestEndToEndBadToken 伪造 token → 401。
func TestEndToEndBadToken(t *testing.T) {
	authnFn, _, _ := testAuthn(t)
	bridgePeer := newFakeBridge()
	cfg := DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	srv, err := New(cfg, authnFn, bridgePeer)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	addr := waitAddr(t, srv)

	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	hdr := http.Header{}
	hdr.Set("Cookie", "access_token=not-a-jwt")
	_, resp, err := dialer.Dial("ws://"+addr+"/ws", hdr)
	if err == nil {
		t.Fatal("bad token accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v", resp)
	}
}
