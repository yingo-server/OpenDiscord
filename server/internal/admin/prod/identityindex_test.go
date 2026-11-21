package prod

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/yingo-server/UFOT/server/internal/admin/service"
)

func TestNormalizeIdentifier(t *testing.T) {
	cases := map[string]string{
		" +86 138-0000-0000 ":  "+8613800000000",
		"(010)1234":            "0101234",
		"  Alice@Example.COM ": "alice@example.com",
		"":                     "",
	}
	for in, want := range cases {
		if got := NormalizeIdentifier(in); got != want {
			t.Fatalf("NormalizeIdentifier(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIDIndexHasherMatchesAuthPath(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	h := IDIndexHasher(key, IDTypePhone, IDTypeEmail)

	got := h("138-0000-0000")
	if len(got) != 2 {
		t.Fatalf("candidates = %d, want 2", len(got))
	}
	// Recompute independently: hex(HMAC-SHA256(key, "<type>:<normalized>")).
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%d:%s", IDTypePhone, "13800000000")
	if want := hex.EncodeToString(mac.Sum(nil)); got[0] != want {
		t.Fatalf("phone index = %s, want %s", got[0], want)
	}
	if len(got[0]) != 64 {
		t.Fatalf("index length = %d, want 64 hex chars", len(got[0]))
	}
	if h("") != nil {
		t.Fatal("empty identifier must yield no candidates")
	}
	if IDIndexHasher(nil, IDTypePhone)("138") != nil {
		t.Fatal("empty key must yield no candidates")
	}
}

func TestUsersLookupByRawPhoneEndToEnd(t *testing.T) {
	db := hotDB(t)
	seedUser(t, db, 7, "phone-user", 0, 100)
	key := []byte("0123456789abcdef0123456789abcdef")
	hasher := IDIndexHasher(key, IDTypePhone)
	index := hasher("13800000000")[0]
	mustExec(t, db, `INSERT INTO identity_identities (id_type, id_value, uid, verified, created_at)
VALUES (?, ?, 7, 1, 100)`, IDTypePhone, index)

	u := NewUsers(db, UsersConfig{Hasher: hasher})
	page, err := u.ListUsers(context.Background(), service.UserFilter{
		Query: "138 0000 0000", Status: -1, Limit: 10,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.Total != 1 || page.Items[0].UID != 7 {
		t.Fatalf("raw phone lookup = %+v", page)
	}

	// Without the hasher only an exact id_value query can match (documented).
	raw := NewUsers(db, UsersConfig{})
	page, err = raw.ListUsers(context.Background(), service.UserFilter{
		Query: index, Status: -1, Limit: 10,
	})
	if err != nil {
		t.Fatalf("list raw: %v", err)
	}
	if page.Total != 1 {
		t.Fatalf("hashed-value lookup = %+v", page)
	}
}
