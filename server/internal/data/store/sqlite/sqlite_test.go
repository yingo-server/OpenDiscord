package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/yingo-server/UFOT/server/internal/data/store"
)

// testDB opens a fresh migrated database in t.TempDir().
func testDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "hot.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func mkConv(t *testing.T, db *DB, creator int64) int64 {
	t.Helper()
	cid, err := db.CreateConv(context.Background(), &store.ConvMeta{
		CType: 2, Title: "t", CreatedBy: creator, CreatedAt: 1,
	}, creator)
	if err != nil {
		t.Fatalf("CreateConv: %v", err)
	}
	return cid
}

func TestAppendAndReadHot(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	cid := mkConv(t, db, 7)

	start, err := db.AllocateSeq(ctx, cid, 5)
	if err != nil {
		t.Fatalf("AllocateSeq: %v", err)
	}
	if start != 1 {
		t.Fatalf("first alloc start = %d, want 1", start)
	}
	var batch []store.PendingMsg
	for i := 0; i < 5; i++ {
		batch = append(batch, store.PendingMsg{
			MsgID: int64(1000 + i), Cid: cid, Seq: start + int64(i),
			SenderID: 7, ClientMsgID: fmt.Sprintf("c-%d", i), Content: []byte("hi"), SentAt: 42,
		})
	}
	if err := db.AppendMessages(ctx, batch); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}

	// desc paging from newest
	got, err := db.ReadHot(ctx, cid, 0, 3, false)
	if err != nil || len(got) != 3 {
		t.Fatalf("ReadHot desc: %v %d", err, len(got))
	}
	if got[0].Seq != 5 || got[2].Seq != 3 {
		t.Fatalf("desc order wrong: %d,%d,%d", got[0].Seq, got[1].Seq, got[2].Seq)
	}
	// keyset: next page before seq=3
	got, err = db.ReadHot(ctx, cid, 3, 10, false)
	if err != nil || len(got) != 2 {
		t.Fatalf("ReadHot keyset: %v %d", err, len(got))
	}
	if got[0].Seq != 2 || got[1].Seq != 1 {
		t.Fatalf("keyset order wrong")
	}
	// asc after 2
	got, err = db.ReadHot(ctx, cid, 2, 10, true)
	if err != nil || len(got) != 3 || got[0].Seq != 3 {
		t.Fatalf("ReadHot asc: %v len=%d", err, len(got))
	}
	// FirstSeqAfter
	fs, err := db.FirstSeqAfter(ctx, cid, 3)
	if err != nil || fs != 4 {
		t.Fatalf("FirstSeqAfter: %v %d", err, fs)
	}
	fs, _ = db.FirstSeqAfter(ctx, cid, 5)
	if fs != 0 {
		t.Fatalf("FirstSeqAfter past end = %d, want 0", fs)
	}
}

func TestAppendDuplicate(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	cid := mkConv(t, db, 7)
	m := store.PendingMsg{MsgID: 1, Cid: cid, Seq: 1, SenderID: 7, ClientMsgID: "x", Content: nil, SentAt: 1}
	if err := db.AppendMessages(ctx, []store.PendingMsg{m}); err != nil {
		t.Fatalf("first append: %v", err)
	}
	m2 := m
	m2.MsgID = 2
	err := db.AppendMessages(ctx, []store.PendingMsg{m2})
	if !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}
	// batch atomicity: one dup inside a batch rolls back the whole batch
	m3 := store.PendingMsg{MsgID: 3, Cid: cid, Seq: 2, SenderID: 7, ClientMsgID: "y", SentAt: 1}
	m4 := store.PendingMsg{MsgID: 4, Cid: cid, Seq: 1, SenderID: 7, ClientMsgID: "x", SentAt: 1} // dup
	if err := db.AppendMessages(ctx, []store.PendingMsg{m3, m4}); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("batch dup: want ErrDuplicate, got %v", err)
	}
	n, _ := db.ReadHot(ctx, cid, 0, 100, false)
	if len(n) != 1 {
		t.Fatalf("batch rollback failed: %d rows", len(n))
	}
}

func TestAllocateSeqConcurrent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	cid := mkConv(t, db, 1)

	const goroutines, per = 100, 10
	var mu sync.Mutex
	seen := map[int64]bool{}
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				start, err := db.AllocateSeq(ctx, cid, 1)
				if err != nil {
					errCh <- err
					return
				}
				mu.Lock()
				if seen[start] {
					mu.Unlock()
					errCh <- fmt.Errorf("duplicate seq %d allocated", start)
					return
				}
				seen[start] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if len(seen) != goroutines*per {
		t.Fatalf("allocated %d distinct seqs, want %d", len(seen), goroutines*per)
	}
	// range allocation
	s, err := db.AllocateSeq(ctx, cid, 100)
	if err != nil || s != goroutines*per+1 {
		t.Fatalf("range alloc: %v %d", err, s)
	}
}

