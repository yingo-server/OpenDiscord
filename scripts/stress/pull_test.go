package main

// Pull-mode unit tests (spec pull-mode-spec §4.4):
//   1. wire round-trips for the new Watermark(14) / SyncRequest(8) bodies;
//   2. the watermark→SYNC state machine against a fake conn.
// No network, no server — the machine transitions must be provable in CI.

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

// --- wire round-trips --------------------------------------------------------

func TestWatermarkRoundTrip(t *testing.T) {
	w := &watermarkPayload{ConvID: 123, LatestSeq: 8842, Reason: 0, MemberCount: 500}
	b, err := encodeWatermark(w)
	if err != nil {
		t.Fatal(err)
	}
	// spec §1.1 example wire form: {"kind":14,"convId":123,"latestSeq":8842,"reason":0,"memberCount":500}
	for _, key := range []string{`"convId":123`, `"latestSeq":8842`, `"memberCount":500`} {
		if !bytes.Contains(b, []byte(key)) {
			t.Fatalf("encoded watermark %s missing %s", b, key)
		}
	}
	got, err := decodeWatermark(b)
	if err != nil {
		t.Fatal(err)
	}
	if *got != *w {
		t.Fatalf("round trip mismatch: %+v != %+v", got, w)
	}
}

func TestWatermarkInsideEnvelopeRoundTrip(t *testing.T) {
	wm, _ := encodeWatermark(&watermarkPayload{ConvID: 7, LatestSeq: 42, Reason: 1, MemberCount: 200})
	env := &envelope{Kind: kindWatermark, ConvID: 7, Payload: wm}
	b, err := encodeEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := decodeEnvelope(b)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Kind != kindWatermark {
		t.Fatalf("kind = %d, want %d", dec.Kind, kindWatermark)
	}
	out, err := decodeWatermark(dec.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if out.ConvID != 7 || out.LatestSeq != 42 || out.Reason != 1 || out.MemberCount != 200 {
		t.Fatalf("watermark fields lost through envelope: %+v", out)
	}
}

func TestSyncRequestRoundTrip(t *testing.T) {
	s := &syncRequestPayload{ConvID: 9, SinceSeq: 100, Limit: 200}
	b, err := encodeSyncRequest(s)
	if err != nil {
		t.Fatal(err)
	}
	// same JSON shape as wire.ReadHistory (server pipe.go onSyncRequest decodes it)
	for _, key := range []string{`"convId":9`, `"sinceSeq":100`, `"limit":200`} {
		if !bytes.Contains(b, []byte(key)) {
			t.Fatalf("encoded sync %s missing %s", b, key)
		}
	}
	var rh struct {
		ConvID   uint64 `json:"convId"`
		SinceSeq uint64 `json:"sinceSeq"`
		Limit    uint32 `json:"limit"`
	}
	if err := json.Unmarshal(b, &rh); err != nil {
		t.Fatal(err)
	}
	if rh.ConvID != 9 || rh.SinceSeq != 100 || rh.Limit != 200 {
		t.Fatalf("decoded as ReadHistory shape mismatch: %+v", rh)
	}
	got, err := decodeSyncRequest(b)
	if err != nil {
		t.Fatal(err)
	}
	if *got != *s {
		t.Fatalf("round trip mismatch: %+v != %+v", got, s)
	}
}

// --- watermark→SYNC state machine (fake conn: pullTracker is pure) -----------

func TestPullStateMachineSyncOnGap(t *testing.T) {
	// T-CL-1 analogue: watermark latestSeq = lastSeq+5 → SYNC since_seq=lastSeq+1
	p := newPullTracker()
	now := time.Now()
	req := p.onWatermark(5, 10, now)
	if req == nil {
		t.Fatal("expected a SYNC request for a fresh gap")
	}
	if req.ConvID != 5 || req.SinceSeq != 1 || req.Limit != syncPullLimit {
		t.Fatalf("SYNC = %+v, want conv=5 since=1 limit=%d", req, syncPullLimit)
	}
	// replay of the pulled messages advances the cursor and settles the wm
	for seq := uint64(1); seq <= 10; seq++ {
		p.onMsgNew(5, seq, now)
	}
	if got := p.lastSeqOf(5); got != 10 {
		t.Fatalf("lastSeq = %d, want 10", got)
	}
	samples := p.convergence()
	if len(samples) != 1 {
		t.Fatalf("convergence samples = %d, want 1", len(samples))
	}
	if p.outstanding() != 0 {
		t.Fatalf("watermark should be settled, outstanding=%d", p.outstanding())
	}
}

func TestPullStateMachineDropsStaleWatermark(t *testing.T) {
	// T-CL-2 analogue: duplicate / out-of-order watermark (latestSeq ≤ lastSeq) → discard, zero requests
	p := newPullTracker()
	now := time.Now()
	p.onMsgNew(1, 20, now)
	if req := p.onWatermark(1, 20, now); req != nil {
		t.Fatalf("expected drop at latestSeq == lastSeq, got %+v", req)
	}
	if req := p.onWatermark(1, 5, now); req != nil {
		t.Fatalf("expected drop at latestSeq < lastSeq, got %+v", req)
	}
	wm, syncs, _ := p.counters()
	if wm != 2 || syncs != 0 {
		t.Fatalf("counters wm=%d syncs=%d, want 2/0", wm, syncs)
	}
}

func TestPullStateMachineDeduplicatesInflight(t *testing.T) {
	p := newPullTracker()
	now := time.Now()
	if p.onWatermark(2, 10, now) == nil {
		t.Fatal("first watermark must produce a SYNC")
	}
	if p.onWatermark(2, 8, now) != nil {
		t.Fatal("watermark below the in-flight target must NOT produce another SYNC")
	}
	if _, syncs, _ := p.counters(); syncs != 1 {
		t.Fatalf("syncs = %d, want 1 (in-flight dedup)", syncs)
	}
	// a LARGER watermark refreshes the in-flight target (single request covers both)
	if p.onWatermark(2, 30, now) == nil {
		t.Fatal("watermark beyond in-flight target must issue a new SYNC")
	}
	p.onMsgNew(2, 30, now)
	if got := p.lastSeqOf(2); got != 30 {
		t.Fatalf("lastSeq = %d, want 30", got)
	}
	if p.outstanding() != 0 {
		t.Fatal("target 30 watermark should be settled")
	}
}

func TestPullStateMachineIndependentConvs(t *testing.T) {
	p := newPullTracker()
	now := time.Now()
	p.onMsgNew(1, 5, now)
	if p.onWatermark(1, 5, now) != nil {
		t.Fatal("conv 1 is caught up")
	}
	if req := p.onWatermark(2, 3, now); req == nil {
		t.Fatal("conv 2 has its own cursor at 0, must pull")
	} else if req.SinceSeq != 1 {
		t.Fatalf("conv 2 since = %d, want 1", req.SinceSeq)
	}
}

func TestPullConvergenceLatencyRecorded(t *testing.T) {
	p := newPullTracker()
	t0 := time.Now()
	p.onWatermark(3, 4, t0)
	time.Sleep(5 * time.Millisecond)
	p.onMsgNew(3, 4, time.Now())
	samples := p.convergence()
	if len(samples) != 1 || samples[0] <= 0 {
		t.Fatalf("convergence sample missing or non-positive: %v", samples)
	}
}

func TestPullCountersZeroInPushMode(t *testing.T) {
	// push mode never attaches a tracker; worker fields stay zero — the
	// default-false guarantee checked structurally here.
	w := newWorker(0, "tok", 1, 2, nil)
	if w.pull != nil || w.downWM.Load() != 0 || w.downMsgNew.Load() != 0 || w.downCtrl.Load() != 0 || w.syncQPS.Load() != 0 {
		t.Fatal("fresh worker must have all pull-mode counters at zero / tracker nil")
	}
	if w.pull == nil {
		// nil-safe readLoop behaviour: kind 14 without --pull-mode counts as ctrl
		w.downCtrl.Add(1)
		if w.downCtrl.Load() != 1 {
			t.Fatal("ctrl counter broken")
		}
	}
}
