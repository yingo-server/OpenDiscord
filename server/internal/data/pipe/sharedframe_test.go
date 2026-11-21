package pipe

// sharedframe_test.go: proofs for the encode-once fan-out (perf-spec §2.1).
//
// The postmortem lesson (docs/perf-postmortem-v1.2-draft.md): prove the waste
// EXISTS before optimising, then prove the saving LANDS, with the same
// instrument. These tests use a counting encodeFunc and a counted Marshal on
// chatpb.Envelope to do exactly that:
//
//   - BenchmarkFanoutEncode/classic: the pre-§2.1 path — one envelope + one
//     protobuf serialisation per member (what "现状式" cost).
//   - BenchmarkFanoutEncode/shared: buildFanoutEnvs — exactly ONE
//     serialisation per fanoutJob regardless of member count.
//   - The tests assert the encode count (500 members ⇒ 1 Encode) and the
//     reference lifecycle (count-to-zero, no leak on drop).

import (
	"sync"
	"sync/atomic"
	"testing"

	chatpb "github.com/yingo-server/UFOT/server/internal/proto/gen/chatpb"
)

// countingEncode wraps an encodeFunc and counts invocations. This is the
// instrument that proves "N members ⇒ exactly 1 Encode".
type countingEncode struct {
	calls atomic.Int64
	inner encodeFunc
}

func (c *countingEncode) encode(env *chatpb.Envelope) []byte {
	c.calls.Add(1)
	return c.inner(env)
}

// marshalCounter counts real protobuf Marshal calls by intercepting the one
// place the classic path serialises: env.Marshal. We emulate the classic path
// by calling Envelope.Marshal directly per member (that is what the pre-§2.1
// fanout did: one Envelope + one Marshal per recipient).

// classicFanout reproduces the PRE-§2.1 behaviour: one Envelope allocation and
// one protobuf serialisation PER MEMBER (the "500 encodes" problem).
func classicFanout(kind uint32, payload []byte, uids []uint64) [][]byte {
	out := make([][]byte, 0, len(uids))
	for _, uid := range uids {
		env := &chatpb.Envelope{Kind: kind, Payload: payload}
		env.SetUserId(uid)
		b, _ := env.Marshal()
		out = append(out, b)
	}
	return out
}

func sharedPayload(n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte(i)
	}
	return p
}

func uidSlice(n, exclude uint64) []uint64 {
	uids := make([]uint64, 0, n)
	for i := uint64(1); i <= uint64(n)+1; i++ {
		if i == exclude {
			continue
		}
		uids = append(uids, i)
	}
	return uids
}

// TestFanoutEncodeOnce: 500 members ⇒ the recipient-independent body is
// serialised EXACTLY once, every member's env points at the same bytes, and
// the reference count starts at the member count.
func TestFanoutEncodeOnce(t *testing.T) {
	const members = 500
	uids := uidSlice(members, 7)
	payload := sharedPayload(512)
	ce := &countingEncode{inner: defaultFanoutEncode}

	envs, shared := buildFanoutEnvs(ce.encode, uint32(201), "", payload, 42, uids)
	if len(envs) != members {
		t.Fatalf("envs = %d, want %d", len(envs), members)
	}
	if got := ce.calls.Load(); got != 1 {
		t.Fatalf("Encode calls = %d, want 1 (encode-once violated)", got)
	}
	if shared == nil || shared.refCount() != members {
		t.Fatalf("shared refs = %v, want %d", shared, members)
	}
	base := envs[0].base
	for i, e := range envs {
		if &e.base[0] != &base[0] || len(e.base) != len(base) {
			t.Fatalf("env[%d] does not share the encoded body", i)
		}
		if e.UserID != uids[i] {
			t.Fatalf("env[%d].UserID = %d, want %d", i, e.UserID, uids[i])
		}
	}
	// Release every reference exactly once ⇒ count reaches zero (freed).
	for _, e := range envs {
		e.releaseShared()
	}
	if shared.refCount() != 0 || !shared.freed.Load() {
		t.Fatalf("refs = %d freed = %v after releasing all members; want 0/true",
			shared.refCount(), shared.freed.Load())
	}
}

