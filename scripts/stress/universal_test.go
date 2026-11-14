package main

// universal_test.go: pure-logic unit tests for the REL/universal-pull helpers
// (S-4 slow-consumer selection, pull JSON row extraction, S-6 comparison
// derivation and verdict). No network, no server, no build side effects.

import (
	"testing"
	"time"
)

func TestSlowForDeterministicSubset(t *testing.T) {
	// 5% of 1000 = 50 connections slowed, deterministic across calls.
	count := 0
	seen := map[int]bool{}
	for id := 0; id < 1000; id++ {
		slow, mult := slowFor(id, 1000, 0.05, 5)
		if mult != 1 && !slow {
			t.Fatalf("id=%d: mult set without slow", id)
		}
		if mult != 1 && mult != 5 {
			t.Fatalf("id=%d: unexpected multiplier %v", id, mult)
		}
		if slow {
			count++
			if seen[id] {
				t.Fatalf("id=%d selected twice", id)
			}
			seen[id] = true
		}
	}
	if count != 50 {
		t.Fatalf("slow count = %d, want 50", count)
	}
	// determinism
	for id := 0; id < 1000; id++ {
		slow, _ := slowFor(id, 1000, 0.05, 5)
		if slow != seen[id] {
			t.Fatalf("id=%d: non-deterministic selection", id)
		}
	}
}

func TestSlowForEdges(t *testing.T) {
	if slow, _ := slowFor(0, 100, 0, 5); slow {
		t.Fatal("ratio 0 must slow nobody")
	}
	if slow, _ := slowFor(0, 100, 0.1, 1); slow {
		t.Fatal("factor 1 is a no-op")
	}
	if slow, _ := slowFor(0, 100, 1.5, 2); !slow {
		t.Fatal("ratio clamped to 1 → at least id 0 slowed")
	}
	// tiny run: guarantee ≥1 injected connection
	any := false
	for id := 0; id < 10; id++ {
		if slow, _ := slowFor(id, 10, 0.01, 2); slow {
			any = true
		}
	}
	if !any {
		t.Fatal("ratio >0 on tiny run must inject at least one slow conn")
	}
}

func TestPullRowFromLevel(t *testing.T) {
	r := &levelResult{
		Conns: 100, WindowSecs: 10, PullMode: true,
		DownWatermarks: 500, DownMsgNew: 4000, DownCtrl: 900,
		SyncRequests: 480, DownRateLimited: 7, ConvergenceOpen: 3,
		Convergence: []time.Duration{10 * time.Millisecond, 20 * time.Millisecond},
		Ops: map[string]*opAgg{
			"msg_send": {Attempted: 200, Acked: 190},
		},
	}
	row := pullRowFromLevel(r)
	if row.Conns != 100 || !row.UniversalPull {
		t.Fatalf("basic fields: %+v", row)
	}
	if row.WatermarkPerSec != 50 || row.MsgNewPerSec != 400 || row.CtrlPerSec != 90 {
		t.Fatalf("per-second split: %+v", row)
	}
	if row.SyncQPS != 48 {
		t.Fatalf("sync qps = %v", row.SyncQPS)
	}
	if row.RateLimited != 7 || row.SlowInjected != 0 {
		t.Fatalf("counters: %+v", row)
	}
	if row.AvailPct != 95 {
		t.Fatalf("availability = %v, want 95", row.AvailPct)
	}
	if row.ConvSamples != 2 || row.ConvOpen != 3 {
		t.Fatalf("convergence meta: %+v", row)
	}
}

func TestAdaptiveCompareRowsAndVerdict(t *testing.T) {
	s := pullJSONRow{WatermarkPerSec: 50, SyncQPS: 48, ConvP50ms: 10, ConvP95ms: 40,
		RateLimited: 2, AvailPct: 99, ConvOpen: 1}
	// healthy adaptive: within jitter bound
	a := pullJSONRow{WatermarkPerSec: 48, SyncQPS: 46, ConvP50ms: 12, ConvP95ms: 55,
		RateLimited: 3, AvailPct: 99, ConvOpen: 1}
	c := adaptiveCompareRows(s, a)
	if c["conv_p95_ms_adaptive"] != 55 {
		t.Fatalf("compare rows: %+v", c)
	}
	if v := s6Verdict(c); v == "" || v[:4] == "FAIL" {
		t.Fatalf("verdict should not fail here: %v", v)
	}
	// broken adaptive: p95 jitter > 2×
	bad := a
	bad.ConvP95ms = 100
	c2 := adaptiveCompareRows(s, bad)
	if v := s6Verdict(c2); v[:4] != "FAIL" {
		t.Fatalf(">2× p95 jitter must fail, got %v", v)
	}
	// regression on availability warns
	reg := a
	reg.AvailPct = 90
	c3 := adaptiveCompareRows(s, reg)
	if v := s6Verdict(c3); v[:4] != "WARN" {
		t.Fatalf("availability regression must warn, got %v", v)
	}
}