func TestIdentityStore(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	u := &store.User{UID: 5, DisplayName: "a", CreatedAt: 1}
	if err := db.UpsertUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	u.DisplayName = "b"
	u.AnonymousAt = 99
	if err := db.UpsertUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetUser(ctx, 5)
	if err != nil || got.DisplayName != "b" || got.AnonymousAt != 99 {
		t.Fatalf("GetUser: %v %+v", err, got)
	}
	if _, err := db.GetUser(ctx, 6); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetUser missing: %v", err)
	}

	// identity: insert via raw SQL (no api for registering identities yet —
	// the hot schema owns identity_identities; lookup is what matters here)
	if _, err := db.Exec(`INSERT INTO identity_identities (id_type, id_value, uid, verified, created_at)
VALUES (?, ?, ?, 1, 1)`, 1, "hmac-abc", 5); err != nil {
		t.Fatal(err)
	}
	uid, err := db.LookupByIdentity(ctx, 1, "hmac-abc")
	if err != nil || uid != 5 {
		t.Fatalf("LookupByIdentity: %v %d", err, uid)
	}
	if _, err := db.LookupByIdentity(ctx, 1, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lookup missing: %v", err)
	}

	// bans
	if b, _ := db.GetBan(ctx, 5); b != nil {
		t.Fatalf("no ban expected, got %+v", b)
	}
	if err := db.SetBan(ctx, &store.Ban{UID: 5, Level: 1, Reason: "r", Until: nowUnix() + 1000, CreatedBy: "op", CreatedAt: nowUnix()}); err != nil {
		t.Fatal(err)
	}
	b, err := db.GetBan(ctx, 5)
	if err != nil || b == nil || b.Level != 1 {
		t.Fatalf("GetBan: %v %+v", err, b)
	}
	// expired ban treated as absent
	if err := db.SetBan(ctx, &store.Ban{UID: 5, Level: 1, Until: nowUnix() - 1, CreatedBy: "op", CreatedAt: nowUnix()}); err != nil {
		t.Fatal(err)
	}
	if b, _ := db.GetBan(ctx, 5); b != nil {
		t.Fatalf("expired ban should be absent")
	}
}

func TestConvStore(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	cid, err := db.CreateConv(ctx, &store.ConvMeta{CType: 1, Title: "d", CreatedBy: 3, CreatedAt: 10}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := db.CheckMember(ctx, cid, 3); err != nil || m == nil || m.Role != 2 {
		t.Fatalf("creator not owner: %v %+v", err, m)
	}
	// add member idempotent
	if err := db.AddMember(ctx, &store.Member{Cid: cid, UID: 4, Role: 0, JoinedAt: 11}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddMember(ctx, &store.Member{Cid: cid, UID: 4, Role: 0, JoinedAt: 12}); err != nil {
		t.Fatalf("idempotent AddMember: %v", err)
	}
	ms, _ := db.ListMembers(ctx, cid)
	if len(ms) != 2 {
		t.Fatalf("members = %d", len(ms))
	}
	if _, err := db.CheckMember(ctx, cid, 99); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("CheckMember missing: %v", err)
	}
	if err := db.SetMemberRole(ctx, cid, 4, 1); err != nil {
		t.Fatal(err)
	}
	if m, _ := db.CheckMember(ctx, cid, 4); m.Role != 1 {
		t.Fatalf("role not set: %+v", m)
	}
	// SingleChatBetween: exact pair only, order-independent.
	if got, err := db.SingleChatBetween(ctx, 3, 4); err != nil || got != cid {
		t.Fatalf("SingleChatBetween(3,4) = %d, %v; want %d", got, err, cid)
	}
	if got, err := db.SingleChatBetween(ctx, 4, 3); err != nil || got != cid {
		t.Fatalf("SingleChatBetween(4,3) = %d, %v; want %d", got, err, cid)
	}
	if _, err := db.SingleChatBetween(ctx, 3, 99); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("SingleChatBetween(3,99) = %v, want ErrNotFound", err)
	}
	// NextSeq peek without reservation
	if n, err := db.NextSeq(ctx, cid); err != nil || n != 1 {
		t.Fatalf("NextSeq: %v %d", err, n)
	}
	// remove idempotent
	if err := db.RemoveMember(ctx, cid, 4); err != nil {
		t.Fatal(err)
	}
	if err := db.RemoveMember(ctx, cid, 4); err != nil {
		t.Fatalf("idempotent RemoveMember: %v", err)
	}
	meta, err := db.GetConvMeta(ctx, cid)
	if err != nil || meta.Title != "d" {
		t.Fatalf("GetConvMeta: %v %+v", err, meta)
	}
	if _, err := db.GetConvMeta(ctx, 99999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetConvMeta missing: %v", err)
	}
}

