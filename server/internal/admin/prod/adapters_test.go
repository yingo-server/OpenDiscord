package prod

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yingo-server/UFOT/server/internal/admin/service"
	"github.com/yingo-server/UFOT/server/internal/data/policy"
	"github.com/yingo-server/UFOT/server/internal/data/store"
)

// writePolicy writes a minimal valid policy.json and returns its path.
func writePolicy(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	return p
}

func TestArchiveWithoutCold(t *testing.T) {
	db := hotDB(t)
	a := NewArchive(db, nil, nil, fixedNow)

	st, err := a.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.State != "not_configured" || st.Running {
		t.Fatalf("status = %+v", st)
	}

	if _, err := a.BeginRun(context.Background()); !errors.Is(err, service.ErrNotConfigured) {
		t.Fatalf("BeginRun err = %v, want ErrNotConfigured", err)
	}
	if err := a.FinishRun(context.Background(), 1, 0, "ok"); !errors.Is(err, service.ErrNotConfigured) {
		t.Fatalf("FinishRun err = %v, want ErrNotConfigured", err)
	}
}

func TestArchiveEstimateFromHot(t *testing.T) {
	db := hotDB(t)
	now := time.Unix(1_800_000_000, 0)
	// Default retention is 90 days; anything older is archivable.
	old := now.Add(-100 * 24 * time.Hour).Unix()
	fresh := now.Add(-1 * 24 * time.Hour).Unix()

	mustExec(t, db, `INSERT INTO msg_messages_hot (msg_id, cid, seq, sender_id, client_msg_id, content, sent_at, edited, recalled)
VALUES (1, 1, 1, 7, 'a', X'00', ?, 0, 0)`, old)
	mustExec(t, db, `INSERT INTO msg_messages_hot (msg_id, cid, seq, sender_id, client_msg_id, content, sent_at, edited, recalled)
VALUES (2, 1, 2, 7, 'b', X'00', ?, 0, 0)`, old)
	mustExec(t, db, `INSERT INTO msg_messages_hot (msg_id, cid, seq, sender_id, client_msg_id, content, sent_at, edited, recalled)
VALUES (3, 2, 1, 7, 'c', X'00', ?, 0, 0)`, old)
	mustExec(t, db, `INSERT INTO msg_messages_hot (msg_id, cid, seq, sender_id, client_msg_id, content, sent_at, edited, recalled)
VALUES (4, 2, 2, 7, 'd', X'00', ?, 0, 0)`, fresh)

	a := NewArchive(db, nil, nil, func() time.Time { return now })
	est, err := a.Estimate(context.Background())
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if est.Rows != 3 || est.Convs != 2 {
		t.Fatalf("estimate = %+v, want rows=3 convs=2", est)
	}
	if est.Minutes != 1 { // 3 rows / 1000 per minute, rounded up
		t.Fatalf("minutes = %d", est.Minutes)
	}

	// A longer retention (200 days) makes the 100-day-old rows too fresh to
	// archive: the cutoff is now-200d, so nothing qualifies.
	pol, err := policy.New(writePolicy(t, `{"hot_retention_days": 200}`))
	if err != nil {
		t.Fatalf("policy store: %v", err)
	}
	a = NewArchive(db, nil, pol, func() time.Time { return now })
	est, err = a.Estimate(context.Background())
	if err != nil {
		t.Fatalf("estimate 2: %v", err)
	}
	if est.Rows != 0 {
		t.Fatalf("rows with 200-day retention = %d, want 0", est.Rows)
	}

	// The archive cron from policy drives NextRunAt (status without cold keeps
	// State=not_configured but still reports the schedule).
	pol2, err := policy.New(writePolicy(t, `{"archive": {"cron": "0 4 * * *"}}`))
	if err != nil {
		t.Fatalf("policy store 2: %v", err)
	}
	a = NewArchive(db, nil, pol2, func() time.Time { return now })
	st, err := a.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.State != "not_configured" || st.NextRunAt == 0 {
		t.Fatalf("status with cron = %+v", st)
	}
}

func TestArchiveNilHotIsNotConfigured(t *testing.T) {
	a := NewArchive(nil, nil, nil, fixedNow)
	if _, err := a.Estimate(context.Background()); !errors.Is(err, service.ErrNotConfigured) {
		t.Fatalf("err = %v", err)
	}
}

