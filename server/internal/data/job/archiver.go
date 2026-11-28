package job

import (
	"context"
	"fmt"
	"time"

	"github.com/yingo-server/UFOT/server/internal/data/store"
)

// HotScanner is the store-wide old-row scan the archiver needs. MsgReader
// (api.go, frozen) is per-cid; a full-store scan is this separate narrow port.
// *sqlite.DB satisfies it via msgReader.ScanOlder (rides idx_msg_sent); the
// pipefakes and tests provide their own.
type HotScanner interface {
	ScanOlder(ctx context.Context, cutoff int64, limit int) ([]store.HotMsg, error)
}

// Archiver moves hot rows older than the policy cutoff into the cold archive.
// It knows ONLY HotScanner + ArchiveSource (cold copy) + HotPruner (hot delete)
// + a PolicySource — how old is "old" is POLICY (docs §4: archiver 不知道归档多旧).
//
// Two independent dependencies are injected on purpose (the old single
// ArchiveTarget conflated them, and pg.ColdDB was wrongly deleting a hot table
// that only exists in SQLite):
//
//	W (store.ArchiveSource) — the COLD side: CopyBatch only. May be nil when
//	    the deployment has no cold DB (a documented skeleton form). It is
//	    NEVER asked to delete hot rows.
//	P (store.HotPruner)     — the HOT side: DeleteHotBefore. Always the SQLite
//	    store; this is the only thing that actually shrinks the hot DB.
//
// Mechanism/policy split: the cutoff VALUE is computed by the assembly from
// policy.hot_retention_days and passed through the Cutoff func; the scan
// definition (sent_at < cutoff ORDER BY sent_at ASC LIMIT n) lives in the
// store layer (ScanOlder).
type Archiver struct {
	S      HotScanner          // store-wide old-row scan
	W      store.ArchiveSource // cold copy; nil = degraded (hot-only prune)
	P      store.HotPruner     // hot delete; required
	Cutoff func() int64        // policy source: unix seconds; rows with sent_at < cutoff are archivable

	// KeepDays, when W == nil (degraded mode), bounds the hot-only prune so the
	// delete never removes anything newer than this many days even if the
	// caller's cutoff says otherwise. 0 falls back to the policy cutoff.
	KeepDays int

	BatchSize int           // rows per copy transaction (policy archive.batch_size)
	Ledger    ArchiveLedger // optional run ledger (breakpoint bookkeeping)
	Now       func() int64  // test seam
}

// ArchiveLedger is the archive_runs ledger seam (pg.ColdDB satisfies it).
// Nil disables bookkeeping (tests without a cold DB).
type ArchiveLedger interface {
	StartRun(ctx context.Context, startedAt int64) (int64, error)
	FinishRun(ctx context.Context, runID, finishedAt, moved int64, status string) error
}

// Name implements Unit.
func (a *Archiver) Name() string { return "archive" }

