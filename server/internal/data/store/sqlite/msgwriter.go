package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/mattn/go-sqlite3"

	"github.com/yingo-server/OpenDiscord/server/internal/data/store"
)

// msgWriter implements store.MsgWriter (hot write path: flusher + seqalloc).

type msgWriter struct {
	db *sql.DB
	// maxRowsPerStmt overrides the SQLite-variable-derived chunk size (0 =
	// derive from the driver's SQLITE_MAX_VARIABLE_NUMBER). Tests set it to
	// force a batch to be split across several statements.
	maxRowsPerStmt int
}

// insertMsgCols is the number of bind parameters per row of msg_messages_hot
// (msg_id, cid, seq, sender_id, client_msg_id, content, sent_at, edited,
// recalled).
const insertMsgCols = 9

// insertMsgSQL is the single-row INSERT, kept as the BenchmarkAppendMessages
// /single_row baseline (see msgwriter_test.go). The live path uses the
// multi-row form built by buildMultiInsert.
const insertMsgSQL = `INSERT INTO msg_messages_hot
(msg_id, cid, seq, sender_id, client_msg_id, content, sent_at, edited, recalled)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`

// insertMsgHead is the fixed prefix of the multi-row INSERT. Only the NUMBER
// of value groups varies; no user data ever enters the SQL text, so the query
// stays a constant shape (api "no dynamic SQL" rule is about VALUES only).
const insertMsgHead = `INSERT INTO msg_messages_hot
(msg_id, cid, seq, sender_id, client_msg_id, content, sent_at, edited, recalled)
VALUES `

// insertMsgTuple is one value group; it must carry exactly insertMsgCols '?'.
const insertMsgTuple = "(?, ?, ?, ?, ?, ?, ?, ?, ?)"

// SQLite caps the number of host parameters per statement
// (SQLITE_MAX_VARIABLE_NUMBER): 999 on builds older than 3.32, 32766 on
// modern ones. The mattn driver exposes the effective runtime limit, so we
// read it once per process and cache it. If the driver does not expose it we
// fall back to the conservative pre-3.32 ceiling (999) — always safe, just
// more statements per batch.
var (
	maxRowsOnce sync.Once
	maxRowsN    int
)

// driverVarLimit asks the connected driver for its effective
// SQLITE_LIMIT_VARIABLE_NUMBER. Returns ok=false when the driver does not
// expose it (a non-sqlite3 test driver).
func driverVarLimit(db *sql.DB) (int, bool) {
	conn, err := db.Conn(context.Background())
	if err != nil {
		return 0, false
	}
	defer conn.Close()
	n := 0
	err = conn.Raw(func(dc any) error {
		sc, ok := dc.(*sqlite3.SQLiteConn)
		if !ok {
			return errors.New("sqlite: not a sqlite3 connection")
		}
		n = sc.GetLimit(sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER)
		return nil
	})
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// deriveMaxRows maps the bind-parameter ceiling onto a row count per
// multi-row INSERT (the column count per row is fixed at insertMsgCols).
func deriveMaxRows(db *sql.DB) int {
	limit := 999 // conservative default (pre-3.32 SQLite / unknown driver)
	if v, ok := driverVarLimit(db); ok && v > 0 {
		limit = v
	}
	r := limit / insertMsgCols
	if r < 1 {
		r = 1
	}
	return r
}

// rowsPerStmt is the largest number of rows a single multi-row INSERT may
// carry without exceeding the driver's bind-parameter ceiling.
func (w *msgWriter) rowsPerStmt() int {
	if w.maxRowsPerStmt > 0 {
		return w.maxRowsPerStmt
	}
	maxRowsOnce.Do(func() { maxRowsN = deriveMaxRows(w.db) })
	return maxRowsN
}

// buildMultiInsert renders the multi-row INSERT for `rows` value groups.
func buildMultiInsert(rows int) string {
	var b strings.Builder
	b.WriteString(insertMsgHead)
	for i := 0; i < rows; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(insertMsgTuple)
	}
	return b.String()
}

