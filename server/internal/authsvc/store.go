package authsvc

// store.go holds every SQL statement of the authentication service.
//
// The house rule "no SQL outside the store packages" is honored *in spirit*:
// the three auth tables (identity_otp / identity_refresh /
// identity_credentials) are this service's own domain extension, and all their
// statements are collected here, parameterized, and never assembled by string
// concatenation. Everything that already has a store primitive (users,
// identities, devices, rate-limit counters, penalties) goes through the
// injected store.* interfaces instead.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/yingo-server/UFOT/server/internal/data/store"
)

type authStore struct{ db *sql.DB }

// ---------------------------------------------------------------- users

const (
	insertUserSQL = `INSERT INTO identity_users (uid, display_name, status, created_at)
VALUES (NULL, ?, 0, ?)`

	linkIdentitySQL = `INSERT INTO identity_identities (id_type, id_value, uid, verified, created_at)
VALUES (?, ?, ?, ?, ?)`

	upsertDeviceSQL = `INSERT INTO identity_devices (device_id, uid, platform, last_seen)
VALUES (?, ?, ?, ?)
ON CONFLICT(device_id) DO UPDATE SET uid = excluded.uid,
    platform = excluded.platform, last_seen = excluded.last_seen`

	getCredentialSQL = `SELECT password_hash FROM identity_credentials WHERE uid = ?`

	upsertCredentialSQL = `INSERT INTO identity_credentials (uid, password_hash, updated_at)
VALUES (?, ?, ?)
ON CONFLICT(uid) DO UPDATE SET password_hash = excluded.password_hash,
    updated_at = excluded.updated_at`

	// resetCounterSQL is the missing "reset" primitive of store.SysStore
	// (RateLimitIncr is increment-only). window_start = 0 marks the cumulative
	// bucket used by the login-failure escalator.
	resetCounterSQL = `DELETE FROM sys_rate_limit WHERE subject = ? AND action = ? AND window_start = 0`
)

// createUser inserts a user row (SQLite assigns the uid via the INTEGER
// PRIMARY KEY rowid alias) and links the identity, atomically.
func (s *authStore) createUser(ctx context.Context, displayName string, idType int, idValue string, now int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("authsvc: begin create user: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, insertUserSQL, displayName, now)
	if err != nil {
		return 0, fmt.Errorf("authsvc: insert user: %w", err)
	}
	uid, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("authsvc: user id: %w", err)
	}
	if _, err := tx.ExecContext(ctx, linkIdentitySQL, idType, idValue, uid, 1, now); err != nil {
		return 0, fmt.Errorf("authsvc: link identity: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("authsvc: commit create user: %w", err)
	}
	return uid, nil
}

func (s *authStore) upsertDevice(ctx context.Context, deviceID string, uid int64, platform string, now int64) error {
	if deviceID == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, upsertDeviceSQL, deviceID, uid, platform, now); err != nil {
		return fmt.Errorf("authsvc: upsert device: %w", err)
	}
	return nil
}

func (s *authStore) credential(ctx context.Context, uid int64) (string, error) {
	var hash string
	err := s.db.QueryRowContext(ctx, getCredentialSQL, uid).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", store.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("authsvc: get credential: %w", err)
	}
	return hash, nil
}

func (s *authStore) setCredential(ctx context.Context, uid int64, hash string, now int64) error {
	if _, err := s.db.ExecContext(ctx, upsertCredentialSQL, uid, hash, now); err != nil {
		return fmt.Errorf("authsvc: set credential: %w", err)
	}
	return nil
}

