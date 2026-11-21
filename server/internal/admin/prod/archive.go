package prod

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/yingo-server/UFOT/server/internal/admin/service"
	"github.com/yingo-server/UFOT/server/internal/data/policy"
	"github.com/yingo-server/UFOT/server/internal/data/store/pg"
)

// Archive implements service.ArchiveRepository.
//
// Ledger (status / begin / finish) lives in the cold sys.archive_runs table —
// the ledger is shared by all three triggers (cron/API/CLI) and must survive
// the hot DB. The dry-run ESTIMATE is computed from the hot store (rows older
// than policy.hot_retention_days), which is exactly what the archiver will
// move, and works without a cold database.
//
// No cold store ⇒ Status reports "not_configured" (200 with an explicit state,
// not a 503: an operator asking "what is the archive doing?" deserves an
// answer) and Run fails with ErrNotConfigured (503). The actual move is
// executed by a 980 JOB_TRIGGER frame injected by service.ArchiveService.
type Archive struct {
	hot  *sql.DB
	cold *pg.ColdDB
	pol  *policy.Store
	now  func() time.Time
}

// NewArchive builds the adapter. hot/cold/pol may be nil (degraded modes).
func NewArchive(hot *sql.DB, cold *pg.ColdDB, pol *policy.Store, now func() time.Time) *Archive {
	if now == nil {
		now = time.Now
	}
	return &Archive{hot: hot, cold: cold, pol: pol, now: now}
}

// archiveStateNotConfigured is the degraded status string (no cold ledger).
const archiveStateNotConfigured = "not_configured"

// archiveStatusSQL reads the newest ledger row.
const archiveStatusSQL = `SELECT id, started_at, COALESCE(finished_at, 0),
       COALESCE(moved_rows, 0), status
FROM sys.archive_runs ORDER BY id DESC LIMIT 1`

// Status returns the latest ledger state.
func (a *Archive) Status(ctx context.Context) (service.ArchiveStatus, error) {
	if a == nil || a.cold == nil || a.cold.DB == nil {
		// Degraded: no ledger, but the schedule is still known from policy.
		return service.ArchiveStatus{
			State:     archiveStateNotConfigured,
			NextRunAt: a.nextRunUnix(),
		}, nil
	}
	var (
		id      int64
		started int64
		finish  int64
		moved   int64
		state   string
	)
	err := a.cold.DB.QueryRowContext(ctx, archiveStatusSQL).
		Scan(&id, &started, &finish, &moved, &state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return service.ArchiveStatus{
			State:     "idle",
			NextRunAt: a.nextRunUnix(),
		}, nil
	case err != nil:
		return service.ArchiveStatus{}, fmt.Errorf("admin/prod: archive status: %w", err)
	}
	return service.ArchiveStatus{
		State:     state,
		Running:   state == "running",
		LastRunID: id,
		LastRunAt: started,
		LastMoved: moved,
		NextRunAt: a.nextRunUnix(),
	}, nil
}

// archiveEstimateSQL counts archivable rows: older than the retention cutoff.
const archiveEstimateSQL = `SELECT COUNT(*), COUNT(DISTINCT cid) FROM msg_messages_hot WHERE sent_at < ?`

// archiveRowsPerMinute is the conservative throughput used to turn a row count
// into a duration estimate (batch copy + pause is dominated by the cold write).
const archiveRowsPerMinute = 1000

// Estimate projects the next run's size from the hot store.
func (a *Archive) Estimate(ctx context.Context) (service.ArchiveEstimate, error) {
	if a == nil || a.hot == nil {
		return service.ArchiveEstimate{}, service.ErrNotConfigured
	}
	cutoff := a.now().Add(-time.Duration(a.retentionDays()) * 24 * time.Hour).Unix()
	var rows, convs int64
	if err := a.hot.QueryRowContext(ctx, archiveEstimateSQL, cutoff).Scan(&rows, &convs); err != nil {
		return service.ArchiveEstimate{}, fmt.Errorf("admin/prod: archive estimate: %w", err)
	}
	minutes := int64(0)
	if rows > 0 {
		minutes = (rows + archiveRowsPerMinute - 1) / archiveRowsPerMinute
	}
	return service.ArchiveEstimate{Rows: rows, Convs: convs, Minutes: minutes}, nil
}

// BeginRun opens a ledger row. Requires the cold store (the ledger is there).
func (a *Archive) BeginRun(ctx context.Context) (service.ArchiveRun, error) {
	if a == nil || a.cold == nil || a.cold.DB == nil {
		return service.ArchiveRun{}, fmt.Errorf("%w: archive ledger requires the cold store", service.ErrNotConfigured)
	}
	id, err := a.cold.StartRun(ctx, a.now().Unix())
	if err != nil {
		return service.ArchiveRun{}, err
	}
	return service.ArchiveRun{RunID: id, State: "running"}, nil
}

// FinishRun closes a ledger row (status: ok / failed).
func (a *Archive) FinishRun(ctx context.Context, runID int64, moved int64, status string) error {
	if a == nil || a.cold == nil || a.cold.DB == nil {
		return service.ErrNotConfigured
	}
	return a.cold.FinishRun(ctx, runID, a.now().Unix(), moved, status)
}

// retentionDays reads policy.hot_retention_days (default 90, matching
// policy.Default()).
func (a *Archive) retentionDays() int {
	const def = 90
	if a == nil || a.pol == nil {
		return def
	}
	if d := a.pol.Current().Policy.HotRetentionDays; d > 0 {
		return d
	}
	return def
}

// nextRunUnix derives the next scheduled run from policy.archive.cron.
// Unknown/missing cron shapes simply report 0 (unknown).
func (a *Archive) nextRunUnix() int64 {
	if a == nil || a.pol == nil {
		return 0
	}
	next, ok := nextDailyRun(a.pol.Current().Policy.Archive.Cron, a.now())
	if !ok {
		return 0
	}
	return next
}

// nextDailyRun resolves a daily schedule to its next occurrence after now.
//
// Accepted forms (the shapes job.ParseCronKind also reduces to daily):
//
//	"@daily HH:MM"
//	"M H * * *"   (classic 5-field; only the minute/hour fields are used)
//
// Anything else (lists, steps, month/day restrictions) returns ok=false — the
// assembly layer is responsible for simplifying schedules, not this adapter.
func nextDailyRun(cron string, now time.Time) (int64, bool) {
	minute, hour, ok := parseDailyCron(cron)
	if !ok {
		return 0, false
	}
	loc := now.Location()
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, loc)
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next.Unix(), true
}

// parseDailyCron extracts (minute, hour) from the supported cron shapes.
func parseDailyCron(cron string) (minute, hour int, ok bool) {
	cron = strings.TrimSpace(cron)
	if cron == "" {
		return 0, 0, false
	}
	var hhmm string
	if rest, found := cutPrefixFold(cron, "@daily"); found {
		hhmm = strings.TrimSpace(rest)
	} else {
		fields := strings.Fields(cron)
		if len(fields) != 5 || fields[2] != "*" || fields[3] != "*" || fields[4] != "*" {
			return 0, 0, false
		}
		hhmm = fields[1] + ":" + fields[0]
	}
	parts := strings.Split(hhmm, ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return m, h, true
}
