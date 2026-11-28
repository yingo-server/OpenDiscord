package job

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeGCIndex is the in-memory MediaGCChecker: a set of referenced object shas
// and thumbnail relative paths.
type fakeGCIndex struct {
	objects map[string]bool
	thumbs  map[string]bool
}

func (f *fakeGCIndex) HasObject(_ context.Context, sha string) (bool, error) {
	return f.objects[sha], nil
}

func (f *fakeGCIndex) HasThumb(_ context.Context, rel string) (bool, error) {
	return f.thumbs[rel], nil
}

// writeFile writes a small file, creating parent dirs, and back-dates it.
func writeAgedFile(t *testing.T, path string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(-age)
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
}

const (
	shaRef1 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaRef2 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	shaOrph = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	shaNew  = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
)

// TestMediaGCDeletesOnlyOrphans is the core contract: of three old files (two
// referenced by a DB row, one orphan) only the orphan is removed; a fresh
// (<24h) orphan-free file is left alone; and a dry-run first removes nothing.
func TestMediaGCDeletesOnlyOrphans(t *testing.T) {
	dir := t.TempDir()
	obj := func(sha string) string { return filepath.Join(dir, sha[:2], sha[2:4], sha) }

	ref1 := obj(shaRef1)
	ref2 := obj(shaRef2)
	orph := obj(shaOrph)
	fresh := obj(shaNew)
	writeAgedFile(t, ref1, 48*time.Hour)
	writeAgedFile(t, ref2, 48*time.Hour)
	writeAgedFile(t, orph, 48*time.Hour) // old + unreferenced => orphan
	writeAgedFile(t, fresh, time.Hour)   // new + unreferenced => protected by age gate

	idx := &fakeGCIndex{objects: map[string]bool{shaRef1: true, shaRef2: true}}
	gc := &MediaGC{Dir: dir, Index: idx}

	// --- dry-run: report, delete nothing ---
	res, err := gc.Run(context.Background(), true)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !res.DryRun {
		t.Fatal("dry-run flag missing")
	}
	if res.MovedRows != 1 {
		t.Fatalf("dry-run orphans = %d, want 1", res.MovedRows)
	}
	if _, err := os.Stat(orph); err != nil {
		t.Fatal("dry-run must NOT delete the orphan")
	}

	// --- real run: orphan gone, everything else stays ---
	res, err = gc.Run(context.Background(), false)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.MovedRows != 1 {
		t.Fatalf("removed = %d, want 1", res.MovedRows)
	}
	if _, err := os.Stat(orph); !os.IsNotExist(err) {
		t.Fatal("orphan should have been deleted")
	}
	for _, keep := range []string{ref1, ref2, fresh} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("file %s must survive: %v", keep, err)
		}
	}
}

// TestMediaGCThumbnailRefs: a thumbnail file under thumbs/ is kept only while a
// row's thumb_path still equals its media_dir-relative path.
func TestMediaGCThumbnailRefs(t *testing.T) {
	dir := t.TempDir()
	sha := shaRef1
	rel := filepath.Join("thumbs", sha[:2], sha[:4], sha+".thumb")
	thumbPath := filepath.Join(dir, rel)
	writeAgedFile(t, thumbPath, 48*time.Hour)

	// Referenced: survives.
	gc := &MediaGC{Dir: dir, Index: &fakeGCIndex{thumbs: map[string]bool{rel: true}}}
	if res, err := gc.Run(context.Background(), false); err != nil || res.MovedRows != 0 {
		t.Fatalf("referenced thumb: res=%+v err=%v", res, err)
	}
	if _, err := os.Stat(thumbPath); err != nil {
		t.Fatal("referenced thumbnail must survive")
	}

	// Unreferenced: removed.
	gc = &MediaGC{Dir: dir, Index: &fakeGCIndex{}}
	if res, err := gc.Run(context.Background(), false); err != nil || res.MovedRows != 1 {
		t.Fatalf("orphan thumb: res=%+v err=%v", res, err)
	}
	if _, err := os.Stat(thumbPath); !os.IsNotExist(err) {
		t.Fatal("orphan thumbnail should be deleted")
	}
}

// TestMediaGCNeverEscapesDir: a symlink inside media_dir pointing to a file
// OUTSIDE media_dir must never cause that outside file to be deleted. A file
// that merely lives outside is obviously untouched (the walk starts at dir).
func TestMediaGCNeverEscapesDir(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.bin")
	if err := os.WriteFile(secret, []byte("do not touch"), 0o640); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(-72 * time.Hour)
	_ = os.Chtimes(secret, mt, mt)

	// A symlink inside media_dir whose target is the outside file.
	link := filepath.Join(dir, "escape")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// A symlink to the outside DIRECTORY (WalkDir must not descend it).
	linkDir := filepath.Join(dir, "escdir")
	if err := os.Symlink(outside, linkDir); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}

	gc := &MediaGC{Dir: dir, Index: &fakeGCIndex{}}
	if _, err := gc.Run(context.Background(), false); err != nil {
		t.Fatalf("gc: %v", err)
	}
	// The outside file (and everything in the outside dir) must still exist.
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("file outside media_dir was deleted — ESCAPE: %v", err)
	}
}

// TestMediaGCRequiresIndex: without a DB index the GC refuses to run (it cannot
// prove a file is orphaned, so it must not guess and delete).
func TestMediaGCRequiresIndex(t *testing.T) {
	gc := &MediaGC{Dir: t.TempDir()}
	if _, err := gc.Run(context.Background(), false); err == nil {
		t.Fatal("nil index must surface an error, not delete blindly")
	}
}

// panickyIndex panics on any lookup, to prove a broken GC unit cannot take the
// process down (the jobber recover converts it to an error).
type panickyIndex struct{}

func (panickyIndex) HasObject(context.Context, string) (bool, error) {
	panic("gc index exploded")
}
func (panickyIndex) HasThumb(context.Context, string) (bool, error) {
	panic("gc index exploded")
}

// TestMediaGCPanicRecovered: a panicking GC unit is recovered by the jobber
// (process + other jobs survive) and surfaces as an error.
func TestMediaGCPanicRecovered(t *testing.T) {
	dir := t.TempDir()
	// One old file so the walk reaches the panicking lookup.
	sha := shaRef1
	writeAgedFile(t, filepath.Join(dir, sha[:2], sha[2:4], sha), 48*time.Hour)

	jb := NewJobber(nil)
	gc := &MediaGC{Dir: dir, Index: panickyIndex{}}
	jb.Register(gc, Spec{Kind: CronInterval, Interval: 3600})

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("jobber did not recover the GC panic: %v", r)
			}
		}()
		_, err := jb.Trigger(context.Background(), "gc", false)
		if err == nil || !strings.Contains(err.Error(), "panicked") {
			t.Fatalf("panicking GC must surface a panic error, got: %v", err)
		}
	}()

	// The process is intact: a second trigger again returns an error (not wedged).
	if _, err := jb.Trigger(context.Background(), "gc", false); err == nil {
		t.Fatal("second panic must also surface an error")
	}
}
