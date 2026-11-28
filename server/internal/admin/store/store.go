// Package adminstore is the persistence layer for the standalone admin.db
// (adm schema: adm_admins, adm_admin_sessions).
//
// Red line: all SQL in the admin plane lives here. Handlers and services never
// see a SQL string; they depend on narrow interfaces declared in package
// service. Every statement is parameterized.
package adminstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/mattn/go-sqlite3"

	datastore "github.com/yingo-server/UFOT/server/internal/data/store"
	"github.com/yingo-server/UFOT/server/internal/data/store/migrate"
	"github.com/yingo-server/UFOT/server/store/migrations"
)

// Admin mirrors adm_admins.
type Admin struct {
	ID         int64
	Username   string
	PWHash     string
	TOTPSecret string
	Role       int
	CreatedAt  int64
}

// Session mirrors adm_admin_sessions. SID is the SHA-256 hex of the opaque
// bearer token, never the token itself.
type Session struct {
	SID       string
	AdminID   int64
	ExpiresAt int64
	CSRF      string
}

// DB wraps the admin SQLite handle.
type DB struct{ *sql.DB }

// Open opens (creating if necessary) admin.db and applies the embedded admin
// migration tree. WAL + busy_timeout match the hot store's safety posture.
func Open(path string) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1", path)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("adminstore: open %s: %w", path, err)
	}
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("adminstore: %s: %w", pragma, err)
		}
	}
	return &DB{DB: db}, nil
}

// New wraps an already-open *sql.DB (assembly may own the handle).
func New(db *sql.DB) *DB { return &DB{DB: db} }

// Migrate applies admin/adm migrations (idempotent, checksum-verified).
func (d *DB) Migrate(ctx context.Context) error {
	r := &migrate.Runner{
		Dialect: migrate.DialectSQLite,
		Dir:     migrations.FS,
		DB:      d.DB,
		Root:    migrate.DirAdmin,
	}
	return r.Up(ctx)
}

// ---------------------------------------------------------------- admins

const getAdminByUsernameSQL = `SELECT admin_id, username, pw_hash, totp_secret, role, created_at
FROM adm_admins WHERE username = ?`

const getAdminSQL = `SELECT admin_id, username, pw_hash, totp_secret, role, created_at
FROM adm_admins WHERE admin_id = ?`

const insertAdminSQL = `INSERT INTO adm_admins (username, pw_hash, totp_secret, role, created_at)
VALUES (?, ?, ?, ?, ?)`

// GetAdminByUsername returns the admin row or datastore.ErrNotFound.
func (d *DB) GetAdminByUsername(ctx context.Context, username string) (*Admin, error) {
	return d.scanAdmin(d.DB.QueryRowContext(ctx, getAdminByUsernameSQL, username))
}

// GetAdmin returns the admin row by id or datastore.ErrNotFound.
func (d *DB) GetAdmin(ctx context.Context, id int64) (*Admin, error) {
	return d.scanAdmin(d.DB.QueryRowContext(ctx, getAdminSQL, id))
}

func (d *DB) scanAdmin(row *sql.Row) (*Admin, error) {
	var a Admin
	err := row.Scan(&a.ID, &a.Username, &a.PWHash, &a.TOTPSecret, &a.Role, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, datastore.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("adminstore: scan admin: %w", err)
	}
	return &a, nil
}

// CreateAdmin inserts an admin row and returns its id. Used by bootstrap
// tooling/tests; production provisioning is a separate operational flow.
func (d *DB) CreateAdmin(ctx context.Context, a *Admin) (int64, error) {
	res, err := d.DB.ExecContext(ctx, insertAdminSQL, a.Username, a.PWHash, a.TOTPSecret, a.Role, a.CreatedAt)
	if err != nil {
		if isDup(err) {
			return 0, datastore.ErrDuplicate
		}
		return 0, fmt.Errorf("adminstore: create admin: %w", err)
	}
	return res.LastInsertId()
}

// -------------------------------------------------------------- sessions

const insertSessionSQL = `INSERT INTO adm_admin_sessions (sid, admin_id, expires_at, csrf)
VALUES (?, ?, ?, ?)`

const getSessionSQL = `SELECT sid, admin_id, expires_at, csrf FROM adm_admin_sessions WHERE sid = ?`

const touchSessionSQL = `UPDATE adm_admin_sessions SET expires_at = ? WHERE sid = ?`

const deleteSessionSQL = `DELETE FROM adm_admin_sessions WHERE sid = ?`

const deleteExpiredSQL = `DELETE FROM adm_admin_sessions WHERE expires_at < ?`

// CreateSession inserts a session row. SID must already be a token hash.
func (d *DB) CreateSession(ctx context.Context, s *Session) error {
	if _, err := d.DB.ExecContext(ctx, insertSessionSQL, s.SID, s.AdminID, s.ExpiresAt, s.CSRF); err != nil {
		return fmt.Errorf("adminstore: create session: %w", err)
	}
	return nil
}

// GetSession returns a session row or datastore.ErrNotFound.
func (d *DB) GetSession(ctx context.Context, sid string) (*Session, error) {
	var s Session
	err := d.DB.QueryRowContext(ctx, getSessionSQL, sid).Scan(&s.SID, &s.AdminID, &s.ExpiresAt, &s.CSRF)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, datastore.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("adminstore: get session: %w", err)
	}
	return &s, nil
}

// TouchSession extends a session's expiry.
func (d *DB) TouchSession(ctx context.Context, sid string, expiresAt int64) error {
	res, err := d.DB.ExecContext(ctx, touchSessionSQL, expiresAt, sid)
	if err != nil {
		return fmt.Errorf("adminstore: touch session: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return datastore.ErrNotFound
	}
	return nil
}

// DeleteSession removes one session (logout).
func (d *DB) DeleteSession(ctx context.Context, sid string) error {
	if _, err := d.DB.ExecContext(ctx, deleteSessionSQL, sid); err != nil {
		return fmt.Errorf("adminstore: delete session: %w", err)
	}
	return nil
}

// DeleteExpired removes sessions past their expiry (opportunistic sweeper).
func (d *DB) DeleteExpired(ctx context.Context, now int64) (int64, error) {
	res, err := d.DB.ExecContext(ctx, deleteExpiredSQL, now)
	if err != nil {
		return 0, fmt.Errorf("adminstore: delete expired: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func isDup(err error) bool {
	var se sqlite3.Error
	if errors.As(err, &se) {
		return se.Code == sqlite3.ErrConstraint &&
			(se.ExtendedCode == sqlite3.ErrConstraintUnique || se.ExtendedCode == sqlite3.ErrConstraintPrimaryKey)
	}
	return false
}
