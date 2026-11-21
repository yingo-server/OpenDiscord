package appendlog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/yingo-server/OpenDiscord/server/internal/data/store"
)

// 编译期接口符合性断言：DB 必须完整满足 pipe 层依赖的两个消息存储接口。
// 缺任何方法都会在编译期暴露，而不是等到装配接线时才 panic。
var (
	_ store.MsgWriter = (*DB)(nil)
	_ store.MsgReader = (*DB)(nil)
)

func testDB(t *testing.T, cfg Config) (*DB, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "appendlog")
	cfg.Dir = dir
	db, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, dir
}

func pending(msgID, cid, seq, sender int64, cmid string, content string) store.PendingMsg {
	return store.PendingMsg{
		MsgID: msgID, Cid: cid, Seq: seq, SenderID: sender,
		ClientMsgID: cmid, Content: []byte(content), SentAt: msgID,
	}
}

// 1. append 1000, read back, seq monotonic, FirstSeqAfter.
func TestAppend1000AndReadBack(t *testing.T) {
	db, _ := testDB(t, Config{})
	ctx := context.Background()
	const total = 1000
	batch := make([]store.PendingMsg, total)
	for i := range batch {
		batch[i] = pending(int64(i+1), 42, int64(i+1), 7, fmt.Sprintf("c%d", i+1), fmt.Sprintf("body-%d", i+1))
	}
	if err := db.AppendMessages(ctx, batch); err != nil {
		t.Fatalf("AppendMessages 1000: %v", err)
	}
	rows, err := db.ReadHot(ctx, 42, 0, total, true)
	if err != nil {
		t.Fatalf("ReadHot asc: %v", err)
	}
	if len(rows) != total {
		t.Fatalf("want %d rows, got %d", total, len(rows))
	}
	for i, m := range rows {
		want := batch[i]
		if m.MsgID != want.MsgID || m.Seq != want.Seq || m.SenderID != want.SenderID ||
			m.ClientMsgID != want.ClientMsgID || !bytes.Equal(m.Content, want.Content) ||
			m.SentAt != want.SentAt || m.Edited != 0 || m.Recalled != 0 {
			t.Fatalf("row %d mismatch: %+v", i, m)
		}
	}
	desc, err := db.ReadHot(ctx, 42, 0, 10, false)
	if err != nil || len(desc) != 10 {
		t.Fatalf("ReadHot desc: len=%d err=%v", len(desc), err)
	}
	for i := range desc {
		if desc[i].Seq != int64(total-i) {
			t.Fatalf("desc row %d seq=%d", i, desc[i].Seq)
		}
	}
	page, err := db.ReadHot(ctx, 42, 900, 5, false)
	if err != nil || len(page) != 5 || page[0].Seq != 899 {
		t.Fatalf("desc cursor: rows=%+v err=%v", page, err)
	}
	if seq, err := db.FirstSeqAfter(ctx, 42, 997); err != nil || seq != 998 {
		t.Fatalf("FirstSeqAfter(997)=%d err=%v", seq, err)
	}
	if seq, err := db.FirstSeqAfter(ctx, 42, 1000); err != nil || seq != 0 {
		t.Fatalf("FirstSeqAfter(1000)=%d err=%v", seq, err)
	}
}

