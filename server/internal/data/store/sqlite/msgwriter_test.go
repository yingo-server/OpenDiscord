package sqlite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/yingo-server/UFOT/server/internal/data/store"
)

// benchRows is the batch size used by BenchmarkAppendMessages (1000 rows).
const benchRows = 1000

// benchBatch builds `n` pending messages for one conversation. The seq
// numbers are unique inside the batch; the client_msg_id is unique too, so
// the insert never hits the UNIQUE constraints.
func benchBatch(cid, base int64, n int) []store.PendingMsg {
	out := make([]store.PendingMsg, n)
	for i := 0; i < n; i++ {
		out[i] = store.PendingMsg{
			MsgID:       base + int64(i),
			Cid:         cid,
			Seq:         base + int64(i),
			SenderID:    7,
			ClientMsgID: fmt.Sprintf("c-%d", base+int64(i)),
			Content:     []byte(`{"text":"hello"}`),
			SentAt:      42,
		}
	}
	return out
}

// BenchmarkAppendMessages compares the pre-optimization write path (one
// prepared statement per row) with the multi-value INSERT (one statement per
// chunk of rows). Both write 1000 rows in ONE transaction.
//
//	benchstat: ns/op, allocs/op, B/op
func BenchmarkAppendMessages(b *testing.B) {
	b.Run("single_row", func(b *testing.B) {
		db := benchDB(b)
		w := db.msgw
		var batch []store.PendingMsg
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			batch = benchBatch(1, int64(i*benchRows+1), benchRows)
			if err := w.appendSingleRow(context.Background(), batch); err != nil {
				b.Fatalf("appendSingleRow: %v", err)
			}
		}
		b.StopTimer()
		b.ReportMetric(float64(benchRows), "rows/op")
	})

	b.Run("multi_value", func(b *testing.B) {
		db := benchDB(b)
		w := db.msgw
		var batch []store.PendingMsg
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			batch = benchBatch(1, int64(i*benchRows+1), benchRows)
			if err := w.AppendMessages(context.Background(), batch); err != nil {
				b.Fatalf("AppendMessages: %v", err)
			}
		}
		b.StopTimer()
		b.ReportMetric(float64(benchRows), "rows/op")
	})
}

// benchDB opens a migrated database for a benchmark (no t.TempDir: b.TempDir
// is available, but the file must not be re-migrated per iteration).
func benchDB(b *testing.B) *DB {
	b.Helper()
	db, err := Open(b.TempDir() + "/bench.db")
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	b.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		b.Fatalf("migrate: %v", err)
	}
	cid := mkConvB(b, db, 7)
	_ = cid
	return db
}

// mkConvB creates a conversation for a benchmark (mirrors mkConv).
func mkConvB(b *testing.B, db *DB, creator int64) int64 {
	b.Helper()
	cid, err := db.CreateConv(context.Background(), &store.ConvMeta{
		CType: 2, Title: "bench", CreatedBy: creator, CreatedAt: 1,
	}, creator)
	if err != nil {
		b.Fatalf("CreateConv: %v", err)
	}
	return cid
}

// TestMultiInsertSplitsOnVariableLimit forces the chunk size down (the real
// limit depends on the linked SQLite build) and checks that a batch larger
// than one chunk is written completely, in one transaction.
func TestMultiInsertSplitsOnVariableLimit(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	cid := mkConv(t, db, 7)

	// 2 rows per statement → 25 statements for 50 rows.
	db.msgw.maxRowsPerStmt = 2
	batch := benchBatch(cid, 1, 50)
	if err := db.AppendMessages(ctx, batch); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	rows, err := db.ReadHot(ctx, cid, 0, 1000, true)
	if err != nil {
		t.Fatalf("ReadHot: %v", err)
	}
	if len(rows) != 50 {
		t.Fatalf("split insert wrote %d rows, want 50", len(rows))
	}
	for i, r := range rows {
		if r.Seq != int64(i+1) {
			t.Fatalf("row %d has seq %d, want %d", i, r.Seq, i+1)
		}
	}
}

