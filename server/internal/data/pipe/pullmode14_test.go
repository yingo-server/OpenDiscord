// pullmode14_test.go: CORE-2 universal-pull tests (docs/universal-pull-spec.md
// §1.2/§1.3/§2.2/§4.2). All assertions go through the OBSERVABLE frames of the
// full chain (101 → seqalloc → group-commit flusher → fan-out split → lanes)
// or through the decision points with an injected clock — no mocks of the
// logic under test.
//
// Key contracts locked here:
//   - UP-1: universal_pull_enabled=true → ALL conversations (≥2 members) emit
//     watermarks; ≤100-member conversations emit ZERO per-member 102 frames.
//   - UP-2: flag off → byte-identical v1.2.1 threshold path (small conv inline,
//     big conv by threshold; adaptive entries ignored).
//   - UP-3: adaptive effective[cid] batch/interval override the static flags
//     ONLY when universal_pull_enabled is on; out-of-bounds writes are rejected
//     (越界拒载, T-AD-4) and the previous value survives.
//   - UP-4: 202 snapshot protection is the FIXED >100 rule in universal mode —
//     it does not read pull_mode_threshold.
package pipe

import (
	"testing"
	"time"

	"github.com/yingo-server/OpenDiscord/server/internal/data/pipe/wire"
	"github.com/yingo-server/OpenDiscord/server/internal/pkg/featureflags"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/kinds"
)

// upRig is a pullRig with the CORE-2 flag store preinstalled.
type upRig struct {
	*pullRig
}

func newUpRig(t *testing.T, nMembers int) *upRig {
	t.Helper()
	r := newPullRig(t, nMembers)
	r.flags.Apply(map[string]any{featureflags.KeyUniversalPullEnabled: true})
	r.p.SetFeatureFlags(r.flags)
	return &upRig{pullRig: r}
}

// UP-1a: universal mode — a 10-member conversation gets watermarks and zero
// inline 102 fan-out (the v1.2 gap: 50-member groups stayed inline).
func TestUP1a_UniversalPullSmallConvWatermarks(t *testing.T) {
	r := newUpRig(t, 10)
	r.send(t, "u1")
	fan := collectFan(t, r.p, 150*time.Millisecond)
	if got := countKind(fan, uint32(kinds.KindMsgNew)); got != 0 {
		t.Fatalf("universal pull: %d inline 102 frames, want 0 (threshold semantics cancelled)", got)
	}
	n := countKind(fan, uint32(kinds.KindWatermark))
	if n != 9 {
		t.Fatalf("universal pull: %d watermarks, want 9 (one per non-sender member)", n)
	}
	wmEnv := firstOfKind(fan, uint32(kinds.KindWatermark))
	var wm wire.Watermark
	if err := wire.Unmarshal(wmEnv.Payload, &wm); err != nil {
		t.Fatalf("watermark decode: %v", err)
	}
	if wm.ConvID != uint64(r.cid) || wm.LatestSeq == 0 {
		t.Fatalf("watermark = %+v, want convId=%d and a committed latestSeq", wm, r.cid)
	}
}

// UP-1b: a 1-member conversation stays inline (no other member to notify —
// the unified branch keeps the v1.2.1 no-op shape).
func TestUP1b_UniversalPullSingleMemberStaysInline(t *testing.T) {
	r := newUpRig(t, 1)
	ack := r.send(t, "u1")
	if ack.Seq == 0 {
		t.Fatal("send ack missing seq")
	}
	fan := collectFan(t, r.p, 120*time.Millisecond)
	if got := countKind(fan, uint32(kinds.KindWatermark)); got != 0 {
		t.Fatalf("1-member conv: %d watermarks, want 0", got)
	}
}

// UP-2: flag OFF → v1.2.1 byte-identical threshold path, and adaptive entries
// are ignored even when present (S-7 rollback baseline).
func TestUP2_FlagOffKeepsThresholdPathAndIgnoresAdaptive(t *testing.T) {
	r := newPullRig(t, 10)
	st := newAdaptiveStore()
	// An aggressive adaptive entry that MUST NOT apply when the flag is off.
	if !st.set(r.cid, 1, 10) {
		t.Fatal("adaptive.set rejected a legal pair")
	}
	r.p.SetAdaptiveStore(st)
	r.flags.Apply(map[string]any{featureflags.KeyPullModeThreshold: 100})
	r.p.SetFeatureFlags(r.flags)
	r.send(t, "a")
	fan := collectFan(t, r.p, 120*time.Millisecond)
	if got := countKind(fan, uint32(kinds.KindWatermark)); got != 0 {
		t.Fatalf("flag-off: %d watermarks for a 10-member conv, want 0 (v1.2.1 inline)", got)
	}
	// 101 members > threshold → watermark path as in v1.2.1.
	big := newPullRig(t, 101)
	bst := newAdaptiveStore()
	bst.set(big.cid, 1, 10)
	big.p.SetAdaptiveStore(bst)
	big.flags.Apply(map[string]any{featureflags.KeyPullModeThreshold: 100})
	big.p.SetFeatureFlags(big.flags)
	big.send(t, "b")
	bfan := collectFan(t, big.p, 150*time.Millisecond)
	if got := countKind(bfan, uint32(kinds.KindWatermark)); got != 100 {
		t.Fatalf("flag-off: %d watermarks for a 101-member conv, want 100 (v1.2.1 threshold path)", got)
	}
}

