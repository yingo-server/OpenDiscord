package prod

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yingo-server/UFOT/server/internal/admin/bridgeproxy"
	"github.com/yingo-server/UFOT/server/internal/data/store/sqlite"
	"github.com/yingo-server/UFOT/server/internal/pkg/log"
)

// hotDB opens a real, migrated hot SQLite database in a temp dir. The adapters
// are SQL-heavy, so tests exercise real SQL — the one thing fakes cannot
// validate (statement validity, column names, new migration files).
func hotDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "hot.db"))
	if err != nil {
		t.Fatalf("open hot: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate hot: %v", err)
	}
	return db.DB
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func seedUser(t *testing.T, db *sql.DB, uid int64, name string, status int, createdAt int64) {
	t.Helper()
	mustExec(t, db, `INSERT INTO identity_users (uid, display_name, avatar_media_id, status, created_at, anonymous_at)
VALUES (?, ?, 0, ?, ?, NULL)`, uid, name, status, createdAt)
}

// fakeDispatch records injected frames; used for the assembly and ban-journal
// tests (no bridge involved).
type fakeDispatch struct {
	calls  int
	kind   uint32
	target bridgeproxy.Target
	body   []byte
	err    error
}

func (f *fakeDispatch) InjectFrame(_ context.Context, kind uint32, target bridgeproxy.Target, payload []byte) (bridgeproxy.Result, error) {
	f.calls++
	f.kind = kind
	f.target = target
	f.body = append([]byte(nil), payload...)
	if f.err != nil {
		return bridgeproxy.Result{}, f.err
	}
	return bridgeproxy.Result{StatusCode: 202, Topic: "sys.admin", Kind: kind}, nil
}

func fixedNow() time.Time { return time.Unix(1_800_000_000, 0) }

// discardLogger redirects the process logger into a temp file so adapter tests
// stay quiet. log.SetOutput is global; tests in a package run sequentially.
func discardLogger(t *testing.T) *log.Logger {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "log-*")
	if err != nil {
		t.Fatalf("temp log: %v", err)
	}
	log.SetOutput(f)
	t.Cleanup(func() {
		log.SetOutput(os.Stdout)
		_ = f.Close()
	})
	return log.For("test")
}
