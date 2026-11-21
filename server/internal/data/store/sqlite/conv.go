package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/yingo-server/UFOT/server/internal/data/store"
)

// convStore implements store.ConvStore over the conv_* tables.
//
// Red line: CheckMember answers "is uid in cid" — role/mute enforcement is
// domain logic above the store. RemoveMember/SetMemberRole assume the caller
// proved actor authority.

type convStore struct{ db *sql.DB }

// CreateConv inserts a conversation and its creator-as-owner membership,
// atomically, returning the allocated cid. m.Cid <= 0 means "you pick one";
// a valid m.Cid is honored (letting deterministic tests / splits pick).
func (s *convStore) CreateConv(ctx context.Context, m *store.ConvMeta, creatorUID int64) (int64, error) {
	if m == nil {
		return 0, fmt.Errorf("sqlite: CreateConv: nil meta")
	}
	var cid int64
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		if m.Cid > 0 {
			cid = m.Cid
		} else {
			var next int64
			if err := tx.QueryRowContext(ctx,
				`SELECT COALESCE(MAX(cid), 10000) + 1 FROM conv_conversations`).Scan(&next); err != nil {
				return fmt.Errorf("sqlite: alloc cid: %w", err)
			}
			cid = next
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO conv_conversations (cid, ctype, title, avatar_media_id, created_by, created_at)
VALUES (?, ?, ?, ?, ?, ?)`,
			cid, m.CType, m.Title, m.AvatarMediaID, m.CreatedBy, m.CreatedAt); err != nil {
			if isDup(err) {
				return fmt.Errorf("sqlite: conv cid=%d exists: %w", cid, store.ErrDuplicate)
			}
			return fmt.Errorf("sqlite: insert conv: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO conv_members (cid, uid, role, joined_at, muted_until) VALUES (?, ?, 2, ?, 0)`,
			cid, creatorUID, m.CreatedAt); err != nil {
			return fmt.Errorf("sqlite: insert creator member: %w", err)
		}
		// Seed the seq allocator row so AllocateSeq's fast path hits.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO conv_seq (cid, next_seq) VALUES (?, 1)`, cid); err != nil {
			return fmt.Errorf("sqlite: seed conv_seq: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return cid, nil
}

// AddMember is idempotent: already-member is a no-op success.
func (s *convStore) AddMember(ctx context.Context, m *store.Member) error {
	if m == nil {
		return fmt.Errorf("sqlite: AddMember: nil member")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO conv_members (cid, uid, role, joined_at, muted_until) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(cid, uid) DO NOTHING`,
		m.Cid, m.UID, m.Role, m.JoinedAt, m.MutedUntil)
	if err != nil {
		return fmt.Errorf("sqlite: add member cid=%d uid=%d: %w", m.Cid, m.UID, err)
	}
	return nil
}

// RemoveMember is idempotent: removing a non-member succeeds.
func (s *convStore) RemoveMember(ctx context.Context, cid, uid int64) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM conv_members WHERE cid = ? AND uid = ?`, cid, uid); err != nil {
		return fmt.Errorf("sqlite: remove member cid=%d uid=%d: %w", cid, uid, err)
	}
	return nil
}

// CheckMember reports membership existence with current role/mute state.
// Absent member -> (nil, ErrNotFound); callers translate to 401/403.
func (s *convStore) CheckMember(ctx context.Context, cid, uid int64) (*store.Member, error) {
	var m store.Member
	err := s.db.QueryRowContext(ctx,
		`SELECT cid, uid, role, joined_at, muted_until FROM conv_members WHERE cid = ? AND uid = ?`,
		cid, uid).Scan(&m.Cid, &m.UID, &m.Role, &m.JoinedAt, &m.MutedUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("sqlite: member cid=%d uid=%d: %w", cid, uid, store.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: check member: %w", err)
	}
	return &m, nil
}

func (s *convStore) ListMembers(ctx context.Context, cid int64) ([]store.Member, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT cid, uid, role, joined_at, muted_until FROM conv_members WHERE cid = ? ORDER BY uid`,
		cid)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list members cid=%d: %w", cid, err)
	}
	defer rows.Close()
	var out []store.Member
	for rows.Next() {
		var m store.Member
		if err := rows.Scan(&m.Cid, &m.UID, &m.Role, &m.JoinedAt, &m.MutedUntil); err != nil {
			return nil, fmt.Errorf("sqlite: scan member: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetMemberRole updates one member's role. Authorization happened above;
// updating a non-member's role is a no-op success (idempotent by row absence).
func (s *convStore) SetMemberRole(ctx context.Context, cid, uid int64, role int) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE conv_members SET role = ? WHERE cid = ? AND uid = ?`,
		role, cid, uid); err != nil {
		return fmt.Errorf("sqlite: set role cid=%d uid=%d: %w", cid, uid, err)
	}
	return nil
}

func (s *convStore) GetConvMeta(ctx context.Context, cid int64) (*store.ConvMeta, error) {
	var m store.ConvMeta
	err := s.db.QueryRowContext(ctx,
		`SELECT cid, ctype, title, avatar_media_id, created_by, created_at
FROM conv_conversations WHERE cid = ?`, cid).
		Scan(&m.Cid, &m.CType, &m.Title, &m.AvatarMediaID, &m.CreatedBy, &m.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("sqlite: conv %d: %w", cid, store.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get conv meta %d: %w", cid, err)
	}
	return &m, nil
}

// SingleChatBetween returns the direct conversation whose member set is
// exactly {uid1, uid2}. Parameterized query only; no string concatenation.
func (s *convStore) SingleChatBetween(ctx context.Context, uid1, uid2 int64) (int64, error) {
	if uid1 <= 0 || uid2 <= 0 || uid1 == uid2 {
		return 0, fmt.Errorf("sqlite: single chat (%d, %d): %w", uid1, uid2, store.ErrNotFound)
	}
	var cid int64
	err := s.db.QueryRowContext(ctx,
		`SELECT c.cid
FROM conv_conversations c
JOIN conv_members m1 ON m1.cid = c.cid AND m1.uid = ?
JOIN conv_members m2 ON m2.cid = c.cid AND m2.uid = ?
WHERE c.ctype = 1
  AND (SELECT COUNT(*) FROM conv_members m WHERE m.cid = c.cid) = 2
ORDER BY c.cid ASC
LIMIT 1`, uid1, uid2).Scan(&cid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("sqlite: single chat (%d, %d): %w", uid1, uid2, store.ErrNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("sqlite: single chat (%d, %d): %w", uid1, uid2, err)
	}
	return cid, nil
}

func (s *convStore) NextSeq(ctx context.Context, cid int64) (int64, error) {
	var next int64
	err := s.db.QueryRowContext(ctx,
		`SELECT next_seq FROM conv_seq WHERE cid = ?`, cid).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		return 1, nil // unminted conversation: first seq is 1 by convention
	}
	if err != nil {
		return 0, fmt.Errorf("sqlite: next seq cid=%d: %w", cid, err)
	}
	return next, nil
}

// MemberPeersOf implements store.ConvStore.MemberPeersOf: the deduplicated
// set of OTHER uids sharing at least one conversation with uid. Parameterized
// query only — no string concatenation.
func (s *convStore) MemberPeersOf(ctx context.Context, uid int64) ([]int64, error) {
	if uid <= 0 {
		return nil, fmt.Errorf("sqlite: MemberPeersOf: invalid uid %d", uid)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT m2.uid
FROM conv_members m1
JOIN conv_members m2 ON m2.cid = m1.cid
WHERE m1.uid = ? AND m2.uid <> ?`, uid, uid)
	if err != nil {
		return nil, fmt.Errorf("sqlite: member peers uid=%d: %w", uid, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var other int64
		if err := rows.Scan(&other); err != nil {
			return nil, fmt.Errorf("sqlite: scan member peer: %w", err)
		}
		out = append(out, other)
	}
	return out, rows.Err()
}