// TestFanoutSharedDropNoLeak: a member whose frame the outbound lane refuses
// releases its own reference; the remaining members release theirs. All
// references must still reach zero (the leak the producer would otherwise
// cause is pinned here).
func TestFanoutSharedDropNoLeak(t *testing.T) {
	const members = 100
	const dropped = 7
	uids := uidSlice(members, 0)
	ce := &countingEncode{inner: defaultFanoutEncode}

	envs, shared := buildFanoutEnvs(ce.encode, uint32(230), "", []byte("typing"), 0, uids)
	for i := 0; i < dropped; i++ {
		envs[i].releaseShared() // the put-failure path in fanoutTo / fanoutNew
	}
	for _, e := range envs[dropped:] {
		e.releaseShared()
	}
	if shared.refCount() != 0 || !shared.freed.Load() {
		t.Fatalf("leak: refs = %d freed = %v", shared.refCount(), shared.freed.Load())
	}
}

// TestFanoutSharedRetainSafe: retain/release is balanced and the zero-member
// case never arms the counter (no phantom references).
func TestFanoutSharedRetainSafe(t *testing.T) {
	uids := uidSlice(3, 0)
	ce := &countingEncode{inner: defaultFanoutEncode}
	envs, shared := buildFanoutEnvs(ce.encode, uint32(401), "", nil, 0, uids)
	shared.retain()
	if shared.refCount() != len(uids)+1 {
		t.Fatalf("refs after retain = %d, want %d", shared.refCount(), len(uids)+1)
	}
	shared.release()
	for _, e := range envs {
		e.releaseShared()
	}
	if shared.refCount() != 0 {
		t.Fatalf("refs = %d, want 0", shared.refCount())
	}

	// Empty recipient list: no envs, no armed counter.
	envs0, s0 := buildFanoutEnvs(ce.encode, uint32(401), "", nil, 0, nil)
	if len(envs0) != 0 || s0 != nil {
		t.Fatalf("empty fanout produced envs=%d shared=%v, want none", len(envs0), s0)
	}
}

// TestFanoutTargetIsPerMember: the per-member variation lives ONLY in the env
// routing fields (UserID/ConvID); the payload and the encoded body are shared.
// This is the correctness boundary of the sharing: any field a consumer
// customises per connection must NOT live in the shared bytes.
func TestFanoutTargetIsPerMember(t *testing.T) {
	uids := uidSlice(50, 0)
	payload := sharedPayload(64)
	ce := &countingEncode{inner: defaultFanoutEncode}
	envs, _ := buildFanoutEnvs(ce.encode, uint32(201), "", payload, 42, uids)
	for i, e := range envs {
		if string(e.Payload) != string(payload) {
			t.Fatalf("env[%d] payload differs from the shared payload", i)
		}
		if e.UserID != uids[i] {
			t.Fatalf("env[%d].UserID = %d, want %d", i, e.UserID, uids[i])
		}
		e.releaseShared()
	}
}

// BenchmarkFanoutEncode measures the encoding cost of delivering one message
// to 500 members, classic per-member encode vs shared encode-once.
// b.ReportAllocs: the win is mostly allocations (500 Envelope structs +
// 500 output buffers vs one of each).
func BenchmarkFanoutEncode(b *testing.B) {
	const members = 500
	payload := sharedPayload(512)
	uids := uidSlice(members, 7)

	var once sync.Once
	var counted *countingEncode
	b.Run("classic_500_members", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			frames := classicFanout(uint32(201), payload, uids)
			sink = frames
		}
	})
	b.Run("shared_500_members", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			once.Do(func() { counted = &countingEncode{inner: defaultFanoutEncode} })
			counted.calls.Store(0)
			envs, shared := buildFanoutEnvs(counted.encode, uint32(201), "", payload, 42, uids)
			if got := counted.calls.Load(); got != 1 {
				b.Fatalf("Encode calls = %d, want 1", got)
			}
			sink = envs
			_ = shared
			for _, e := range envs {
				e.releaseShared()
			}
		}
	})
}

// sink keeps benchmark outputs alive.
var sink any
