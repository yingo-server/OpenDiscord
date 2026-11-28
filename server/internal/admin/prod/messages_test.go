package prod

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/yingo-server/UFOT/server/internal/admin/service"
)

func seedMessage(t *testing.T, db *sql.DB, msgID, cid, seq, sender int64, clientID string, content string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO msg_messages_hot
(msg_id, cid, seq, sender_id, client_msg_id, content, sent_at, edited, recalled)
VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0)`,
		msgID, cid, seq, sender, clientID, []byte(content), 1000+seq)
}

func TestMessageWhereGrammar(t *testing.T) {
	cases := []struct {
		q     string
		cid   int64
		want  string
		args  int
		badRe bool
	}{
		{q: "", cid: 0, want: "", args: 0},
		{q: "", cid: 9, want: " AND cid = ?", args: 1},
		{q: "seq:42", cid: 0, want: " AND seq = ?", args: 1},
		{q: "seq:10-20", cid: 3, want: " AND cid = ? AND seq >= ? AND seq <= ?", args: 3},
		{q: "seq:>=7", cid: 0, want: " AND seq >= ?", args: 1},
		{q: "seq:<7", cid: 0, want: " AND seq < ?", args: 1},
		{q: "seq:<=7", cid: 0, want: " AND seq <= ?", args: 1},
		{q: "seq:>7", cid: 0, want: " AND seq > ?", args: 1},
		{q: "123456789", cid: 0, want: " AND (msg_id = ? OR seq = ?)", args: 2},
		{q: "cli-abc", cid: 0, want: ` AND client_msg_id LIKE ? ESCAPE '\'`, args: 1},
		{q: "cmsg:cli-abc", cid: 0, want: ` AND client_msg_id LIKE ? ESCAPE '\'`, args: 1},
		{q: "seq:20-10", cid: 0, badRe: true},
		{q: "seq:", cid: 0, badRe: true},
		{q: "seq:abc", cid: 0, badRe: true},
	}
	for _, c := range cases {
		got, args, err := messageWhere(c.q, c.cid)
		if c.badRe {
			if !errors.Is(err, service.ErrInvalid) {
				t.Fatalf("q=%q err = %v, want ErrInvalid", c.q, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("q=%q err = %v", c.q, err)
		}
		if got != c.want {
			t.Fatalf("q=%q where = %q, want %q", c.q, got, c.want)
		}
		if len(args) != c.args {
			t.Fatalf("q=%q args = %d, want %d (%#v)", c.q, len(args), c.args, args)
		}
	}
}

func TestMessageWhereEscapesPrefix(t *testing.T) {
	_, args, err := messageWhere("a%b", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 1 || args[0] != `a\%b%` {
		t.Fatalf("prefix args = %#v", args)
	}
}

func TestMessagesSearchHot(t *testing.T) {
	db := hotDB(t)
	seedMessage(t, db, 1001, 1, 1, 7, "cli-aaa", `{"text":"hello world"}`)
	seedMessage(t, db, 1002, 1, 2, 7, "cli-bbb", `{"text":"second message body"}`)
	seedMessage(t, db, 1003, 1, 3, 8, "cli-ccc", `{"text":"third"}`)
	seedMessage(t, db, 2001, 2, 1, 9, "other-1", `{"text":"other conv"}`)

	m := NewMessages(db)
	ctx := context.Background()

	// conv listing
	page, err := m.SearchMessages(ctx, service.MessageQuery{ConvID: 1, Limit: 10})
	if err != nil {
		t.Fatalf("conv listing: %v", err)
	}
	if page.Total != 3 || len(page.Items) != 3 {
		t.Fatalf("conv listing total=%d items=%d", page.Total, len(page.Items))
	}
	if page.Items[0].Seq != 3 {
		t.Fatalf("newest seq first, got %d", page.Items[0].Seq)
	}

	// seq range
	rng, err := m.SearchMessages(ctx, service.MessageQuery{ConvID: 1, Query: "seq:2-3", Limit: 10})
	if err != nil {
		t.Fatalf("seq range: %v", err)
	}
	if rng.Total != 2 || len(rng.Items) != 2 {
		t.Fatalf("seq range total=%d items=%d", rng.Total, len(rng.Items))
	}

	// client_msg_id prefix
	pref, err := m.SearchMessages(ctx, service.MessageQuery{Query: "cli-b", Limit: 10})
	if err != nil {
		t.Fatalf("prefix: %v", err)
	}
	if pref.Total != 1 || pref.Items[0].MsgID != 1002 {
		t.Fatalf("prefix = %+v", pref.Items)
	}

	// bare digits match msg_id or seq
	byID, err := m.SearchMessages(ctx, service.MessageQuery{Query: "1003", Limit: 10})
	if err != nil {
		t.Fatalf("msg id: %v", err)
	}
	if byID.Total != 1 || byID.Items[0].MsgID != 1003 {
		t.Fatalf("msg id search = %+v", byID.Items)
	}

	// text is a read-time projection, not a match key
	if page.Items[0].Text != "third" {
		t.Fatalf("text projection = %q", page.Items[0].Text)
	}

	// paging
	paged, err := m.SearchMessages(ctx, service.MessageQuery{ConvID: 1, Limit: 1, Offset: 1})
	if err != nil {
		t.Fatalf("paged: %v", err)
	}
	if paged.Total != 3 || len(paged.Items) != 1 || paged.Items[0].Seq != 2 {
		t.Fatalf("paged = %+v", paged)
	}
}

func TestExcerpt(t *testing.T) {
	if got := excerpt("hello", 10); got != "hello" {
		t.Fatalf("excerpt = %q", got)
	}
	long := ""
	for i := 0; i < 250; i++ {
		long += "x"
	}
	got := excerpt(long, 200)
	if len([]rune(got)) != 201 || got[len(got)-3:] != "…" {
		t.Fatalf("excerpt long len=%d", len([]rune(got)))
	}
	if excerpt("", 10) != "" || excerpt("abc", 0) != "" {
		t.Fatal("empty cases")
	}
}
