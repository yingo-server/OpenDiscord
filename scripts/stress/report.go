package main

// report.go holds the measurement primitives: per-worker ring buffers, the
// pure aggregation helpers (percentile / availability / merge) and the honest
// report rendering. The three pure functions are unit-tested in report_test.go
// and must not depend on the network or a running server.

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ring is a fixed-capacity circular buffer of latency samples. Each worker owns
// one (or one per op) so two goroutines never share the same append path — the
// defect that made the old p95/avg statistically meaningless.
type ring struct {
	buf  []time.Duration
	size int
	next int
}

func newRing(capacity int) *ring {
	if capacity <= 0 {
		capacity = 256
	}
	return &ring{buf: make([]time.Duration, capacity)}
}

func (r *ring) add(d time.Duration) {
	if len(r.buf) == 0 {
		return
	}
	r.buf[r.next] = d
	r.next = (r.next + 1) % len(r.buf)
	if r.size < len(r.buf) {
		r.size++
	}
}

// snapshot returns the samples in insertion order (already de-ringed).
func (r *ring) snapshot() []time.Duration {
	out := make([]time.Duration, 0, r.size)
	start := 0
	if r.size == len(r.buf) {
		start = r.next
	}
	for i := 0; i < r.size; i++ {
		out = append(out, r.buf[(start+i)%len(r.buf)])
	}
	return out
}

// mergeSamples concatenates worker sample slices into one, preserving every
// sample (it does not de-duplicate: two workers can legitimately record the
// same duration). The returned slice is never nil so callers can range safely.
func mergeSamples(parts [][]time.Duration) []time.Duration {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]time.Duration, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// percentile returns the p-quantile (0..1) using the nearest-rank method on a
// sorted copy of the samples. This is a *real* quantile — the old tool reused a
// single shared atomic (lastLat) so its "p95" was one sample repeated 100×.
// Empty input returns 0. p is clamped to [0,1].
func percentile(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	if p < 0 {
		p = 0
	}
	if p > 1 {
		p = 1
	}
	cp := make([]time.Duration, len(samples))
	copy(cp, samples)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	// nearest-rank: rank = ceil(p * n), index = rank-1, clamped.
	rank := int(float64(len(cp)) * p)
	if float64(rank) < float64(len(cp))*p {
		rank++
	}
	if rank < 1 {
		rank = 1
	}
	if rank > len(cp) {
		rank = len(cp)
	}
	return cp[rank-1]
}

// availability returns acked/attempted as a percentage. A zero denominator is
// NOT 100% — nothing sent means nothing proven, so it returns 0. This mirrors
// the corrected rule: availability = received_acks / attempted_sends.
func availability(acked, attempted int64) float64 {
	if attempted <= 0 {
		return 0
	}
	v := float64(acked) / float64(attempted) * 100
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// ---------------------------------------------------------------------------
// Aggregated view used by the reporters
// ---------------------------------------------------------------------------

// opAgg is the merged, per-level view of one operation type.
type opAgg struct {
	Attempted  int64
	Acked      int64 // msg_send: acks with status==0 (the success definition)
	OK         int64 // write-ok count (non-ack ops: write succeeded)
	WriteErr   int64
	ServerDrop int64
	Samples    []time.Duration
}

// successRate is what "成功率" means for one op:
//   - msg_send: acked/attempted (ACK rate — server round trip proven)
//   - others  : ok/attempted (write accepted by the client socket)
func (o *opAgg) successRate() float64 {
	if o.Attempted <= 0 {
		return 0
	}
	num := o.OK
	if o.Acked > 0 {
		num = o.Acked
	}
	return float64(num) / float64(o.Attempted) * 100
}

// latencySamples returns the raw samples for quantile computation.
func (o *opAgg) latencySamples() []time.Duration { return o.Samples }

// fmtDur renders a duration compactly for the table (µs when small).
func fmtDur(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	if d < time.Millisecond {
		return fmt.Sprintf("%dµs", d.Microseconds())
	}
	return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000.0)
}

// fmtLat3 renders p50/p95/p99 as p50/p95/p99 string.
func fmtLat3(samples []time.Duration) string {
	return fmt.Sprintf("%s/%s/%s", fmtDur(percentile(samples, 0.50)),
		fmtDur(percentile(samples, 0.95)), fmtDur(percentile(samples, 0.99)))
}

// printPullStats renders the §4.4 item-2 pull-mode statistic block. Called
// only for --pull-mode runs (all counters are zero in push mode).
func printPullStats(r *levelResult) {
	if !r.PullMode {
		return
	}
	fmt.Printf("\npull-mode (§4.4): down frames/s: watermark=%.1f msg_new=%.1f ctrl=%.1f\n",
		perSec(r.DownWatermarks, r.WindowSecs),
		perSec(r.DownMsgNew, r.WindowSecs),
		perSec(r.DownCtrl, r.WindowSecs))
	fmt.Printf("client kind-8 SYNC QPS: %.1f (requests=%d)\n",
		perSec(r.SyncRequests, r.WindowSecs), r.SyncRequests)
	conv := r.Convergence
	fmt.Printf("convergence (watermark latestSeq → lastSeq caught up): p50=%s p95=%s samples=%d unsettled=%d\n",
		fmtDur(percentile(conv, 0.50)), fmtDur(percentile(conv, 0.95)),
		len(conv), r.ConvergenceOpen)
	// REL/S-6 evidence line: rate-limit hits (kind 13) as a proxy for the
	// adaptive controller's rate-limit-hit counter on the server dump.
	fmt.Printf("rate-limit hits (kind 13): %d\n", r.DownRateLimited)
}

// heading helpers ------------------------------------------------------------

func rule(ch string, n int) string { return strings.Repeat(ch, n) }
