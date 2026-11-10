package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newUpgradeDB builds a temp hot DB with the tables/columns `upgrade` touches:
// media_media (thumbs step) plus a scratch table so VACUUM has something to
// reclaim. Returns the open DB and its path.
func newUpgradeDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hot.db")
	db, err := sql.Open("sqlite3", "file:"+path+"?_journal_mode=WAL")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE media_media (
		media_id   TEXT PRIMARY KEY,
		sha256     TEXT,
		thumb_data TEXT NOT NULL DEFAULT '',
		thumb_path TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	return db, path
}

func upgradeEnvFor(path string, mediaDir string) *environment {
	e := defaultEnv()
	e.runCmd = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		// No systemd in tests: service counts as stopped.
		return nil, errTestNoSystemd
	}
	_ = path
	_ = mediaDir
	return e
}

var errTestNoSystemd = context.Canceled // any non-nil error → serviceActive false

// TestUpgradeDryRunDefault is the core safety contract: without --run the
// command reports the pending steps but changes nothing — no thumb files, no
// thumb_data cleared, auto_vacuum untouched.
func TestUpgradeDryRunDefault(t *testing.T) {
	db, path := newUpgradeDB(t)
	dir := t.TempDir()
	seedThumbRow(t, db, "id-1", migSha1, thumbB64("legacy-one"))
	db.Close()

	e := upgradeEnvFor(path, dir)
	e.getenv = func(string) string { return "" }
	_ = os.Setenv("OPENDISCORD_MEDIA_DIR", "")
	g := globals{}
	if err := cmdUpgrade(context.Background(), e, g, []string{"--hot-db", path, "--media-dir", dir}); err != nil {
		t.Fatalf("cmdUpgrade dry-run: %v", err)
	}

	// Reopen and verify nothing was written.
	db2, err := sql.Open("sqlite3", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	var n int
	if err := db2.QueryRow(`SELECT COUNT(*) FROM media_media WHERE thumb_data != ''`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("dry-run migrated rows: got %d, want 1 (untouched)", n)
	}
	var av int
	if err := db2.QueryRow(`PRAGMA auto_vacuum`).Scan(&av); err != nil {
		t.Fatal(err)
	}
	if av != 0 {
		t.Fatalf("dry-run changed auto_vacuum: got %d, want 0", av)
	}
}

// TestUpgradeRunExecutesSteps: with --run, thumb rows are migrated to files
// and auto_vacuum ends up as 2 (INCREMENTAL) after the VACUUM conversion.
func TestUpgradeRunExecutesSteps(t *testing.T) {
	db, path := newUpgradeDB(t)
	dir := t.TempDir()
	seedThumbRow(t, db, "id-1", migSha1, thumbB64("legacy-run"))
	db.Close()

	e := upgradeEnvFor(path, dir)
	if err := cmdUpgrade(context.Background(), e, globals{},
		[]string{"--run", "--hot-db", path, "--media-dir", dir}); err != nil {
		t.Fatalf("cmdUpgrade --run: %v", err)
	}

	db2, err := sql.Open("sqlite3", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	var n int
	if err := db2.QueryRow(`SELECT COUNT(*) FROM media_media WHERE thumb_data != ''`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("run left %d thumb rows, want 0", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "thumbs", migSha1[:2], migSha1[:4], migSha1+".thumb")); err != nil {
		t.Fatalf("thumbnail file missing: %v", err)
	}
	var av int
	if err := db2.QueryRow(`PRAGMA auto_vacuum`).Scan(&av); err != nil {
		t.Fatal(err)
	}
	if av != 2 {
		t.Fatalf("auto_vacuum after run = %d, want 2 (INCREMENTAL)", av)
	}
}

// TestUpgradeRunIdempotent: a second --run on a converted DB is a no-op plan —
// both steps report not needed and nothing fails.
func TestUpgradeRunIdempotent(t *testing.T) {
	_, path := newUpgradeDB(t)
	e := upgradeEnvFor(path, t.TempDir())
	e.getenv = func(string) string { return "" }

	// First run converts (no thumb rows, auto_vacuum conversion only).
	if err := cmdUpgrade(context.Background(), e, globals{},
		[]string{"--run", "--hot-db", path, "--media-dir", t.TempDir()}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Second run: capture JSON to assert both steps unneeded.
	var sb strings.Builder
	e.stdout = &sb
	e2 := *e
	e2.stdout = &sb
	if err := cmdUpgrade(context.Background(), &e2, globals{JSON: true},
		[]string{"--run", "--hot-db", path, "--media-dir", t.TempDir()}); err != nil {
		t.Fatalf("second run: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, `"dry_run": false`) {
		t.Fatalf("second run not marked executed: %s", out)
	}
	if strings.Contains(out, `"needed": true`) {
		t.Fatalf("second run still plans work (should be no-op): %s", out)
	}
}

// TestUpgradeRunRefusesWhileServiceActive: the stop-the-service guard.
func TestUpgradeRunRefusesWhileServiceActive(t *testing.T) {
	_, path := newUpgradeDB(t)
	e := upgradeEnvFor(path, t.TempDir())
	e.runCmd = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte("active\n"), nil // systemctl is-active: running
	}
	err := cmdUpgrade(context.Background(), e, globals{},
		[]string{"--run", "--hot-db", path, "--media-dir", t.TempDir()})
	if err == nil {
		t.Fatal("expected refusal while service is active")
	}
	var de *deniedErr
	if !asDenied(err, &de) {
		t.Fatalf("want deniedErr (exit 77), got %T: %v", err, err)
	}
	// --force bypasses the guard.
	if err := cmdUpgrade(context.Background(), e, globals{},
		[]string{"--run", "--force", "--hot-db", path, "--media-dir", t.TempDir()}); err != nil {
		t.Fatalf("--force should proceed: %v", err)
	}
}

func asDenied(err error, target **deniedErr) bool {
	if de, ok := err.(*deniedErr); ok {
		*target = de
		return true
	}
	return false
}

// TestUpgradeMissingDB: nice error (exit 65 shape) when hot-db doesn't exist.
func TestUpgradeMissingDB(t *testing.T) {
	e := upgradeEnvFor(filepath.Join(t.TempDir(), "nope.db"), t.TempDir())
	err := cmdUpgrade(context.Background(), e, globals{},
		[]string{"--hot-db", "/nonexistent/hot.db"})
	if err == nil {
		t.Fatal("expected error for missing hot db")
	}
	var nf *notFoundErr
	if !asNotFound(err, &nf) {
		t.Fatalf("want notFoundErr, got %T: %v", err, err)
	}
}

func asNotFound(err error, target **notFoundErr) bool {
	if nf, ok := err.(*notFoundErr); ok {
		*target = nf
		return true
	}
	return false
}
