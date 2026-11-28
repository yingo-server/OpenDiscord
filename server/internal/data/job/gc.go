package job

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MediaGC reclaims the disk side of the media plane: it deletes object and
// thumbnail FILES that no longer have a media_media row backing them.
//
// Division of labour with the Trimmer (do not conflate them):
//   - Trimmer  manages DB ROWS: it deletes expired/unreferenced media_media
//     rows (ref_count = 0 and older than the retention window). That shrinks
//     the SQLite file, NOT the disk.
//   - MediaGC  manages DISK FILES: it walks media_dir, reverse-looks-up each
//     file in media_media, and removes the file when no row references it.
//     Once the trimmer has dropped a row, the next GC pass frees its bytes.
//
// Ordering invariant: a file is only ever an "orphan" AFTER its DB row is
// already gone. GC never deletes a file that a live row still points at, so it
// is safe to run on any schedule; a run that finds nothing to do is a no-op.
//
// Three SAFETY constraints (each independently enforced):
//  1. Only files INSIDE media_dir are ever removed. filepath.Clean + a
//     separator-aware prefix assertion (never a bare HasPrefix, so a sibling
//     "media_evil" cannot match "media") gate every path; symlinks are skipped
//     outright so a link pointing outside media_dir can never be followed or
//     unlinked-by-target.
//  2. Only files whose mtime is older than MinAge (default 24h) are removed.
//     This protects an in-flight upload's temp file (".upload-*") from being
//     treated as an orphan mid-write.
//  3. A single pass examines at most MaxFiles (default 10000) files, so a huge
//     media_dir cannot pin the jobber's single-flight slot for an unbounded
//     time. The next run continues from whatever remains.
//
// dry-run reports the counts without unlinking anything.
type MediaGC struct {
	// Dir is media_dir (the root that bounds every deletion).
	Dir string

	// Index is the DB reverse-lookup seam (media_media). Required — without it
	// the GC cannot prove a file is unreferenced, so it refuses to run rather
	// than guess and delete live bytes. *sqlite.DB satisfies it.
	Index MediaGCChecker

	// MinAge gates deletion by file age (default 24h). MaxFiles caps files
	// examined per pass (default 10000).
	MinAge   time.Duration
	MaxFiles int

	Now func() time.Time // test seam; nil = time.Now
	Log Logger           // optional progress sink
}

// MediaGCChecker is the narrow reverse-lookup surface the GC needs. It answers
// "does any media_media row still reference this object/thumbnail?" — nothing
// more (mechanism, not policy).
type MediaGCChecker interface {
	// HasObject reports whether a media_media row has sha256 == sha.
	HasObject(ctx context.Context, sha string) (bool, error)
	// HasThumb reports whether a media_media row has thumb_path == relPath.
	HasThumb(ctx context.Context, relPath string) (bool, error)
}

// Name implements Unit.
func (g *MediaGC) Name() string { return "gc" }

// Defaults for the two tunables (fixed here; policy has no media-GC fields yet).
const (
	defaultGCMinAge   = 24 * time.Hour
	defaultGCMaxFiles = 10000
)

// Run executes one GC pass. See the type doc for the safety constraints.
func (g *MediaGC) Run(ctx context.Context, dryRun bool) (Result, error) {
	if g.Dir == "" {
		return Result{}, fmt.Errorf("gc: media dir is not configured")
	}
	if g.Index == nil {
		return Result{}, fmt.Errorf("gc: no media index wired (assembly must set Index, the MediaGCChecker port)")
	}
	minAge := g.MinAge
	if minAge <= 0 {
		minAge = defaultGCMinAge
	}
	maxFiles := g.MaxFiles
	if maxFiles <= 0 {
		maxFiles = defaultGCMaxFiles
	}
	now := time.Now()
	if g.Now != nil {
		if n := g.Now(); !n.IsZero() {
			now = n
		}
	}

	base := filepath.Clean(g.Dir)
	fi, err := os.Stat(base)
	if err != nil {
		return Result{}, fmt.Errorf("gc: stat media dir %q: %w", base, err)
	}
	if !fi.IsDir() {
		return Result{}, fmt.Errorf("gc: media dir %q is not a directory", base)
	}

	var scanned, orphans, removed int64
	var firstErr error

	walkErr := filepath.WalkDir(base, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// A per-entry error must not abort the whole pass.
			if firstErr == nil {
				firstErr = fmt.Errorf("gc: walk %s: %w", path, walkErr)
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			return nil
		}
		// Constraint 1a: never follow or unlink a symlink (its target may be
		// outside media_dir; unlinking the link itself is not reclaiming media).
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if scanned >= int64(maxFiles) {
			return fs.SkipAll // Constraint 3: cap files examined per pass.
		}
		scanned++

		// Constraint 1b: path must be strictly under base (separator-aware).
		cleanPath := filepath.Clean(path)
		if cleanPath != base && !strings.HasPrefix(cleanPath, base+string(filepath.Separator)) {
			return nil // refuse to touch anything outside media_dir
		}
		info, ierr := d.Info()
		if ierr != nil || !info.Mode().IsRegular() {
			return nil // directories, devices, fifos: not our business
		}
		// Constraint 2: age gate — a fresh file may be an in-flight upload.
		if now.Sub(info.ModTime()) < minAge {
			return nil
		}
		rel, rerr := filepath.Rel(base, cleanPath)
		if rerr != nil {
			return nil
		}
		referenced, rerr := g.isReferenced(ctx, rel)
		if rerr != nil {
			if firstErr == nil {
				firstErr = rerr
			}
			return nil
		}
		if referenced {
			return nil
		}
		orphans++
		if dryRun {
			return nil
		}
		if rmErr := os.Remove(cleanPath); rmErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("gc: remove %s: %w", rel, rmErr)
			}
			return nil
		}
		removed++
		return nil
	})
	if walkErr != nil && firstErr == nil {
		firstErr = walkErr
	}

	acted := removed
	if dryRun {
		acted = orphans
	}
	detail := fmt.Sprintf("scanned %d files, found %d orphans, removed %d (min_age=%s, cap=%d, dir=%s)",
		scanned, orphans, removed, minAge, maxFiles, base)
	if dryRun {
		detail = fmt.Sprintf("would remove %d orphans of %d scanned files (min_age=%s, cap=%d, dir=%s)",
			orphans, scanned, minAge, maxFiles, base)
	}
	if g.Log != nil {
		g.Log.Info("media gc pass", "scanned", scanned, "orphans", orphans, "removed", removed, "dry_run", dryRun)
	}
	return Result{MovedRows: acted, ConvsTouched: int(scanned), DryRun: dryRun, Detail: detail}, firstErr
}

// isReferenced classifies a file by its media_dir-relative path and asks the
// index whether a live row still points at it. Files under thumbs/ are matched
// by their stored thumb_path; everything else is an object matched by basename
// (the content-addressed sha256).
func (g *MediaGC) isReferenced(ctx context.Context, rel string) (bool, error) {
	if strings.HasPrefix(rel, "thumbs"+string(filepath.Separator)) {
		return g.Index.HasThumb(ctx, rel)
	}
	return g.Index.HasObject(ctx, filepath.Base(rel))
}
