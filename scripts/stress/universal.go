package main

// universal.go implements the REL batch (lts-1.4-plan §6 + universal-pull-spec
// §4): the --universal-pull stress mode and the static-vs-adaptive comparison
// report used for the S-6 gate.
//
// Everything here is client-side measurement scaffolding on top of the existing
// pull-mode machinery (pull.go / workload.go); no server files are touched.
//
// Modes added:
//   --universal-pull         all sessions are watermark-driven (kind 14 → 8 SYNC,
//                            reusing pullTracker); reports watermark/s, SYNC QPS,
//                            convergence latency and the three-way downstream
//                            frame split (watermark / msg_new / control).
//   --slow-ratio --slow-factor  S-4 slow-consumer injection: the given fraction of
//                            connections pace their sends slowFactor× slower
//                            (mimicking a stalled consumer without breaking the
//                            read loop).
//   -mode=restart-storm      S-5 restart storm (already implemented in storm.go,
//                            reused unchanged).
//   --adaptive-compare a.json,b.json   offline: merge two universal-pull result
//                            JSONs (static run vs adaptive run of the SAME
//                            scenario) and print the batch/interval effective
//                            evidence + rate-limit hit counts for S-6 judgement.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// slowFor decides whether worker id is a slow consumer and its pacing
// multiplier. Pure function (unit-tested): ratio is the fraction [0..1] of
// connections slowed; factor is the pacing multiplier (>=1). Deterministic
// even spacing (stride sampling) so injection is reproducible run to run.
func slowFor(id, total int, ratio float64, factor float64) (slow bool, mult float64) {
	if total <= 0 || ratio <= 0 || factor <= 1 {
		return false, 1
	}
	if ratio > 1 {
		ratio = 1
	}
	slowCount := int(ratio * float64(total))
	if slowCount == 0 && ratio > 0 {
		slowCount = 1 // guarantee at least one injected connection for tiny runs
	}
	if slowCount > total {
		slowCount = total
	}
	if slowCount == 0 {
		return false, 1
	}
	stride := float64(total) / float64(slowCount)
	for k := 0; k < slowCount; k++ {
		if id == int(float64(k)*stride) {
			return true, factor
		}
	}
	return false, 1
}

// ---------------------------------------------------------------------------
// result JSON round-trip for the adaptive comparison
// ---------------------------------------------------------------------------

// pullJSONRow is the pull/universal-pull subset persisted per level row. These
// fields were previously write-only (not in jsonRow); the comparison needs them
// back, so they are added to the result document alongside the existing schema
// (additive only — old readers ignore unknown keys).
type pullJSONRow struct {
	Conns           int     `json:"conns"`
	UniversalPull   bool    `json:"universal_pull"`
	WatermarkPerSec float64 `json:"watermark_per_sec"`
	MsgNewPerSec    float64 `json:"msg_new_per_sec"`
	CtrlPerSec      float64 `json:"ctrl_per_sec"`
	SyncQPS         float64 `json:"sync_qps"`
	RateLimited     int64   `json:"rate_limited_hits"`
	SlowInjected    int     `json:"slow_injected"`
	ConvP50ms       float64 `json:"convergence_p50_ms"`
	ConvP95ms       float64 `json:"convergence_p95_ms"`
	ConvSamples     int     `json:"convergence_samples"`
	ConvOpen        int64   `json:"convergence_open"`
	AvailPct        float64 `json:"availability_pct"`
}

type resultDoc struct {
	Levels []jsonRow      `json:"levels"`
	Pull   []pullJSONRow  `json:"pull_levels,omitempty"`
	Extra  map[string]any `json:"-"`
}

// pullRowFromLevel extracts the pull-mode measurement block of one level into
// its JSON form. Pure function (unit-tested).
func pullRowFromLevel(r *levelResult) pullJSONRow {
	conv := r.Convergence
	return pullJSONRow{
		Conns:           r.Conns,
		UniversalPull:   r.PullMode,
		WatermarkPerSec: perSec(r.DownWatermarks, r.WindowSecs),
		MsgNewPerSec:    perSec(r.DownMsgNew, r.WindowSecs),
		CtrlPerSec:      perSec(r.DownCtrl, r.WindowSecs),
		SyncQPS:         perSec(r.SyncRequests, r.WindowSecs),
		RateLimited:     r.DownRateLimited,
		ConvP50ms:       msDur(conv, 0.50),
		ConvP95ms:       msDur(conv, 0.95),
		ConvSamples:     len(conv),
		ConvOpen:        r.ConvergenceOpen,
		AvailPct:        availability(r.Ops["msg_send"].Acked, r.Ops["msg_send"].Attempted),
	}
}

