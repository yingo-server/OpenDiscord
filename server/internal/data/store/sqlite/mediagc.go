package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Media-GC reverse lookups. *DB satisfies job.MediaGCChecker: the GC walks
// media_dir, derives each file's sha256 (objects) or stored thumb_path
// (thumbnails), and asks these whether a media_media row still backs the file.
// Both ride indexes (idx_media_media_sha, idx_media_media_thumb_path, 0004) so
// a bounded GC pass is O(files·log rows), never a per-file full scan.

// HasObject reports whether any media_media row has sha256 == sha.
func (db *DB) HasObject(ctx context.Context, sha string) (bool, error) {
	if sha == "" {
		return false, nil
	}
	var one int
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM media_media WHERE sha256 = ? LIMIT 1`, sha).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sqlite: media gc has object %q: %w", sha, err)
	}
	return true, nil
}

// HasThumb reports whether any media_media row has thumb_path == relPath.
func (db *DB) HasThumb(ctx context.Context, relPath string) (bool, error) {
	if relPath == "" {
		return false, nil
	}
	var one int
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM media_media WHERE thumb_path = ? LIMIT 1`, relPath).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sqlite: media gc has thumb %q: %w", relPath, err)
	}
	return true, nil
}
