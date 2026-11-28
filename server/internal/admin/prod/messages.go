package prod

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/yingo-server/UFOT/server/internal/admin/service"
	"github.com/yingo-server/UFOT/server/internal/data/store/pg"
)

// Messages implements service.MessageRepository over the hot msg_messages_hot
// table.
//
// IMPORTANT — this is NOT full-text search:
//   - content is an opaque BLOB (Envelope payload, possibly E2EE ciphertext), so
//     the hot store can only filter by structural keys: cid + seq range and
//     client_msg_id prefix;
//   - the Text field of a hit is a best-effort plaintext projection of the
//     envelope (pg.ExtractText), truncated for the admin UI. It is NOT used for
//     matching.
//
// 全文搜索需冷库 pg_trgm（msg.messages_text + `text % $1`），接入后替换本实现：
// the pg.ColdDB.Search method already exists; wiring it here is a one-function
// change once the cold deployment is guaranteed.
type Messages struct{ db *sql.DB }

// NewMessages builds the adapter; nil db = not configured (503).
func NewMessages(db *sql.DB) *Messages { return &Messages{db: db} }

const messageColumnsSQL = `msg_id, cid, seq, sender_id, content, sent_at, recalled`

// messageWhere builds the predicate for the hot store. Pure function:
// the query string is parsed into a fixed grammar and every value is bound.
//
// Grammar (q):
//
//	""            → no text filter (conv listing)
//	seq:N         → exact sequence
//	seq:A-B       → inclusive seq range
//	seq:>=N | >N | <=N | <N
//	cmsg:PREFIX   → client_msg_id prefix
//	<digits>      → msg_id OR seq equality (operators paste either)
//	anything else → client_msg_id prefix (best the hot store can do)
func messageWhere(q string, cid int64) (string, []any, error) {
	var b strings.Builder
	args := make([]any, 0, 4)
	if cid > 0 {
		b.WriteString(" AND cid = ?")
		args = append(args, cid)
	}
	q = strings.TrimSpace(q)
	if q == "" {
		return b.String(), args, nil
	}

	if rest, ok := cutPrefixFold(q, "seq:"); ok {
		frag, seqArgs, err := seqPredicate(strings.TrimSpace(rest))
		if err != nil {
			return "", nil, err
		}
		b.WriteString(frag)
		args = append(args, seqArgs...)
		return b.String(), args, nil
	}
	if rest, ok := cutPrefixFold(q, "cmsg:"); ok {
		b.WriteString(` AND client_msg_id LIKE ? ESCAPE '\'`)
		args = append(args, escapeLike(rest)+"%")
		return b.String(), args, nil
	}
	if isDigits(q) {
		b.WriteString(" AND (msg_id = ? OR seq = ?)")
		args = append(args, q, q)
		return b.String(), args, nil
	}
	b.WriteString(` AND client_msg_id LIKE ? ESCAPE '\'`)
	args = append(args, escapeLike(q)+"%")
	return b.String(), args, nil
}

// seqPredicate parses the part after "seq:".
func seqPredicate(spec string) (string, []any, error) {
	invalid := func() (string, []any, error) {
		return "", nil, fmt.Errorf("%w: invalid seq filter %q (want N, A-B, >=N, >N, <=N, <N)", service.ErrInvalid, spec)
	}
	if spec == "" {
		return invalid()
	}
	for _, op := range []string{">=", "<=", ">", "<"} {
		if strings.HasPrefix(spec, op) {
			n, err := strconv.ParseInt(strings.TrimSpace(spec[len(op):]), 10, 64)
			if err != nil || n < 0 {
				return invalid()
			}
			return " AND seq " + op + " ?", []any{n}, nil
		}
	}
	if a, bb, ok := strings.Cut(spec, "-"); ok {
		from, err1 := strconv.ParseInt(strings.TrimSpace(a), 10, 64)
		to, err2 := strconv.ParseInt(strings.TrimSpace(bb), 10, 64)
		if err1 != nil || err2 != nil || from < 0 || to < from {
			return invalid()
		}
		return " AND seq >= ? AND seq <= ?", []any{from, to}, nil
	}
	n, err := strconv.ParseInt(spec, 10, 64)
	if err != nil || n < 0 {
		return invalid()
	}
	return " AND seq = ?", []any{n}, nil
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// SearchMessages returns one page of hot-store hits, newest seq first.
func (m *Messages) SearchMessages(ctx context.Context, q service.MessageQuery) (service.MessagePage, error) {
	if m == nil || m.db == nil {
		return service.MessagePage{}, service.ErrNotConfigured
	}
	where, args, err := messageWhere(q.Query, q.ConvID)
	if err != nil {
		return service.MessagePage{}, err
	}
	limit, offset := clampPage(q.Limit, 50, 200, q.Offset)

	var total int64
	if err := m.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM msg_messages_hot WHERE 1=1"+where, args...).Scan(&total); err != nil {
		return service.MessagePage{}, fmt.Errorf("admin/prod: count messages: %w", err)
	}
	rows, err := m.db.QueryContext(ctx,
		"SELECT "+messageColumnsSQL+" FROM msg_messages_hot WHERE 1=1"+where+
			" ORDER BY seq DESC LIMIT ? OFFSET ?",
		append(args, limit, offset)...)
	if err != nil {
		return service.MessagePage{}, fmt.Errorf("admin/prod: search messages: %w", err)
	}
	defer rows.Close()

	items := make([]service.MessageRecord, 0, limit)
	for rows.Next() {
		var rec service.MessageRecord
		var content []byte
		var recalled int64
		if err := rows.Scan(&rec.MsgID, &rec.Cid, &rec.Seq, &rec.SenderID,
			&content, &rec.SentAt, &recalled); err != nil {
			return service.MessagePage{}, fmt.Errorf("admin/prod: scan message: %w", err)
		}
		rec.Recalled = recalled != 0
		// Projection only — never matched on (see the type doc).
		rec.Text = excerpt(pg.ExtractText(content), 200)
		items = append(items, rec)
	}
	if err := rows.Err(); err != nil {
		return service.MessagePage{}, fmt.Errorf("admin/prod: message rows: %w", err)
	}
	return service.MessagePage{Items: items, Total: total, Limit: limit, Offset: offset}, nil
}

// excerpt truncates by runes (admin UI safety; audit events never carry it).
func excerpt(s string, max int) string {
	if max <= 0 || s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