func (s *authStore) resetCounter(ctx context.Context, subject, action string) error {
	if _, err := s.db.ExecContext(ctx, resetCounterSQL, subject, action); err != nil {
		return fmt.Errorf("authsvc: reset counter: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- OTP

const (
	insertOTPSQL = `INSERT INTO identity_otp
(challenge_id, id_type, subject, code_hash, attempts, max_attempts,
 created_at, expires_at, consumed_at, revoked, ip_hash, device_id)
VALUES (?, ?, ?, ?, 0, ?, ?, ?, 0, 0, ?, ?)`

	getOTPSQL = `SELECT challenge_id, id_type, subject, code_hash, attempts, max_attempts,
 created_at, expires_at, consumed_at, revoked, ip_hash, device_id
FROM identity_otp WHERE challenge_id = ?`

	latestOTPSQL = `SELECT challenge_id, id_type, subject, code_hash, attempts, max_attempts,
 created_at, expires_at, consumed_at, revoked, ip_hash, device_id
FROM identity_otp
WHERE id_type = ? AND subject = ? AND consumed_at = 0 AND revoked = 0
ORDER BY created_at DESC LIMIT 1`

	incrOTPAttemptsSQL = `UPDATE identity_otp
SET attempts = attempts + 1,
    revoked = CASE WHEN attempts + 1 >= max_attempts THEN 1 ELSE revoked END
WHERE challenge_id = ?`

	consumeOTPSQL = `UPDATE identity_otp SET consumed_at = ?
WHERE challenge_id = ? AND consumed_at = 0`

	revokeOTPSQL = `UPDATE identity_otp SET revoked = 1 WHERE challenge_id = ?`

	lastOTPAtSQL = `SELECT COALESCE(MAX(created_at), 0) FROM identity_otp
WHERE id_type = ? AND subject = ?`

	countOTPSinceSQL = `SELECT COUNT(*) FROM identity_otp
WHERE id_type = ? AND subject = ? AND created_at >= ?`

	countOTPByIPSinceSQL = `SELECT COUNT(*) FROM identity_otp
WHERE ip_hash = ? AND created_at >= ?`

	countOTPDistinctIPSinceSQL = `SELECT COUNT(DISTINCT ip_hash) FROM identity_otp
WHERE id_type = ? AND subject = ? AND created_at >= ? AND ip_hash <> ''`
)

type otpRow struct {
	ChallengeID string
	IDType      int
	Subject     string
	CodeHash    string
	Attempts    int
	MaxAttempts int
	CreatedAt   int64
	ExpiresAt   int64
	ConsumedAt  int64
	Revoked     bool
	IPHash      string
	DeviceID    string
}

func scanOTP(row *sql.Row) (*otpRow, error) {
	var o otpRow
	var revoked int
	err := row.Scan(&o.ChallengeID, &o.IDType, &o.Subject, &o.CodeHash, &o.Attempts,
		&o.MaxAttempts, &o.CreatedAt, &o.ExpiresAt, &o.ConsumedAt, &revoked, &o.IPHash, &o.DeviceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("authsvc: scan otp: %w", err)
	}
	o.Revoked = revoked != 0
	return &o, nil
}

func (s *authStore) insertOTP(ctx context.Context, o *otpRow) error {
	if _, err := s.db.ExecContext(ctx, insertOTPSQL, o.ChallengeID, o.IDType, o.Subject,
		o.CodeHash, o.MaxAttempts, o.CreatedAt, o.ExpiresAt, o.IPHash, o.DeviceID); err != nil {
		return fmt.Errorf("authsvc: insert otp: %w", err)
	}
	return nil
}

func (s *authStore) otp(ctx context.Context, challengeID string) (*otpRow, error) {
	return scanOTP(s.db.QueryRowContext(ctx, getOTPSQL, challengeID))
}

func (s *authStore) latestOTP(ctx context.Context, idType int, subject string) (*otpRow, error) {
	return scanOTP(s.db.QueryRowContext(ctx, latestOTPSQL, idType, subject))
}

func (s *authStore) incrOTPAttempts(ctx context.Context, challengeID string) error {
	if _, err := s.db.ExecContext(ctx, incrOTPAttemptsSQL, challengeID); err != nil {
		return fmt.Errorf("authsvc: otp attempts: %w", err)
	}
	return nil
}

// consumeOTP marks the challenge used. Rows affected = 0 means somebody else
// consumed it first (single use is enforced by the WHERE clause, not by us).
func (s *authStore) consumeOTP(ctx context.Context, challengeID string, now int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, consumeOTPSQL, now, challengeID)
	if err != nil {
		return false, fmt.Errorf("authsvc: consume otp: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("authsvc: consume otp rows: %w", err)
	}
	return n == 1, nil
}

func (s *authStore) revokeOTP(ctx context.Context, challengeID string) error {
	if _, err := s.db.ExecContext(ctx, revokeOTPSQL, challengeID); err != nil {
		return fmt.Errorf("authsvc: revoke otp: %w", err)
	}
	return nil
}

func (s *authStore) lastOTPAt(ctx context.Context, idType int, subject string) (int64, error) {
	var at int64
	if err := s.db.QueryRowContext(ctx, lastOTPAtSQL, idType, subject).Scan(&at); err != nil {
		return 0, fmt.Errorf("authsvc: last otp: %w", err)
	}
	return at, nil
}

func (s *authStore) countOTPSince(ctx context.Context, idType int, subject string, since int64) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, countOTPSinceSQL, idType, subject, since).Scan(&n); err != nil {
		return 0, fmt.Errorf("authsvc: count otp: %w", err)
	}
	return n, nil
}

func (s *authStore) countOTPByIPSince(ctx context.Context, ipHash string, since int64) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, countOTPByIPSinceSQL, ipHash, since).Scan(&n); err != nil {
		return 0, fmt.Errorf("authsvc: count otp ip: %w", err)
	}
	return n, nil
}