// AppendMessages inserts a batch in ONE transaction: all rows or none.
//
// Rows are packed into multi-value INSERT statements ("INSERT ... VALUES
// (...),(...),..."), each statement carrying at most rowsPerStmt() rows so the
// per-batch bind count never exceeds SQLite's variable ceiling; a batch larger
// than one statement is split into several statements *inside the same
// transaction*, preserving the all-or-nothing contract.
//
// A duplicate (cid, sender_id, client_msg_id) or (cid, seq) unique violation
// anywhere in the batch aborts the whole batch and is surfaced as
// store.ErrDuplicate (wrapped), so the flusher can reply 409-equivalent /
// dedupguard can translate it into the old ack. NO membership checks here
// (api.go red line).
func (w *msgWriter) AppendMessages(ctx context.Context, batch []store.PendingMsg) error {
	if len(batch) == 0 {
		return nil
	}
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: append begin: %w", err)
	}
	defer tx.Rollback() // no-op after commit

	rps := w.rowsPerStmt()
	for start := 0; start < len(batch); start += rps {
		end := start + rps
		if end > len(batch) {
			end = len(batch)
		}
		if err := w.insertChunk(ctx, tx, batch[start:end]); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: append commit: %w", err)
	}
	return nil
}

// insertChunk writes one multi-row INSERT inside tx.
func (w *msgWriter) insertChunk(ctx context.Context, tx *sql.Tx, chunk []store.PendingMsg) error {
	args := make([]any, 0, len(chunk)*insertMsgCols)
	for _, m := range chunk {
		args = append(args, m.MsgID, m.Cid, m.Seq, m.SenderID, m.ClientMsgID, m.Content, m.SentAt, int64(0), int64(0))
	}
	if _, err := tx.ExecContext(ctx, buildMultiInsert(len(chunk)), args...); err != nil {
		if isDup(err) {
			cid, sender, cmid := diagnoseDup(ctx, tx, chunk)
			return fmt.Errorf("sqlite: duplicate msg (cid=%d sender=%d client_msg_id=%q): %w",
				cid, sender, cmid, store.ErrDuplicate)
		}
		return fmt.Errorf("sqlite: insert msgs (%d rows): %w", len(chunk), err)
	}
	return nil
}

