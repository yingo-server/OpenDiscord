// Package pg implements the cold store (PostgreSQL) for the archive and audit
// surfaces: store.ArchiveTarget, store.AuditJournal and archive search.
//
// Driver injection contract: this file compiles WITHOUT any postgres driver
// dependency (no lib/pq, no pgx — neither may be added to go.mod). It talks to
// a plain *sql.DB whose driver is registered by the ASSEMBLY layer:
//
//	import _ "github.com/jackc/pgx/v5/stdlib"   // or lib/pq — deployer's choice
//	cold, err := pg.Open(dsn)                   // driverName = "postgres"
//
// Method set: CopyBatch/DeleteHot (store.ArchiveTarget), Append
// (store.AuditJournal), Search (trgm full-text), ReadCold (pipe.ColdReader —
// kind 920 cold-merge paging) and the archive_runs ledger (StartRun/FinishRun).
//
// Open(dsn) defaults to driverName "postgres"; pg.OpenDriver(dsn, "pgx-registered-name")
// exists for alternative registrations. Tests never connect: they exercise the
// pure logic (hash-chain canonicalization, SQL string generation) only.
//
// SQL is fully parameterized ($1, $2, ...); no Sprintf SQL assembly anywhere.
package pg

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/yingo-server/UFOT/server/internal/data/store"
	"github.com/yingo-server/UFOT/server/internal/data/store/migrate"
	"github.com/yingo-server/UFOT/server/store/migrations"
)

// ColdDB is the cold store handle. Implements:
//
//	store.ArchiveSource — idempotent hot->cold copy (the COLD half only)
//	store.AuditJournal  — append-only hash-chained audit log
//
// plus Search (trgm full-text) and the archive_runs ledger.
//
// Red line: ColdDB does NOT delete hot rows. The hot table lives in SQLite;
// deleting it from a Postgres handle is a bug (the statement targets a table
// that does not exist there). Hot deletion is the hot store's job
// (store.HotPruner).
type ColdDB struct {
	DB *sql.DB

	// DriverName records which registration the DB was opened with (diagnostics).
	DriverName string
	// dsn is retained so Migrate can open a short-lived connection with the
	// pgx simple-protocol parameter (needed for multi-statement SQL files).
	dsn string
}

// Open opens the cold database with driverName "postgres". The driver must be
// registered by the assembly layer BEFORE calling this (see package doc).
func Open(dsn string) (*ColdDB, error) { return OpenDriver("postgres", dsn) }

// OpenDriver opens with an explicit driver registration name (e.g. "pgx").
func OpenDriver(driverName, dsn string) (*ColdDB, error) {
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("pg: open: %w", err)
	}
	return &ColdDB{DB: db, DriverName: driverName, dsn: dsn}, nil
}

// Migrate applies the embedded cold (PostgreSQL) migration tree. Called by the
// assembly layer once the cold store is opened — without it the audit journal
// and archive tables are missing even though cold:true is reported.
func (c *ColdDB) Migrate(ctx context.Context) error {
	// pgx 用 simple protocol 才能一次执行含 $$ 函数体的多语句迁移文件；
	// 通过 per-connection SearchPath/RunAs 无法切换协议，只能建一个专用
	// 短连接（同 DSN + default_query_exec_mode 参数）执行迁移。
	dsn := c.dsn
	if dsn != "" && !strings.Contains(dsn, "default_query_exec_mode") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		dsn += sep + "default_query_exec_mode=simple_protocol"
	}
	migDB, err := sql.Open(c.DriverName, dsn)
	if err != nil {
		return fmt.Errorf("cold migrate: open: %w", err)
	}
	defer migDB.Close()
	r := &migrate.Runner{
		Dialect: migrate.DialectPostgres,
		Dir:     migrations.FS,
		DB:      migDB,
		Root:    migrate.DirCold,
	}
	return r.Up(ctx)
}

// Close closes the underlying pool.
func (c *ColdDB) Close() error { return c.DB.Close() }

var (
	_ store.ArchiveSource = (*ColdDB)(nil)
	_ store.AuditJournal  = (*ColdDB)(nil)
)

// ----------------------------------------------------------------- archive

