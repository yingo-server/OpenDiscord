package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/yingo-server/UFOT/server/internal/data/store"
)

// identityStore implements store.IdentityStore over the identity_* tables.
//
// Red line: LookupByIdentity matches by HMAC'd IdValue ONLY. Raw identifiers
// never reach this layer; HMAC is applied in the auth path before lookup.

type identityStore struct{ db *sql.DB }

const upsertUserSQL = `INSERT INTO identity_users
(uid, display_name, avatar_media_id, status, created_at, anonymous_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(uid) DO UPDATE SET
display_name = excluded.display_name,
avatar_media_id = excluded.avatar_media_id,
status = excluded.status,
anonymous_at = excluded.anonymous_at`

func (s *identityStore) UpsertUser(ctx context.Context, u *store.User) error {
	if u == nil {
		return fmt.Errorf("sqlite: UpsertUser: nil user")
	}
	var anon any // NULL when not anonymized
	if u.AnonymousAt != 0 {
		anon = u.AnonymousAt
	}
	if _, err := s.db.ExecContext(ctx, upsertUserSQL,
		u.UID, u.DisplayName, u.AvatarMediaID, u.Status, u.CreatedAt, anon); err != nil {
		return fmt.Errorf("sqlite: upsert user uid=%d: %w", u.UID, err)
	}
	return nil
}

// SearchUsers implements store.IdentityStore.SearchUsers. Only public profile
// columns are returned, and only active (status=0, not anonymized) accounts.
// Parameterized query only; the keyword is passed through LIKE with escaped
// wildcards so a client cannot turn a search into a full table scan.
func (s *identityStore) SearchUsers(ctx context.Context, keyword string, limit int) ([]store.User, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	kw := strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(keyword), `\`, `\\`), `%`, `\%`)
	kw = strings.ReplaceAll(kw, `_`, `\_`)
	if kw == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT uid, display_name, avatar_media_id, status, created_at, anonymous_at
FROM identity_users
WHERE status = 0 AND anonymous_at IS NULL AND display_name LIKE ? ESCAPE '\'
ORDER BY uid ASC LIMIT ?`, "%"+kw+"%", limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: search users: %w", err)
	}
	defer rows.Close()
	var out []store.User
	for rows.Next() {
		var u store.User
		var avatar, anon sql.NullInt64
		if err := rows.Scan(&u.UID, &u.DisplayName, &avatar, &u.Status, &u.CreatedAt, &anon); err != nil {
			return nil, fmt.Errorf("sqlite: scan search result: %w", err)
		}
		u.AvatarMediaID = avatar.Int64
		u.AnonymousAt = anon.Int64
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *identityStore) LookupByIdentity(ctx context.Context, idType int, idValue string) (int64, error) {
	var uid int64
	err := s.db.QueryRowContext(ctx,
		`SELECT uid FROM identity_identities WHERE id_type = ? AND id_value = ?`,
		idType, idValue).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("sqlite: identity (%d, %q): %w", idType, idValue, store.ErrNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("sqlite: lookup identity: %w", err)
	}
	return uid, nil
}

func (s *identityStore) GetUser(ctx context.Context, uid int64) (*store.User, error) {
	var u store.User
	var avatar, anon sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT uid, display_name, avatar_media_id, status, created_at, anonymous_at
FROM identity_users WHERE uid = ?`, uid).
		Scan(&u.UID, &u.DisplayName, &avatar, &u.Status, &u.CreatedAt, &anon)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("sqlite: user %d: %w", uid, store.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get user %d: %w", uid, err)
	}
	u.AvatarMediaID = avatar.Int64
	u.AnonymousAt = anon.Int64
	return &u, nil
}

// GetBan returns the latest ban row for uid; expired bans (Until != 0 and in
// the past) are treated as absent per api.go. Until=0 is permanent.
func (s *identityStore) GetBan(ctx context.Context, uid int64) (*store.Ban, error) {
	var b store.Ban
	err := s.db.QueryRowContext(ctx,
		`SELECT uid, level, reason, until, created_by, created_at
FROM identity_bans WHERE uid = ? ORDER BY created_at DESC, rowid DESC LIMIT 1`,
		uid).Scan(&b.UID, &b.Level, &b.Reason, &b.Until, &b.CreatedBy, &b.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get ban %d: %w", uid, err)
	}
	if b.Until != 0 && b.Until <= nowUnix() {
		return nil, nil // expired: lazily ignored (api.go doc)
	}
	return &b, nil
}

// SetBan appends a ban history row. Level <= 0 means "lift": we still append
// the record (history is kept); effectiveness is derived by expiry, and a lift
// is represented by the caller as a Until in the past / level 0 row, which
// GetBan's recency ordering + expiry check makes non-effective.
func (s *identityStore) SetBan(ctx context.Context, b *store.Ban) error {
	if b == nil {
		return fmt.Errorf("sqlite: SetBan: nil ban")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO identity_bans (uid, level, reason, until, created_by, created_at)
VALUES (?, ?, ?, ?, ?, ?)`,
		b.UID, b.Level, b.Reason, b.Until, b.CreatedBy, b.CreatedAt); err != nil {
		return fmt.Errorf("sqlite: set ban uid=%d: %w", b.UID, err)
	}
	return nil
}
