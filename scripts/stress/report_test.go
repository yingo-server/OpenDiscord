package main

// Table-driven unit tests for the three pure measurement functions. No network,
// no server, no clock dependency — these are the parts the report's numbers rest
// on, so they must be provably correct in CI.

import (
	"testing"
	"time"
)

func TestPercentile(t *testing.T) {
	ms := func(ns ...int) []time.Duration {
		out := make([]time.Duration, len(ns))
		for i, v := range ns {
			out[i] = time.Duration(v) * time.Millisecond
		}
		return out
	}
	cases := []struct {
		name    string
		samples []time.Duration
		p       float64
		want    time.Duration
	}{
		{"empty", nil, 0.95, 0},
		{"single", ms(7), 0.50, 7 * time.Millisecond},
		{"single-p99", ms(7), 0.99, 7 * time.Millisecond},
		// 1..10 ms, nearest-rank:
		// p50 -> rank ceil(5)=5 -> 5ms ; p95 -> ceil(9.5)=10 -> 10ms ; p99 ->10ms
		{"p50-of-1..10", ms(1, 2, 3, 4, 5, 6, 7, 8, 9, 10), 0.50, 5 * time.Millisecond},
		{"p95-of-1..10", ms(1, 2, 3, 4, 5, 6, 7, 8, 9, 10), 0.95, 10 * time.Millisecond},
		{"p99-of-1..10", ms(1, 2, 3, 4, 5, 6, 7, 8, 9, 10), 0.99, 10 * time.Millisecond},
		{"p0-min", ms(9, 1, 5), 0.0, 1 * time.Millisecond},
		{"p1-max", ms(9, 1, 5), 1.0, 9 * time.Millisecond},
		{"unsorted-input", ms(30, 10, 20), 0.50, 20 * time.Millisecond},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := percentile(c.samples, c.p)
			if got != c.want {
				t.Fatalf("percentile(%v, %.2f) = %v, want %v", c.samples, c.p, got, c.want)
			}
		})
	}
}

func TestPercentileDoesNotMutateInput(t *testing.T) {
	in := []time.Duration{3 * time.Millisecond, 1 * time.Millisecond, 2 * time.Millisecond}
	_ = percentile(in, 0.5)
	if in[0] != 3*time.Millisecond || in[1] != 1*time.Millisecond || in[2] != 2*time.Millisecond {
		t.Fatalf("percentile mutated its input: %v", in)
	}
}

func TestAvailability(t *testing.T) {
	cases := []struct {
		name      string
		acked     int64
		attempted int64
		want      float64
	}{
		{"zero-attempted-is-zero", 0, 0, 0},
		{"all-acked", 100, 100, 100},
		{"half", 50, 100, 50},
		{"none-acked", 0, 100, 0},
		{"99-of-100", 99, 100, 99},
		{"clamped-over-100", 150, 100, 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := availability(c.acked, c.attempted)
			if got != c.want {
				t.Fatalf("availability(%d,%d) = %v, want %v", c.acked, c.attempted, got, c.want)
			}
		})
	}
}

func TestMergeSamples(t *testing.T) {
	a := []time.Duration{1 * time.Millisecond, 2 * time.Millisecond}
	b := []time.Duration{3 * time.Millisecond}
	var empty []time.Duration
	cases := []struct {
		name  string
		parts [][]time.Duration
		want  []time.Duration
	}{
		{"empty-parts", nil, []time.Duration{}},
		{"single-part", [][]time.Duration{a}, a},
		{"two-parts", [][]time.Duration{a, b}, []time.Duration{1 * time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond}},
		{"with-empty", [][]time.Duration{a, empty, b}, []time.Duration{1 * time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond}},
		{"all-empty", [][]time.Duration{empty, empty}, []time.Duration{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mergeSamples(c.parts)
			if len(got) != len(c.want) {
				t.Fatalf("len = %d, want %d (%v)", len(got), len(c.want), got)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("mergeSamples[%d] = %v, want %v", i, got[i], c.want[i])
				}
			}
			if got == nil {
				t.Fatalf("mergeSamples returned nil, want non-nil empty slice")
			}
		})
	}
}

func TestRingWraps(t *testing.T) {
	r := newRing(3)
	for _, v := range []int{1, 2, 3, 4, 5} {
		r.add(time.Duration(v) * time.Millisecond)
	}
	got := r.snapshot()
	want := []time.Duration{3 * time.Millisecond, 4 * time.Millisecond, 5 * time.Millisecond}
	if len(got) != len(want) {
		t.Fatalf("ring len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ring[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}