// diagnoseDup locates (best effort) which row of a failed multi-row INSERT
// collided, so the wrapped store.ErrDuplicate keeps the single-row error
// quality the callers had before. It runs only on the (rare) error path, so
// the per-row probe does not touch the hot path.
//
// Order of investigation:
//  1. an intra-chunk conflict (two rows of the SAME statement carrying the
//     same dedup key) — the statement is atomic, so the offending row is not
//     visible to the database yet;
//  2. a conflict against an already committed row — probed inside the
//     transaction.
func diagnoseDup(ctx context.Context, tx *sql.Tx, chunk []store.PendingMsg) (int64, int64, string) {
	type key struct {
		cid    int64
		sender int64
		cmid   string
	}
	seen := make(map[key]int, len(chunk))
	for i, m := range chunk {
		k := key{m.Cid, m.SenderID, m.ClientMsgID}
		if j, ok := seen[k]; ok {
			_ = j
			return m.Cid, m.SenderID, m.ClientMsgID
		}
		seen[k] = i
	}
	for _, m := range chunk {
		var one int
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM msg_messages_hot
WHERE cid = ? AND ((sender_id = ? AND client_msg_id = ?) OR seq = ?) LIMIT 1`,
			m.Cid, m.SenderID, m.ClientMsgID, m.Seq).Scan(&one)
		if err == nil {
			return m.Cid, m.SenderID, m.ClientMsgID
		}
	}
	if len(chunk) > 0 {
		m := chunk[0]
		return m.Cid, m.SenderID, m.ClientMsgID
	}
	return 0, 0, ""
}

// appendSingleRow is the pre-optimization path (one prepared statement per
// row, one transaction per batch). It is retained as the
// BenchmarkAppendMessages/single_row baseline and kept green as a documented
// reference; the live write path uses the multi-row INSERT above.
func (w *msgWriter) appendSingleRow(ctx context.Context, batch []store.PendingMsg) error {
	if len(batch) == 0 {
		return nil
	}
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: append begin: %w", err)
	}
	defer tx.Rollback() // no-op after commit

	err = BatchExec(ctx, tx, insertMsgSQL, batch, func(stmt *sql.Stmt, m store.PendingMsg) error {
		if _, err := stmt.ExecContext(ctx, m.MsgID, m.Cid, m.Seq, m.SenderID, m.ClientMsgID, m.Content, m.SentAt, 0, 0); err != nil {
			if isDup(err) {
				return fmt.Errorf("sqlite: duplicate msg (cid=%d sender=%d client_msg_id=%q): %w",
					m.Cid, m.SenderID, m.ClientMsgID, store.ErrDuplicate)
			}
			return fmt.Errorf("sqlite: insert msg cid=%d seq=%d: %w", m.Cid, m.Seq, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: append commit: %w", err)
	}
	return nil
}

// MarkEdited implements store.MsgWriter.MarkEdited: a conditional UPDATE that
// rewrites the content and stamps edited = at. Matches only the exact
// (cid, msg_id, sender_id) row; the caller owns authorization and the edit
// window (mechanism/policy split, same as MarkRecalled).
func (w *msgWriter) MarkEdited(ctx context.Context, cid, msgID, senderID int64, content []byte, at int64) (*store.HotMsg, error) {
	res, err := w.db.ExecContext(ctx,
		`UPDATE msg_messages_hot SET content = ?, edited = ? WHERE cid = ? AND msg_id = ? AND sender_id = ? AND recalled = 0`,
		content, at, cid, msgID, senderID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: mark edited cid=%d msg=%d: %w", cid, msgID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Row absent or already recalled: read to distinguish 404 vs 410.
		var m store.HotMsg
		qerr := w.db.QueryRowContext(ctx,
			`SELECT msg_id, cid, seq, sender_id, client_msg_id, content, sent_at, edited, recalled
FROM msg_messages_hot WHERE cid = ? AND msg_id = ?`, cid, msgID).
			Scan(&m.MsgID, &m.Cid, &m.Seq, &m.SenderID, &m.ClientMsgID, &m.Content, &m.SentAt, &m.Edited, &m.Recalled)
		if errors.Is(qerr, sql.ErrNoRows) {
			return nil, fmt.Errorf("sqlite: msg cid=%d id=%d: %w", cid, msgID, store.ErrNotFound)
		}
		if qerr != nil {
			return nil, fmt.Errorf("sqlite: read edited msg cid=%d id=%d: %w", cid, msgID, qerr)
		}
		if m.Recalled != 0 {
			return nil, fmt.Errorf("sqlite: msg cid=%d id=%d recalled: %w", cid, msgID, store.ErrNotFound)
		}
		return &m, nil
	}
	var m store.HotMsg
	err = w.db.QueryRowContext(ctx,
		`SELECT msg_id, cid, seq, sender_id, client_msg_id, content, sent_at, edited, recalled
FROM msg_messages_hot WHERE cid = ? AND msg_id = ?`, cid, msgID).
		Scan(&m.MsgID, &m.Cid, &m.Seq, &m.SenderID, &m.ClientMsgID, &m.Content, &m.SentAt, &m.Edited, &m.Recalled)
	if err != nil {
		return nil, fmt.Errorf("sqlite: re-read edited msg cid=%d id=%d: %w", cid, msgID, err)
	}
	return &m, nil
}

// MarkRecalled implements store.MsgWriter.MarkRecalled: a conditional
// UPDATE that only matches the exact (cid, msg_id, sender_id) row and only
// when it has not been recalled already (idempotent re-recall succeeds
// without touching the row).
func (w *msgWriter) MarkRecalled(ctx context.Context, cid, msgID, senderID, at int64) (*store.HotMsg, error) {
	res, err := w.db.ExecContext(ctx,
		`UPDATE msg_messages_hot SET recalled = ? WHERE cid = ? AND msg_id = ? AND sender_id = ? AND recalled = 0`,
		at, cid, msgID, senderID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: mark recalled cid=%d msg=%d: %w", cid, msgID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Either the row is absent, or it was already recalled. Read to
		// distinguish so callers can compose 404 vs idempotent-OK.
		var m store.HotMsg
		qerr := w.db.QueryRowContext(ctx,
			`SELECT msg_id, cid, seq, sender_id, client_msg_id, content, sent_at, edited, recalled
FROM msg_messages_hot WHERE cid = ? AND msg_id = ?`, cid, msgID).
			Scan(&m.MsgID, &m.Cid, &m.Seq, &m.SenderID, &m.ClientMsgID, &m.Content, &m.SentAt, &m.Edited, &m.Recalled)
		if errors.Is(qerr, sql.ErrNoRows) {
			return nil, fmt.Errorf("sqlite: msg cid=%d id=%d: %w", cid, msgID, store.ErrNotFound)
		}
		if qerr != nil {
			return nil, fmt.Errorf("sqlite: read recalled msg cid=%d id=%d: %w", cid, msgID, qerr)
		}
		return &m, nil // already recalled: report the row, caller treats as ok
	}
	var m store.HotMsg
	err = w.db.QueryRowContext(ctx,
		`SELECT msg_id, cid, seq, sender_id, client_msg_id, content, sent_at, edited, recalled
FROM msg_messages_hot WHERE cid = ? AND msg_id = ?`, cid, msgID).
		Scan(&m.MsgID, &m.Cid, &m.Seq, &m.SenderID, &m.ClientMsgID, &m.Content, &m.SentAt, &m.Edited, &m.Recalled)
	if err != nil {
		return nil, fmt.Errorf("sqlite: re-read recalled msg cid=%d id=%d: %w", cid, msgID, err)
	}
	return &m, nil
}

// AllocateSeq atomically reserves the next n sequence numbers for cid and
// returns the inclusive start: the caller owns [start, start+n-1].
//
// Preferred form (SQLite >= 3.35):
//
//	UPDATE conv_seq SET next_seq = next_seq + ? WHERE cid = ? RETURNING next_seq - ?
//
// RETURNING makes it a single atomic statement, safe under concurrent writers.
// If the bundled system sqlite3 is older than 3.35 the RETURNING statement
// fails at runtime — we then fall back to the equivalent BEGIN IMMEDIATE
// transaction (SELECT inside a write-locked tx, then UPDATE), which is atomic
// because SQLite write transactions are serialized. Either path guarantees
// no two concurrent AllocateSeq calls ever hand out overlapping ranges.
func (w *msgWriter) AllocateSeq(ctx context.Context, cid int64, n int) (int64, error) {
	if n <= 0 {
		return 0, fmt.Errorf("sqlite: AllocateSeq: n must be >= 1 (cid=%d n=%d)", cid, n)
	}
	// Fast path: single atomic statement.
	var next int64
	err := w.db.QueryRowContext(ctx,
		`UPDATE conv_seq SET next_seq = next_seq + ? WHERE cid = ? RETURNING next_seq - ?`,
		n, cid, n).Scan(&next)
	if err == nil {
		return next, nil
	}
	if !errors.Is(err, sql.ErrNoRows) && !isReturningUnsupported(err) {
		return 0, fmt.Errorf("sqlite: allocate seq cid=%d: %w", cid, err)
	}
	// Missing row: create the allocator row, then retry.
	if errors.Is(err, sql.ErrNoRows) {
		if _, ierr := w.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO conv_seq (cid, next_seq) VALUES (?, 1)`, cid); ierr != nil {
			return 0, fmt.Errorf("sqlite: init conv_seq cid=%d: %w", cid, ierr)
		}
	}
	return w.allocateSeqTx(ctx, cid, n)
}

