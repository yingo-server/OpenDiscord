package prod

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/yingo-server/UFOT/server/internal/admin/service"
)

// Reach implements service.ReachCounter: the dry-run estimate for a broadcast.
//
// Estimate semantics (deliberately an UPPER BOUND, never an exact promise):
//
//	global broadcast : online connections + total users
//	                   — online connections (bridge topology / sys_presence)
//	                   count every live socket; total users (identity_users)
//	                   covers the offline push fan-out. Summing them can
//	                   double-count multi-device users, which is fine for a
//	                   "how many inboxes will this hit" preview.
//	conv-scoped      : distinct members of the listed conversations
//
// The online source is injectable (Options.Online) so the assembly can hand in
// the real bridge session count (session.Manager.Count) instead of the
// sys_presence projection.
type Reach struct {
	db     *sql.DB
	online func() (int64, error)
}

// ReachConfig tunes the estimate.
type ReachConfig struct {
	// Online overrides the sys_presence-based connection count.
	Online func() (int64, error)
}

// NewReach builds the counter.
func NewReach(db *sql.DB, cfg ReachConfig) *Reach {
	return &Reach{db: db, online: cfg.Online}
}

// CountReachable implements service.ReachCounter.
func (r *Reach) CountReachable(ctx context.Context, convIDs []int64) (int64, error) {
	if r == nil || r.db == nil {
		return 0, service.ErrNotConfigured
	}
	if len(convIDs) > 0 {
		return r.countConvMembers(ctx, convIDs)
	}
	online, err := r.countOnline(ctx)
	if err != nil {
		return 0, err
	}
	total, err := r.countUsers(ctx)
	if err != nil {
		return 0, err
	}
	return online + total, nil
}

// countConvMembers counts distinct members of the given conversations.
// Placeholders are generated indices only; ids stay bound values.
func (r *Reach) countConvMembers(ctx context.Context, convIDs []int64) (int64, error) {
	ids := make([]int64, 0, len(convIDs))
	for _, cid := range convIDs {
		if cid > 0 {
			ids = append(ids, cid)
		}
	}
	if len(ids) == 0 {
		return 0, fmt.Errorf("%w: conv_ids must be positive", service.ErrInvalid)
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	var n int64
	if err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(DISTINCT uid) FROM conv_members WHERE cid IN ("+ph+")",
		args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("admin/prod: reach conv members: %w", err)
	}
	return n, nil
}

func (r *Reach) countOnline(ctx context.Context) (int64, error) {
	if r.online != nil {
		return r.online()
	}
	var n int64
	if err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sys_presence WHERE online = 1").Scan(&n); err != nil {
		return 0, fmt.Errorf("admin/prod: reach online: %w", err)
	}
	return n, nil
}

// countUsers counts live accounts (status 3 = deleted is excluded).
func (r *Reach) countUsers(ctx context.Context) (int64, error) {
	var n int64
	if err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM identity_users WHERE status <> 3").Scan(&n); err != nil {
		return 0, fmt.Errorf("admin/prod: reach users: %w", err)
	}
	return n, nil
}

// Flags implements service.FlagStore over the hot sys_feature_flags table.
// Flag documents are policy-as-data: writes bump version so the 971 fan-out
// can be incremental.
type Flags struct{ db *sql.DB }

// NewFlags builds the adapter.
func NewFlags(db *sql.DB) *Flags { return &Flags{db: db} }

// ListFlags returns every flag ordered by key.
func (f *Flags) ListFlags(ctx context.Context) ([]service.Flag, error) {
	if f == nil || f.db == nil {
		return nil, service.ErrNotConfigured
	}
	rows, err := f.db.QueryContext(ctx,
		"SELECT key, value, version FROM sys_feature_flags ORDER BY key")
	if err != nil {
		return nil, fmt.Errorf("admin/prod: list flags: %w", err)
	}
	defer rows.Close()
	out := make([]service.Flag, 0, 16)
	for rows.Next() {
		var fl service.Flag
		if err := rows.Scan(&fl.Key, &fl.Value, &fl.Version); err != nil {
			return nil, fmt.Errorf("admin/prod: scan flag: %w", err)
		}
		out = append(out, fl)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("admin/prod: flag rows: %w", err)
	}
	return out, nil
}

const upsertFlagSQL = `INSERT INTO sys_feature_flags (key, value, version) VALUES (?, ?, 1)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, version = version + 1`

// SetFlag upserts one flag document.
func (f *Flags) SetFlag(ctx context.Context, key, value string) error {
	if f == nil || f.db == nil {
		return service.ErrNotConfigured
	}
	if _, err := f.db.ExecContext(ctx, upsertFlagSQL, key, value); err != nil {
		return fmt.Errorf("admin/prod: set flag %q: %w", key, err)
	}
	return nil
}
