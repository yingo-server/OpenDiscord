// Package sqlite implements the hot store (SQLite, WAL) for every domain
// interface in internal/data/store/api.go. One DB struct composes per-domain
// implementations (struct embedding, not inheritance of behavior).
//
// Rules honored here:
//   - ALL SQL is parameterized; ORDER BY directions come from a fixed
//     whitelist (orderSQL map in msgreader.go). No fmt.Sprintf SQL assembly
//     anywhere except identifier quoting for constant table names.
//   - Store methods are NOT authorization boundaries (api.go red line):
//     membership checks happen in the dispatcher/domain layer before writes
//     enter the pipeline.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/mattn/go-sqlite3"

	"github.com/yingo-server/OpenDiscord/server/internal/data/store"
	"github.com/yingo-server/OpenDiscord/server/internal/data/store/migrate"
	"github.com/yingo-server/OpenDiscord/server/store/migrations"
)

// DB is the hot store: one *sql.DB over a SQLite file, embedding the
// per-domain implementations. Satisfies (compile-time asserted in each file):
//
//	store.MsgWriter, store.MsgReader, store.IdentityStore,
//	store.ConvStore, store.MediaStore, store.SysStore
type DB struct {
	*sql.DB
	msgw      *msgWriter
	msgr      *msgReader
	identity  *identityStore
	conv      *convStore
	media     *mediaStore
	sys       *sysStore
	sqldriver string
}

// ScanOlder delegates to the message reader's archive scan (see msgreader.go).
// Promotion: DB composes per-domain implementations; *DB satisfies job.HotScanner.
func (db *DB) ScanOlder(ctx context.Context, cutoff int64, limit int) ([]store.HotMsg, error) {
	return db.msgr.ScanOlder(ctx, cutoff, limit)
}

// MarkRecalled delegates to the message writer's soft delete (msgwriter.go).
func (db *DB) MarkRecalled(ctx context.Context, cid, msgID, senderID, at int64) (*store.HotMsg, error) {
	return db.msgw.MarkRecalled(ctx, cid, msgID, senderID, at)
}

// MarkEdited delegates to the message writer's content soft-update
// (msgwriter.go, FE-1a #1).
func (db *DB) MarkEdited(ctx context.Context, cid, msgID, senderID int64, content []byte, at int64) (*store.HotMsg, error) {
	return db.msgw.MarkEdited(ctx, cid, msgID, senderID, content, at)
}

// InsertReport appends one row to sys_reports (FE-1a #4): reporter uid, the
// reported message / user ids, the reason enum and a bounded detail string.
// Ids + enum only — the caller enforces the no-PII red line.
func (db *DB) InsertReport(ctx context.Context, reporter int64, targetMsg, targetUser int64, reason int, detail string, at int64) error {
	return db.sys.InsertReport(ctx, reporter, targetMsg, targetUser, reason, detail, at)
}

// MemberPeersOf delegates to the conv store's presence peer set (conv.go).
func (db *DB) MemberPeersOf(ctx context.Context, uid int64) ([]int64, error) {
	return db.conv.MemberPeersOf(ctx, uid)
}

// Open opens (creating if needed) the hot SQLite database at path and applies
// the hot migration tree. PRAGMAs per docs: WAL journaling (readers never
// block the single writer), busy_timeout 5000ms (writers queue politely
// instead of failing instantly), foreign_keys ON.
//
// Space management (the hot DB is the largest artifact in a deployment):
//
//	auto_vacuum=INCREMENTAL — the DB reclaims freed pages at the end of the
//	  file, so DELETEd rows actually give the disk back. IMPORTANT: this
//	  pragma only takes effect on a database that has NEVER been written to
//	  (it must be set before the first table is created). An EXISTING
//	  database needs a one-time conversion: `PRAGMA auto_vacuum=INCREMENTAL;
//	  VACUUM;` while the service is STOPPED (see docs/deploy.md).
//	journal_size_limit=64MB — WAL files are truncated instead of growing
//	  without bound.
//	wal_autocheckpoint=2000 — a passive checkpoint is attempted every ~2000
//	  pages (~8MB) of WAL, up from SQLite's 1000-page default, so the hot
//	  write path spends less time stalling on checkpoints; journal_size_limit
//	  still bounds the file.
//	cache_size=-8000 — 8MB page cache per connection (negative = KiB), a
//	  bound instead of the driver's default.
//
// driverName is "sqlite3" (mattn, CGO). It is kept a parameter so tests and
// future drivers can inject a variant without touching call sites.
func Open(path string) (*DB, error) {
	return openDriver("sqlite3", path)
}

