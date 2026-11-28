package main

// pull.go implements the §4.4 pull-mode client behaviour as a pure state
// machine (spec §2.1), deliberately decoupled from the websocket so the
// watermark→SYNC→converge transitions are unit-testable against a fake conn.
//
//   onWatermark(w):
//     1. latestSeq <= local lastSeq            → drop (duplicate / late)
//     2. latestSeq >  lastSeq, pull in flight  → refresh target, no new SYNC
//     3. otherwise                             → SYNC{since_seq=lastSeq+1, limit=200}
//
//   onMsgNew(seq): lastSeq = max(lastSeq, seq); when lastSeq >= watermark
//   target the watermark is settled and a convergence-latency sample
//   (watermark receive time → lastSeq catch-up) is recorded.
//
// This mirrors the client pull-scheduler minus jitter/min_interval: a stress
// generator wants the pull to happen immediately, not politely.

import (
	"sync"
	"time"
)

type pullTracker struct {
	mu      sync.Mutex
	lastSeq map[uint64]uint64 // conv → local cursor (highest seq ingested)
	target  map[uint64]uint64 // conv → latestSeq of the watermark being pulled
	arrived map[uint64]time.Time

	watermarks int64 // kind 14 frames seen
	syncsSent  int64 // kind 8 frames sent
	msgNews    int64 // kind 102 frames ingested (incl. catch-up replays)

	convSamples []time.Duration // convergence latency per settled watermark
}

func newPullTracker() *pullTracker {
	return &pullTracker{
		lastSeq: make(map[uint64]uint64, 64),
		target:  make(map[uint64]uint64, 64),
		arrived: make(map[uint64]time.Time, 64),
	}
}

// lastSeqOf reports the local cursor (0 = nothing ingested yet).
func (p *pullTracker) lastSeqOf(conv uint64) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastSeq[conv]
}

// onWatermark applies spec §2.1 steps 2–3. It returns the SYNC request to send
// (nil = drop / already covered by an in-flight pull).
func (p *pullTracker) onWatermark(conv, latestSeq uint64, now time.Time) *syncRequestPayload {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.watermarks++
	if latestSeq <= p.lastSeq[conv] {
		return nil // duplicate / out-of-order watermark: discard, zero requests
	}
	if tgt, inflight := p.target[conv]; inflight && latestSeq <= tgt {
		// a pull covering this watermark is already in flight; the 102 replay
		// will carry us past latestSeq — do not pile up duplicate SYNCs.
		return nil
	}
	p.target[conv] = latestSeq
	p.arrived[conv] = now
	p.syncsSent++
	return &syncRequestPayload{ConvID: conv, SinceSeq: p.lastSeq[conv] + 1, Limit: syncPullLimit}
}

// onMsgNew ingests one MSG_NEW (live fan-out or catch-up replay), advancing the
// cursor and settling any watermark it satisfies.
func (p *pullTracker) onMsgNew(conv, seq uint64, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgNews++
	if seq <= p.lastSeq[conv] {
		return
	}
	p.lastSeq[conv] = seq
	if tgt, ok := p.target[conv]; ok && seq >= tgt {
		if t0, ok2 := p.arrived[conv]; ok2 && len(p.convSamples) < 500000 {
			p.convSamples = append(p.convSamples, now.Sub(t0))
		}
		delete(p.target, conv)
		delete(p.arrived, conv)
	}
}

// counters snapshots the frame counters.
func (p *pullTracker) counters() (watermarks, syncs, msgNews int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.watermarks, p.syncsSent, p.msgNews
}

// convergence returns the settled watermark→catch-up latency samples.
func (p *pullTracker) convergence() []time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]time.Duration, len(p.convSamples))
	copy(out, p.convSamples)
	return out
}

// outstanding reports convs with a watermark not yet settled (diagnostic:
// >0 at end of window means the tail of the window was still converging).
func (p *pullTracker) outstanding() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.target)
}