// 2. AllocateSeq is atomic, monotonic and survives crash recovery.
func TestAllocateSeqMonotonicAndDurable(t *testing.T) {
	db, dir := testDB(t, Config{})
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		start, err := db.AllocateSeq(ctx, 9, 3)
		if err != nil {
			t.Fatalf("AllocateSeq: %v", err)
		}
		if want := int64(i*3 + 1); start != want {
			t.Fatalf("alloc %d: got %d want %d", i, start, want)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db2, err := Open(Config{Dir: dir})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	start, err := db2.AllocateSeq(ctx, 9, 2)
	if err != nil || start != 16 {
		t.Fatalf("post-recovery alloc=%d err=%v", start, err)
	}
}

// 3. Crash recovery: truncate the last (incomplete) frame.
func TestCrashRecoveryTruncatesTornTail(t *testing.T) {
	db, dir := testDB(t, Config{Shards: 1})
	ctx := context.Background()
	batch := []store.PendingMsg{
		pending(1, 5, 1, 1, "a", "first"),
		pending(2, 5, 2, 1, "b", "second"),
	}
	if err := db.AppendMessages(ctx, batch); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	path := filepath.Join(dir, "msg-0000.log")
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.Stat()
	var last int64
	if err := foreachFrame(f, 0, st.Size(), func(off int64, _ []byte) error {
		last = off
		return nil
	}); err != nil || last < 0 {
		t.Fatalf("locate last frame: off=%d err=%v", last, err)
	}
	// Simulate a crash mid-write: keep the header, drop part of the payload.
	if err := f.Truncate(last + frameHeaderSize + 2); err != nil {
		t.Fatal(err)
	}
	f.Sync()
	f.Close()

	db2, err := Open(Config{Dir: dir, Shards: 1})
	if err != nil {
		t.Fatalf("reopen after torn tail: %v", err)
	}
	defer db2.Close()
	rows, err := db2.ReadHot(ctx, 5, 0, 10, true)
	if err != nil {
		t.Fatalf("read after recovery: %v", err)
	}
	if len(rows) != 1 || string(rows[0].Content) != "first" {
		t.Fatalf("want only first row, got %+v", rows)
	}
	// Fresh writes go after the truncated boundary and are readable.
	if err := db2.AppendMessages(ctx, []store.PendingMsg{pending(3, 5, 2, 1, "c", "third")}); err != nil {
		t.Fatalf("append after recovery: %v", err)
	}
	rows, _ = db2.ReadHot(ctx, 5, 0, 10, true)
	if len(rows) != 2 || string(rows[1].Content) != "third" {
		t.Fatalf("post-recovery rows: %+v", rows)
	}
}

// 4. Dedup idempotency: LRU hit and durable tail-scan hit after reopen.
func TestDedupIdempotent(t *testing.T) {
	db, dir := testDB(t, Config{Shards: 1})
	ctx := context.Background()
	m := pending(100, 1, 1, 22, "client-1", "hello")
	if err := db.AppendMessages(ctx, []store.PendingMsg{m}); err != nil {
		t.Fatalf("append: %v", err)
	}
	// Within LRU.
	dup := pending(999, 1, 99, 22, "client-1", "different")
	if err := db.AppendMessages(ctx, []store.PendingMsg{dup}); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("LRU dedup: want ErrDuplicate, got %v", err)
	}
	// Batch with an intra-batch duplicate aborts without writing anything.
	if err := db.AppendMessages(ctx, []store.PendingMsg{
		pending(200, 1, 2, 22, "x", "x"), pending(201, 1, 2, 22, "y", "y"),
	}); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("intra-batch seq conflict: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Drop the LRU by reopening with capacity 1: the tail scan is the only
	// remaining dedup layer, and it must replay (msg_id, seq) from the log.
	db2, err := Open(Config{Dir: dir, Shards: 1, DedupCapacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if err := db2.AppendMessages(ctx, []store.PendingMsg{pending(300, 2, 1, 33, "z", "z")}); err != nil {
		t.Fatalf("fill LRU: %v", err)
	}
	if err := db2.AppendMessages(ctx, []store.PendingMsg{pending(301, 1, 5, 22, "client-1", "again")}); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("durable tail dedup: want ErrDuplicate, got %v", err)
	}
}

// 5. Edit/recall overlays are append-only and visible in ReadHot.
func TestEditRecallOverlay(t *testing.T) {
	db, _ := testDB(t, Config{})
	ctx := context.Background()
	base := pending(10, 1, 1, 44, "e", "original")
	if err := db.AppendMessages(ctx, []store.PendingMsg{base}); err != nil {
		t.Fatal(err)
	}
	edited, err := db.MarkEdited(ctx, 1, 10, 44, []byte("edited body"), 1000)
	if err != nil || !bytes.Equal(edited.Content, []byte("edited body")) || edited.Edited != 1000 {
		t.Fatalf("MarkEdited: %+v err=%v", edited, err)
	}
	// Re-recall is idempotent (no error, current row returned).
	recalled, err := db.MarkRecalled(ctx, 1, 10, 44, 2000)
	if err != nil || recalled.Recalled != 2000 || !bytes.Equal(recalled.Content, []byte("edited body")) {
		t.Fatalf("MarkRecalled: %+v err=%v", recalled, err)
	}
	recalled2, err := db.MarkRecalled(ctx, 1, 10, 44, 3000)
	if err != nil || recalled2.Recalled != 2000 {
		t.Fatalf("re-recall: %+v err=%v", recalled2, err)
	}
	if _, err := db.MarkEdited(ctx, 1, 10, 44, []byte("late"), 4000); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("edit after recall: want ErrNotFound, got %v", err)
	}
	rows, _ := db.ReadHot(ctx, 1, 0, 10, true)
	if len(rows) != 1 || rows[0].Edited != 1000 || rows[0].Recalled != 2000 ||
		!bytes.Equal(rows[0].Content, []byte("edited body")) {
		t.Fatalf("overlaid read: %+v", rows)
	}
	// Wrong sender is not found (mechanical match like SQLite).
	if _, err := db.MarkEdited(ctx, 1, 10, 99, nil, 5); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("wrong sender edit: %v", err)
	}
}