// openDriver is the injection seam: same construction, parameterized driver
// registration name (see pg/cold.go for the cold-side analog).
func openDriver(driverName, path string) (*DB, error) {
	// The DSN carries the per-connection invariants (mattn extensions); the
	// Exec pass below repeats them so a future driver that ignores DSN
	// params still gets the same behavior.
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1"+
		"&_auto_vacuum=2&_cache_size=-8000&_txlock=immediate", path)
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	// A bounded pool: SQLite serializes writers anyway, and an unbounded pool
	// multiplied the per-connection page cache.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(0)
	for _, p := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
		"PRAGMA synchronous=NORMAL",          // WAL-safe default; durability knobs are deployment policy
		"PRAGMA journal_size_limit=67108864", // 64MB: truncate the WAL, don't hoard it
		"PRAGMA wal_autocheckpoint=2000",     // checkpoint every ~8MB of WAL (vs 1000-page/~4MB default): fewer passive-checkpoint stalls on the hot write path; the WAL is capped by journal_size_limit above
		"PRAGMA cache_size=-8000",            // 8MB page cache per connection
	} {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("sqlite: %s: %w", p, err)
		}
	}
	// auto_vacuum=INCREMENTAL must be set on a FRESH database (before any
	// table exists) to take effect. On an existing database the pragma is a
	// no-op — the deployment must run the one-time `PRAGMA
	// auto_vacuum=INCREMENTAL; VACUUM;` conversion instead (docs/deploy.md).
	if err := enableAutoVacuum(ctxBackground(), db, path); err != nil {
		db.Close()
		return nil, err
	}
	d := &DB{DB: db, sqldriver: driverName}
	d.msgw = &msgWriter{db: db}
	d.msgr = &msgReader{db: db}
	d.identity = &identityStore{db: db}
	d.conv = &convStore{db: db}
	d.media = &mediaStore{db: db}
	d.sys = &sysStore{db: db}
	return d, nil
}

// ctxBackground is a tiny helper for the open-time pragma (no caller ctx
// exists yet at Open time).
func ctxBackground() context.Context { return context.Background() }

