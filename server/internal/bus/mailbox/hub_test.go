// hub_test.go —— P2 服务端单测（v3-bus-design §7.3）：
//
//	T-SV-1 并发 Offer 同 uid 1000 条 → mailbox_seq 无重复无空洞；
//	T-SV-6 compaction 只删过期 acked/unacked 边界，活跃条目不误删；
//	去重 LRU 命中不重复追加。
//
// 用真 SQLite 临时库（sqlite_test.go testDB 风格），但只依赖
// database/sql + mattn driver，不 import data 层；DDL 直接套用 P0 迁移 SQL。
package mailbox

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// migrationSQL 与 store/migrations/hot/mailbox/0001_mailbox_init.sql 同步
// （测试不 import migrate/embed FS，避免 bus→data 依赖）。
const migrationSQL = `
CREATE TABLE IF NOT EXISTS mailbox_entries (
    mailbox_seq  INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER NOT NULL,
    envelope     BLOB    NOT NULL,
    state        INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL,
    delivered_at INTEGER,
    dedup_key    TEXT,
    reserved1    INTEGER,
    reserved2    INTEGER
);
CREATE INDEX IF NOT EXISTS idx_mailbox_user_seq ON mailbox_entries (user_id, mailbox_seq);
CREATE INDEX IF NOT EXISTS idx_mailbox_state_delivered ON mailbox_entries (state, delivered_at);
CREATE TABLE IF NOT EXISTS mailbox_cursors (
    user_id    INTEGER PRIMARY KEY,
    cursor     INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL
);`

// testStore 打开临时 SQLite 库并套用 P0 迁移 SQL。
func testStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "mailbox.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, p := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.Exec(p); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	if _, err := db.Exec(migrationSQL); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewStore(db)
}

// testHub 打开启用 v3 的 hub。
func testHub(t *testing.T) (*MailboxHub, *Store) {
	t.Helper()
	st := testStore(t)
	h := NewHub(st, func() map[string]any {
		return map[string]any{"v3_bus_enabled": true}
	}, HubConfig{})
	return h, st
}

func env(kind int, msgID, seq int64) Envelope {
	return Envelope{Kind: kind, MsgID: msgID, Seq: seq, Blob: []byte{byte(kind)}}
}

