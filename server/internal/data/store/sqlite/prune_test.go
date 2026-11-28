package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/yingo-server/UFOT/server/internal/data/store"
)

// TestDeleteHotBeforePrunesOldRows: insert 3 messages (2 old, 1 new), prune
// the old ones, expect exactly the new one to remain.
func TestDeleteHotBeforePrunesOldRows(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	cid := mkConv(t, db, 7)

	old1 := store.PendingMsg{MsgID: 1001, Cid: cid, Seq: 1, SenderID: 7, ClientMsgID: "o1", Content: []byte("a"), SentAt: 100}
	old2 := store.PendingMsg{MsgID: 1002, Cid: cid, Seq: 2, SenderID: 7, ClientMsgID: "o2", Content: []byte("b"), SentAt: 200}
	fresh := store.PendingMsg{MsgID: 1003, Cid: cid, Seq: 3, SenderID: 7, ClientMsgID: "n1", Content: []byte("c"), SentAt: 999999}
	if err := db.AppendMessages(ctx, []store.PendingMsg{old1, old2, fresh}); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}

	n, err := db.DeleteHotBefore(ctx, 1000)
	if err != nil {
		t.Fatalf("DeleteHotBefore: %v", err)
	}
	if n != 2 {
		t.Fatalf("deleted = %d, want 2", n)
	}

	rows, err := db.ReadHot(ctx, cid, 0, 10, false)
	if err != nil {
		t.Fatalf("ReadHot: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("remaining rows = %d, want 1", len(rows))
	}
	if rows[0].MsgID != fresh.MsgID || rows[0].SentAt != fresh.SentAt {
		t.Fatalf("remaining row = %+v, want the NEW message (msg %d)", rows[0], fresh.MsgID)
	}

	// Idempotent: pruning again deletes nothing.
	n, err = db.DeleteHotBefore(ctx, 1000)
	if err != nil {
		t.Fatalf("DeleteHotBefore rerun: %v", err)
	}
	if n != 0 {
		t.Fatalf("rerun deleted = %d, want 0", n)
	}
}

// TestDeleteHotBeforeBoundary: sent_at == cutoff is NOT deleted (strict <).
func TestDeleteHotBeforeBoundary(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	cid := mkConv(t, db, 7)
	rows := []store.PendingMsg{
		{MsgID: 2001, Cid: cid, Seq: 1, SenderID: 7, ClientMsgID: "b1", Content: []byte("a"), SentAt: 500},
	}
	if err := db.AppendMessages(ctx, rows); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	if n, err := db.DeleteHotBefore(ctx, 500); err != nil {
		t.Fatalf("DeleteHotBefore: %v", err)
	} else if n != 0 {
		t.Fatalf("deleted = %d, want 0 (strict less-than boundary)", n)
	}
}

// TestTrimTableRateLimit: old sys_rate_limit buckets are deleted, new ones
// stay. The table name goes through the whitelist map.
func TestTrimTableRateLimit(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	old := time.Now().Add(-8 * 24 * time.Hour).Unix()
	fresh := time.Now().Unix()
	for i, at := range []int64{old, old, fresh} {
		subject := fmt.Sprintf("u:%d", i)
		if _, err := db.ExecContext(ctx,
			`INSERT INTO sys_rate_limit (subject, action, window_start, count) VALUES (?, 'media.upload', ?, 1)`,
			subject, at); err != nil {
			t.Fatalf("seed rate limit: %v", err)
		}
	}

	n, err := db.TrimTable(ctx, "sys_rate_limit", time.Now().Unix())
	if err != nil {
		t.Fatalf("TrimTable: %v", err)
	}
	if n != 2 {
		t.Fatalf("trimmed = %d, want 2 (old rows only)", n)
	}
	var remaining int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sys_rate_limit`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("remaining = %d, want 1 (the fresh bucket)", remaining)
	}
}

// TestTrimTableRefreshTokens: expired identity_refresh rows are deleted;
// unexpired ones stay.
func TestTrimTableRefreshTokens(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	now := time.Now().Unix()
	expired := now - 3600
	live := now + 24*3600
	for i, exp := range []int64{expired, live} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO identity_refresh (jti, family_id, uid, device_id, token_hash, issued_at, expires_at)
VALUES (?, 'fam', 1, 'dev', 'h', ?, ?)`,
			fmt.Sprintf("jti-%d", i), now, exp); err != nil {
			t.Fatalf("seed refresh: %v", err)
		}
	}

	n, err := db.TrimTable(ctx, "identity_refresh", now)
	if err != nil {
		t.Fatalf("TrimTable: %v", err)
	}
	if n != 1 {
		t.Fatalf("trimmed = %d, want 1 (expired token only)", n)
	}
}