// 6. Shard routing: messages of different cids spread and stay isolated.
func TestMultiShardRouting(t *testing.T) {
	n := 4
	db, dir := testDB(t, Config{Shards: n})
	ctx := context.Background()
	seen := map[int]int64{}
	batch := make([]store.PendingMsg, 0, 80)
	for i := 0; i < 80; i++ {
		cid := int64(i % 40)
		s := shardOf(cid, n)
		seen[s]++
		idx := i / 40 // 0 or 1: per-cid seq must be unique (SQLite UNIQUE(cid,seq)).
		batch = append(batch, pending(int64(1000+i), cid, int64(idx+1), 1, fmt.Sprintf("c%d", i), "payload"))
	}
	if len(seen) < 2 {
		t.Fatalf("expected multiple shards, got %v", seen)
	}
	if err := db.AppendMessages(ctx, batch); err != nil {
		t.Fatalf("multi-shard append: %v", err)
	}
	for cid := int64(0); cid < 40; cid++ {
		rows, err := db.ReadHot(ctx, cid, 0, 10, true)
		if err != nil || len(rows) != 2 {
			t.Fatalf("cid %d: rows=%d err=%v", cid, len(rows), err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db2, err := Open(Config{Dir: dir, Shards: n})
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	for cid := int64(0); cid < 40; cid++ {
		rows, _ := db2.ReadHot(ctx, cid, 0, 10, true)
		if len(rows) != 2 {
			t.Fatalf("cid %d after reopen: %d rows", cid, len(rows))
		}
	}
}

// 7. Recovery replays overlays too: edit/recall survive restart.
func TestRecoveryReplaysOverlays(t *testing.T) {
	db, dir := testDB(t, Config{Shards: 1})
	ctx := context.Background()
	if err := db.AppendMessages(ctx, []store.PendingMsg{pending(1, 1, 1, 1, "a", "v1")}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.MarkEdited(ctx, 1, 1, 1, []byte("v2"), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := db.MarkRecalled(ctx, 1, 1, 1, 200); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db2, err := Open(Config{Dir: dir, Shards: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	rows, _ := db2.ReadHot(ctx, 1, 0, 10, true)
	if len(rows) != 1 || string(rows[0].Content) != "v2" || rows[0].Edited != 100 || rows[0].Recalled != 200 {
		t.Fatalf("replayed overlay: %+v", rows)
	}
}

// 8. Sparse index is exercised with a non-default interval and concurrent
// writers sharing one shard (AllocateSeq + AppendMessages).
func TestSparseIndexAndConcurrentAlloc(t *testing.T) {
	db, _ := testDB(t, Config{Shards: 2, IndexEvery: 7})
	ctx := context.Background()
	const workers, each = 4, 25
	errCh := make(chan error, workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			cid := int64(7)
			for i := 0; i < each; i++ {
				start, err := db.AllocateSeq(ctx, cid, 1)
				if err != nil {
					errCh <- err
					return
				}
				msgID := int64(w*each+i+1) * 10
				if err := db.AppendMessages(ctx, []store.PendingMsg{
					pending(msgID, cid, start, int64(w+1), fmt.Sprintf("w%d-%d", w, i), "x"),
				}); err != nil {
					errCh <- err
					return
				}
			}
			errCh <- nil
		}(w)
	}
	for w := 0; w < workers; w++ {
		if err := <-errCh; err != nil {
			t.Fatalf("writer: %v", err)
		}
	}
	rows, err := db.ReadHot(ctx, 7, 0, 200, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != workers*each {
		t.Fatalf("want %d rows, got %d", workers*each, len(rows))
	}
	seqs := make([]int, len(rows))
	for i, m := range rows {
		seqs[i] = int(m.Seq)
	}
	if !sort.IntsAreSorted(seqs) || seqs[0] != 1 || seqs[len(seqs)-1] != workers*each {
		t.Fatalf("seq not contiguous/in-order: first=%d last=%d", seqs[0], seqs[len(seqs)-1])
	}
	if runtime.NumCPU() < 1 {
		t.Fatal("impossible")
	}
}

// 9. ScanOlder / DeleteHotBefore logical prune (archiver ports).
func TestScanOlderAndPrune(t *testing.T) {
	db, _ := testDB(t, Config{Shards: 1})
	ctx := context.Background()
	batch := []store.PendingMsg{
		pending(1, 1, 1, 1, "a", "old"),
		pending(2, 1, 2, 1, "b", "new"),
	}
	batch[0].SentAt, batch[1].SentAt = 10, 100
	if err := db.AppendMessages(ctx, batch); err != nil {
		t.Fatal(err)
	}
	rows, err := db.ScanOlder(ctx, 50, 10)
	if err != nil || len(rows) != 1 || rows[0].MsgID != 1 {
		t.Fatalf("ScanOlder: %+v err=%v", rows, err)
	}
	n, err := db.DeleteHotBefore(ctx, 50)
	if err != nil || n != 1 {
		t.Fatalf("prune n=%d err=%v", n, err)
	}
	rows, _ = db.ReadHot(ctx, 1, 0, 10, true)
	if len(rows) != 1 || rows[0].MsgID != 2 {
		t.Fatalf("after prune: %+v", rows)
	}
}

// 10. Invalid config is rejected and empty batches are no-ops.
func TestConfigAndEmptyBatch(t *testing.T) {
	ctx := context.Background()
	if _, err := Open(Config{}); err == nil {
		t.Fatal("empty Dir should fail")
	}
	db, _ := testDB(t, Config{Shards: 0})
	if err := db.AppendMessages(ctx, nil); err != nil {
		t.Fatalf("empty append: %v", err)
	}
	if _, err := db.AllocateSeq(ctx, 1, 0); err == nil {
		t.Fatal("n=0 should fail")
	}
	if _, err := db.ReadHot(ctx, 1, 0, 0, true); err == nil {
		t.Fatal("limit=0 should fail")
	}
}
