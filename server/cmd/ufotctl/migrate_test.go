package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

// newMediaDB builds a temp hot DB with just the media_media columns the
// migration touches (media_id / sha256 / thumb_data / thumb_path).
func newMediaDB(t *testing.T) (*sql.DB, string) {
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

func seedThumbRow(t *testing.T, db *sql.DB, id, sha, thumb string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO media_media (media_id, sha256, thumb_data, thumb_path)
		VALUES (?, ?, ?, '')`, id, sha, thumb); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

const (
	migSha1 = "1111111111111111111111111111111111111111111111111111111111111111"
	migSha2 = "2222222222222222222222222222222222222222222222222222222222222222"
)

func thumbB64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// TestMigrateThumbsMovesFilesAndColumns is the end-to-end contract: two rows
// with inline thumb_data become two files, thumb_path is filled, thumb_data is
// cleared, and a re-run migrates 0 rows (idempotent).
func TestMigrateThumbsMovesFilesAndColumns(t *testing.T) {
	db, _ := newMediaDB(t)
	dir := t.TempDir()
	v1 := thumbB64("thumbnail-one-bytes")
	v2 := thumbB64("second-thumbnail")
	seedThumbRow(t, db, "id-1", migSha1, v1)
	seedThumbRow(t, db, "id-2", migSha2, v2)

	rep, err := migrateThumbs(context.Background(), db, dir, false, 500)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if rep.Migrated != 2 || rep.Files != 2 {
		t.Fatalf("migrated=%d files=%d, want 2/2", rep.Migrated, rep.Files)
	}
	if rep.FreedBytes <= 0 {
		t.Fatalf("freed bytes = %d, want > 0", rep.FreedBytes)
	}

	// Each row: thumb_path filled, thumb_data empty, file on disk with the
	// exact original base64 text (the reader returns file bytes as thumb_data).
	for _, c := range []struct{ id, sha, val string }{
		{"id-1", migSha1, v1}, {"id-2", migSha2, v2},
	} {
		var thumbPath, thumbData string
		if err := db.QueryRow(`SELECT thumb_path, thumb_data FROM media_media WHERE media_id = ?`, c.id).
			Scan(&thumbPath, &thumbData); err != nil {
			t.Fatal(err)
		}
		if thumbData != "" {
			t.Fatalf("%s: thumb_data not cleared: %q", c.id, thumbData)
		}
		wantRel := filepath.Join("thumbs", c.sha[:2], c.sha[:4], c.sha+".thumb")
		if thumbPath != wantRel {
			t.Fatalf("%s: thumb_path = %q, want %q", c.id, thumbPath, wantRel)
		}
		got, err := os.ReadFile(filepath.Join(dir, thumbPath))
		if err != nil {
			t.Fatalf("%s: read migrated file: %v", c.id, err)
		}
		if string(got) != c.val {
			t.Fatalf("%s: file content = %q, want %q", c.id, got, c.val)
		}
	}

	// Idempotent: a second run finds nothing.
	rep2, err := migrateThumbs(context.Background(), db, dir, false, 500)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if rep2.ScannedRows != 0 || rep2.Migrated != 0 {
		t.Fatalf("re-run scanned=%d migrated=%d, want 0/0", rep2.ScannedRows, rep2.Migrated)
	}
}

// TestMigrateThumbsDryRunTouchesNothing: dry-run reports the counts but writes
// no file and leaves the column untouched.
func TestMigrateThumbsDryRunTouchesNothing(t *testing.T) {
	db, _ := newMediaDB(t)
	dir := t.TempDir()
	v := thumbB64("inline-thumb")
	seedThumbRow(t, db, "id-1", migSha1, v)

	rep, err := migrateThumbs(context.Background(), db, dir, true, 500)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !rep.DryRun || rep.ScannedRows != 1 || rep.Migrated != 0 || rep.FreedBytes != 0 {
		t.Fatalf("dry-run report = %+v", rep)
	}
	// No file written.
	if _, err := os.Stat(filepath.Join(dir, "thumbs")); !os.IsNotExist(err) {
		t.Fatalf("dry-run created thumbs/ (err=%v)", err)
	}
	// Column unchanged.
	var td string
	if err := db.QueryRow(`SELECT thumb_data FROM media_media WHERE media_id='id-1'`).Scan(&td); err != nil {
		t.Fatal(err)
	}
	if td != v {
		t.Fatalf("dry-run modified thumb_data: %q", td)
	}
}

// TestMigrateThumbsBatchSmall: a batch of 1 still migrates all rows across
// multiple rounds (keyset pagination).
func TestMigrateThumbsBatchSmall(t *testing.T) {
	db, _ := newMediaDB(t)
	dir := t.TempDir()
	seedThumbRow(t, db, "id-1", migSha1, thumbB64("one"))
	seedThumbRow(t, db, "id-2", migSha2, thumbB64("two"))

	rep, err := migrateThumbs(context.Background(), db, dir, false, 1)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if rep.Migrated != 2 {
		t.Fatalf("migrated = %d, want 2 with batch=1", rep.Migrated)
	}
	var remaining int
	if err := db.QueryRow(`SELECT COUNT(*) FROM media_media WHERE thumb_data != ''`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("remaining inline rows = %d, want 0", remaining)
	}
}