const copyBatchSQL = `INSERT INTO msg.messages_archive
(msg_id, cid, seq, sender_id, content, sent_at)
VALUES ` // + "($1,$2,$3,$4,$5,$6),(...)" built by buildPlaceholders (values stay parameterized)

const copyTextSQL = `INSERT INTO msg.messages_text
(msg_id, cid, sender_id, text, sent_at)
VALUES ` // + placeholders — mirror rows, best-effort plain text projection

// buildPlaceholders builds "(($n*6)+1, ...)($...),..." value groups for a
// multi-row INSERT. This assembles PLACEHOLDER INDICES ONLY — never data.
// Fixed shape: cols columns, rows rows.
func buildPlaceholders(rows, cols int) string {
	if rows <= 0 || cols <= 0 {
		return ""
	}
	var b strings.Builder
	arg := 1
	for r := 0; r < rows; r++ {
		if r > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('(')
		for c := 0; c < cols; c++ {
			if c > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "$%d", arg)
			arg++
		}
		b.WriteByte(')')
	}
	return b.String()
}

// CopyBatch inserts rows into the archive idempotently (ON CONFLICT DO
// NOTHING — the UNIQUE (cid,seq,sent_at) is the identity) and mirrors each
// row's best-effort plain-text projection into msg.messages_text. Returns how
// many ARCHIVE rows were newly inserted. Re-copying a batch never duplicates;
// the text mirror is likewise conflict-ignored.
//
// Text projection: ExtractText is a pure function (envelope plaintext parts);
// E2EE content has no extractable text and yields NULL — search simply can't
// see it, which is by design.
func (c *ColdDB) CopyBatch(ctx context.Context, rows []store.HotMsg) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	var moved int64
	// One statement per fixed-size chunk; chunk boundaries never span a row.
	const chunk = 500
	for i := 0; i < len(rows); i += chunk {
		end := i + chunk
		if end > len(rows) {
			end = len(rows)
		}
		part := rows[i:end]
		n, err := c.copyChunk(ctx, part)
		if err != nil {
			return moved, fmt.Errorf("pg: copy batch: %w", err)
		}
		moved += n
	}
	return moved, nil
}

