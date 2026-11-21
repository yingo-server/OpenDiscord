package main

// jobs.go: the data layer's job plane wiring (mechanism assembled here,
// policy read from policy.json — docs §4 机制/策略分离).
//
//   - Archiver (job.Unit) reads hot rows older than policy.hot_retention_days
//     via sqlite.ScanOlder and moves them through pg.ColdDB
//     (store.ArchiveSource — the COLD copy) with the archive_runs ledger for
//     breakpoints. The HOT delete is done by the SQLite store itself
//     (store.HotPruner): the cold DB must never touch the hot table.
//   - Trimmer (job.Unit) executes policy.trim plus the built-in expiry rules
//     against the hot SQLite store (no cold DB needed).
//   - Jobber fires them per policy cron (job.ParseCronKind) and serves manual
//     triggers: a 980 JOB_TRIGGER frame on the data lane becomes
//     jobber.Trigger (pipe.Trunkee), replied with 981 JOB_STATUS.
//
// Degradation: without a cold DB the archiver degrades to a HOT-ONLY prune
// (keep the last N days) instead of doing nothing — the hot DB must not grow
// without bound. The trimmer always runs (all its tables are in the hot DB).

import (
	"context"
	"fmt"
	"time"

	"github.com/yingo-server/UFOT/server/internal/data/job"
	"github.com/yingo-server/UFOT/server/internal/data/policy"
	"github.com/yingo-server/UFOT/server/internal/data/store/pg"
	"github.com/yingo-server/UFOT/server/internal/data/store/sqlite"
	"github.com/yingo-server/UFOT/server/internal/pkg/log"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
)

// jobPlane bundles the scheduler and its units.
type jobPlane struct {
	jobber   *job.Jobber
	archiver *job.Archiver
	trimmer  *job.Trimmer
	gc       *job.MediaGC
	cancel   context.CancelFunc
}

// archiveCutoff turns policy.hot_retention_days into a unix-seconds cutoff
// (POLICY — the archiver only consumes the number).
func archiveCutoff(pol *policy.Store, now func() time.Time) func() int64 {
	return func() int64 {
		days := 0
		if pol != nil {
			days = pol.Current().Policy.HotRetentionDays
		}
		if days <= 0 {
			days = 90 // matches policy.Default(); a missing policy must not archive everything
		}
		return now().Unix() - int64(days)*24*3600
	}
}

// newJobPlane builds the jobber + archiver + trimmer + media GC. cold may be
// nil (degraded mode: the archiver prunes hot rows only, the trimmer runs as
// usual). mediaDir is the media plane's root; the GC runs only when it is set.
func newJobPlane(hot *sqlite.DB, cold *pg.ColdDB, pol *policy.Store, lg *log.Logger, mediaDir string) *jobPlane {
	now := time.Now
	batch := 1000
	hotRetentionDays := 90
	if pol != nil {
		if b := pol.Current().Policy.Archive.BatchSize; b > 0 {
			batch = b
		}
		if d := pol.Current().Policy.HotRetentionDays; d > 0 {
			hotRetentionDays = d
		}
	}
	arch := &job.Archiver{
		S: hot, // *sqlite.DB implements job.HotScanner via ScanOlder
		// W = the COLD copy side. pg.ColdDB implements store.ArchiveSource
		// (CopyBatch only — it must not delete hot rows).
		P:         hot, // *sqlite.DB implements store.HotPruner via DeleteHotBefore
		Cutoff:    archiveCutoff(pol, now),
		BatchSize: batch,
		// Degraded mode keeps the last N days in the hot store.
		KeepDays: hotRetentionDays,
	}
	if cold != nil {
		arch.W = cold
		arch.Ledger = cold // archive_runs ledger (breakpoint bookkeeping)
	}
	trim := &job.Trimmer{
		T:      hot, // *sqlite.DB implements store.TrimTarget via TrimTable
		Now:    func() int64 { return now().Unix() },
		Vacuum: hot.IncrementalVacuum,
		Log:    lg,
	}
	if pol != nil {
		trim.Rules = job.PolicyRules{Rules: pol.Current().Policy.Trim}
	}
	jb := job.NewJobber(lg)
	jb.Register(arch, archiveSpec(pol, lg))
	jb.Register(trim, trimSpec(pol, lg))

	// Media disk GC: the disk-side complement to the trimmer (trim shrinks the
	// DB, GC shrinks the disk). Only wired when media_dir is configured — with
	// no media plane there is nothing to reclaim.
	var gc *job.MediaGC
	if mediaDir != "" {
		gc = &job.MediaGC{
			Dir:   mediaDir,
			Index: hot, // *sqlite.DB implements job.MediaGCChecker
			Log:   lg,
		}
		jb.Register(gc, gcSpec(pol, lg))
	}
	return &jobPlane{jobber: jb, archiver: arch, trimmer: trim, gc: gc}
}