func TestParseDailyCron(t *testing.T) {
	cases := []struct {
		cron      string
		min, hour int
		ok        bool
	}{
		{"0 4 * * *", 0, 4, true},
		{"30 23 * * *", 30, 23, true},
		{"@daily 04:30", 30, 4, true},
		{"@daily 4:5", 5, 4, true},
		{"*/5 4 * * *", 0, 0, false},
		{"0 4 * * 1", 0, 0, false},
		{"@daily 25:00", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		min, hour, ok := parseDailyCron(c.cron)
		if ok != c.ok || (ok && (min != c.min || hour != c.hour)) {
			t.Fatalf("parseDailyCron(%q) = %d,%d,%v want %d,%d,%v",
				c.cron, min, hour, ok, c.min, c.hour, c.ok)
		}
	}
}

func TestNextDailyRun(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, loc)
	next, ok := nextDailyRun("0 4 * * *", now)
	if !ok {
		t.Fatal("cron not parsed")
	}
	want := time.Date(2026, 10, 30, 4, 0, 0, 0, loc).Unix()
	if next != want {
		t.Fatalf("next = %v, want %v", time.Unix(next, 0).UTC(), time.Unix(want, 0).UTC())
	}
	// Earlier same day: today's occurrence.
	early := time.Date(2026, 10, 29, 1, 0, 0, 0, loc)
	next, _ = nextDailyRun("0 4 * * *", early)
	if next != time.Date(2026, 10, 29, 4, 0, 0, 0, loc).Unix() {
		t.Fatalf("same-day next = %v", time.Unix(next, 0).UTC())
	}
	if _, ok := nextDailyRun("garbage", now); ok {
		t.Fatal("garbage cron accepted")
	}
}

func TestReachCounts(t *testing.T) {
	db := hotDB(t)
	seedUser(t, db, 1, "a", 0, 1)
	seedUser(t, db, 2, "b", 0, 2)
	seedUser(t, db, 3, "c", 3, 3) // deleted: excluded from total
	mustExec(t, db, `INSERT INTO sys_presence (uid, online, last_seen) VALUES (1, 1, 1)`)
	mustExec(t, db, `INSERT INTO sys_presence (uid, online, last_seen) VALUES (2, 0, 1)`)
	mustExec(t, db, `INSERT INTO conv_members (cid, uid, role, joined_at, muted_until) VALUES (10, 1, 0, 1, 0)`)
	mustExec(t, db, `INSERT INTO conv_members (cid, uid, role, joined_at, muted_until) VALUES (10, 2, 0, 1, 0)`)
	mustExec(t, db, `INSERT INTO conv_members (cid, uid, role, joined_at, muted_until) VALUES (11, 2, 0, 1, 0)`)

	r := NewReach(db, ReachConfig{})
	ctx := context.Background()

	global, err := r.CountReachable(ctx, nil)
	if err != nil {
		t.Fatalf("global: %v", err)
	}
	if global != 1+2 { // 1 online + 2 live users
		t.Fatalf("global reach = %d, want 3", global)
	}

	conv, err := r.CountReachable(ctx, []int64{10, 11})
	if err != nil {
		t.Fatalf("conv: %v", err)
	}
	if conv != 2 {
		t.Fatalf("conv reach = %d, want 2 distinct members", conv)
	}

	// Injected online source (bridge session count) wins over sys_presence.
	r = NewReach(db, ReachConfig{Online: func() (int64, error) { return 42, nil }})
	global, err = r.CountReachable(ctx, nil)
	if err != nil {
		t.Fatalf("global 2: %v", err)
	}
	if global != 44 {
		t.Fatalf("global with injected online = %d, want 44", global)
	}

	if _, err := r.CountReachable(ctx, []int64{0, -1}); !errors.Is(err, service.ErrInvalid) {
		t.Fatalf("bad conv ids err = %v, want ErrInvalid", err)
	}
	if _, err := NewReach(nil, ReachConfig{}).CountReachable(ctx, nil); !errors.Is(err, service.ErrNotConfigured) {
		t.Fatalf("nil db err = %v, want ErrNotConfigured", err)
	}
}

func TestFlagsListAndSet(t *testing.T) {
	db := hotDB(t)
	f := NewFlags(db)
	ctx := context.Background()

	if err := f.SetFlag(ctx, "chat.recall_window", `600`); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := f.SetFlag(ctx, "chat.recall_window", `900`); err != nil {
		t.Fatalf("set 2: %v", err)
	}
	if err := f.SetFlag(ctx, "a.first", `true`); err != nil {
		t.Fatalf("set 3: %v", err)
	}
	flags, err := f.ListFlags(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(flags) != 2 || flags[0].Key != "a.first" {
		t.Fatalf("flags = %+v", flags)
	}
	if flags[1].Value != "900" || flags[1].Version != 2 {
		t.Fatalf("upsert = %+v (want value 900, version 2)", flags[1])
	}
	if _, err := NewFlags(nil).ListFlags(ctx); !errors.Is(err, service.ErrNotConfigured) {
		t.Fatalf("nil db err = %v", err)
	}
}

func TestAuditReaderDegradesWithoutCold(t *testing.T) {
	a := NewAuditReader(nil, nil)
	rows, err := a.ListAudit(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("degraded read must not fail: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %d, want 0", len(rows))
	}
}

func TestLogJournalCountsDrops(t *testing.T) {
	j := &logJournal{}
	ev := store.AuditEvent{Actor: "root", Action: "user.ban", Subject: "user:1", At: 1}
	if err := j.Append(context.Background(), ev); err != nil {
		t.Fatalf("append: %v", err)
	}
	if j.Dropped() != 1 {
		t.Fatalf("dropped = %d", j.Dropped())
	}
	var nilJournal *logJournal
	if err := nilJournal.Append(context.Background(), ev); err != nil {
		t.Fatalf("nil journal must be a no-op: %v", err)
	}
}