// UP-3a: adaptive effective[cid] batch/interval override the static flags when
// the universal flag is on: interval=10ms/batch=1 → every message emits.
func TestUP3a_AdaptiveOverridesStaticWhenUniversalOn(t *testing.T) {
	r := newUpRig(t, 5)
	st := newAdaptiveStore()
	if !st.set(r.cid, 1, 10) {
		t.Fatal("adaptive.set rejected a legal pair")
	}
	r.p.SetAdaptiveStore(st)
	for i := 0; i < 3; i++ {
		r.send(t, "m"+string(rune('a'+i)))
		time.Sleep(25 * time.Millisecond) // interval=10ms → each send crosses it
	}
	fan := collectFan(t, r.p, 120*time.Millisecond)
	if got := countKind(fan, uint32(kinds.KindWatermark)); got < 3 {
		t.Fatalf("adaptive batch=1/interval=10ms: %d watermark fan-out rounds, want ≥3 (one per message)", got)
	}
}

// UP-3b: out-of-bounds writes are rejected (越界拒载, spec §2.2 / T-AD-4);
// a previously accepted value survives the rejection.
func TestUP3b_AdaptiveRejectsOutOfBounds(t *testing.T) {
	st := newAdaptiveStore()
	if st.set(1, 0, 500) {
		t.Fatal("batch=0 must be rejected")
	}
	if st.set(1, 201, 500) {
		t.Fatal("batch=201 must be rejected")
	}
	if st.set(1, 50, 2001) {
		t.Fatal("interval=2001 must be rejected")
	}
	if st.set(1, 50, 9) {
		t.Fatal("interval=9 must be rejected")
	}
	if !st.set(1, 50, 500) {
		t.Fatal("50/500 must be accepted")
	}
	if st.set(1, 0, 500) {
		t.Fatal("rejecting write must not clobber state")
	}
	s, ok := st.get(1)
	if !ok || s.batch != 50 || s.intervalMS != 500 {
		t.Fatalf("after reject, effective = %+v ok=%v, want 50/500 preserved", s, ok)
	}
}

// UP-3c: adaptive overrides are hot — clearing the entry (adaptive_control
// off / rollback) restores the static defaults on the next message. The first
// watermark (reason=1) is still emitted immediately (v1.2.1 shape, spec §1.3
// 规则 3), so aggregation is asserted from the SECOND message on.
func TestUP3c_AdaptiveClearFallsBackToStatic(t *testing.T) {
	r := newUpRig(t, 5)
	st := newAdaptiveStore()
	st.set(r.cid, 200, 2000) // batch 200: after the first watermark, nothing more
	r.p.SetAdaptiveStore(st)
	r.send(t, "q1") // seq 1 → first watermark (reason=1), one per member
	fan := collectFan(t, r.p, 120*time.Millisecond)
	if got := countKind(fan, uint32(kinds.KindWatermark)); got != 4 {
		t.Fatalf("seq1: %d watermark rounds, want 4 (first watermark)", got)
	}
	r.send(t, "q2") // seq 2: advance 1 < batch 200, interval not elapsed → held
	fan = collectFan(t, r.p, 120*time.Millisecond)
	if got := countKind(fan, uint32(kinds.KindWatermark)); got != 0 {
		t.Fatalf("batch=200: %d watermark rounds for seq 2, want 0 (aggregation holds)", got)
	}
	st.clear(r.cid) // rollback to static defaults (batch=50, interval=2000ms)
	r.send(t, "q3")
	r.send(t, "q4") // seq 4: advance 2 < 50, interval not elapsed → still held
	fan = collectFan(t, r.p, 120*time.Millisecond)
	if got := countKind(fan, uint32(kinds.KindWatermark)); got != 0 {
		t.Fatalf("static defaults: %d watermark rounds for seq 3/4, want 0", got)
	}
}

// UP-4: the 202 snapshot protection under universal pull is the FIXED >100
// rule — a 101-member conv substitutes members_omitted even when
// pull_mode_threshold is set above 101, and a 50-member conv keeps the full
// snapshot.
func TestUP4_SnapshotProtectionFixed100(t *testing.T) {
	r := newUpRig(t, 101)
	r.flags.Apply(map[string]any{
		featureflags.KeyPullModeThreshold: 500, // would disable the old rule — must be ignored
	})
	d := r.p.conv
	big := membersN(101)
	if !d.pullSnapshot(len(big)) {
		t.Fatal("universal pull: 101-member conv must keep members_omitted (fixed >100 rule)")
	}
	small := membersN(50)
	if d.pullSnapshot(len(small)) {
		t.Fatal("universal pull: 50-member conv must keep the full snapshot")
	}
	// And the substituted payload really carries members_omitted + member_count.
	full, err := wire.Marshal(wire.ConvPayload{ConvID: uint64(r.cid)})
	if err != nil {
		t.Fatal(err)
	}
	body := d.eventPayload(big, full)
	var p wire.ConvPayload
	if err := wire.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	if !p.MembersOmitted || p.MemberCount != 101 || len(p.Members) != 0 {
		t.Fatalf("202 payload = omitted=%v count=%d members=%d, want omitted/count=101/empty",
			p.MembersOmitted, p.MemberCount, len(p.Members))
	}
}
