// v3_test.go —— P3b 装配层单测（docs/v3-bus-design.md §6/§7.3/§9）：
//
//	T-SV-5 灰度中间态：v3_bus_enabled=false 时 Hub.Offer 为 no-op、
//	       WireV3.Enabled()=false；热改 true/percent=100 后 Enabled()=true
//	       且 Offer 真落库；
//	T-SV-7 灰度 uid 稳定分桶：同 uid 反复判定恒一致，percent 单调扩大桶只增不减；
//	S-5    重启恢复：写若干条 → 新建 Hub（模拟重启）→ Restore → Cursor/Head 不回退。
//
// 用临时真 SQLite 库；DDL 内嵌同形于 store/migrations/hot/mailbox/0001
// （与 internal/bus/mailbox/hub_test.go 的做法一致：测试不 import migrate/embed FS）。
package opendiscord

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync/atomic"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/yingo-server/OpenDiscord/server/internal/bus/delivery"
	"github.com/yingo-server/OpenDiscord/server/internal/bus/mailbox"
	"github.com/yingo-server/OpenDiscord/server/internal/pkg/featureflags"
)

// testMigrationSQL 与 store/migrations/hot/mailbox/0001_mailbox_init.sql 同形。
const testMigrationSQL = `
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

// testDB 打开临时真 SQLite 库并套用 P0 迁移 DDL。
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "v3wire.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(testMigrationSQL); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// flagStore 模拟 911/SIGHUP 热重载：原子替换快照 map。
type flagStore struct{ v atomic.Value }

func newFlagStore(m map[string]any) *flagStore {
	s := &flagStore{}
	s.v.Store(m)
	return s
}
func (s *flagStore) snapshot() map[string]any { return s.v.Load().(map[string]any) }
func (s *flagStore) set(m map[string]any)     { s.v.Store(m) }

func newTestWire(t *testing.T, db *sql.DB, fs *flagStore) *WireV3 {
	t.Helper()
	return NewWireV3(db, fs.snapshot, delivery.Config{})
}

// 默认关闭快照：显式 off 构造（§F.2）。注释保留旧定调（§6/§9 主干默认），
// 1.4 转生产后 Defaults() 已默认开启，off 语义一律显式构造，不依赖 Defaults()。
func closedFlags() map[string]any {
	return map[string]any{
		featureflags.KeyV3BusEnabled: false,
		featureflags.KeyV3BusPercent: 0,
	}
}

// T-SV-5：flag 全关 → Offer no-op；热改 true/percent=100 → Enabled 且真落库。
func TestTSV5GrayToggle(t *testing.T) {
	db := testDB(t)
	fs := newFlagStore(closedFlags())
	w := newTestWire(t, db, fs)

	ctx := context.Background()
	// §F.2：off 基线来自显式 closedFlags() 构造，不依赖 Defaults()（1.4
	// 转生产后 Defaults() 已将 v3_bus_enabled 默认 true）。
	if w.Enabled() {
		t.Fatal("explicit closedFlags() store: Enabled() must be false (defaults are on, see TestProductionDefaults)")
	}
	seq, err := w.Hub.Offer(ctx, 42, mailbox.Envelope{Kind: 103, MsgID: 1, Blob: []byte("a")})
	if err != nil {
		t.Fatalf("offer(off): %v", err)
	}
	if seq != 0 {
		t.Fatalf("offer(off): expected no-op seq=0, got %d", seq)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM mailbox_entries`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("offer(off): expected 0 rows persisted, got %d", n)
	}

	// 热改：总开关打开 + 全量灰度。
	fs.set(map[string]any{
		featureflags.KeyV3BusEnabled: true,
		featureflags.KeyV3BusPercent: 100,
	})
	if !w.Enabled() {
		t.Fatal("after hot toggle: Enabled() must be true")
	}
	seq2, err := w.Hub.Offer(ctx, 42, mailbox.Envelope{Kind: 103, MsgID: 2, Blob: []byte("b")})
	if err != nil || seq2 == 0 {
		t.Fatalf("offer(on): seq=%d err=%v", seq2, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM mailbox_entries`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("offer(on): expected 1 row persisted, got %d", n)
	}
}

// T-SV-7（装配层视角）：同 uid 稳定分桶；percent 调大桶只扩不缩。
func TestGrayBucketStable(t *testing.T) {
	fs := newFlagStore(map[string]any{
		featureflags.KeyV3BusEnabled: true,
		featureflags.KeyV3BusPercent: 50,
	})
	w := NewWireV3(nil, fs.snapshot, delivery.Config{})

	for uid := int64(1); uid <= 500; uid++ {
		first := w.InBucket(uid)
		for i := 0; i < 5; i++ {
			if got := w.InBucket(uid); got != first {
				t.Fatalf("uid %d bucket unstable: %v vs %v", uid, first, got)
			}
		}
	}
	// percent 100：人人都在桶内；percent 0：无人。
	fs.set(map[string]any{featureflags.KeyV3BusEnabled: true, featureflags.KeyV3BusPercent: 100})
	for uid := int64(1); uid <= 200; uid++ {
		if !w.InBucket(uid) {
			t.Fatalf("percent=100: uid %d must be in bucket", uid)
		}
	}
	fs.set(map[string]any{featureflags.KeyV3BusEnabled: true, featureflags.KeyV3BusPercent: 0})
	if w.InBucket(1) {
		t.Fatal("percent=0: uid must not be in bucket")
	}
	fs.set(closedFlags())
	if w.InBucket(1) {
		t.Fatal("enabled=false: uid must not be in bucket regardless of percent")
	}
}

// S-5：崩溃恢复——重启后 Restore 使 Head/Cursor 不回退。
func TestS5RestoreAfterRestart(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	flags := map[string]any{
		featureflags.KeyV3BusEnabled: true,
		featureflags.KeyV3BusPercent: 100,
	}
	fs := newFlagStore(flags)
	const uid = int64(7)

	w1 := NewWireV3(db, fs.snapshot, delivery.Config{})
	var lastSeq int64
	for i := int64(1); i <= 5; i++ {
		seq, err := w1.Hub.Offer(ctx, uid, mailbox.Envelope{Kind: 103, MsgID: i, Blob: []byte("m")})
		if err != nil {
			t.Fatalf("offer %d: %v", i, err)
		}
		lastSeq = seq
	}
	if err := w1.Hub.Ack(ctx, uid, 3); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if got := w1.Hub.Cursor(uid); got != 3 {
		t.Fatalf("pre-restart cursor=%d, want 3", got)
	}

	// 模拟重启：同一 SQLite 文件、全新 Hub/Store 实例。
	w2 := NewWireV3(db, fs.snapshot, delivery.Config{})
	if err := w2.Hub.Restore(ctx, uid); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := w2.Hub.Head(uid); got != lastSeq {
		t.Fatalf("post-restart head=%d, want %d (no regression)", got, lastSeq)
	}
	if got := w2.Hub.Cursor(uid); got != 3 {
		t.Fatalf("post-restart cursor=%d, want 3 (no regression)", got)
	}
	// 恢复后仍可继续追加且 seq 续前单调。
	seq, err := w2.Hub.Offer(ctx, uid, mailbox.Envelope{Kind: 104, MsgID: 6, Blob: []byte("n")})
	if err != nil || seq <= lastSeq {
		t.Fatalf("post-restart offer: seq=%d lastSeq=%d err=%v", seq, lastSeq, err)
	}
}