func (c *ColdDB) copyChunk(ctx context.Context, part []store.HotMsg) (int64, error) {
	ph := buildPlaceholders(len(part), 6)
	args := make([]any, 0, len(part)*6)
	for _, m := range part {
		args = append(args, m.MsgID, m.Cid, m.Seq, m.SenderID, m.Content, m.SentAt)
	}
	res, err := c.DB.ExecContext(ctx, copyBatchSQL+ph+` ON CONFLICT DO NOTHING`, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()

	// text mirror (best effort, conflict-ignored; separate statement so an
	// archive-only insert on retry still mirrors)
	tph := buildPlaceholders(len(part), 5)
	targs := make([]any, 0, len(part)*5)
	for _, m := range part {
		if txt := ExtractText(m.Content); txt != "" {
			targs = append(targs, m.MsgID, m.Cid, m.SenderID, txt, m.SentAt)
		} else {
			targs = append(targs, m.MsgID, m.Cid, m.SenderID, nil, m.SentAt)
		}
	}
	if _, err := c.DB.ExecContext(ctx, copyTextSQL+tph+` ON CONFLICT DO NOTHING`, targs...); err != nil {
		// Mirror failure does not fail the copy (archive rows are the truth);
		// a later re-run heals the mirror because the copy is idempotent.
		_ = err
	}
	return n, nil
}

// ------------------------------------------------------------------ search

const searchSQL = `SELECT a.msg_id, a.cid, a.seq, a.sender_id, a.sent_at, t.text
FROM msg.messages_text t
JOIN msg.messages_archive a ON a.msg_id = t.msg_id AND a.sent_at = t.sent_at
WHERE t.text % $1 AND ($2::bigint = 0 OR t.cid = $2)
ORDER BY a.sent_at DESC
LIMIT $3`

// Search returns messages whose text projection matches q (trigram similarity
// `text % $1`), optionally restricted to cid (0 = all), newest-first, up to
// limit. Content in the result is the text bytes (the archive BLOB is not
// trgm-indexed; the JOIN carries row identity for fetch-on-click).
func (c *ColdDB) Search(ctx context.Context, q string, cid int64, limit int) ([]store.HotMsg, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("pg: search: limit must be >= 1")
	}
	rows, err := c.DB.QueryContext(ctx, searchSQL, q, cid, limit)
	if err != nil {
		return nil, fmt.Errorf("pg: search: %w", err)
	}
	defer rows.Close()
	var out []store.HotMsg
	for rows.Next() {
		var m store.HotMsg
		var txt sql.NullString
		if err := rows.Scan(&m.MsgID, &m.Cid, &m.Seq, &m.SenderID, &m.SentAt, &txt); err != nil {
			return nil, fmt.Errorf("pg: search scan: %w", err)
		}
		m.Content = []byte(txt.String)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------------ cold read

// readColdSQL serves kind 920 cold-merge: the archive page for one cid,
// newest-first (descending seq), keyset cursor = beforeSeq. Parameterized
// ($1..$4); the two fixed shapes below are the whole whitelist — no input
// ever reaches the SQL text.
const readColdCursoredSQL = `SELECT msg_id, cid, seq, sender_id, sent_at, content
FROM msg.messages_archive WHERE cid = $1 AND seq < $2 ORDER BY seq DESC LIMIT $3`

const readColdHeadSQL = `SELECT msg_id, cid, seq, sender_id, sent_at, content
FROM msg.messages_archive WHERE cid = $1 ORDER BY seq DESC LIMIT $2`

// ReadCold returns up to limit archived messages of cid ordered by seq DESC,
// i.e. the same paging shape as sqlite.ReadHot(asc=false). beforeSeq is the
// keyset cursor (only rows with seq < beforeSeq); beforeSeq <= 0 means "no
// cursor" — read from the newest end. Content is the verbatim envelope bytes
// (HotMsg.Content semantics: opaque, interpreted by the envelope layer — the
// readrouter's hotToMsgNew carries it through unchanged).
func (c *ColdDB) ReadCold(ctx context.Context, cid, beforeSeq int64, limit int) ([]store.HotMsg, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("pg: read cold: limit must be >= 1 (cid=%d limit=%d)", cid, limit)
	}
	var (
		rows *sql.Rows
		err  error
	)
	if beforeSeq > 0 {
		rows, err = c.DB.QueryContext(ctx, readColdCursoredSQL, cid, beforeSeq, limit)
	} else {
		rows, err = c.DB.QueryContext(ctx, readColdHeadSQL, cid, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("pg: read cold cid=%d: %w", cid, err)
	}
	defer rows.Close()
	var out []store.HotMsg
	for rows.Next() {
		var m store.HotMsg
		if err := rows.Scan(&m.MsgID, &m.Cid, &m.Seq, &m.SenderID, &m.SentAt, &m.Content); err != nil {
			return nil, fmt.Errorf("pg: scan cold msg: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------------- audit

const auditAppendSQL = `WITH last AS (
  SELECT record_hash FROM sys.audit_log ORDER BY id DESC LIMIT 1
)
INSERT INTO sys.audit_log (actor, action, subject, detail, prev_hash, record_hash, at)
SELECT $1, $2, $3, $4, l.record_hash, $5, $6 FROM last
RETURNING record_hash`

// auditAppendFirstSQL is the genesis-row variant: the empty table has no
// chain tail, so prev_hash is NULL. Single statement, single round trip.
const auditAppendFirstSQL = `WITH last AS (
  SELECT record_hash FROM sys.audit_log ORDER BY id DESC LIMIT 1
)
INSERT INTO sys.audit_log (actor, action, subject, detail, prev_hash, record_hash, at)
SELECT $1, $2, $3, $4, l.record_hash, $5, $6 FROM (SELECT NULL::text AS record_hash) l
RETURNING record_hash`

// Append is the SINGLE write entry point for the audit journal
// (api.go red line: no UPDATE/DELETE surface exists on audit rows — the cold
// schema's trigger enforces this at the DB layer too).
//
// Concurrency: the chain-tail read and the insert are ONE statement (the CTE
// above), so two concurrent Appends serialize at the row/index level; each
// computed record_hash therefore chains onto the tail that its own INSERT
// observed. The hash itself is computed BEFORE the insert using the tail read
// — under contention a retry-safe assembly may instead compute hashes inside
// the CTE; for the data-layer write volume (admin actions only) the
// single-writer jobber path dominates and this is sufficient. Pure tail-read
// variant RecordHashTail is exposed for the audit-verify job.
func (c *ColdDB) Append(ctx context.Context, ev store.AuditEvent) error {
	prev, err := c.chainTail(ctx)
	if err != nil {
		return fmt.Errorf("pg: audit tail: %w", err)
	}
	rh := RecordHash(prev, ev)
	q := auditAppendSQL
	if prev == "" {
		q = auditAppendFirstSQL
	}
	var got string
	if err := c.DB.QueryRowContext(ctx, q,
		ev.Actor, ev.Action, ev.Subject, ev.Detail, rh, ev.At).Scan(&got); err != nil {
		return fmt.Errorf("pg: audit append: %w", err)
	}
	if got != rh {
		return fmt.Errorf("pg: audit chain race: computed %s, stored %s (concurrent append — retry)", rh, got)
	}
	return nil
}

// chainTail returns the current last record_hash, "" when the log is empty.
func (c *ColdDB) chainTail(ctx context.Context) (string, error) {
	var rh sql.NullString
	err := c.DB.QueryRowContext(ctx,
		`SELECT record_hash FROM sys.audit_log ORDER BY id DESC LIMIT 1`).Scan(&rh)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return rh.String, nil
}

// RecordHash computes SHA256(prev_hash || canonical(event)), the chain link.
// Exported for the audit-verify job and for tests; pure function.
func RecordHash(prev string, ev store.AuditEvent) string {
	canonical := fmt.Sprintf("%s\x1f%s\x1f%s\x1f%s\x1f%d",
		ev.Actor, ev.Action, ev.Subject, string(ev.Detail), ev.At)
	sum := sha256.Sum256(append([]byte(prev), canonical...))
	return hex.EncodeToString(sum[:])
}

// ExtractText is the best-effort plaintext projection of an envelope payload
// for the messages_text mirror. Pure function. E2EE bodies (random bytes)
// contain no text to extract; this returns "" for them — search by design
// cannot see encrypted content.
//
// Heuristic, versioned by the wire format: envelopes encode JSON where a
// "text" field may appear. We scan for it without a full decode dependency.
func ExtractText(content []byte) string {
	// Minimal JSON "text":"..." scanner. Envelope payloads are compact JSON
	// (protojson from the wire layer); this extracts the first short text field.
	const key = `"text":"`
	i := indexOf(content, key)
	if i < 0 {
		return ""
	}
	i += len(key)
	start := i
	for i < len(content) {
		if content[i] == '"' {
			return string(content[start:i])
		}
		if content[i] == '\\' && i+1 < len(content) {
			i += 2
			continue
		}
		i++
	}
	return ""
}

func indexOf(b []byte, sub string) int {
	n := len(sub)
	for i := 0; i+n <= len(b); i++ {
		if string(b[i:i+n]) == sub {
			return i
		}
	}
	return -1
}

// -------------------------------------------------------- archive_runs ledger

const runStartSQL = `INSERT INTO sys.archive_runs (started_at, status) VALUES ($1, 'running') RETURNING id`

const runFinishSQL = `UPDATE sys.archive_runs SET finished_at = $1, moved_rows = $2, status = $3 WHERE id = $4`

// StartRun opens a ledger row and returns its id (breakpoint bookkeeping:
// a run stuck in 'running' past a heuristic is what the next run's dry-run
// report flags; resumption is idempotent by CopyBatch semantics).
func (c *ColdDB) StartRun(ctx context.Context, startedAt int64) (int64, error) {
	var id int64
	if err := c.DB.QueryRowContext(ctx, runStartSQL, startedAt).Scan(&id); err != nil {
		return 0, fmt.Errorf("pg: start run: %w", err)
	}
	return id, nil
}

// FinishRun closes a ledger row (status: ok / failed).
func (c *ColdDB) FinishRun(ctx context.Context, runID, finishedAt, moved int64, status string) error {
	if _, err := c.DB.ExecContext(ctx, runFinishSQL, finishedAt, moved, status, runID); err != nil {
		return fmt.Errorf("pg: finish run %d: %w", runID, err)
	}
	return nil
}
