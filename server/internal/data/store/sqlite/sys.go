package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/yingo-server/OpenDiscord/server/internal/data/store"
)

// sysStore implements store.SysStore over sys_* tables.
//
// Red line: RateLimitIncr is ONLY a counter primitive. Whether a count is too
// many is decided by the caller reading policy — never here.

type sysStore struct{ db *sql.DB }

// UpsertFlag stores a flag document, bumping version on each write
// (version = old + 1; first insert = 1) for incremental frame fan-out.
func (s *sysStore) UpsertFlag(ctx context.Context, f *store.Flag) error {
	if f == nil {
		return fmt.Errorf("sqlite: UpsertFlag: nil flag")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sys_feature_flags (key, value, version) VALUES (?, ?, 1)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, version = version + 1`,
		f.Key, f.Value)
	if err != nil {
		return fmt.Errorf("sqlite: upsert flag %q: %w", f.Key, err)
	}
	return nil
}

func (s *sysStore) AllFlags(ctx context.Context) ([]store.Flag, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, value, version FROM sys_feature_flags ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: all flags: %w", err)
	}
	defer rows.Close()
	var out []store.Flag
	for rows.Next() {
		var f store.Flag
		if err := rows.Scan(&f.Key, &f.Value, &f.Version); err != nil {
			return nil, fmt.Errorf("sqlite: scan flag: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// InsertReport appends one row to sys_reports (FE-1a #4, ADDITIVE ONLY — the
// table already exists via store/migrations/hot/sys/0002_reports.sql). The
// detail string is bounded by the caller; ids + reason enum only.
func (s *sysStore) InsertReport(ctx context.Context, reporter, targetMsg, targetUser int64, reason int, detail string, at int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sys_reports (reporter, target_msg, target_user, reason, status, at)
VALUES (?, ?, ?, ?, 0, ?)`,
		reporter, targetMsg, targetUser, reason, detail, at)
	if err != nil {
		return fmt.Errorf("sqlite: insert report reporter=%d msg=%d: %w", reporter, targetMsg, err)
	}
	return nil
}

func (s *sysStore) UpsertPresence(ctx context.Context, p *store.Presence) error {
	if p == nil {
		return fmt.Errorf("sqlite: UpsertPresence: nil presence")
	}
	online := 0
	if p.Online {
		online = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sys_presence (uid, online, last_seen) VALUES (?, ?, ?)
ON CONFLICT(uid) DO UPDATE SET online = excluded.online, last_seen = excluded.last_seen`,
		p.UID, online, p.LastSeen)
	if err != nil {
		return fmt.Errorf("sqlite: upsert presence uid=%d: %w", p.UID, err)
	}
	return nil
}

// GetPresence returns the presence row; a never-seen uid yields an offline
// zero-value row (api.go explicitly allows this).
func (s *sysStore) GetPresence(ctx context.Context, uid int64) (*store.Presence, error) {
	var online int
	var p store.Presence
	err := s.db.QueryRowContext(ctx,
		`SELECT uid, online, last_seen FROM sys_presence WHERE uid = ?`, uid).
		Scan(&p.UID, &online, &p.LastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return &store.Presence{UID: uid, Online: false, LastSeen: 0}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get presence uid=%d: %w", uid, err)
	}
	p.Online = online != 0
	return &p, nil
}

// RateLimitIncr atomically increments the (subject, action, windowStart)
// bucket and returns the post-increment state. WindowLength is echoed from
// the caller's policy (the table does not store it). Implemented with
// INSERT ... ON CONFLICT DO UPDATE ... RETURNING (SQLite 3.35+); on an older
// engine it falls back to a serialized read-modify-write transaction.
func (s *sysStore) RateLimitIncr(ctx context.Context, subject, action string, windowStart int64) (*store.RateWindow, error) {
	var count int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO sys_rate_limit (subject, action, window_start, count) VALUES (?, ?, ?, 1)
ON CONFLICT(subject, action, window_start) DO UPDATE SET count = count + 1 RETURNING count`,
		subject, action, windowStart).Scan(&count)
	if err == nil {
		return &store.RateWindow{Subject: subject, Action: action, WindowStart: windowStart, Count: count}, nil
	}
	if !isReturningUnsupported(err) {
		return nil, fmt.Errorf("sqlite: rate incr (%s,%s,%d): %w", subject, action, windowStart, err)
	}
	// Fallback path (pre-3.35).
	if err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO sys_rate_limit (subject, action, window_start, count) VALUES (?, ?, ?, 0)`,
			subject, action, windowStart); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE sys_rate_limit SET count = count + 1 WHERE subject = ? AND action = ? AND window_start = ?`,
			subject, action, windowStart); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx,
			`SELECT count FROM sys_rate_limit WHERE subject = ? AND action = ? AND window_start = ?`,
			subject, action, windowStart).Scan(&count)
	}); err != nil {
		return nil, fmt.Errorf("sqlite: rate incr tx (%s,%s,%d): %w", subject, action, windowStart, err)
	}
	return &store.RateWindow{Subject: subject, Action: action, WindowStart: windowStart, Count: count}, nil
}

func (s *sysStore) GetPenalty(ctx context.Context, subject, penalty string) (*store.Penalty, error) {
	var p store.Penalty
	err := s.db.QueryRowContext(ctx,
		`SELECT subject, penalty, until FROM sys_rate_limit_penalty WHERE subject = ? AND penalty = ?`,
		subject, penalty).Scan(&p.Subject, &p.Penalty, &p.Until)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get penalty (%s,%s): %w", subject, penalty, err)
	}
	if p.Until <= nowUnix() {
		return nil, nil // expired: treated as absent
	}
	return &p, nil
}

func (s *sysStore) SetPenalty(ctx context.Context, p *store.Penalty) error {
	if p == nil {
		return fmt.Errorf("sqlite: SetPenalty: nil penalty")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sys_rate_limit_penalty (subject, penalty, until) VALUES (?, ?, ?)
ON CONFLICT(subject, penalty) DO UPDATE SET until = excluded.until`,
		p.Subject, p.Penalty, p.Until)
	if err != nil {
		return fmt.Errorf("sqlite: set penalty (%s,%s): %w", p.Subject, p.Penalty, err)
	}
	return nil
}
