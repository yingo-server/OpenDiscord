package job

import (
	"context"
	"strings"
	"testing"

	"github.com/yingo-server/UFOT/server/internal/data/pipe/pipefakes"
	"github.com/yingo-server/UFOT/server/internal/data/policy"
	"github.com/yingo-server/UFOT/server/internal/data/store"
)

// ---------------------------------------------------------------- archiver e2e

// TestArchiverCopiesThenPrunesHot: with a cold store wired, the copy lands in
// the archive AND the hot side is pruned.
func TestArchiverCopiesThenPrunesHot(t *testing.T) {
	old := store.HotMsg{MsgID: 1, Cid: 5, Seq: 1, SenderID: 1, SentAt: 100, Content: []byte("a")}
	fresh := store.HotMsg{MsgID: 3, Cid: 5, Seq: 3, SenderID: 1, SentAt: 999999, Content: []byte("c")}
	s := &fakeScanner{rows: []store.HotMsg{old, fresh}}
	cold := &pipefakes.FakeArchive{}
	seedHot(cold, old, fresh)
	arch := &Archiver{S: s, W: cold, P: cold,
		Cutoff: func() int64 { return 1000 }, BatchSize: 10,
		Now: func() int64 { return cutoffNow }}

	res, err := arch.Run(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if res.MovedRows != 1 {
		t.Fatalf("moved = %d, want 1", res.MovedRows)
	}
	if len(cold.Copied) != 1 || cold.Copied[0].MsgID != 1 {
		t.Fatalf("archive rows = %+v, want the old row", cold.Copied)
	}
	// Hot side: only the fresh row survives.
	rows := cold.Rows()
	if len(rows) != 1 || rows[0].MsgID != 3 {
		t.Fatalf("hot rows after archive = %+v, want only the fresh row", rows)
	}
	if cold.DeleteCalls != 1 {
		t.Fatalf("hot prune calls = %d, want 1", cold.DeleteCalls)
	}
}

// seedHot pre-populates the FakeArchive's HOT side (the fake only populates
// it during CopyBatch, which the degraded/copy tests need from the start).
func seedHot(f *pipefakes.FakeArchive, rows ...store.HotMsg) {
	f.SeedHot(rows...)
}

// TestArchiverNoColdPrunesHotOnly: without a cold store (W == nil) the
// archiver degrades to a hot-only prune keeping the last N days — it must
// NOT error and NOT do nothing.
func TestArchiverNoColdPrunesHotOnly(t *testing.T) {
	old := store.HotMsg{MsgID: 1, Cid: 5, Seq: 1, SenderID: 1, SentAt: 100, Content: []byte("a")}
	fresh := store.HotMsg{MsgID: 3, Cid: 5, Seq: 3, SenderID: 1, SentAt: 999999, Content: []byte("c")}
	s := &fakeScanner{rows: []store.HotMsg{old, fresh}}
	pruner := &pipefakes.FakeArchive{}
	seedHot(pruner, old, fresh)

	arch := &Archiver{S: s, P: pruner, // W deliberately nil: no cold store
		Cutoff: func() int64 { return 1000 }, BatchSize: 10, KeepDays: 7,
		Now: func() int64 { return cutoffNow }}

	res, err := arch.Run(context.Background(), false)
	if err != nil {
		t.Fatalf("degraded run must not fail: %v", err)
	}
	if res.MovedRows != 1 {
		t.Fatalf("pruned = %d, want 1", res.MovedRows)
	}
	rows := pruner.Rows()
	if len(rows) != 1 || rows[0].MsgID != 3 {
		t.Fatalf("hot rows after degraded prune = %+v, want only the fresh row", rows)
	}
}

// TestArchiverNilColdNoPanic: W == nil and P == nil must never panic — nil
// deps surface as errors (a panic here used to take the whole process down).
func TestArchiverNilColdNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("archiver panicked with nil cold store: %v", r)
		}
	}()
	s := &fakeScanner{rows: []store.HotMsg{{MsgID: 1, Cid: 5, Seq: 1, SenderID: 1, SentAt: 100}}}
	// No pruner either: the run must return an error, not panic.
	arch := &Archiver{S: s, // W nil, P nil
		Cutoff: func() int64 { return 1000 }, BatchSize: 10,
		Now: func() int64 { return cutoffNow }}
	if _, err := arch.Run(context.Background(), false); err == nil {
		t.Fatal("nil pruner must surface an error")
	}

	// Pruner wired, cold nil: fine (degraded path).
	pruner := &pipefakes.FakeArchive{}
	arch.P = pruner
	if _, err := arch.Run(context.Background(), false); err != nil {
		t.Fatalf("degraded run with pruner: %v", err)
	}
}

