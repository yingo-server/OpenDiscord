package pipe

// call_test.go: unit coverage for the WebRTC call-signaling relay (kinds
// 350-354). These exercise the dispatcher directly (no peer): state machine,
// permission red line, auto-BUSY, teardown, ring timeout and the flag gate.

import (
	"context"
	"testing"
	"time"

	"github.com/yingo-server/UFOT/server/internal/data/pipe/wire"
	"github.com/yingo-server/UFOT/server/internal/data/store"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
)

// enableCalls flips sys_feature_flags.call_enabled on in the fake sys store.
func enableCalls(t *testing.T, sys interface {
	UpsertFlag(context.Context, *store.Flag) error
}) {
	t.Helper()
	if err := sys.UpsertFlag(context.Background(), &store.Flag{Key: "call_enabled", Value: "true"}); err != nil {
		t.Fatalf("enable calls: %v", err)
	}
}

// twoMemberConv creates a conversation owned by 7 with 8 added as a member.
func twoMemberConv(t *testing.T, p *Pipe, convs interface {
	CreateConv(context.Context, *store.ConvMeta, int64) (int64, error)
	AddMember(context.Context, *store.Member) error
}) int64 {
	t.Helper()
	ctx := context.Background()
	cid, err := convs.CreateConv(ctx, &store.ConvMeta{CType: 2, CreatedBy: 7, CreatedAt: 1}, 7)
	if err != nil {
		t.Fatalf("create conv: %v", err)
	}
	if err := convs.AddMember(ctx, &store.Member{Cid: cid, UID: 8, Role: 1, JoinedAt: 1}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	return cid
}

func offerPayload(cid int64, callID string, from, to int64) string {
	b, _ := wire.Marshal(wire.CallSignal{
		CallID: callID, ConvID: uint64(cid), ToUserID: uint64(to),
		SDP: "v=0 offer", Media: "audio", FromUserID: uint64(from), // bogus from, must be ignored
	})
	return string(b)
}

func decodeCall(t *testing.T, e *env) wire.CallSignal {
	t.Helper()
	var s wire.CallSignal
	if err := wire.Unmarshal(e.Payload, &s); err != nil {
		t.Fatalf("decode call signal: %v (payload %s)", err, e.Payload)
	}
	return s
}

// TestCallSignalRelay: A→B offer is forwarded to B with fromUserId forced to A.
func TestCallSignalRelay(t *testing.T) {
	p, _, convs, sys := newTestPipe(t)
	enableCalls(t, sys)
	cid := twoMemberConv(t, p, convs)
	ctx := context.Background()

	// Client lies: fromUserId=999 in the payload.
	p.dispatchReq(ctx, mkReq(uint32(kinds.KindCallOffer), cid, 7, offerPayload(cid, "c-relay", 999, 8)))
	e := waitEnv(t, p.Outbound())
	if e.Kind != uint32(kinds.KindCallOffer) {
		t.Fatalf("relayed kind = %d, want 350 (%s)", e.Kind, e.Payload)
	}
	if e.UserID != 8 {
		t.Fatalf("relayed target = %d, want 8 (callee)", e.UserID)
	}
	sig := decodeCall(t, e)
	if sig.FromUserID != 7 {
		t.Fatalf("fromUserId = %d, want 7 (server must backfill real sender)", sig.FromUserID)
	}
	if sig.CallID != "c-relay" || sig.SDP != "v=0 offer" {
		t.Fatalf("payload mutated: %+v", sig)
	}
}

// TestCallNonMemberRejected: a non-member OFFER gets 403 and is not forwarded.
func TestCallNonMemberRejected(t *testing.T) {
	p, _, convs, sys := newTestPipe(t)
	enableCalls(t, sys)
	ctx := context.Background()
	// Conv owned by 7; user 8 is NOT a member.
	cid, _ := convs.CreateConv(ctx, &store.ConvMeta{CType: 2, CreatedBy: 7, CreatedAt: 1}, 7)

	p.dispatchReq(ctx, mkReq(uint32(kinds.KindCallOffer), cid, 7, offerPayload(cid, "c-nonmember", 7, 8)))
	e := waitEnv(t, p.Outbound())
	if e.Kind != uint32(kinds.KindError) {
		t.Fatalf("kind = %d, want ERROR (%s)", e.Kind, e.Payload)
	}
	var er struct {
		Code uint32 `json:"code"`
	}
	_ = wire.Unmarshal(e.Payload, &er)
	if er.Code != 403 {
		t.Fatalf("code = %d, want 403 (%s)", er.Code, e.Payload)
	}
	// Nothing else must be emitted (no forwarding).
	select {
	case extra := <-p.Outbound():
		t.Fatalf("unexpected extra frame (kind %d) — non-member offer was forwarded", extra.Kind)
	default:
	}
}

// TestCallBusyWhenPeerInCall: offering to a callee already ringing answers the
// second caller with CALL_BUSY(busy) instead of forwarding.
func TestCallBusyWhenPeerInCall(t *testing.T) {
	p, _, convs, sys := newTestPipe(t)
	enableCalls(t, sys)
	ctx := context.Background()
	cid, _ := convs.CreateConv(ctx, &store.ConvMeta{CType: 2, CreatedBy: 7, CreatedAt: 1}, 7)
	_ = convs.AddMember(ctx, &store.Member{Cid: cid, UID: 8, Role: 1, JoinedAt: 1})
	_ = convs.AddMember(ctx, &store.Member{Cid: cid, UID: 9, Role: 1, JoinedAt: 1})

	// 7 → 8 (callee 8 now ringing).
	p.dispatchReq(ctx, mkReq(uint32(kinds.KindCallOffer), cid, 7, offerPayload(cid, "c-first", 7, 8)))
	first := waitEnv(t, p.Outbound())
	if first.Kind != uint32(kinds.KindCallOffer) || first.UserID != 8 {
		t.Fatalf("first offer not relayed to 8: kind=%d uid=%d", first.Kind, first.UserID)
	}

	// 9 → 8: callee busy → CALL_BUSY(busy) back to 9.
	p.dispatchReq(ctx, mkReq(uint32(kinds.KindCallOffer), cid, 9, offerPayload(cid, "c-second", 9, 8)))
	e := waitEnv(t, p.Outbound())
	if e.Kind != uint32(kinds.KindCallBusy) {
		t.Fatalf("kind = %d, want CALL_BUSY 353 (%s)", e.Kind, e.Payload)
	}
	if e.UserID != 9 {
		t.Fatalf("busy target = %d, want 9 (second caller)", e.UserID)
	}
	sig := decodeCall(t, e)
	if sig.Reason != "busy" {
		t.Fatalf("reason = %q, want busy", sig.Reason)
	}
}

// TestCallHangupClearsState: HANGUP is relayed to the peer and clears the table
// so the callee is free again.
func TestCallHangupClearsState(t *testing.T) {
	p, _, convs, sys := newTestPipe(t)
	enableCalls(t, sys)
	cid := twoMemberConv(t, p, convs)
	ctx := context.Background()

	p.dispatchReq(ctx, mkReq(uint32(kinds.KindCallOffer), cid, 7, offerPayload(cid, "c-hang", 7, 8)))
	if e := waitEnv(t, p.Outbound()); e.Kind != uint32(kinds.KindCallOffer) {
		t.Fatalf("offer not relayed: kind=%d", e.Kind)
	}
	if p.calls.get("c-hang") == nil {
		t.Fatal("call state not created on OFFER")
	}

	hp, _ := wire.Marshal(wire.CallSignal{CallID: "c-hang", Reason: "hangup", FromUserID: 7})
	p.dispatchReq(ctx, mkReq(uint32(kinds.KindCallHangup), 0, 7, string(hp)))
	e := waitEnv(t, p.Outbound())
	if e.Kind != uint32(kinds.KindCallHangup) {
		t.Fatalf("hangup kind = %d, want 354 (%s)", e.Kind, e.Payload)
	}
	if e.UserID != 8 {
		t.Fatalf("hangup target = %d, want 8", e.UserID)
	}
	if p.calls.get("c-hang") != nil {
		t.Fatal("call state not cleared after HANGUP")
	}
	if p.calls.busy(8) {
		t.Fatal("callee still marked busy after HANGUP")
	}
}

// TestCallDisabledByFlag: with call_enabled=false every signal answers 403 and
// nothing is relayed.
func TestCallDisabledByFlag(t *testing.T) {
	p, _, convs, _ := newTestPipe(t) // no enableCalls → flag absent → default false
	cid := twoMemberConv(t, p, convs)
	ctx := context.Background()

	p.dispatchReq(ctx, mkReq(uint32(kinds.KindCallOffer), cid, 7, offerPayload(cid, "c-off", 7, 8)))
	e := waitEnv(t, p.Outbound())
	if e.Kind != uint32(kinds.KindError) {
		t.Fatalf("kind = %d, want ERROR (%s)", e.Kind, e.Payload)
	}
	var er struct {
		Code   uint32 `json:"code"`
		Reason string `json:"reason"`
	}
	_ = wire.Unmarshal(e.Payload, &er)
	if er.Code != 403 || er.Reason != "calls disabled" {
		t.Fatalf("got %+v, want 403 calls disabled", er)
	}
	select {
	case extra := <-p.Outbound():
		t.Fatalf("call forwarded while disabled (kind %d)", extra.Kind)
	default:
	}
}

// collectTimeouts drains the outbound lane until both parties of wantCallID
// have received a CALL_HANGUP(reason=timeout). Fails if none arrive in time.
func collectTimeouts(t *testing.T, p *Pipe, wantUIDs ...uint64) map[uint64]bool {
	t.Helper()
	got := map[uint64]bool{}
	deadline := time.After(2 * time.Second)
	for len(got) < len(wantUIDs) {
		select {
		case e := <-p.Outbound():
			if e.Kind != uint32(kinds.KindCallHangup) {
				continue
			}
			if sig := decodeCall(t, e); sig.Reason != "timeout" {
				t.Fatalf("hangup reason = %q, want timeout", sig.Reason)
			}
			got[e.UserID] = true
		case <-deadline:
			t.Fatalf("timeout hangups not delivered; got %v want %v", got, wantUIDs)
		}
	}
	return got
}

// TestCallRingTimeout: an unanswered ringing call is auto-hung-up to BOTH
// parties with reason=timeout once the (injected, short) ring deadline passes
// and a single sweep runs.
func TestCallRingTimeout(t *testing.T) {
	p, _, convs, sys := newTestPipe(t)
	enableCalls(t, sys)
	cid := twoMemberConv(t, p, convs)
	ctx := context.Background()

	base := time.Unix(10_000, 0)
	cur := base
	p.calls.now = func() time.Time { return cur }
	p.calls.timeout = 30 * time.Second

	p.dispatchReq(ctx, mkReq(uint32(kinds.KindCallOffer), cid, 7, offerPayload(cid, "c-timeout", 7, 8)))
	if e := waitEnv(t, p.Outbound()); e.Kind != uint32(kinds.KindCallOffer) {
		t.Fatalf("offer not relayed: kind=%d", e.Kind)
	}

	// Advance past the ring deadline and sweep once.
	cur = base.Add(31 * time.Second)
	p.sweepCalls(cur)

	got := collectTimeouts(t, p, 7, 8)
	if !got[7] || !got[8] {
		t.Fatalf("timeout not delivered to both parties: %v", got)
	}
	if p.calls.get("c-timeout") != nil {
		t.Fatal("timed-out call not cleared")
	}
	if p.calls.busy(7) || p.calls.busy(8) {
		t.Fatal("userInCall not released after ring timeout")
	}
}

// TestCallActiveCallTimesOut: once ANSWERED, the ring timer is gone — the P0
// leak — so a hard max-call-duration deadline must reap an active call. Advance
// the fake clock past maxCallDuration, sweep once, both parties get
// CALL_HANGUP(timeout) and the map/userInCall are released.
func TestCallActiveCallTimesOut(t *testing.T) {
	p, _, convs, sys := newTestPipe(t)
	enableCalls(t, sys)
	cid := twoMemberConv(t, p, convs)
	ctx := context.Background()

	base := time.Unix(20_000, 0)
	cur := base
	p.calls.now = func() time.Time { return cur }
	p.calls.timeout = 60 * time.Second
	p.calls.maxCallDur = 100 * time.Second

	// OFFER 7 → 8.
	p.dispatchReq(ctx, mkReq(uint32(kinds.KindCallOffer), cid, 7, offerPayload(cid, "c-active", 7, 8)))
	if e := waitEnv(t, p.Outbound()); e.Kind != uint32(kinds.KindCallOffer) {
		t.Fatalf("offer not relayed: kind=%d", e.Kind)
	}
	// ANSWER from callee 8 → relayed to caller 7, call becomes active.
	ap, _ := wire.Marshal(wire.CallSignal{CallID: "c-active", ConvID: uint64(cid)})
	p.dispatchReq(ctx, mkReq(uint32(kinds.KindCallAnswer), cid, 8, string(ap)))
	if e := waitEnv(t, p.Outbound()); e.Kind != uint32(kinds.KindCallAnswer) {
		t.Fatalf("answer not relayed: kind=%d", e.Kind)
	}
	if st := p.calls.get("c-active"); st == nil || st.state != callStateActive {
		t.Fatalf("call not active after ANSWER: %+v", st)
	}

	// Just before the ceiling: nothing expires.
	cur = base.Add(99 * time.Second)
	p.sweepCalls(cur)
	select {
	case e := <-p.Outbound():
		t.Fatalf("unexpected frame before expiry: kind=%d", e.Kind)
	default:
	}

	// Past the hard ceiling: active call is reaped.
	cur = base.Add(101 * time.Second)
	p.sweepCalls(cur)

	got := collectTimeouts(t, p, 7, 8)
	if !got[7] || !got[8] {
		t.Fatalf("active timeout not delivered to both parties: %v", got)
	}
	if p.calls.get("c-active") != nil {
		t.Fatal("active call not cleared after max-duration timeout")
	}
	if p.calls.busy(7) || p.calls.busy(8) {
		t.Fatal("userInCall still pinned after active timeout (leak)")
	}
}

// TestCallRejectsWhenTableFull: once the call table hits its cap, a new OFFER
// to a non-busy callee is answered CALL_BUSY(busy) instead of being admitted.
func TestCallRejectsWhenTableFull(t *testing.T) {
	p, _, convs, sys := newTestPipe(t)
	enableCalls(t, sys)
	ctx := context.Background()
	cid, _ := convs.CreateConv(ctx, &store.ConvMeta{CType: 2, CreatedBy: 7, CreatedAt: 1}, 7)
	for _, u := range []int64{8, 9, 10} {
		if err := convs.AddMember(ctx, &store.Member{Cid: cid, UID: u, Role: 1, JoinedAt: 1}); err != nil {
			t.Fatalf("add member %d: %v", u, err)
		}
	}
	p.calls.maxCalls = 1

	// First call occupies the single slot.
	p.dispatchReq(ctx, mkReq(uint32(kinds.KindCallOffer), cid, 7, offerPayload(cid, "c-fill", 7, 8)))
	if e := waitEnv(t, p.Outbound()); e.Kind != uint32(kinds.KindCallOffer) {
		t.Fatalf("first offer not relayed: kind=%d", e.Kind)
	}

	// Second call, callee 10 is free but the table is full.
	p.dispatchReq(ctx, mkReq(uint32(kinds.KindCallOffer), cid, 9, offerPayload(cid, "c-over", 9, 10)))
	e := waitEnv(t, p.Outbound())
	if e.Kind != uint32(kinds.KindCallBusy) {
		t.Fatalf("kind = %d, want CALL_BUSY 353 (%s)", e.Kind, e.Payload)
	}
	if e.UserID != 9 {
		t.Fatalf("busy target = %d, want 9 (caller)", e.UserID)
	}
	if sig := decodeCall(t, e); sig.Reason != "busy" {
		t.Fatalf("reason = %q, want busy", sig.Reason)
	}
	if p.calls.Count() != 1 {
		t.Fatalf("table len = %d, want 1 (over-cap call must be rejected)", p.calls.Count())
	}
}

// TestCallDuplicateCallIDRejected: a second OFFER reusing a live callId must
// return 409 and must NOT overwrite the existing call's state.
func TestCallDuplicateCallIDRejected(t *testing.T) {
	p, _, convs, sys := newTestPipe(t)
	enableCalls(t, sys)
	ctx := context.Background()
	cid, _ := convs.CreateConv(ctx, &store.ConvMeta{CType: 2, CreatedBy: 7, CreatedAt: 1}, 7)
	for _, u := range []int64{8, 9, 10} {
		if err := convs.AddMember(ctx, &store.Member{Cid: cid, UID: u, Role: 1, JoinedAt: 1}); err != nil {
			t.Fatalf("add member %d: %v", u, err)
		}
	}

	// Live call: 7 → 8 with callId "dup".
	p.dispatchReq(ctx, mkReq(uint32(kinds.KindCallOffer), cid, 7, offerPayload(cid, "dup", 7, 8)))
	if e := waitEnv(t, p.Outbound()); e.Kind != uint32(kinds.KindCallOffer) {
		t.Fatalf("first offer not relayed: kind=%d", e.Kind)
	}

	// Second OFFER reuses "dup" from 9 → 10 (callee 10 is free → hits create).
	p.dispatchReq(ctx, mkReq(uint32(kinds.KindCallOffer), cid, 9, offerPayload(cid, "dup", 9, 10)))
	e := waitEnv(t, p.Outbound())
	if e.Kind != uint32(kinds.KindError) {
		t.Fatalf("kind = %d, want ERROR (%s)", e.Kind, e.Payload)
	}
	var er struct {
		Code uint32 `json:"code"`
	}
	_ = wire.Unmarshal(e.Payload, &er)
	if er.Code != 409 {
		t.Fatalf("code = %d, want 409 (%s)", er.Code, e.Payload)
	}
	// Original call untouched (still 7 → 8, not overwritten by 9 → 10).
	st := p.calls.get("dup")
	if st == nil {
		t.Fatal("original call was dropped")
	}
	if st.from != 7 || st.to != 8 {
		t.Fatalf("call overwritten: from=%d to=%d, want 7→8", st.from, st.to)
	}
}

// TestCallSingleTickerSweepsExpired: one sweep pass clears ALL expired calls at
// once (the single-ticker model), releasing every user.
func TestCallSingleTickerSweepsExpired(t *testing.T) {
	p, _, convs, sys := newTestPipe(t)
	enableCalls(t, sys)
	ctx := context.Background()
	cid, _ := convs.CreateConv(ctx, &store.ConvMeta{CType: 2, CreatedBy: 7, CreatedAt: 1}, 7)
	for _, u := range []int64{8, 9, 10, 11, 12} {
		if err := convs.AddMember(ctx, &store.Member{Cid: cid, UID: u, Role: 1, JoinedAt: 1}); err != nil {
			t.Fatalf("add member %d: %v", u, err)
		}
	}
	base := time.Unix(30_000, 0)
	cur := base
	p.calls.now = func() time.Time { return cur }
	p.calls.timeout = 30 * time.Second

	calls := []struct {
		id       string
		from, to int64
	}{
		{"s1", 7, 8}, {"s2", 9, 10}, {"s3", 11, 12},
	}
	for _, c := range calls {
		p.dispatchReq(ctx, mkReq(uint32(kinds.KindCallOffer), cid, c.from, offerPayload(cid, c.id, c.from, c.to)))
		if e := waitEnv(t, p.Outbound()); e.Kind != uint32(kinds.KindCallOffer) {
			t.Fatalf("offer %s not relayed: kind=%d", c.id, e.Kind)
		}
	}
	if p.calls.Count() != 3 {
		t.Fatalf("table len = %d, want 3", p.calls.Count())
	}

	// Advance past every ring deadline and sweep ONCE.
	cur = base.Add(31 * time.Second)
	p.sweepCalls(cur)

	// 3 calls × 2 parties = 6 timeout hangups from a single sweep.
	got := map[uint64]int{}
	deadline := time.After(2 * time.Second)
	for len(got) < 6 {
		select {
		case e := <-p.Outbound():
			if e.Kind != uint32(kinds.KindCallHangup) {
				continue
			}
			got[e.UserID]++
		case <-deadline:
			t.Fatalf("single sweep did not clear all; got %v", got)
		}
	}
	if p.calls.Count() != 0 {
		t.Fatalf("table not empty after sweep: %d", p.calls.Count())
	}
	if p.calls.UserInCallCount() != 0 {
		t.Fatalf("userInCall not released: %d", p.calls.UserInCallCount())
	}
}