// adaptiveCompareRows is the pure core of the S-6 comparison: given the static
// and adaptive rows of the SAME scenario it derives the deltas the S-6 gate
// cares about (universal-pull-spec §4.1: convergence p95 jitter ≤2×, no
// availability regression, rate-limit hits recorded on both sides).
func adaptiveCompareRows(s, a pullJSONRow) map[string]float64 {
	out := map[string]float64{
		"wm_per_sec_static":         s.WatermarkPerSec,
		"wm_per_sec_adaptive":       a.WatermarkPerSec,
		"sync_qps_static":           s.SyncQPS,
		"sync_qps_adaptive":         a.SyncQPS,
		"conv_p95_ms_static":        s.ConvP95ms,
		"conv_p95_ms_adaptive":      a.ConvP95ms,
		"conv_p50_ms_static":        s.ConvP50ms,
		"conv_p50_ms_adaptive":      a.ConvP50Ms(),
		"rate_limited_static":       float64(s.RateLimited),
		"rate_limited_adaptive":     float64(a.RateLimited),
		"availability_static":       s.AvailPct,
		"availability_adaptive":     a.AvailPct,
		"convergence_open_static":   float64(s.ConvOpen),
		"convergence_open_adaptive": float64(a.ConvOpen),
	}
	return out
}

// ConvP50Ms keeps the struct literal above readable without an unkeyed field.
func (p pullJSONRow) ConvP50Ms() float64 { return p.ConvP50ms }

// s6Verdict applies the S-6 pre-check on the comparison numbers. This is a
// client-side PLAUSIBILITY check only (the authoritative gate runs on the
// server-side adaptive dump per spec §4.1 S6-1..S6-5); it flags gross jitter
// and regressions so a broken run is not mistaken for evidence.
func s6Verdict(c map[string]float64) string {
	base := c["conv_p95_ms_static"]
	adapt := c["conv_p95_ms_adaptive"]
	if base > 0 && adapt > 2*base {
		return "FAIL: adaptive convergence p95 > 2× static (S6-3 jitter bound)"
	}
	if c["availability_adaptive"] < c["availability_static"] {
		return "WARN: adaptive availability below static baseline"
	}
	return "PASS (client-side plausibility; server-side S6-1..S6-5 dump evidence still required)"
}

// runAdaptiveCompare loads two result JSONs and prints the S-6 evidence block.
func runAdaptiveCompare(spec string) {
	var sPath, aPath string
	if parts := strings.SplitN(spec, ",", 2); len(parts) == 2 {
		sPath, aPath = parts[0], parts[1]
	}
	if sPath == "" || aPath == "" {
		fmt.Println("adaptive-compare: need two comma-separated result JSONs (static,adaptive)")
		osExit(2)
		return
	}
	sDoc, err := readPullRows(sPath)
	if err != nil {
		fmt.Println("adaptive-compare static:", err)
		osExit(2)
		return
	}
	aDoc, err := readPullRows(aPath)
	if err != nil {
		fmt.Println("adaptive-compare adaptive:", err)
		osExit(2)
		return
	}
	if len(sDoc) == 0 || len(aDoc) == 0 {
		fmt.Println("adaptive-compare: one of the result files has no pull-mode levels " +
			"(both runs must be recorded with --universal-pull)")
		osExit(2)
		return
	}
	s, a := sDoc[len(sDoc)-1], aDoc[len(aDoc)-1]
	fmt.Println("\n== S-6 adaptive comparison (same scenario, last level) ==")
	fmt.Printf("%-28s %14s %14s\n", "metric", "static", "adaptive")
	c := adaptiveCompareRows(s, a)
	rows := []struct {
		label  string
		sk, ak string
	}{
		{"watermark frames/s", "wm_per_sec_static", "wm_per_sec_adaptive"},
		{"SYNC QPS (kind 8/s)", "sync_qps_static", "sync_qps_adaptive"},
		{"convergence p50 ms", "conv_p50_ms_static", "conv_p50_ms_adaptive"},
		{"convergence p95 ms", "conv_p95_ms_static", "conv_p95_ms_adaptive"},
		{"rate-limit hits (13)", "rate_limited_static", "rate_limited_adaptive"},
		{"availability %", "availability_static", "availability_adaptive"},
		{"unsettled watermarks", "convergence_open_static", "convergence_open_adaptive"},
	}
	for _, r := range rows {
		fmt.Printf("%-28s %14.2f %14.2f\n", r.label, c[r.sk], c[r.ak])
	}
	fmt.Printf("\nbatch/interval 生效样本: 需对照两侧 SIGUSR1 dump 的 adaptive 子表与 " +
		"adaptive.change/reject/fallback 审计事件回放（spec §4.1 S6-4），压测端 JSON 不承载该数据。\n")
	fmt.Println("verdict:", s6Verdict(c))
}

func readPullRows(path string) ([]pullJSONRow, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Pull []pullJSONRow `json:"pull_levels"`
	}
	if err := json.Unmarshal(blob, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return doc.Pull, nil
}