// countRows 数某 uid 的条目（经原始 SQL，绕过被测方法做交叉验证）。
func countRows(t *testing.T, st *Store, uid int64) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM mailbox_entries WHERE user_id = ?`, uid).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestT_SV_1_ConcurrentOfferSeqMonotonic(t *testing.T) {
	h, st := testHub(t)
	ctx := context.Background()
	const uid = 42
	const goroutines, per = 20, 50 // 1000 条

	var mu sync.Mutex
	seqs := make([]int64, 0, goroutines*per)
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				// 每条唯一 (msg_id) 避免去重干扰本测试关注点
				seq, err := h.Offer(ctx, uid, env(102, int64(g*per+i+1), 0))
				if err != nil {
					errCh <- err
					return
				}
				mu.Lock()
				seqs = append(seqs, seq)
				mu.Unlock()
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	if len(seqs) != goroutines*per {
		t.Fatalf("offered %d, want %d", len(seqs), goroutines*per)
	}
	seen := make(map[int64]bool, len(seqs))
	minSeq, maxSeq := int64(1<<62), int64(0)
	for _, s := range seqs {
		if s <= 0 {
			t.Fatalf("seq %d is not positive", s)
		}
		if seen[s] {
			t.Fatalf("duplicate mailbox_seq %d", s)
		}
		seen[s] = true
		if s < minSeq {
			minSeq = s
		}
		if s > maxSeq {
			maxSeq = s
		}
	}
	// 无重复无空洞：唯一序列数 == max-min+1，且从 1 起
	if int64(len(seen)) != maxSeq-minSeq+1 {
		t.Fatalf("gap detected: %d distinct over [%d,%d]", len(seen), minSeq, maxSeq)
	}
	if minSeq != 1 {
		t.Fatalf("first seq = %d, want 1", minSeq)
	}
	if got := h.Head(uid); got != maxSeq {
		t.Fatalf("head = %d, want %d", got, maxSeq)
	}
	if got := countRows(t, st, uid); got != goroutines*per {
		t.Fatalf("rows = %d, want %d", got, goroutines*per)
	}
}

func TestT_SV_6_CompactionBoundaries(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	const uid = 7

	// 手工落库四种条目（created_at/delivered_at 显式控制，绕过 nowMillis）：
	//   a: acked 24h 前   → 删
	//   b: acked 1h 前    → 留（未过 acked 边界）
	//   c: unacked 8d 前  → 删（过 unacked 7d 边界）
	//   d: unacked 1h 前  → 留（活跃）
	//   e: inflight(state=1) 8d 前 → 删（unacked 边界含 inflight）
	now := time.Now().UnixMilli()
	day := int64(24 * time.Hour / time.Millisecond)
	hour := int64(time.Hour / time.Millisecond)
	insert := func(state int, created, delivered sql.NullInt64) {
		t.Helper()
		if _, err := st.db.Exec(
			`INSERT INTO mailbox_entries (user_id, envelope, state, created_at, delivered_at)
			 VALUES (?, ?, ?, ?, ?)`, uid, []byte{1}, state, created.Int64, delivered); err != nil {
			t.Fatal(err)
		}
	}
	ms := func(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }
	na := sql.NullInt64{}
	insert(StateAcked, ms(now-25*hour), sql.NullInt64{Int64: now - day - hour, Valid: true}) // a
	insert(StateAcked, ms(now-2*hour), sql.NullInt64{Int64: now - hour, Valid: true})        // b
	insert(StatePending, ms(now-8*day), na)                                                  // c
	insert(StatePending, ms(now-hour), na)                                                   // d
	insert(StateInflight, ms(now-8*day), na)                                                 // e

	ackedBefore := now - day     // acked 24h 边界
	unackedBefore := now - 7*day // unacked 7d 边界
	n, err := st.Compact(ctx, ackedBefore, unackedBefore)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if n != 3 {
		t.Fatalf("deleted %d rows, want 3 (a,c,e)", n)
	}
	// 活跃条目不误删：b 与 d 必须仍在
	entries, err := st.ListSince(ctx, uid, 0, 100)
	if err != nil {
		t.Fatalf("ListSince: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("survivors = %d, want 2 (b,d)", len(entries))
	}
	for _, e := range entries {
		if e.State != StateAcked && e.CreatedAt < unackedBefore {
			t.Fatalf("active entry wrongly deleted: state=%d created=%d", e.State, e.CreatedAt)
		}
	}
}

func TestDedupLRUNoDuplicateAppend(t *testing.T) {
	h, st := testHub(t)
	ctx := context.Background()
	const uid = 9

	// 同一 delivery_key（同 kind+msg_id）重投多次 → 只落一条
	for i := 0; i < 5; i++ {
		seq, err := h.Offer(ctx, uid, env(102, 1001, 1))
		if err != nil {
			t.Fatalf("offer %d: %v", i, err)
		}
		if i == 0 && seq == 0 {
			t.Fatal("first offer should allocate a seq")
		}
		if i > 0 && seq != 0 {
			t.Fatalf("retry %d allocated seq %d, want 0 (dedup)", i, seq)
		}
	}
	if got := countRows(t, st, uid); got != 1 {
		t.Fatalf("rows = %d, want 1", got)
	}

	// LRU 容量：小容量 hub 里灌满后旧键被淘汰，可再次追加（近似 LRU 语义）
	small := NewHub(st, func() map[string]any { return map[string]any{"v3_bus_enabled": true} },
		HubConfig{DedupCapacityPerUID: 4})
	for i := int64(0); i < 4; i++ {
		if _, err := small.Offer(ctx, uid, env(103, 2000+i, 0)); err != nil {
			t.Fatal(err)
		}
	}
	for i := int64(0); i < 4; i++ { // 灌入新批，旧批被淘汰
		if _, err := small.Offer(ctx, uid, env(103, 3000+i, 0)); err != nil {
			t.Fatal(err)
		}
	}
	seq, err := small.Offer(ctx, uid, env(103, 2000, 0)) // 旧键已被淘汰 → 允许追加
	if err != nil {
		t.Fatal(err)
	}
	if seq == 0 {
		t.Fatal("evicted key should be appendable again")
	}
}

func TestFlagDisabledOfferNoop(t *testing.T) {
	st := testStore(t)
	h := NewHub(st, func() map[string]any {
		return map[string]any{"v3_bus_enabled": false} // 灰度中间态（§6/§9）
	}, HubConfig{})
	seq, err := h.Offer(context.Background(), 1, env(102, 1, 1))
	if err != nil || seq != 0 {
		t.Fatalf("flag off offer = (%d,%v), want (0,nil)", seq, err)
	}
	if got := countRows(t, st, 1); got != 0 {
		t.Fatalf("flag off appended %d rows, want 0", got)
	}
	// nil flagsFn 同样视为全关
	h2 := NewHub(st, nil, HubConfig{})
	if seq, err := h2.Offer(context.Background(), 1, env(102, 1, 1)); err != nil || seq != 0 {
		t.Fatalf("nil flags offer = (%d,%v)", seq, err)
	}
}

func TestAckCursorAdvancesMonotonic(t *testing.T) {
	h, st := testHub(t)
	ctx := context.Background()
	const uid = 3

	for i := int64(1); i <= 5; i++ {
		if _, err := h.Offer(ctx, uid, env(102, 9000+i, 0)); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Ack(ctx, uid, 3); err != nil {
		t.Fatal(err)
	}
	if got := h.Cursor(uid); got != 3 {
		t.Fatalf("cursor = %d, want 3", got)
	}
	// 乱序/重复 ACK 幂等
	if err := h.Ack(ctx, uid, 2); err != nil {
		t.Fatal(err)
	}
	if err := h.Ack(ctx, uid, 3); err != nil {
		t.Fatal(err)
	}
	if got := h.Cursor(uid); got != 3 {
		t.Fatalf("cursor after stale ack = %d, want 3", got)
	}
	// ListSince 只给 cursor 之后的
	entries, err := st.ListSince(ctx, uid, 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].MailboxSeq != 4 {
		t.Fatalf("ListSince after cursor: %d entries", len(entries))
	}
	// Restore：重建 head/cursor 不回退
	fresh := NewHub(st, func() map[string]any { return map[string]any{"v3_bus_enabled": true} }, HubConfig{})
	if err := fresh.Restore(ctx, uid); err != nil {
		t.Fatal(err)
	}
	if got := fresh.Head(uid); got != 5 {
		t.Fatalf("restored head = %d, want 5", got)
	}
	if got := fresh.Cursor(uid); got != 3 {
		t.Fatalf("restored cursor = %d, want 3", got)
	}
}

func TestStats(t *testing.T) {
	h, _ := testHub(t)
	ctx := context.Background()
	for _, uid := range []int64{1, 2} {
		for i := int64(1); i <= 2; i++ {
			if _, err := h.Offer(ctx, uid, env(102, uid*100+i, 0)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := h.Ack(ctx, 1, 1); err != nil {
		t.Fatal(err)
	}
	st := h.Stats()
	// mailbox_seq 是全局自增（P0 迁移语义），故 heads 是各 uid 全局 head 之和，
	// 不是 per-uid 计数之和；用 Head() 聚合值锁定 Stats 的口径。
	wantHeads := h.Head(1) + h.Head(2)
	if st.Users != 2 || st.Heads != wantHeads || st.Cursors != 1 {
		t.Fatalf("stats = %+v, want users=2 heads=%d cursors=1", st, wantHeads)
	}
	if h.Head(2) <= h.Head(1) {
		t.Fatalf("global seq must be monotonic across uids: head1=%d head2=%d", h.Head(1), h.Head(2))
	}
}

// 静默守护：Append 的 dedup_key 落盘形态可解码回 uint64（T-SV-4 恢复去重的兜底前提）。
func TestDedupKeyRoundTrip(t *testing.T) {
	k := uint64(0xDEADBEEFCAFEBABE)
	st := testStore(t)
	if _, err := st.Append(context.Background(), 1, dedupKeyString(k), []byte{9}); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := st.db.QueryRow(`SELECT dedup_key FROM mailbox_entries WHERE user_id=1`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 8 {
		t.Fatalf("dedup_key len = %d, want 8", len(got))
	}
	var back uint64
	for i := 0; i < 8; i++ {
		back |= uint64(got[i]) << (8 * i)
	}
	if back != k {
		t.Fatalf("round trip = %x, want %x", back, k)
	}
}