// Run executes one archive pass.
//
// Degradation paths (data safety first):
//   - WIRED (W != nil): copy the batch to the cold archive, THEN (and only
//     after the copy committed) prune the hot rows.
//   - NO COLD (W == nil): skip the copy and prune the hot store directly,
//     keeping the last N days (KeepDays, else the policy cutoff). This stops
//     the "hot DB only grows" bleeding instead of doing nothing.
//   - COPY FAILED: the pass aborts WITHOUT deleting anything — a row is never
//     lost from hot unless it is already in the archive.
//
// Idempotent by construction: CopyBatch uses ON CONFLICT DO NOTHING and
// DeleteHotBefore only fires after the copy of those rows COMMITTED. Re-running
// a crashed pass re-copies (0 new rows) and re-prunes nothing that was not
// already copied. SIGTERM (ctx cancel) stops at the batch boundary.
func (a *Archiver) Run(ctx context.Context, dryRun bool) (Result, error) {
	if a.P == nil {
		return Result{}, fmt.Errorf("archiver: no hot pruner wired (assembly must set P, the HotPruner port)")
	}
	now := time.Now().Unix()
	if a.Now != nil {
		if n := a.Now(); n != 0 {
			now = n
		}
	}
	cutoff := a.Cutoff()
	if cutoff <= 0 {
		return Result{}, fmt.Errorf("archiver: invalid cutoff %d (policy hot_retention_days missing?)", cutoff)
	}
	// Degraded mode: never prune newer than KeepDays, even if the policy
	// cutoff is more aggressive (a no-cold deployment has no archive safety
	// net, so be conservative). The effective cutoff is the EARLIER (more
	// conservative) of the two — deleting less, never more.
	degraded := a.W == nil
	if degraded && a.KeepDays > 0 {
		keep := now - int64(a.KeepDays)*24*3600
		if keep < cutoff {
			cutoff = keep
		}
	}
	batch := a.BatchSize
	if batch <= 0 {
		batch = 1000
	}

	if dryRun {
		return a.dryRun(ctx, cutoff, batch, degraded)
	}

	var runID int64
	if a.Ledger != nil {
		id, err := a.Ledger.StartRun(ctx, now)
		if err != nil {
			return Result{}, fmt.Errorf("archiver: ledger start: %w", err)
		}
		runID = id
	}
	fail := func() {
		if a.Ledger != nil {
			a.Ledger.FinishRun(ctx, runID, now, 0, "failed")
		}
	}

	var total int64
	// watermark guards against a scan that does not advance (stale cutoff, or
	// a reader ignoring deletes): without it a non-advancing batch would loop
	// forever and pin the jobber's single-flight slot.
	var watermark int64
	maxBatches := 10000 // belt and braces: hard cap on batches per pass
	for batchNo := 0; batchNo < maxBatches; batchNo++ {
		if ctx.Err() != nil {
			fail()
			return Result{MovedRows: total}, ctx.Err()
		}
		if degraded {
			// No cold archive: a single hot-only prune keeps the last N days.
			// DeleteHotBefore rides idx_msg_sent and is bounded by the cutoff.
			deleted, err := a.P.DeleteHotBefore(ctx, cutoff)
			if err != nil {
				fail()
				return Result{MovedRows: total}, fmt.Errorf("archiver: hot prune: %w", err)
			}
			total += deleted
			detail := fmt.Sprintf("degraded (no cold store): pruned %d hot rows older than cutoff %d (keeping %d days)", deleted, cutoff, a.KeepDays)
			if a.Ledger != nil {
				if err := a.Ledger.FinishRun(ctx, runID, now, total, "ok"); err != nil {
					return Result{MovedRows: total}, fmt.Errorf("archiver: ledger finish: %w", err)
				}
			}
			return Result{MovedRows: total, DryRun: false, Detail: detail}, nil
		}
		// read one batch of archivable rows (oldest first, sent_at < cutoff).
		rows, err := a.archivable(ctx, cutoff, batch)
		if err != nil {
			fail()
			return Result{MovedRows: total}, fmt.Errorf("archiver: read batch: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		var batchMax int64
		for _, r := range rows {
			if r.MsgID > batchMax {
				batchMax = r.MsgID
			}
		}
		if batchMax <= watermark {
			// Source did not advance past what we already handled.
			break
		}
		watermark = batchMax
		// copy FIRST (idempotent), THEN delete hot — the archiver's ordering
		// contract with the cold side (api.go ArchiveSource).
		moved, err := a.W.CopyBatch(ctx, rows)
		if err != nil {
			// Copy failed: DO NOT delete. Data safety first — a hot row is only
			// ever dropped once it is provably in the archive.
			fail()
			return Result{MovedRows: total}, fmt.Errorf("archiver: copy batch: %w", err)
		}
		// Confirmed in archive: now prune the hot side for the same cutoff.
		deleted, err := a.P.DeleteHotBefore(ctx, cutoff)
		if err != nil {
			fail()
			return Result{MovedRows: total}, fmt.Errorf("archiver: delete hot: %w", err)
		}
		total += moved
		// No-progress guard: a batch that neither copied nor deleted anything
		// means the source is not advancing (rows outside the cutoff, or a
		// reader that ignores deletes). Looping forever would pin the jobber's
		// single-flight slot and starve every other job.
		if moved == 0 && deleted == 0 {
			break
		}
		if len(rows) < batch {
			break
		}
	}
	if a.Ledger != nil {
		if err := a.Ledger.FinishRun(ctx, runID, now, total, "ok"); err != nil {
			return Result{MovedRows: total}, fmt.Errorf("archiver: ledger finish: %w", err)
		}
	}
	return Result{MovedRows: total, DryRun: false}, nil
}

// dryRun uses the SAME scan the real run uses (defect fix: previously a
// separate sample path that disagreed with the mover).
func (a *Archiver) dryRun(ctx context.Context, cutoff int64, batch int, degraded bool) (Result, error) {
	if degraded {
		rows, err := a.archivable(ctx, cutoff, batch)
		if err != nil {
			return Result{}, err
		}
		seen := map[int64]bool{}
		for _, r := range rows {
			seen[r.Cid] = true
		}
		return Result{MovedRows: int64(len(rows)), ConvsTouched: len(seen), DryRun: true,
			Detail: fmt.Sprintf("degraded (no cold store): would prune %d hot rows older than cutoff %d (keeping %d days)",
				len(rows), cutoff, a.KeepDays)}, nil
	}
	rows, err := a.archivable(ctx, cutoff, batch)
	if err != nil {
		return Result{}, err
	}
	seen := map[int64]bool{}
	for _, r := range rows {
		seen[r.Cid] = true
	}
	n := int64(len(rows))
	return Result{MovedRows: n, ConvsTouched: len(seen), DryRun: true,
		Detail: fmt.Sprintf("would archive %d rows from %d conversations (cutoff %d)", n, len(seen), cutoff)}, nil
}

// archivable selects one oldest batch of hot rows with sent_at < cutoff.
// The cutoff VALUE is policy (set by the assembly from hot_retention_days);
// the scan itself is the store's ScanOlder (mechanism).
func (a *Archiver) archivable(ctx context.Context, cutoff int64, batch int) ([]store.HotMsg, error) {
	if a.S == nil {
		return nil, fmt.Errorf("archiver: no hot scanner wired (assembly must set S, the HotScanner port)")
	}
	return a.S.ScanOlder(ctx, cutoff, batch)
}