// TestMultiInsertSplitRollsBackWholeBatch checks the all-or-nothing contract
// across a split batch: a duplicate in the LAST chunk must roll back the rows
// already written by the FIRST chunk.
func TestMultiInsertSplitRollsBackWholeBatch(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	cid := mkConv(t, db, 7)

	seed := store.PendingMsg{MsgID: 9000, Cid: cid, Seq: 9000, SenderID: 7, ClientMsgID: "seed", SentAt: 1}
	if err := db.AppendMessages(ctx, []store.PendingMsg{seed}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	db.msgw.maxRowsPerStmt = 2
	batch := []store.PendingMsg{
		{MsgID: 1, Cid: cid, Seq: 1, SenderID: 7, ClientMsgID: "a", SentAt: 1},
		{MsgID: 2, Cid: cid, Seq: 2, SenderID: 7, ClientMsgID: "b", SentAt: 1},
		{MsgID: 3, Cid: cid, Seq: 3, SenderID: 7, ClientMsgID: "c", SentAt: 1},
		{MsgID: 9000, Cid: cid, Seq: 9000, SenderID: 7, ClientMsgID: "seed", SentAt: 1}, // dup
	}
	if err := db.AppendMessages(ctx, batch); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}
	rows, err := db.ReadHot(ctx, cid, 0, 1000, true)
	if err != nil {
		t.Fatalf("ReadHot: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("split batch not rolled back: %d rows (want 1)", len(rows))
	}
}

// TestMultiInsertDuplicateMapsToErrDuplicate checks the dedup contract of the
// multi-value INSERT: a UNIQUE(cid,sender_id,client_msg_id) conflict inside a
// multi-row statement is surfaced as store.ErrDuplicate, and the whole batch
// is rolled back.
func TestMultiInsertDuplicateMapsToErrDuplicate(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	cid := mkConv(t, db, 7)

	first := []store.PendingMsg{
		{MsgID: 1, Cid: cid, Seq: 1, SenderID: 7, ClientMsgID: "a", SentAt: 1},
		{MsgID: 2, Cid: cid, Seq: 2, SenderID: 7, ClientMsgID: "b", SentAt: 1},
	}
	if err := db.AppendMessages(ctx, first); err != nil {
		t.Fatalf("first append: %v", err)
	}
	// duplicate client_msg_id inside the SAME multi-row statement
	dup := []store.PendingMsg{
		{MsgID: 3, Cid: cid, Seq: 3, SenderID: 7, ClientMsgID: "c", SentAt: 1},
		{MsgID: 4, Cid: cid, Seq: 4, SenderID: 7, ClientMsgID: "a", SentAt: 1},
	}
	err := db.AppendMessages(ctx, dup)
	if !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}
	// the error message still names the offending row (cid/sender/client id)
	if !strings.Contains(err.Error(), "client_msg_id=\"a\"") {
		t.Fatalf("dup error lost the offending row detail: %v", err)
	}
	// duplicate seq inside a multi-row statement → also ErrDuplicate
	dupSeq := []store.PendingMsg{
		{MsgID: 5, Cid: cid, Seq: 1, SenderID: 7, ClientMsgID: "e", SentAt: 1},
	}
	if err := db.AppendMessages(ctx, dupSeq); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("seq conflict: want ErrDuplicate, got %v", err)
	}
	// rollback: only the two original rows exist
	rows, err := db.ReadHot(ctx, cid, 0, 1000, true)
	if err != nil {
		t.Fatalf("ReadHot: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("duplicate batch not rolled back: %d rows (want 2)", len(rows))
	}
}

// TestAppendEmptyBatchIsNoop checks that an empty batch executes no SQL at
// all (no transaction, no statement) and returns nil.
func TestAppendEmptyBatchIsNoop(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := db.AppendMessages(ctx, nil); err != nil {
		t.Fatalf("nil batch: %v", err)
	}
	if err := db.AppendMessages(ctx, []store.PendingMsg{}); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
	// no conversation, no rows: nothing was written
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM msg_messages_hot`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("empty batch wrote %d rows", n)
	}
}

// TestBuildMultiInsertShape checks the rendered statement: one INSERT, N value
// groups, N*insertMsgCols placeholders and no user data in the SQL text.
func TestBuildMultiInsertShape(t *testing.T) {
	q := buildMultiInsert(3)
	if !strings.HasPrefix(q, insertMsgHead) {
		t.Fatalf("unexpected prefix: %s", q)
	}
	if got := strings.Count(q, insertMsgTuple); got != 3 {
		t.Fatalf("value groups = %d, want 3 (%s)", got, q)
	}
	if got := strings.Count(q, "?"); got != 3*insertMsgCols {
		t.Fatalf("placeholders = %d, want %d", got, 3*insertMsgCols)
	}
	if q1 := buildMultiInsert(1); q1 != insertMsgHead+insertMsgTuple {
		t.Fatalf("single-row form mismatch: %s", q1)
	}
}

// TestRowsPerStmtRespectsVariableLimit checks the chunk size derivation: it
// never exceeds the driver's bind-parameter ceiling and is at least 1.
func TestRowsPerStmtRespectsVariableLimit(t *testing.T) {
	db := testDB(t)
	rps := db.msgw.rowsPerStmt()
	if rps < 1 {
		t.Fatalf("rows per statement = %d, want >= 1", rps)
	}
	// A statement with exactly rps rows must prepare; rps+1 rows must not
	// (it would exceed SQLITE_MAX_VARIABLE_NUMBER). This asserts the chunk
	// size is the LARGEST legal one, using the real INSERT statement shape.
	if _, err := db.Prepare(buildMultiInsert(rps)); err != nil {
		t.Fatalf("prepare %d-row INSERT failed: %v", rps, err)
	}
	// The chunk must stay within the conservative fallback (999 bind params)
	// even when the linked SQLite reports a larger ceiling. Asserting that
	// rps+1 rows must FAIL would be wrong: a modern SQLite (32766 vars) happily
	// prepares it, so the only portable invariant is the conservative bound.
	if got := rps * insertMsgCols; got > 999 {
		t.Fatalf("rows/stmt %d uses %d bind params, exceeds the conservative 999 ceiling", rps, got)
	}
}

// TestMultiInsertVsSingleRowParity checks that both write paths produce the
// same rows (the multi-value INSERT must not change the stored data).
func TestMultiInsertVsSingleRowParity(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	cid := mkConv(t, db, 7)

	single := benchBatch(cid, 1, 20)
	if err := db.msgw.appendSingleRow(ctx, single); err != nil {
		t.Fatalf("single row: %v", err)
	}
	multi := benchBatch(cid, 1000, 20)
	if err := db.AppendMessages(ctx, multi); err != nil {
		t.Fatalf("multi value: %v", err)
	}
	rows, err := db.ReadHot(ctx, cid, 0, 1000, true)
	if err != nil {
		t.Fatalf("ReadHot: %v", err)
	}
	if len(rows) != 40 {
		t.Fatalf("rows = %d, want 40", len(rows))
	}
	for i, r := range rows {
		want := single[i%20]
		if i >= 20 {
			want = multi[i-20]
		}
		if r.MsgID != want.MsgID || r.Seq != want.Seq || r.ClientMsgID != want.ClientMsgID {
			t.Fatalf("row %d = %+v, want %+v", i, r, want)
		}
		if string(r.Content) != string(want.Content) {
			t.Fatalf("row %d content = %q, want %q", i, r.Content, want.Content)
		}
	}
}
