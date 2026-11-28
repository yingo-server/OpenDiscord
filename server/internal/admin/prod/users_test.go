package prod

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yingo-server/UFOT/server/internal/admin/service"
	"github.com/yingo-server/UFOT/server/internal/data/store"
)

func TestUsersListFilters(t *testing.T) {
	db := hotDB(t)
	now := fixedNow()
	seedUser(t, db, 1, "alice", 0, 100)
	seedUser(t, db, 2, "bob", 1, 200)
	seedUser(t, db, 3, "carol", 0, 300)
	mustExec(t, db, `INSERT INTO identity_identities (id_type, id_value, uid, verified, created_at)
VALUES (1, ?, 1, 1, 100)`, "hmac-13800000000")

	u := NewUsers(db, UsersConfig{
		Hasher: func(raw string) []string { return []string{"hmac-" + raw} },
		Now:    func() time.Time { return now },
	})
	ctx := context.Background()

	all, err := u.ListUsers(ctx, service.UserFilter{Status: -1, Limit: 10})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if all.Total != 3 || len(all.Items) != 3 {
		t.Fatalf("total=%d items=%d, want 3/3", all.Total, len(all.Items))
	}
	if all.Items[0].UID != 3 {
		t.Fatalf("first item uid=%d, want newest (3)", all.Items[0].UID)
	}

	normal, err := u.ListUsers(ctx, service.UserFilter{Status: 0, Limit: 10})
	if err != nil {
		t.Fatalf("list status 0: %v", err)
	}
	if normal.Total != 2 || len(normal.Items) != 2 {
		t.Fatalf("status filter total=%d items=%d, want 2/2", normal.Total, len(normal.Items))
	}

	byName, err := u.ListUsers(ctx, service.UserFilter{Query: "aro", Status: -1, Limit: 10})
	if err != nil {
		t.Fatalf("list q: %v", err)
	}
	if byName.Total != 1 || byName.Items[0].UID != 3 {
		t.Fatalf("display_name LIKE: total=%d items=%+v", byName.Total, byName.Items)
	}

	// q goes through IDHasher first: the raw phone matches the stored HMAC.
	byIdentity, err := u.ListUsers(ctx, service.UserFilter{Query: "13800000000", Status: -1, Limit: 10})
	if err != nil {
		t.Fatalf("list identity: %v", err)
	}
	if byIdentity.Total != 1 || byIdentity.Items[0].UID != 1 {
		t.Fatalf("identity HMAC match: total=%d items=%+v", byIdentity.Total, byIdentity.Items)
	}

	// LIKE wildcards in the query are literal, not patterns.
	mustExec(t, db, `UPDATE identity_users SET display_name = 'a%b' WHERE uid = 2`)
	lit, err := u.ListUsers(ctx, service.UserFilter{Query: "%", Status: -1, Limit: 10})
	if err != nil {
		t.Fatalf("list wildcard: %v", err)
	}
	if lit.Total != 1 || lit.Items[0].UID != 2 {
		t.Fatalf("escaped LIKE: total=%d items=%+v", lit.Total, lit.Items)
	}

	paged, err := u.ListUsers(ctx, service.UserFilter{Status: -1, Limit: 1, Offset: 1})
	if err != nil {
		t.Fatalf("list paged: %v", err)
	}
	if paged.Total != 3 || len(paged.Items) != 1 || paged.Items[0].UID != 2 {
		t.Fatalf("paging: total=%d items=%+v", paged.Total, paged.Items)
	}
	if paged.Limit != 1 || paged.Offset != 1 {
		t.Fatalf("limit/offset echo = %d/%d", paged.Limit, paged.Offset)
	}
}

