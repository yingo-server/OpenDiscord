package prod

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yingo-server/UFOT/server/internal/admin/service"
	"github.com/yingo-server/UFOT/server/internal/data/store"
)

// Users implements service.UserRepository over the hot SQLite identity_*
// tables, plus the ban-journal write used by the dispatcher decorator.
//
// Read model:
//
//	identity_users      profile + status (0 normal / 1 banned / 2 cooling / 3 deleted)
//	identity_identities HMAC index (id_type, id_value) → uid; raw identifiers
//	                    are NEVER stored, so Phone/Email stay empty here and
//	                    the service layer's masking is a no-op pass-through
//	identity_bans       append-only history; effectiveness = latest row whose
//	                    until is 0 (permanent) or in the future
//
// Red line: identity_identities is looked up by HMAC'd id_value only. The
// adapter accepts the mapping function (Options.IDHasher) instead of hashing
// itself — the key must never live in this package.
type Users struct {
	db   *sql.DB
	hash func(string) []string
	now  func() time.Time
}

// UsersConfig tunes the adapter.
type UsersConfig struct {
	// Hasher maps a raw identifier to its candidate HMAC index values (one per
	// identifier type). nil = match the query string against id_value verbatim
	// (callers may pass an already-hashed value).
	Hasher func(string) []string
	// Now injects a clock (ban expiry evaluation).
	Now func() time.Time
}

// NewUsers builds the adapter. A nil db yields an adapter whose ports report
// service.ErrNotConfigured (503) instead of panicking.
func NewUsers(db *sql.DB, cfg UsersConfig) *Users {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Users{db: db, hash: cfg.Hasher, now: now}
}

const userColumnsSQL = `u.uid, u.display_name, u.status, u.created_at,
       b.level, b.reason, b.until, b.created_by, b.created_at`

// userFromSQL joins the LATEST ban row per user; expiry is decided in Go so the
// SQL stays a pure read (mirrors sqlite.GetBan semantics: until=0 permanent).
const userFromSQL = ` FROM identity_users u
LEFT JOIN identity_bans b ON b.rowid = (
    SELECT x.rowid FROM identity_bans x
    WHERE x.uid = u.uid
    ORDER BY x.created_at DESC, x.rowid DESC
    LIMIT 1
)`