// enableAutoVacuum applies `PRAGMA auto_vacuum=INCREMENTAL` when the
// database file is still empty (no user tables), which is the only moment
// the setting can be established. An existing database is left untouched —
// converting it requires the offline VACUUM procedure documented in
// docs/deploy.md, and silently rewriting a production file at startup is
// not acceptable.
func enableAutoVacuum(ctx context.Context, db *sql.DB, path string) error {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`).Scan(&n); err != nil {
		return fmt.Errorf("sqlite: auto_vacuum probe %s: %w", path, err)
	}
	if n > 0 {
		// Existing database: the pragma would be ignored anyway. Report the
		// state so the operator can act on it.
		return nil
	}
	if _, err := db.ExecContext(ctx, `PRAGMA auto_vacuum=INCREMENTAL`); err != nil {
		return fmt.Errorf("sqlite: auto_vacuum=INCREMENTAL: %w", err)
	}
	return nil
}

// Migrate applies the embedded hot migration tree (migrations.FS, dialect
// sqlite, root "hot"). Idempotent; checksum-verified (migrate.ErrChecksumMismatch
// on tampered history).
func (d *DB) Migrate(ctx context.Context) error {
	r := &migrate.Runner{
		Dialect: migrate.DialectSQLite,
		Dir:     migrations.FS,
		DB:      d.DB,
		Root:    migrate.DirHot,
	}
	return r.Up(ctx)
}

// BatchExec runs fn once per element inside a single transaction using a
// prepared statement, committing at the end (rolling back on any error).
// All-or-nothing is the group-commit invariant (api.go MsgWriter doc).
func BatchExec[T any](ctx context.Context, tx *sql.Tx, query string, items []T, fn func(stmt *sql.Stmt, v T) error) error {
	if len(items) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return fmt.Errorf("sqlite: prepare: %w", err)
	}
	defer stmt.Close()
	for _, v := range items {
		if err := fn(stmt, v); err != nil {
			return err
		}
	}
	return nil
}

// inTx runs fn inside a transaction; a returned error rolls back.
func inTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// isDup maps a driver-level unique violation onto store.ErrDuplicate so that
// callers can errors.Is it without importing the sqlite3 driver.
func isDup(err error) bool {
	var se sqlite3.Error
	if errors.As(err, &se) {
		return se.Code == sqlite3.ErrConstraint &&
			(se.ExtendedCode == sqlite3.ErrConstraintUnique || se.ExtendedCode == sqlite3.ErrConstraintPrimaryKey)
	}
	return false
}

// assert interfaces.
var (
	_ store.MsgWriter     = (*DB)(nil)
	_ store.MsgReader     = (*DB)(nil)
	_ store.IdentityStore = (*DB)(nil)
	_ store.ConvStore     = (*DB)(nil)
	_ store.MediaStore    = (*DB)(nil)
	_ store.MediaQuota    = (*DB)(nil)
	_ store.SysStore      = (*DB)(nil)
)

// Domain accessors (explicit delegation — no accidental method promotion of
// sql.DB methods that would shadow interface semantics).

func (d *DB) AppendMessages(ctx context.Context, batch []store.PendingMsg) error {
	return d.msgw.AppendMessages(ctx, batch)
}

func (d *DB) AllocateSeq(ctx context.Context, cid int64, n int) (int64, error) {
	return d.msgw.AllocateSeq(ctx, cid, n)
}

func (d *DB) ReadHot(ctx context.Context, cid int64, beforeSeq int64, limit int, asc bool) ([]store.HotMsg, error) {
	return d.msgr.ReadHot(ctx, cid, beforeSeq, limit, asc)
}

func (d *DB) FirstSeqAfter(ctx context.Context, cid int64, since int64) (int64, error) {
	return d.msgr.FirstSeqAfter(ctx, cid, since)
}

func (d *DB) UpsertUser(ctx context.Context, u *store.User) error {
	return d.identity.UpsertUser(ctx, u)
}

func (d *DB) LookupByIdentity(ctx context.Context, idType int, idValue string) (int64, error) {
	return d.identity.LookupByIdentity(ctx, idType, idValue)
}

func (d *DB) GetUser(ctx context.Context, uid int64) (*store.User, error) {
	return d.identity.GetUser(ctx, uid)
}

func (d *DB) GetBan(ctx context.Context, uid int64) (*store.Ban, error) {
	return d.identity.GetBan(ctx, uid)
}

func (d *DB) SetBan(ctx context.Context, b *store.Ban) error { return d.identity.SetBan(ctx, b) }

func (d *DB) CreateConv(ctx context.Context, m *store.ConvMeta, creatorUID int64) (int64, error) {
	return d.conv.CreateConv(ctx, m, creatorUID)
}

func (d *DB) AddMember(ctx context.Context, m *store.Member) error { return d.conv.AddMember(ctx, m) }

func (d *DB) RemoveMember(ctx context.Context, cid, uid int64) error {
	return d.conv.RemoveMember(ctx, cid, uid)
}

func (d *DB) CheckMember(ctx context.Context, cid, uid int64) (*store.Member, error) {
	return d.conv.CheckMember(ctx, cid, uid)
}

func (d *DB) ListMembers(ctx context.Context, cid int64) ([]store.Member, error) {
	return d.conv.ListMembers(ctx, cid)
}

func (d *DB) SetMemberRole(ctx context.Context, cid, uid int64, role int) error {
	return d.conv.SetMemberRole(ctx, cid, uid, role)
}

func (d *DB) GetConvMeta(ctx context.Context, cid int64) (*store.ConvMeta, error) {
	return d.conv.GetConvMeta(ctx, cid)
}

func (d *DB) SingleChatBetween(ctx context.Context, uid1, uid2 int64) (int64, error) {
	return d.conv.SingleChatBetween(ctx, uid1, uid2)
}

func (d *DB) NextSeq(ctx context.Context, cid int64) (int64, error) { return d.conv.NextSeq(ctx, cid) }

func (d *DB) Insert(ctx context.Context, m *store.Media) error { return d.media.Insert(ctx, m) }

func (d *DB) Get(ctx context.Context, mediaID string) (*store.Media, error) {
	return d.media.Get(ctx, mediaID)
}

func (d *DB) UpdateStatus(ctx context.Context, mediaID string, status int) error {
	return d.media.UpdateStatus(ctx, mediaID, status)
}

func (d *DB) IncRefCount(ctx context.Context, mediaID string, delta int64) error {
	return d.media.IncRefCount(ctx, mediaID, delta)
}

func (d *DB) AddRef(ctx context.Context, mediaID string) error { return d.media.AddRef(ctx, mediaID) }

func (d *DB) UsageOf(ctx context.Context, uid int64) (int64, error) {
	return d.media.UsageOf(ctx, uid)
}

func (d *DB) TotalUsage(ctx context.Context) (int64, error) {
	return d.media.TotalUsage(ctx)
}

func (d *DB) UpsertFlag(ctx context.Context, f *store.Flag) error { return d.sys.UpsertFlag(ctx, f) }

func (d *DB) AllFlags(ctx context.Context) ([]store.Flag, error) { return d.sys.AllFlags(ctx) }

func (d *DB) UpsertPresence(ctx context.Context, p *store.Presence) error {
	return d.sys.UpsertPresence(ctx, p)
}

func (d *DB) GetPresence(ctx context.Context, uid int64) (*store.Presence, error) {
	return d.sys.GetPresence(ctx, uid)
}

func (d *DB) RateLimitIncr(ctx context.Context, subject, action string, windowStart int64) (*store.RateWindow, error) {
	return d.sys.RateLimitIncr(ctx, subject, action, windowStart)
}

func (d *DB) GetPenalty(ctx context.Context, subject, penalty string) (*store.Penalty, error) {
	return d.sys.GetPenalty(ctx, subject, penalty)
}

func (d *DB) SetPenalty(ctx context.Context, p *store.Penalty) error { return d.sys.SetPenalty(ctx, p) }