func TestUsersShowAndBanState(t *testing.T) {
	db := hotDB(t)
	now := fixedNow()
	seedUser(t, db, 1, "alice", 0, 100)
	seedUser(t, db, 2, "bob", 1, 200)
	seedUser(t, db, 3, "carol", 0, 300)
	seedUser(t, db, 4, "dave", 0, 400)

	// uid 2: active ban; uid 3: expired ban (must not show).
	mustExec(t, db, `INSERT INTO identity_bans (uid, level, reason, until, created_by, created_at)
VALUES (2, 2, 'spam', ?, 'root', 210)`, now.Unix()+3600)
	mustExec(t, db, `INSERT INTO identity_bans (uid, level, reason, until, created_by, created_at)
VALUES (3, 1, 'old', ?, 'root', 310)`, now.Unix()-10)

	u := NewUsers(db, UsersConfig{Now: func() time.Time { return now }})
	ctx := context.Background()

	got, err := u.GetUser(ctx, 2)
	if err != nil {
		t.Fatalf("get 2: %v", err)
	}
	if got.Ban == nil || got.Ban.Level != 2 || got.Ban.Reason != "spam" || got.Ban.CreatedBy != "root" {
		t.Fatalf("ban = %+v", got.Ban)
	}
	if got.DisplayName != "bob" || got.Status != 1 {
		t.Fatalf("record = %+v", got)
	}

	expired, err := u.GetUser(ctx, 3)
	if err != nil {
		t.Fatalf("get 3: %v", err)
	}
	if expired.Ban != nil {
		t.Fatalf("expired ban must be absent, got %+v", expired.Ban)
	}

	if _, err := u.GetUser(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing user err = %v, want store.ErrNotFound", err)
	}
	if _, err := u.GetUser(ctx, 0); !errors.Is(err, service.ErrInvalid) {
		t.Fatalf("uid 0 err = %v, want ErrInvalid", err)
	}

	// Ban writes the journal row that GetUser then reports.
	if err := u.RecordBan(ctx, 4, 1, "flood", 0, "ops"); err != nil {
		t.Fatalf("RecordBan: %v", err)
	}
	dave, err := u.GetUser(ctx, 4)
	if err != nil {
		t.Fatalf("get 4: %v", err)
	}
	if dave.Ban == nil || dave.Ban.Level != 1 || dave.Ban.Until != 0 || dave.Ban.Reason != "flood" {
		t.Fatalf("recorded ban = %+v", dave.Ban)
	}

	// Unban appends a lift row: history kept, state lifted.
	if err := u.RecordBanLift(ctx, 4, "appeal accepted", "ops"); err != nil {
		t.Fatalf("RecordBanLift: %v", err)
	}
	dave, err = u.GetUser(ctx, 4)
	if err != nil {
		t.Fatalf("get 4 after lift: %v", err)
	}
	if dave.Ban != nil {
		t.Fatalf("lifted ban still effective: %+v", dave.Ban)
	}
	var history int
	if err := db.QueryRow(`SELECT COUNT(*) FROM identity_bans WHERE uid = 4`).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if history != 2 {
		t.Fatalf("ban history rows = %d, want 2 (append-only)", history)
	}
}

func TestUsersNilDBIsNotConfigured(t *testing.T) {
	u := NewUsers(nil, UsersConfig{})
	if _, err := u.ListUsers(context.Background(), service.UserFilter{Status: -1}); !errors.Is(err, service.ErrNotConfigured) {
		t.Fatalf("list err = %v, want ErrNotConfigured", err)
	}
	if _, err := u.GetUser(context.Background(), 1); !errors.Is(err, service.ErrNotConfigured) {
		t.Fatalf("get err = %v, want ErrNotConfigured", err)
	}
}

func TestUserWhereShape(t *testing.T) {
	where, args := userWhere(service.UserFilter{Query: "bob", Status: 1, Limit: 10},
		[]string{"hmac-bob", "hmac-bob-2"})
	if len(args) != 4 {
		t.Fatalf("args = %d, want 4 (like, id_value, id_value, status)", len(args))
	}
	if args[0] != "%bob%" || args[1] != "hmac-bob" || args[2] != "hmac-bob-2" || args[3] != 1 {
		t.Fatalf("args = %#v", args)
	}
	if !strings.Contains(where, "i.id_value IN (?,?)") {
		t.Fatalf("where = %q", where)
	}
	if where == "" || where[0] != ' ' {
		t.Fatalf("where = %q", where)
	}
	// Status -1 means "all": no status predicate.
	where, args = userWhere(service.UserFilter{Status: -1}, nil)
	if where != " WHERE 1=1" || len(args) != 0 {
		t.Fatalf("empty filter = %q / %#v", where, args)
	}
}

func TestEscapeLike(t *testing.T) {
	if got, want := escapeLike(`a%b_c\d`), `a\%b\_c\\d`; got != want {
		t.Fatalf("escapeLike = %q, want %q", got, want)
	}
}

func TestClampPage(t *testing.T) {
	if l, o := clampPage(0, 50, 200, -3); l != 50 || o != 0 {
		t.Fatalf("clampPage(0,50,200,-3) = %d,%d", l, o)
	}
	if l, _ := clampPage(9999, 50, 200, 0); l != 200 {
		t.Fatalf("clamp max = %d", l)
	}
}
