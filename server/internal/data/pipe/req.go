package pipe

import (
	"github.com/yingo-server/UFOT/server/internal/proto"
	"github.com/yingo-server/UFOT/server/internal/transport"
)

// req.go — Req view + FrameSource (see req comment in pipe.go).

// Req is the pipe's view of one INBOUND request frame. transport.Peer yields
// *proto.Frame whose Env is the generated chatpb.Envelope; since that
// generated package is absent in this environment (no protoc — see
// wire/wire.go), the assembly layer (or the test harness) converts each frame
// into a Req via a FrameSource before it reaches the pipe. When chatpb lands,
// the FrameSource implementation is the only code that changes.
type Req struct {
	Kind      uint32
	RequestID string
	ConvID    uint64 // target conversation (0 = none)
	UserID    uint64 // actor uid (bridge routing context)
	TraceID   string
	Payload   []byte // verbatim Envelope payload bytes
	Frame     *proto.Frame
}

// FrameSource converts an inbound *proto.Frame into a Req.
// The assembly layer installs the chatpb-backed one at process start; the
// shell default reads only the frame shell (tests).
type FrameSource func(*proto.Frame) Req

// ShellFrameSource is the default FrameSource (kind shell only).
//
// It must carry the payload across: frames that are NOT wrapped in an
// InternalFrame (e.g. the user layer's presence connect/disconnect events)
// reach the pipe as bare envelopes, and dropping their payload made the
// data layer parse an empty body (uid 0) — the event was silently dropped.
func ShellFrameSource(f *proto.Frame) Req {
	r := Req{Kind: f.Kind, Frame: f}
	if f != nil && f.Env != nil {
		r.Payload = f.Env.GetPayload()
		r.ConvID = f.Env.GetConvId()
		r.RequestID = f.Env.GetRequestId()
		if uid := f.Env.GetUserId(); uid > 0 {
			r.UserID = uint64(uid)
		}
	}
	return r
}

// EmitFunc converts an outbound env into a transport frame for Peer.Send.
// Assembly replaces it with the chatpb-backed converter; the default emits a
// Kind shell.
type EmitFunc func(*env) *proto.Frame

// DefaultEmit is the shell converter.
func DefaultEmit(e *env) *proto.Frame { return &proto.Frame{Kind: e.Kind} }

// emitFor is the per-Pipe converter. It used to be a package-level global,
// which meant two Pipes in one process (two chatd assemblies in tests, or a
// future split deployment) overwrote each other's emitter — CI -race caught
// exactly that. Kept as a method so the wiring is per instance.

// TestPeer is a minimal transport.Peer for pipe tests (chan-based).
type TestPeer struct {
	in     chan *proto.Frame
	out    chan *proto.Frame
	closed chan struct{}
}

// NewTestPeer builds a peer with buffered lanes.
func NewTestPeer() *TestPeer {
	return &TestPeer{in: make(chan *proto.Frame, 64), out: make(chan *proto.Frame, 64),
		closed: make(chan struct{})}
}

func (t *TestPeer) Send(f *proto.Frame) error {
	select {
	case t.out <- f:
		return nil
	case <-t.closed:
		return errPeerClosed
	}
}
func (t *TestPeer) Frames() <-chan *proto.Frame { return t.in }

// Out exposes the outbound lane for tests that pump sendOne concurrently.
func (t *TestPeer) Out() <-chan *proto.Frame { return t.out }
func (t *TestPeer) Close() error {
	select {
	case <-t.closed:
	default:
		close(t.closed)
	}
	return nil
}
func (t *TestPeer) Layer() string { return "data" }

// Inject feeds an inbound frame (test harness).
func (t *TestPeer) Inject(f *proto.Frame) { t.in <- f }

// InjectReq builds a shell frame for a Req view and feeds it.
func (t *TestPeer) InjectReq(r Req) {
	t.in <- &proto.Frame{Kind: r.Kind}
}

// Sent returns the outbound lane for assertions.
func (t *TestPeer) Sent() <-chan *proto.Frame { return t.out }

type closedErr struct{}

func (closedErr) Error() string { return "peer closed" }

var errPeerClosed error = closedErr{}

var _ transport.Peer = (*TestPeer)(nil)

// SetEmit installs the assembly's chatpb-backed emitter without exposing the
// unexported env type. The adapter receives the plain fields, so cmd/ can build
// a *proto.Frame (Envelope + FrameMeta) while the pipe keeps its wire-agnostic
// outbound representation.
func (p *Pipe) SetEmit(fn func(kind uint32, requestID string, convID, userID uint64, payload []byte) *proto.Frame) {
	if fn == nil {
		p.emit = DefaultEmit
		return
	}
	p.emit = func(e *env) *proto.Frame {
		if e == nil {
			return nil
		}
		return fn(e.Kind, e.RequestID, e.ConvID, e.UserID, e.Payload)
	}
}