// TestTrimTableMediaOnlyZeroRef: media_media rows are deleted only when BOTH
// created_at is old AND ref_count = 0.
func TestTrimTableMediaOnlyZeroRef(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	old := time.Now().Add(-30 * 24 * time.Hour).Unix()
	fresh := time.Now().Unix()
	rows := []struct {
		id string
		rc int64
		at int64
	}{
		{"gone", 0, old},         // old + unreferenced → deleted
		{"kept-ref", 2, old},     // old but referenced → kept
		{"kept-fresh", 0, fresh}, // fresh → kept
	}
	for _, r := range rows {
		if err := db.Insert(ctx, &store.Media{
			MediaID: r.id, UID: 1, MType: 4, Name: "n", Size: 10, Mime: "text/plain",
			Status: 1, SHA256: "a" + r.id, CreatedAt: r.at, RefCount: r.rc,
		}); err != nil {
			t.Fatalf("seed media %s: %v", r.id, err)
		}
	}

	n, err := db.TrimTable(ctx, "media_media", time.Now().Add(-7*24*time.Hour).Unix())
	if err != nil {
		t.Fatalf("TrimTable: %v", err)
	}
	if n != 1 {
		t.Fatalf("trimmed = %d, want 1 (old zero-ref row only)", n)
	}
	for _, id := range []string{"kept-ref", "kept-fresh"} {
		if _, err := db.Get(ctx, id); err != nil {
			t.Fatalf("media %s must survive: %v", id, err)
		}
	}
	if _, err := db.Get(ctx, "gone"); err == nil {
		t.Fatal("media 'gone' must have been trimmed")
	}
}

// TestTrimTableRejectsUnknownTable: a table outside the whitelist is an
// error, never SQL text.
func TestTrimTableRejectsUnknownTable(t *testing.T) {
	db := testDB(t)
	if _, err := db.TrimTable(context.Background(), "msg_messages_hot; DROP TABLE x", 1); err == nil {
		t.Fatal("unknown table must be rejected")
	}
}

// TestTrimTableMissingTableIsNoop: identity_login_history lives in the COLD
// store; trimming it against the hot DB must be a no-op, not an error (the
// trimmer tolerates a missing table so the rest of the pass keeps going).
func TestTrimTableMissingTableIsNoop(t *testing.T) {
	db := testDB(t)
	n, err := db.TrimTable(context.Background(), "identity_login_history", 1)
	if err != nil {
		t.Fatalf("missing table must be a no-op, got %v", err)
	}
	if n != 0 {
		t.Fatalf("no-op trimmed %d rows on a nonexistent table", n)
	}
}

// TestPragmasApplied: the space-management invariants are set at open time.
func TestPragmasApplied(t *testing.T) {
	db := testDB(t)
	row := db.QueryRow(`PRAGMA auto_vacuum`)
	var av int
	if err := row.Scan(&av); err != nil {
		t.Fatal(err)
	}
	if av != 2 { // 2 = INCREMENTAL
		t.Fatalf("auto_vacuum = %d, want 2 (INCREMENTAL)", av)
	}
	row = db.QueryRow(`PRAGMA journal_size_limit`)
	var jsl int64
	if err := row.Scan(&jsl); err != nil {
		t.Fatal(err)
	}
	if jsl != 64<<20 {
		t.Fatalf("journal_size_limit = %d, want %d", jsl, 64<<20)
	}
	row = db.QueryRow(`PRAGMA cache_size`)
	var cs int64
	if err := row.Scan(&cs); err != nil {
		t.Fatal(err)
	}
	if cs != -8000 {
		t.Fatalf("cache_size = %d, want -8000", cs)
	}
}

// TestExistingDatabaseKeepsSchema: opening an EXISTING (migrated) database
// again does not corrupt or reset it — the auto_vacuum probe finds user
// tables and leaves the file alone.
func TestExistingDatabaseKeepsSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hot.db")

	db1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := db1.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db1.Insert(ctx, &store.Media{
		MediaID: "m", UID: 1, MType: 4, Name: "n", Size: 1, Mime: "text/plain",
		Status: 1, SHA256: "aa", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	db1.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	if err := db2.Migrate(ctx); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	if _, err := db2.Get(ctx, "m"); err != nil {
		t.Fatalf("existing row lost after reopen: %v", err)
	}
}

// TestMediaQuotaUsage: UsageOf / TotalUsage account SUM(size) per user and
// overall.
func TestMediaQuotaUsage(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	seed := []struct {
		id   string
		uid  int64
		size int64
	}{
		{"a", 1, 100},
		{"b", 1, 50},
		{"c", 2, 10},
	}
	for _, s := range seed {
		if err := db.Insert(ctx, &store.Media{
			MediaID: s.id, UID: s.uid, MType: 4, Name: "n", Size: s.size, Mime: "text/plain",
			Status: 1, SHA256: "x" + s.id, CreatedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := db.UsageOf(ctx, 1); err != nil || got != 150 {
		t.Fatalf("UsageOf(1) = %d err=%v, want 150", got, err)
	}
	if got, err := db.UsageOf(ctx, 2); err != nil || got != 10 {
		t.Fatalf("UsageOf(2) = %d err=%v, want 10", got, err)
	}
	if got, err := db.UsageOf(ctx, 3); err != nil || got != 0 {
		t.Fatalf("UsageOf(3) = %d err=%v, want 0", got, err)
	}
	if got, err := db.TotalUsage(ctx); err != nil || got != 160 {
		t.Fatalf("TotalUsage = %d err=%v, want 160", got, err)
	}
}