func TestMediaStore(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	m := &store.Media{MediaID: "AAAAAAAAAAAAAAAAAAAAAA", UID: 1, MType: 0, Size: 10, Mime: "image/png", Status: 0, CreatedAt: 1}
	if err := db.Insert(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := db.Insert(ctx, m); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("dup insert: %v", err)
	}
	got, err := db.Get(ctx, m.MediaID)
	if err != nil || got.Status != 0 {
		t.Fatalf("Get: %v %+v", err, got)
	}
	if _, err := db.Get(ctx, "zzz"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get missing: %v", err)
	}
	if err := db.UpdateStatus(ctx, m.MediaID, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateStatus(ctx, "zzz", 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UpdateStatus missing: %v", err)
	}
	if err := db.AddRef(ctx, m.MediaID); err != nil {
		t.Fatal(err)
	}
	if err := db.IncRefCount(ctx, m.MediaID, -2); err != nil {
		t.Fatal(err)
	}
	got, _ = db.Get(ctx, m.MediaID)
	if got.RefCount != -1 || got.Status != 1 {
		t.Fatalf("refcount/status wrong: %+v", got)
	}
	if err := db.AddRef(ctx, "zzz"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("AddRef missing: %v", err)
	}
}

func TestSysStore(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// flags with version bump
	if err := db.UpsertFlag(ctx, &store.Flag{Key: "a", Value: `{"x":1}`}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFlag(ctx, &store.Flag{Key: "a", Value: `{"x":2}`}); err != nil {
		t.Fatal(err)
	}
	fs, _ := db.AllFlags(ctx)
	if len(fs) != 1 || fs[0].Version != 2 || fs[0].Value != `{"x":2}` {
		t.Fatalf("flags: %+v", fs)
	}

	// presence upsert + never-seen
	if err := db.UpsertPresence(ctx, &store.Presence{UID: 1, Online: true, LastSeen: 5}); err != nil {
		t.Fatal(err)
	}
	if p, _ := db.GetPresence(ctx, 1); !p.Online || p.LastSeen != 5 {
		t.Fatalf("presence: %+v", p)
	}
	if p, _ := db.GetPresence(ctx, 2); p == nil || p.Online {
		t.Fatalf("never-seen presence: %+v", p)
	}

	// rate limit increments atomically
	for i := int64(1); i <= 5; i++ {
		w, err := db.RateLimitIncr(ctx, "u:1", "msg", 1000)
		if err != nil || w.Count != i {
			t.Fatalf("RateLimitIncr: %v count=%d want=%d", err, w.Count, i)
		}
	}
	var cnt int64
	db.QueryRow(`SELECT count FROM sys_rate_limit WHERE subject='u:1' AND action='msg' AND window_start=1000`).Scan(&cnt)
	if cnt != 5 {
		t.Fatalf("stored count = %d", cnt)
	}
	// separate window starts at 1
	w, _ := db.RateLimitIncr(ctx, "u:1", "msg", 2000)
	if w.Count != 1 {
		t.Fatalf("new window count = %d", w.Count)
	}

	// penalties
	if p, _ := db.GetPenalty(ctx, "u:1", "msg"); p != nil {
		t.Fatalf("unexpected penalty")
	}
	if err := db.SetPenalty(ctx, &store.Penalty{Subject: "u:1", Penalty: "msg", Until: nowUnix() + 100}); err != nil {
		t.Fatal(err)
	}
	if p, _ := db.GetPenalty(ctx, "u:1", "msg"); p == nil || p.Until <= nowUnix() {
		t.Fatalf("penalty: %+v", p)
	}
	if err := db.SetPenalty(ctx, &store.Penalty{Subject: "u:1", Penalty: "msg", Until: nowUnix() - 1}); err != nil {
		t.Fatal(err)
	}
	if p, _ := db.GetPenalty(ctx, "u:1", "msg"); p != nil {
		t.Fatalf("expired penalty should be absent: %+v", p)
	}
}

func TestMigrateIdempotent(t *testing.T) {
	db := testDB(t)
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM _migrations`).Scan(&n)
	if n == 0 {
		t.Fatalf("no migrations recorded")
	}
}

// compile-time: DB exposes the embedded *sql.DB for generic queries in tests.
var _ = func(d *DB) *sql.DB { return d.DB }
