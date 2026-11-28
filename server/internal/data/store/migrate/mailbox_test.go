package migrate

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func realMigrationFS(t *testing.T) fs.FS {
	abs, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "store", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	return osDirFS(abs)
}

func osDirFS(dir string) fs.FS { return os.DirFS(dir) }

// TestMailboxDomainDiscovery 锁定 v3 mailbox 分域骨架（v3-bus-design §9 P0）：
// hot/mailbox/0001_mailbox_init.sql 必须被发现、命名合规、含三张表。
func TestMailboxDomainDiscovery(t *testing.T) {
	r := &Runner{Dialect: DialectSQLite, Dir: realMigrationFS(t)}
	ms, err := r.Discover()
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	var found *Migration
	for i := range ms {
		if ms[i].Domain == "mailbox" {
			if found != nil {
				t.Fatal("duplicate mailbox domain entry")
			}
			found = &ms[i]
		}
	}
	if found == nil {
		t.Fatal("mailbox domain not discovered")
	}
	if found.Version != 1 {
		t.Fatalf("mailbox version = %d, want 1", found.Version)
	}
	for _, table := range []string{
		"CREATE TABLE IF NOT EXISTS mailbox_entries",
		"CREATE TABLE IF NOT EXISTS mailbox_cursors",
		"idx_mailbox_user_seq",
		"idx_mailbox_state_delivered",
	} {
		if !strings.Contains(found.SQL, table) {
			t.Errorf("mailbox 0001 missing %q", table)
		}
	}
}