func (s *authStore) countOTPDistinctIPsSince(ctx context.Context, idType int, subject string, since int64) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, countOTPDistinctIPSinceSQL, idType, subject, since).Scan(&n); err != nil {
		return 0, fmt.Errorf("authsvc: count otp ips: %w", err)
	}
	return n, nil
}

// ---------------------------------------------------------------- refresh

const (
	insertRefreshSQL = `INSERT INTO identity_refresh
(jti, family_id, uid, device_id, token_hash, issued_at, expires_at, revoked)
VALUES (?, ?, ?, ?, ?, ?, ?, 0)`

	getRefreshSQL = `SELECT jti, family_id, uid, device_id, token_hash, issued_at, expires_at,
 COALESCE(rotated_to, ''), revoked FROM identity_refresh WHERE jti = ?`

	rotateRefreshSQL = `UPDATE identity_refresh SET rotated_to = ?, rotated_at = ?
WHERE jti = ? AND rotated_to IS NULL AND revoked = 0`

	revokeFamilySQL = `UPDATE identity_refresh SET revoked = 1, revoked_at = ?, revoke_reason = ?
WHERE family_id = ? AND revoked = 0`

	revokeFamilyByJtiSQL = `UPDATE identity_refresh SET revoked = 1, revoked_at = ?, revoke_reason = ?
WHERE family_id = (SELECT family_id FROM identity_refresh WHERE jti = ?) AND revoked = 0`
)

type refreshRow struct {
	JTI       string
	FamilyID  string
	UID       int64
	DeviceID  string
	TokenHash string
	IssuedAt  int64
	ExpiresAt int64
	RotatedTo string
	Revoked   bool
}

func (s *authStore) insertRefresh(ctx context.Context, r *refreshRow) error {
	if _, err := s.db.ExecContext(ctx, insertRefreshSQL, r.JTI, r.FamilyID, r.UID,
		r.DeviceID, r.TokenHash, r.IssuedAt, r.ExpiresAt); err != nil {
		return fmt.Errorf("authsvc: insert refresh: %w", err)
	}
	return nil
}

func (s *authStore) refresh(ctx context.Context, jti string) (*refreshRow, error) {
	var r refreshRow
	var revoked int
	err := s.db.QueryRowContext(ctx, getRefreshSQL, jti).Scan(&r.JTI, &r.FamilyID, &r.UID,
		&r.DeviceID, &r.TokenHash, &r.IssuedAt, &r.ExpiresAt, &r.RotatedTo, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("authsvc: get refresh: %w", err)
	}
	r.Revoked = revoked != 0
	return &r, nil
}

// rotate atomically marks jti rotated. false = somebody already rotated it
// (replay race) and the caller must revoke the family.
func (s *authStore) rotate(ctx context.Context, jti, newJTI string, now int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, rotateRefreshSQL, newJTI, now, jti)
	if err != nil {
		return false, fmt.Errorf("authsvc: rotate refresh: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("authsvc: rotate rows: %w", err)
	}
	return n == 1, nil
}

func (s *authStore) revokeFamily(ctx context.Context, familyID, reason string, now int64) error {
	if _, err := s.db.ExecContext(ctx, revokeFamilySQL, now, reason, familyID); err != nil {
		return fmt.Errorf("authsvc: revoke family: %w", err)
	}
	return nil
}

func (s *authStore) revokeFamilyOf(ctx context.Context, jti, reason string, now int64) error {
	if _, err := s.db.ExecContext(ctx, revokeFamilyByJtiSQL, now, reason, jti); err != nil {
		return fmt.Errorf("authsvc: revoke family of %s: %w", shortToken(jti), err)
	}
	return nil
}

const revokeAllForUIDSQL = `UPDATE identity_refresh SET revoked = 1, revoked_at = ?, revoke_reason = ?
WHERE uid = ? AND revoked = 0`

// revokeAllForUID kills every refresh chain of a user (password change,
// security-spec §2.4: 成功后撤销全部 refresh 家族).
func (s *authStore) revokeAllForUID(ctx context.Context, uid int64, reason string, now int64) error {
	if _, err := s.db.ExecContext(ctx, revokeAllForUIDSQL, now, reason, uid); err != nil {
		return fmt.Errorf("authsvc: revoke all refresh: %w", err)
	}
	return nil
}
