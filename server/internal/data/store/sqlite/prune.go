package sqlite

import (
	"context"
	"fmt"

	"github.com/yingo-server/UFOT/server/internal/data/store"
)

// deleteHotBeforeSQL prunes the hot message table by sent_at. It rides
// idx_msg_sent (msg_messages_hot.sent_at), so the delete is an index range
// scan, not a table scan.
const deleteHotBeforeSQL = `DELETE FROM msg_messages_hot WHERE sent_at < ?`

// DeleteHotBefore removes hot message rows strictly older than the cutoff
// (unix seconds) and returns how many rows were removed.
//
// This is the HOT-side deletion of the archive flow: the archiver copies the
// batch into the cold archive FIRST and only then calls this. A cold (Postgres)
// handle must never expose this operation — the hot table lives here.
//
// Idempotent: deleting an already-deleted range is a no-op returning 0.
func (db *DB) DeleteHotBefore(ctx context.Context, before int64) (int64, error) {
	res, err := db.ExecContext(ctx, deleteHotBeforeSQL, before)
	if err != nil {
		return 0, fmt.Errorf("sqlite: delete hot before %d: %w", before, err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ------------------------------------------------------- trim (expiry rules)

// trimStatements is the WHITELIST mapping from a policy table name to a
// fixed DELETE statement. The table name is a KEY, never SQL text — the same
// discipline as the ORDER BY whitelist in msgreader.go. A name that is not
// in this map is rejected, so no caller-supplied string ever reaches SQL.
//
// Each statement deletes rows whose EXPIRY column is older than the cutoff:
//
//	sys_rate_limit         — window_start (fixed-window counters)
//	identity_login_history — at (login history; cold side is mirrored too)
//	identity_refresh       — expires_at (refresh tokens)
//	media_media            — created_at AND ref_count = 0 (unreferenced media)
var trimStatements = map[string]string{
	"sys_rate_limit": `DELETE FROM sys_rate_limit WHERE window_start < ?`,
	// identity_login_history lives in the COLD store (identity.login_history).
	// The hot DB has no such table, so the statement is a no-op guarded by a
	// table-existence check in TrimTable.
	"identity_login_history": `DELETE FROM identity_login_history WHERE at < ?`,
	"identity_refresh":       `DELETE FROM identity_refresh WHERE expires_at < ?`,
	"media_media":            `DELETE FROM media_media WHERE created_at < ? AND ref_count = 0`,
}

// TrimTable deletes expired rows of one whitelisted table. The table name is
// looked up in a fixed map — unknown names are an error, never SQL text.
// olderThan is a unix-seconds cutoff.
//
// Tables that do not exist in this database (e.g. identity_login_history,
// which lives in the cold store) are skipped with 0 rows removed rather than
// failing the whole trim pass.
func (db *DB) TrimTable(ctx context.Context, table string, olderThan int64) (int64, error) {
	stmt, ok := trimStatements[table]
	if !ok {
		return 0, fmt.Errorf("sqlite: trim: table %q is not in the trim whitelist", table)
	}
	exists, err := db.tableExists(ctx, table)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	res, err := db.ExecContext(ctx, stmt, olderThan)
	if err != nil {
		return 0, fmt.Errorf("sqlite: trim %s: %w", table, err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// tableExists reports whether a table is present in the main schema. The
// name comes from the whitelist map, never from a caller.
func (db *DB) tableExists(ctx context.Context, table string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name = ?`, table).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("sqlite: table exists %s: %w", table, err)
	}
	return n > 0, nil
}

// IncrementalVacuum reclaims free pages at the end of the database file.
// It only has an effect when the database was created with
// auto_vacuum=INCREMENTAL (see sqlite.Open / PRAGMA notes); on a database
// without it, the statement is a harmless no-op.
func (db *DB) IncrementalVacuum(ctx context.Context) error {
	if _, err := db.ExecContext(ctx, `PRAGMA incremental_vacuum`); err != nil {
		return fmt.Errorf("sqlite: incremental vacuum: %w", err)
	}
	return nil
}

// assert the hot store satisfies the archiver/trimmer ports.
var (
	_ store.HotPruner  = (*DB)(nil)
	_ store.TrimTarget = (*DB)(nil)
)