// archiveSpec parses policy.archive.cron. A malformed cron is a loud WARN +
// the documented default "0 4 * * *" (daily 04:00) — policy problems must be
// visible, but a job must still be schedulable.
func archiveSpec(pol *policy.Store, lg *log.Logger) job.Spec {
	cron := ""
	if pol != nil {
		cron = pol.Current().Policy.Archive.Cron
	}
	kind, h, m, err := job.ParseCronKind(cron)
	if err != nil {
		if lg != nil {
			lg.Warn("archive cron invalid; falling back to daily 04:00", "cron", cron, "err", err.Error())
		}
		return job.Spec{Kind: job.CronDaily, Hour: 4, Min: 0}
	}
	_ = kinds.KindJobTrigger // doc link
	switch kind {
	case job.CronInterval:
		return job.Spec{Kind: job.CronInterval, Interval: time.Duration(h) * time.Second}
	default:
		return job.Spec{Kind: job.CronDaily, Hour: h, Min: m}
	}
}

// trimSpec parses the trim schedule. The trim rules themselves live in
// policy.trim (no separate cron field yet): the trimmer runs daily at 03:30,
// half an hour before the archive pass, so expired rows are gone before the
// hot DB is pruned and vacuumed.
func trimSpec(pol *policy.Store, lg *log.Logger) job.Spec {
	_ = pol
	_ = lg
	return job.Spec{Kind: job.CronDaily, Hour: 3, Min: 30}
}

// gcSpec schedules the media disk GC. The desired cadence is "every Sunday at
// 04:00" (weekly, right after the weekend's trim/archive passes). The current
// jobber (job.ParseCronKind) supports only daily/interval triggers — a 5-field
// cron with a day-of-week field is REJECTED, and policy has no media-GC cron
// field yet. So, per the task's "policy 没有对应字段就硬编码并在注释里说明",
// this is hardcoded to a DAILY 04:00 pass, which is a safe superset of the
// weekly intent: the GC is idempotent, age-gated (24h) and file-capped
// (10000/pass), so running it daily just reclaims orphaned bytes sooner. To
// get true weekly-Sunday semantics, add a day-of-week aware cron kind to the
// jobber (see 未做项).
func gcSpec(pol *policy.Store, lg *log.Logger) job.Spec {
	_ = pol
	_ = lg
	return job.Spec{Kind: job.CronDaily, Hour: 4, Min: 0}
}

// start launches the scheduler loop. jobber.Run returns when ctx is canceled
// (SIGTERM): running archives stop at the batch boundary (archiver checks
// ctx.Err per batch) and resume idempotently on the next start.
func (jp *jobPlane) start(ctx context.Context) {
	if jp == nil || jp.jobber == nil {
		return
	}
	jctx, cancel := context.WithCancel(ctx)
	jp.cancel = cancel
	go jp.jobber.Run(jctx)
}

// stop cancels the scheduler (idempotent).
func (jp *jobPlane) stop() {
	if jp != nil && jp.cancel != nil {
		jp.cancel()
	}
}

// TriggerJob implements pipe.Trunkee: kind 980 → jobber.Trigger, single-flight
// (concurrent triggers answer job.ErrRunning, which the pipe maps to a busy
// 981 status).
func (jp *jobPlane) TriggerJob(ctx context.Context, name string, dryRun bool) (job.Result, error) {
	return jp.triggerJob(ctx, name, dryRun)
}

func (jp *jobPlane) triggerJob(ctx context.Context, name string, dryRun bool) (job.Result, error) {
	if jp == nil || jp.jobber == nil {
		return job.Result{}, fmt.Errorf("job plane not wired")
	}
	return jp.jobber.Trigger(ctx, name, dryRun)
}
