package pipe

// sharedframe.go: encode-once fan-out (perf-spec v1.2 §2.1).
//
// The hot fan-out path (MSG_NEW to every conversation member, and the transient
// signals typing/presence/receipts) used to build and Encode one envelope PER
// MEMBER. For a 500-member group one message therefore cost 500 envelope
// allocations and 500 protobuf serialisations, each one copying the (shared)
// payload bytes into a fresh output buffer — the single largest per-message
// waste in the whole server.
//
// The recipient-independent part of an outbound frame (kind + request_id +
// payload) is IDENTICAL for every member. Only the outer Envelope target
// (user_id, a varint) differs. So we serialise that shared body EXACTLY ONCE,
// hand the same immutable byte slice to every member's frame, and let the
// delivery layer append the four-or-so bytes of the per-member target.
//
// Lifecycle: the shared body is immutable and reference-counted (sharedFrame).
// The producer owns one reference per recipient; every consumer that materialises
// or drops its frame releases exactly one. When the count reaches zero the body
// is marked freed and becomes eligible for GC. A producer that fails to enqueue
// a member's frame must release that reference itself (see fanoutTo / fanoutNew)
// or the counter leaks — sharedframe_test.go pins both the count-to-zero and the
// no-leak property.

import (
	"sync/atomic"

	"github.com/yingo-server/UFOT/server/internal/proto/gen/chatpb"
)

// encodeFunc serialises a recipient-independent Envelope body. It is injectable
// so tests can wrap it with a counter and prove "N members ⇒ exactly 1 Encode".
type encodeFunc func(*chatpb.Envelope) []byte

// defaultFanoutEncode is the production encoder used by the fan-out producers.
func defaultFanoutEncode(env *chatpb.Envelope) []byte {
	b, _ := env.Marshal()
	return b
}

// sharedFrame is the reference-counted holder of one immutable encoded body.
// b is never mutated after construction; every member's proto.Frame points at
// the SAME underlying array (frame.PreEncoded = shared.b), so the payload is not
// copied per member at the data layer.
type sharedFrame struct {
	b     []byte
	refs  atomic.Int32
	freed atomic.Bool
}

// newSharedFrame cre a holder over b with n initial references. Guard the zero
// case so a message with no recipients never arms the counter.
func newSharedFrame(b []byte, n int) *sharedFrame {
	s := &sharedFrame{b: b}
	if n > 0 {
		s.refs.Store(int32(n))
	} else {
		s.freed.Store(true)
	}
	return s
}

// retain adds one reference (used by tests / future deferral paths).
func (s *sharedFrame) retain() {
	if s == nil {
		return
	}
	s.refs.Add(1)
}

// release drops one reference. The last release marks the body freed. The bytes
// themselves are GC-owned (the slice header is shared, so we must NOT return it
// to a pool while any consumer could still read it); the flag makes the
// lifecycle observable and testable.
func (s *sharedFrame) release() {
	if s == nil {
		return
	}
	if s.refs.Add(-1) == 0 {
		s.freed.Store(true)
	}
}

// refCount reports the live reference count (tests / metrics).
func (s *sharedFrame) refCount() int {
	if s == nil {
		return 0
	}
	return int(s.refs.Load())
}

// buildFanoutEnvs serialises the recipient-independent envelope body ONCE (via
// enc) and returns one *env per uid, all sharing the same immutable base. The
// returned sharedFrame carries one reference per uid; callers MUST release each
// reference exactly once (sendOne does it for enqueued frames; the enqueue
// failure path calls env.releaseShared()).
func buildFanoutEnvs(enc encodeFunc, kind uint32, requestID string, payload []byte, convID uint64, uids []uint64) ([]*env, *sharedFrame) {
	if len(uids) == 0 {
		return nil, nil
	}
	base := enc(&chatpb.Envelope{Kind: kind, RequestId: requestID, Payload: payload})
	shared := newSharedFrame(base, len(uids))
	envs := make([]*env, 0, len(uids))
	for _, uid := range uids {
		envs = append(envs, &env{
			Kind:      kind,
			RequestID: requestID,
			ConvID:    convID,
			UserID:    uid,
			Payload:   payload,
			base:      base,
			shared:    shared,
		})
	}
	return envs, shared
}

// releaseShared drops this env's reference to the shared body (no-op when the
// env was not produced by a fan-out). Safe to call on nil.
func (e *env) releaseShared() {
	if e != nil && e.shared != nil {
		e.shared.release()
	}
}