// userWhere builds the predicate from FIXED fragments only; values are always
// bound (?, ?, ...). Pure function — unit-tested without a database.
//
// The identity branch expands to an IN (...) over the candidate HMAC values;
// placeholders are generated indices, never data.
func userWhere(f service.UserFilter, idValues []string) (string, []any) {
	var b strings.Builder
	b.WriteString(" WHERE 1=1")
	args := make([]any, 0, 6)
	if q := strings.TrimSpace(f.Query); q != "" {
		b.WriteString(` AND (u.display_name LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(q)+"%")
		if len(idValues) > 0 {
			ph := strings.TrimSuffix(strings.Repeat("?,", len(idValues)), ",")
			b.WriteString(` OR EXISTS (SELECT 1 FROM identity_identities i ` +
				`WHERE i.uid = u.uid AND i.id_value IN (` + ph + `))`)
			for _, v := range idValues {
				args = append(args, v)
			}
		}
		b.WriteString(")")
	}
	if f.Status >= 0 {
		b.WriteString(" AND u.status = ?")
		args = append(args, f.Status)
	}
	return b.String(), args
}

// escapeLike neutralises LIKE wildcards so a user query is a literal
// substring search (paired with ESCAPE '\' in the statement).
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// idValues maps q to the identity index candidates: the hasher's output when
// configured (one per identifier type), otherwise q verbatim so callers may
// pass an already-hashed value.
func (u *Users) idValues(q string) []string {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil
	}
	if u.hash != nil {
		if vs := u.hash(q); len(vs) > 0 {
			out := make([]string, 0, len(vs))
			for _, v := range vs {
				if v = strings.TrimSpace(v); v != "" {
					out = append(out, v)
				}
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	return []string{q}
}

// ListUsers returns one page of users ordered newest-first.
func (u *Users) ListUsers(ctx context.Context, f service.UserFilter) (service.UserPage, error) {
	if u == nil || u.db == nil {
		return service.UserPage{}, service.ErrNotConfigured
	}
	where, args := userWhere(f, u.idValues(f.Query))

	var total int64
	if err := u.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM identity_users u"+where, args...).Scan(&total); err != nil {
		return service.UserPage{}, fmt.Errorf("admin/prod: count users: %w", err)
	}

	limit, offset := clampPage(f.Limit, 50, 1000, f.Offset)
	q := "SELECT " + userColumnsSQL + userFromSQL + where +
		" ORDER BY u.created_at DESC, u.uid DESC LIMIT ? OFFSET ?"
	rows, err := u.db.QueryContext(ctx, q, append(args, limit, offset)...)
	if err != nil {
		return service.UserPage{}, fmt.Errorf("admin/prod: list users: %w", err)
	}
	defer rows.Close()

	now := u.now().Unix()
	items := make([]service.UserRecord, 0, limit)
	for rows.Next() {
		rec, err := scanUser(rows, now)
		if err != nil {
			return service.UserPage{}, fmt.Errorf("admin/prod: scan user: %w", err)
		}
		items = append(items, rec)
	}
	if err := rows.Err(); err != nil {
		return service.UserPage{}, fmt.Errorf("admin/prod: user rows: %w", err)
	}
	return service.UserPage{Items: items, Total: total, Limit: limit, Offset: offset}, nil
}

// GetUser fetches one user. store.ErrNotFound is returned when absent so the
// service maps it onto 404.
func (u *Users) GetUser(ctx context.Context, uid int64) (*service.UserRecord, error) {
	if u == nil || u.db == nil {
		return nil, service.ErrNotConfigured
	}
	if uid <= 0 {
		return nil, fmt.Errorf("%w: uid must be positive", service.ErrInvalid)
	}
	q := "SELECT " + userColumnsSQL + userFromSQL + " WHERE u.uid = ?"
	rec, err := scanUser(u.db.QueryRowContext(ctx, q, uid), u.now().Unix())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("admin/prod: user %d: %w", uid, store.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("admin/prod: get user %d: %w", uid, err)
	}
	return &rec, nil
}

// scanner is the shared shape of *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

func scanUser(s scanner, nowUnix int64) (service.UserRecord, error) {
	var rec service.UserRecord
	var level, until, banAt sql.NullInt64
	var reason, by sql.NullString
	if err := s.Scan(&rec.UID, &rec.DisplayName, &rec.Status, &rec.CreatedAt,
		&level, &reason, &until, &by, &banAt); err != nil {
		return rec, err
	}
	// Effective ban only: a lift row / expired row is history, not state.
	if level.Valid && level.Int64 > 0 && (until.Int64 == 0 || until.Int64 > nowUnix) {
		rec.Ban = &service.BanInfo{
			Level:     int(level.Int64),
			Reason:    reason.String,
			Until:     until.Int64,
			CreatedBy: by.String,
			CreatedAt: banAt.Int64,
		}
	}
	// Phone/Email: identity_identities stores only HMAC values, so there is
	// nothing to return. The fields stay empty by design (PII never leaves the
	// auth path); see the package doc.
	return rec, nil
}

// RecordBan appends a ban row to identity_bans (append-only history).
//
// This is the admin-side journal of a 960 ban command; the data layer owns the
// authoritative enforcement. until=0 means permanent.
func (u *Users) RecordBan(ctx context.Context, uid int64, level int, reason string, until int64, by string) error {
	if u == nil || u.db == nil {
		return service.ErrNotConfigured
	}
	if uid <= 0 || level <= 0 {
		return fmt.Errorf("%w: uid and level must be positive", service.ErrInvalid)
	}
	return u.insertBan(ctx, uid, level, reason, until, by)
}

// RecordBanLift appends the "lifted" row: level 0 with an expiry in the past.
// Effectiveness is derived (GetBan ignores expired rows), history is kept.
func (u *Users) RecordBanLift(ctx context.Context, uid int64, reason, by string) error {
	if u == nil || u.db == nil {
		return service.ErrNotConfigured
	}
	if uid <= 0 {
		return fmt.Errorf("%w: uid must be positive", service.ErrInvalid)
	}
	return u.insertBan(ctx, uid, 0, reason, u.now().Unix(), by)
}

const insertBanSQL = `INSERT INTO identity_bans (uid, level, reason, until, created_by, created_at)
VALUES (?, ?, ?, ?, ?, ?)`

func (u *Users) insertBan(ctx context.Context, uid int64, level int, reason string, until int64, by string) error {
	if _, err := u.db.ExecContext(ctx, insertBanSQL,
		uid, level, reason, until, by, u.now().Unix()); err != nil {
		return fmt.Errorf("admin/prod: insert ban uid=%d: %w", uid, err)
	}
	return nil
}

// clampPage applies the defensive page bounds shared by the adapters.
func clampPage(limit, def, max, offset int) (int, int) {
	if limit <= 0 {
		limit = def
	}
	if limit > max {
		limit = max
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}