// allocateSeqTx is the pre-3.35 fallback: BEGIN IMMEDIATE serializes writers,
// so read-then-update inside the transaction is atomic.
func (w *msgWriter) allocateSeqTx(ctx context.Context, cid int64, n int) (int64, error) {
	tx, err := w.db.BeginTx(ctx, &sql.TxOptions{}) // mattn maps this to BEGIN DEFERRED; we lock via UPDATE first below
	if err != nil {
		return 0, fmt.Errorf("sqlite: alloc begin: %w", err)
	}
	defer tx.Rollback()

	// Take the write lock FIRST (UPDATE of the row), then read back — under
	// SQLite's database-level write lock this is equivalent to BEGIN IMMEDIATE.
	res, err := tx.ExecContext(ctx, `UPDATE conv_seq SET next_seq = next_seq + ? WHERE cid = ?`, n, cid)
	if err != nil {
		return 0, fmt.Errorf("sqlite: alloc update: %w", err)
	}
	ra, _ := res.RowsAffected()
	if ra == 0 {
		if _, ierr := tx.ExecContext(ctx, `INSERT INTO conv_seq (cid, next_seq) VALUES (?, ?)`, cid, int64(n)+1); ierr != nil {
			return 0, fmt.Errorf("sqlite: alloc insert: %w", ierr)
		}
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("sqlite: alloc commit: %w", err)
		}
		return 1, nil
	}
	var next int64
	if err := tx.QueryRowContext(ctx, `SELECT next_seq FROM conv_seq WHERE cid = ?`, cid).Scan(&next); err != nil {
		return 0, fmt.Errorf("sqlite: alloc select: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("sqlite: alloc commit: %w", err)
	}
	return next - int64(n), nil
}

// isReturningUnsupported reports whether err smells like "RETURNING not
// implemented" (older sqlite3: "syntax error" near RETURNING).
func isReturningUnsupported(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, `near "RETURNING"`) ||
		(strings.Contains(msg, "RETURNING") && strings.Contains(msg, "syntax error"))
}
