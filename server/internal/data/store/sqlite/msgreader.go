package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/yingo-server/UFOT/server/internal/data/store"
)

// msgReader implements store.MsgReader (hot read path: readrouter + archiver).

type msgReader struct{ db *sql.DB }

// orderSQL is the ORDER BY whitelist: keyset pagination's sort direction must
// never be assembled from caller input. Exactly two fixed strings exist.
var orderSQL = map[bool]string{
	false: "seq DESC", // typical paging: newest first, walking backwards
	true:  "seq ASC",  // catch-up sync: oldest first
}

const readHotSQL = `SELECT msg_id, cid, seq, sender_id, client_msg_id, content, sent_at, edited, recalled
FROM msg_messages_hot WHERE cid = ? AND seq `

// comparator whitelist mirrors orderSQL — paired fixed strings, no input.
var cmpSQL = map[bool]string{false: "<", true: ">"}

// ReadHot returns up to limit messages of cid ordered by seq (asc=false:
// descending from beforeSeq; asc=true: ascending after beforeSeq).
// beforeSeq <= 0 means "no cursor" — from the respective end (newest / oldest).
// Result order always follows the requested direction; callers who need
// ascending presentation of a descending page reverse it themselves.
func (r *msgReader) ReadHot(ctx context.Context, cid int64, beforeSeq int64, limit int, asc bool) ([]store.HotMsg, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("sqlite: ReadHot: limit must be >= 1 (cid=%d limit=%d)", cid, limit)
	}
	cmp := cmpSQL[asc]
	if beforeSeq <= 0 {
		// No cursor: drop the cursor predicate entirely (fixed strings).
		cmp = "" // sentinel handled below
	}
	var rows *sql.Rows
	var err error
	if cmp == "" {
		rows, err = r.db.QueryContext(ctx,
			`SELECT msg_id, cid, seq, sender_id, client_msg_id, content, sent_at, edited, recalled
FROM msg_messages_hot WHERE cid = ? ORDER BY `+orderSQL[asc]+` LIMIT ?`,
			cid, limit)
	} else {
		rows, err = r.db.QueryContext(ctx,
			readHotSQL+cmp+` ? ORDER BY `+orderSQL[asc]+` LIMIT ?`,
			cid, beforeSeq, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: read hot cid=%d: %w", cid, err)
	}
	defer rows.Close()
	var out []store.HotMsg
	for rows.Next() {
		var m store.HotMsg
		if err := rows.Scan(&m.MsgID, &m.Cid, &m.Seq, &m.SenderID, &m.ClientMsgID, &m.Content, &m.SentAt, &m.Edited, &m.Recalled); err != nil {
			return nil, fmt.Errorf("sqlite: scan hot msg: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// FirstSeqAfter returns the smallest seq of cid strictly greater than since,
// or 0 when none exists.
func (r *msgReader) FirstSeqAfter(ctx context.Context, cid int64, since int64) (int64, error) {
	var seq sql.NullInt64
	err := r.db.QueryRowContext(ctx,
		`SELECT MIN(seq) FROM msg_messages_hot WHERE cid = ? AND seq > ?`,
		cid, since).Scan(&seq)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("sqlite: first seq after cid=%d since=%d: %w", cid, since, err)
	}
	return seq.Int64, nil
}

// scanOlderSQL is the archive-scan whitelist: cutoff and limit are the only
// parameters (both fixed position), ORDER BY sent_at ASC rides idx_msg_sent.
const scanOlderSQL = `SELECT msg_id, cid, seq, sender_id, client_msg_id, content, sent_at, edited, recalled
FROM msg_messages_hot WHERE sent_at < ? ORDER BY sent_at ASC LIMIT ?`

// ScanOlder is the archiver's dedicated scan (mechanism, not policy): ALL hot
// rows older than the cutoff, oldest first, up to limit. MsgReader is per-cid
// (api.go frozen), so a store-wide scan is this separate method instead of a
// ReadHot(cid=0) convention sqlite never implemented. The cutoff VALUE is
// policy (assembly passes policy.hot_retention_days-derived); this query only
// defines WHAT a scan is.
func (r *msgReader) ScanOlder(ctx context.Context, cutoff int64, limit int) ([]store.HotMsg, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("sqlite: ScanOlder: limit must be >= 1 (cutoff=%d limit=%d)", cutoff, limit)
	}
	rows, err := r.db.QueryContext(ctx, scanOlderSQL, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: scan older cutoff=%d: %w", cutoff, err)
	}
	defer rows.Close()
	var out []store.HotMsg
	for rows.Next() {
		var m store.HotMsg
		if err := rows.Scan(&m.MsgID, &m.Cid, &m.Seq, &m.SenderID, &m.ClientMsgID, &m.Content, &m.SentAt, &m.Edited, &m.Recalled); err != nil {
			return nil, fmt.Errorf("sqlite: scan older row: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