// TestArchiverCopyFailKeepsHot: when the cold copy fails, the hot rows are
// NOT deleted (data safety first).
func TestArchiverCopyFailKeepsHot(t *testing.T) {
	old := store.HotMsg{MsgID: 1, Cid: 5, Seq: 1, SenderID: 1, SentAt: 100, Content: []byte("a")}
	s := &fakeScanner{rows: []store.HotMsg{old}}
	cold := &pipefakes.FakeArchive{FailCopy: true}

	arch := &Archiver{S: s, W: cold, P: cold,
		Cutoff: func() int64 { return 1000 }, BatchSize: 10,
		Now: func() int64 { return cutoffNow }}

	if _, err := arch.Run(context.Background(), false); err == nil {
		t.Fatal("copy failure must surface as an error")
	}
	// The hot row must still be there — nothing is deleted without a
	// committed copy. The fake's hot side starts empty on the copy-fail path
	// (CopyBatch never succeeded), so assert via DeleteCalls == 0.
	if cold.DeleteCalls != 0 {
		t.Fatalf("hot delete fired %d times after a FAILED copy — data loss risk", cold.DeleteCalls)
	}
}

// TestArchiverDegradedKeepDaysBoundsPrune: in degraded mode the prune never
// removes anything newer than KeepDays even if the cutoff would allow it.
func TestArchiverDegradedKeepDaysBoundsPrune(t *testing.T) {
	const now = int64(1_000_000)
	// 3 days old: the policy cutoff (1 day) would prune it, but the degraded
	// path is bounded by KeepDays=5, so it must survive. This is exactly the
	// invariant under test: the degraded prune is more conservative than policy.
	threeDaysOld := now - 3*24*3600
	older := now - 10*24*3600 // 10 days old: older than KeepDays → pruned
	s := &fakeScanner{rows: []store.HotMsg{
		{MsgID: 1, Cid: 5, Seq: 1, SenderID: 1, SentAt: threeDaysOld},
		{MsgID: 2, Cid: 5, Seq: 2, SenderID: 1, SentAt: older},
	}}
	pruner := &fakePruner{rows: map[int64]store.HotMsg{
		1: {MsgID: 1, SentAt: threeDaysOld}, 2: {MsgID: 2, SentAt: older},
	}}

	arch := &Archiver{S: s, P: pruner, // W nil: degraded
		Cutoff:   func() int64 { return now - 1*24*3600 }, // policy says 1 day
		KeepDays: 5, BatchSize: 10,
		Now: func() int64 { return now }}

	res, err := arch.Run(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if res.MovedRows != 1 {
		t.Fatalf("pruned = %d, want 1 (only the 10-day-old row)", res.MovedRows)
	}
	if _, ok := pruner.rows[1]; !ok {
		t.Fatal("the 3-day-old row must survive: the degraded prune is bounded by KeepDays, not by the 1-day policy cutoff")
	}
}

// fakePruner is a minimal HotPruner for the KeepDays test.
type fakePruner struct {
	rows map[int64]store.HotMsg
}

func (f *fakePruner) DeleteHotBefore(ctx context.Context, before int64) (int64, error) {
	var n int64
	for id, r := range f.rows {
		if r.SentAt < before {
			delete(f.rows, id)
			n++
		}
	}
	return n, nil
}

// ---------------------------------------------------------------- trimmer

// fakeTrimTarget records TrimTable calls.
type fakeTrimTarget struct {
	calls map[string]int64 // table → cutoff
	ret   map[string]int64 // table → rows removed
	err   map[string]error // table → forced error
}

func (f *fakeTrimTarget) TrimTable(ctx context.Context, table string, olderThan int64) (int64, error) {
	if f.calls == nil {
		f.calls = map[string]int64{}
	}
	f.calls[table] = olderThan
	if err, ok := f.err[table]; ok {
		return 0, err
	}
	if n, ok := f.ret[table]; ok {
		return n, nil
	}
	return 0, nil
}

func staticRules(rules ...policy.TrimRule) TrimRuleSource {
	return PolicyRules{Rules: rules}
}

// TestTrimmerRunsPolicyRules: policy-declared tables are trimmed with the
// correct cutoffs (retention_days → unix seconds).
func TestTrimmerRunsPolicyRules(t *testing.T) {
	target := &fakeTrimTarget{ret: map[string]int64{"sys_rate_limit": 3}}
	var now int64 = 1_000_000
	trim := &Trimmer{
		T:     target,
		Rules: staticRules(policy.TrimRule{Table: "sys_rate_limit", RetentionDays: 7}),
		Now:   func() int64 { return now },
	}
	res, err := trim.Run(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if res.MovedRows != 3 {
		t.Fatalf("trimmed = %d, want 3", res.MovedRows)
	}
	cutoff, ok := target.calls["sys_rate_limit"]
	if !ok {
		t.Fatal("sys_rate_limit was not trimmed")
	}
	if cutoff != now-7*24*3600 {
		t.Fatalf("sys_rate_limit cutoff = %d, want %d (7 days)", cutoff, now-7*24*3600)
	}
}

// TestTrimmerTTLHours: ttl_hours rules convert to a seconds cutoff.
func TestTrimmerTTLHours(t *testing.T) {
	target := &fakeTrimTarget{}
	var now int64 = 1_000_000
	trim := &Trimmer{
		T:     target,
		Rules: staticRules(policy.TrimRule{Table: "sys_rate_limit", TTLHours: 5}),
		Now:   func() int64 { return now },
	}
	if _, err := trim.Run(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if cutoff := target.calls["sys_rate_limit"]; cutoff != now-5*3600 {
		t.Fatalf("ttl_hours cutoff = %d, want %d", cutoff, now-5*3600)
	}
}

// TestTrimmerBuiltinRules: identity_refresh and media_media are always
// trimmed (built-in expiry semantics) even with no policy rules.
func TestTrimmerBuiltinRules(t *testing.T) {
	target := &fakeTrimTarget{}
	var now int64 = 1_000_000
	trim := &Trimmer{T: target, Now: func() int64 { return now }}
	if _, err := trim.Run(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if _, ok := target.calls["identity_refresh"]; !ok {
		t.Fatal("identity_refresh (built-in) was not trimmed")
	}
	if _, ok := target.calls["media_media"]; !ok {
		t.Fatal("media_media (built-in) was not trimmed")
	}
	// identity_refresh cutoff = now (rows expire by their own expires_at).
	if c := target.calls["identity_refresh"]; c != now {
		t.Fatalf("identity_refresh cutoff = %d, want %d", c, now)
	}
}

// TestTrimmerSkipsNonWhitelisted: a policy rule for a table outside the
// whitelist is ignored (defense in depth — the policy loader also rejects it).
func TestTrimmerSkipsNonWhitelisted(t *testing.T) {
	target := &fakeTrimTarget{}
	trim := &Trimmer{
		T:     target,
		Rules: staticRules(policy.TrimRule{Table: "evil_table", RetentionDays: 1}),
		Now:   func() int64 { return 1000 },
	}
	if _, err := trim.Run(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if _, ok := target.calls["evil_table"]; ok {
		t.Fatal("non-whitelisted table must never reach the store")
	}
}

// TestTrimmerOneFailureDoesNotStopOthers: a failing table is reported but the
// remaining tables are still trimmed.
func TestTrimmerOneFailureDoesNotStopOthers(t *testing.T) {
	target := &fakeTrimTarget{
		err: map[string]error{"sys_rate_limit": context.Canceled},
		ret: map[string]int64{"identity_refresh": 5},
	}
	trim := &Trimmer{
		T:     target,
		Rules: staticRules(policy.TrimRule{Table: "sys_rate_limit", RetentionDays: 7}),
		Now:   func() int64 { return 1000 },
	}
	res, err := trim.Run(context.Background(), false)
	if err == nil {
		t.Fatal("the failing table must surface its error")
	}
	if res.MovedRows != 5 {
		t.Fatalf("trimmed = %d, want 5 (the healthy table still ran)", res.MovedRows)
	}
}

// TestTrimmerNilTargetErrors: a nil target is a wiring error, not a panic.
func TestTrimmerNilTargetErrors(t *testing.T) {
	trim := &Trimmer{Now: func() int64 { return 1 }}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("trimmer panicked on nil target: %v", r)
		}
	}()
	if _, err := trim.Run(context.Background(), false); err == nil {
		t.Fatal("nil target must surface an error")
	}
}

// TestTrimmerDryRunDeletesNothing: dry-run touches no table.
func TestTrimmerDryRunDeletesNothing(t *testing.T) {
	target := &fakeTrimTarget{}
	trim := &Trimmer{T: target, Now: func() int64 { return 1000 },
		Rules: staticRules(policy.TrimRule{Table: "sys_rate_limit", RetentionDays: 7})}
	res, err := trim.Run(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun {
		t.Fatal("dry-run flag missing")
	}
	if len(target.calls) != 0 {
		t.Fatalf("dry-run must not delete, calls = %v", target.calls)
	}
}

// TestTrimmerRunsVacuum: the vacuum hook is invoked after a real pass and
// not during a dry run.
func TestTrimmerRunsVacuum(t *testing.T) {
	vacuumed := 0
	trim := &Trimmer{
		T:      &fakeTrimTarget{},
		Now:    func() int64 { return 1000 },
		Vacuum: func(ctx context.Context) error { vacuumed++; return nil },
	}
	if _, err := trim.Run(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if vacuumed != 1 {
		t.Fatalf("vacuum calls = %d, want 1", vacuumed)
	}
	if _, err := trim.Run(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if vacuumed != 1 {
		t.Fatalf("dry-run must not vacuum, calls = %d", vacuumed)
	}
}

// ---------------------------------------------------------------- jobber recover

// panicUnit is a broken job: Run panics. The jobber must recover and keep
// the process (and other jobs) alive.
type panicUnit struct{ fired int }

func (p *panicUnit) Name() string { return "panic-job" }
func (p *panicUnit) Run(ctx context.Context, dryRun bool) (Result, error) {
	panic("boom: job unit exploded")
}

// healthyUnit is a well-behaved job used to prove the scheduler survives the
// panicking sibling.
type healthyUnit struct{ ran int }

func (h *healthyUnit) Name() string { return "healthy-job" }
func (h *healthyUnit) Run(ctx context.Context, dryRun bool) (Result, error) {
	h.ran++
	return Result{MovedRows: 1}, nil
}

// TestJobberRecoversFromPanic: a panicking job returns an error (not a crash)
// and a subsequent trigger of the SAME job still works.
func TestJobberRecoversFromPanic(t *testing.T) {
	jb := NewJobber(nil)
	pu := &panicUnit{}
	hu := &healthyUnit{}
	jb.Register(pu, Spec{Kind: CronInterval, Interval: 3600})
	jb.Register(hu, Spec{Kind: CronInterval, Interval: 3600})

	// The panicking job: recovered, surfaced as an error, process alive.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("jobber did not recover the panic: %v", r)
			}
		}()
		if _, err := jb.Trigger(context.Background(), "panic-job", false); err == nil {
			t.Fatal("a panicking job must surface an error")
		} else if !strings.Contains(err.Error(), "panicked") {
			t.Fatalf("panic error must mention the panic, got: %v", err)
		}
	}()

	// The healthy job still runs — the process and scheduler are intact.
	res, err := jb.Trigger(context.Background(), "healthy-job", false)
	if err != nil || res.MovedRows != 1 || hu.ran != 1 {
		t.Fatalf("healthy job after sibling panic: res=%+v err=%v ran=%d", res, err, hu.ran)
	}

	// The panicked job can be triggered again (not wedged in "running").
	if _, err := jb.Trigger(context.Background(), "panic-job", false); err == nil {
		t.Fatal("second panic must also surface an error")
	}
	if _, err := jb.Trigger(context.Background(), "healthy-job", false); err != nil || hu.ran != 2 {
		t.Fatalf("healthy job wedged after second sibling panic: err=%v ran=%d", err, hu.ran)
	}
}
