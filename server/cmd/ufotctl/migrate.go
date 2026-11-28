package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yingo-server/UFOT/server/internal/pkg/i18n"
)

// media migration: move the legacy inline `thumb_data` blob out of the hot DB
// and onto disk (one-shot, idempotent).
//
// Background (migrations/hot/media/0003): thumb_data held a base64 preview
// (~10.7KB of TEXT per row, >10GB at a million images). New uploads keep the
// thumbnail in a FILE and store only its path in thumb_path; readers already
// prefer the file and fall back to the inline column. This command finishes the
// job for the LEGACY rows: write the file, set thumb_path, clear thumb_data.
// Re-running is a no-op (rows with empty thumb_data are skipped).

// thumbMigrateReport is the payload for `migrate thumbs`.
type thumbMigrateReport struct {
	DryRun      bool   `json:"dry_run"`
	ScannedRows int64  `json:"scanned_rows"`  // rows with thumb_data != '' at start
	Migrated    int64  `json:"migrated"`      // rows actually moved (0 in dry-run)
	Skipped     int64  `json:"skipped"`       // rows left untouched (bad/empty sha, write err)
	Files       int64  `json:"files_written"` // thumbnail files created
	BeforeBytes int64  `json:"thumb_data_bytes_before"`
	AfterBytes  int64  `json:"thumb_data_bytes_after"`
	FreedBytes  int64  `json:"freed_bytes"`
	Batch       int    `json:"batch"`
	MediaDir    string `json:"media_dir,omitempty"`
}

func cmdMigrate(ctx context.Context, e *environment, g globals, args []string) error {
	if len(args) == 0 {
		return usagef("migrate: 需要子命令 thumbs")
	}
	switch args[0] {
	case "thumbs":
		return cmdMigrateThumbs(ctx, e, g, args[1:])
	default:
		return usagef("migrate: 未知子命令 %q (want thumbs)", args[0])
	}
}

func cmdMigrateThumbs(ctx context.Context, e *environment, g globals, args []string) error {
	fs := flag.NewFlagSet("migrate thumbs", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "只统计将迁移的行数与字节数，不写盘不改库")
	batch := fs.Int("batch", 500, "每批处理的行数")
	dbPath := fs.String("hot-db", "", "热库路径")
	mediaDir := fs.String("media-dir", "", "媒体目录（默认读 UFOT_MEDIA_DIR / ./data/media）")
	if err := parseFlags(fs, e, "migrate thumbs", args, true); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usagef("migrate thumbs: unexpected argument %q", fs.Arg(0))
	}
	if *batch <= 0 {
		return usagef("migrate thumbs: --batch 必须 > 0")
	}

	path := hotDBPath(e, *dbPath)
	if _, err := e.stat(path); err != nil {
		return notFoundf("热库不存在: %s (%v)", path, err)
	}
	dir := resolveMediaDir(e, *mediaDir)
	if dir == "" {
		return usagef("migrate thumbs: 未指定媒体目录（--media-dir 或 UFOT_MEDIA_DIR）")
	}

	// dry-run stays read-only; a real run needs a writable handle.
	open := e.openDB
	if !*dry {
		open = e.openDBRW
	}
	db, err := open(path)
	if err != nil {
		return fmt.Errorf("打开热库 %s: %w", path, err)
	}
	defer db.Close()

	rep, err := migrateThumbs(ctx, db, dir, *dry, *batch)
	if err != nil {
		return err
	}
	rep.MediaDir = dir

	if g.JSON {
		return emitJSON(e, "migrate thumbs", rep)
	}
	renderThumbMigrate(e, rep)
	return nil
}

// renderThumbMigrate prints the human-readable summary.
func renderThumbMigrate(e *environment, rep thumbMigrateReport) {
	if i18n.StdinIsTTY() {
		e.printf("+--------------------------------------------------\n")
		defer e.printf("+--------------------------------------------------\n")
	}
	mode := "迁移"
	if rep.DryRun {
		mode = "dry-run（未写盘未改库）"
	}
	e.printf("migrate thumbs: %s\n", mode)
	e.printf("  media_dir   : %s\n", rep.MediaDir)
	e.printf("  待迁移行数  : %d\n", rep.ScannedRows)
	e.printf("  已迁移行数  : %d\n", rep.Migrated)
	if rep.Skipped > 0 {
		e.printf("  跳过行数    : %d\n", rep.Skipped)
	}
	e.printf("  写出文件数  : %d\n", rep.Files)
	e.printf("  thumb_data   : %s → %s (释放 %s)\n",
		humanBytes(uint64(rep.BeforeBytes)), humanBytes(uint64(rep.AfterBytes)),
		humanBytes(uint64(rep.FreedBytes)))
}

// migrateThumbs does the real work against an already-open DB. It is a pure
// function of (db, mediaDir, dryRun, batch) so tests drive it directly.
//
// It paginates media_media by media_id (keyset) in `batch`-row chunks, each
// chunk a short transaction, so a big table never holds one long write lock on
// the live hot DB. For every row: decode+write the thumbnail file (base64 text
// verbatim — the reader returns file bytes as thumb_data, so the file must hold
// the same shape the inline column did), then set thumb_path and clear
// thumb_data. Rows whose thumb_data is already empty are never selected, so a
// re-run migrates 0 rows.
func migrateThumbs(ctx context.Context, db *sql.DB, mediaDir string, dryRun bool, batch int) (thumbMigrateReport, error) {
	rep := thumbMigrateReport{DryRun: dryRun, Batch: batch}
	if batch <= 0 {
		batch = 500
	}
	base := filepath.Clean(mediaDir)

	// Baseline bytes (chars) held by the inline column.
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(LENGTH(thumb_data)),0) FROM media_media WHERE thumb_data != ''`).
		Scan(&rep.BeforeBytes); err != nil {
		return rep, fmt.Errorf("统计 thumb_data 体积: %w", err)
	}

	if dryRun {
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM media_media WHERE thumb_data != ''`).Scan(&rep.ScannedRows); err != nil {
			return rep, fmt.Errorf("统计待迁移行数: %w", err)
		}
		rep.AfterBytes = rep.BeforeBytes // nothing changes
		rep.FreedBytes = 0
		return rep, nil
	}

	lastID := ""
	for {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		rows, err := db.QueryContext(ctx,
			`SELECT media_id, sha256, thumb_data FROM media_media
			 WHERE thumb_data != '' AND media_id > ? ORDER BY media_id LIMIT ?`,
			lastID, batch)
		if err != nil {
			return rep, fmt.Errorf("读取待迁移批次: %w", err)
		}
		type row struct {
			id, sha, thumb string
		}
		var chunk []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.sha, &r.thumb); err != nil {
				rows.Close()
				return rep, fmt.Errorf("扫描行: %w", err)
			}
			chunk = append(chunk, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return rep, fmt.Errorf("遍历批次: %w", err)
		}
		if len(chunk) == 0 {
			break
		}
		rep.ScannedRows += int64(len(chunk))

		// Process the chunk in one short transaction.
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return rep, fmt.Errorf("开启事务: %w", err)
		}
		upd, err := tx.PrepareContext(ctx,
			`UPDATE media_media SET thumb_path = ?, thumb_data = '' WHERE media_id = ?`)
		if err != nil {
			tx.Rollback()
			return rep, fmt.Errorf("准备更新: %w", err)
		}
		for _, r := range chunk {
			rel, perr := thumbRelPath(r.sha)
			if perr != nil {
				rep.Skipped++
				continue
			}
			// Decode only to validate the payload (a corrupt blob should not be
			// migrated into a file the reader would then choke on).
			if _, derr := decodeThumbData(r.thumb); derr != nil {
				rep.Skipped++
				continue
			}
			if werr := writeThumbFile(base, rel, []byte(r.thumb)); werr != nil {
				// Leave the row intact so a later run retries.
				rep.Skipped++
				continue
			}
			rep.Files++
			if _, uerr := upd.ExecContext(ctx, rel, r.id); uerr != nil {
				tx.Rollback()
				return rep, fmt.Errorf("更新行 %s: %w", r.id, uerr)
			}
			rep.Migrated++
		}
		if err := upd.Close(); err != nil {
			tx.Rollback()
			return rep, fmt.Errorf("关闭语句: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return rep, fmt.Errorf("提交批次: %w", err)
		}

		lastID = chunk[len(chunk)-1].id
		if len(chunk) < batch {
			break
		}
	}

	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(LENGTH(thumb_data)),0) FROM media_media WHERE thumb_data != ''`).
		Scan(&rep.AfterBytes); err != nil {
		return rep, fmt.Errorf("统计迁移后体积: %w", err)
	}
	rep.FreedBytes = rep.BeforeBytes - rep.AfterBytes
	return rep, nil
}

// thumbRelPath builds the media_dir-relative thumbnail path for sha, matching
// mediasvc.thumbPathFor: thumbs/<sha[:2]>/<sha[:4]>/<sha>.thumb.
func thumbRelPath(sha string) (string, error) {
	sha = strings.TrimSpace(sha)
	if len(sha) < 4 {
		return "", fmt.Errorf("short sha %q", sha)
	}
	for _, c := range sha {
		ok := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !ok {
			return "", fmt.Errorf("non-hex sha %q", sha)
		}
	}
	return filepath.Join("thumbs", sha[:2], sha[:4], sha+".thumb"), nil
}

// writeThumbFile writes payload to <mediaDir>/<rel> atomically, refusing any
// rel that would escape mediaDir (defense in depth — rel is derived from the
// row's sha, but never resolve it blindly). Content-addressed: an existing file
// is a no-op (identical bytes).
func writeThumbFile(mediaDir, rel string, payload []byte) error {
	abs, err := safeMediaPath(mediaDir, rel)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err == nil {
		return nil // already present
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".thumb-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, abs)
}

// safeMediaPath joins rel under mediaDir, refusing absolute paths or ".."
// components (the same contract as mediasvc.safeJoin).
func safeMediaPath(base, rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", fmt.Errorf("empty rel path")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute rel path %q", rel)
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == ".." {
			return "", fmt.Errorf("rel path escapes base: %q", rel)
		}
	}
	baseClean := filepath.Clean(base)
	joined := filepath.Join(baseClean, rel)
	if joined != baseClean && !strings.HasPrefix(joined, baseClean+string(filepath.Separator)) {
		return "", fmt.Errorf("rel path escapes base: %q", rel)
	}
	return joined, nil
}

// decodeThumbData validates an inline thumb_data value and returns the decoded
// image bytes. It tolerates a data: URI prefix and both padded/raw base64.
func decodeThumbData(v string) ([]byte, error) {
	raw := strings.TrimSpace(v)
	if raw == "" {
		return nil, fmt.Errorf("empty thumb_data")
	}
	if strings.HasPrefix(strings.ToLower(raw), "data:") {
		comma := strings.IndexByte(raw, ',')
		if comma < 0 {
			return nil, fmt.Errorf("malformed data URI")
		}
		raw = raw[comma+1:]
	}
	raw = strings.TrimSpace(raw)
	dec, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		dec, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(raw, "="))
		if err != nil {
			return nil, fmt.Errorf("invalid base64: %w", err)
		}
	}
	if len(dec) == 0 {
		return nil, fmt.Errorf("empty decoded payload")
	}
	return dec, nil
}

// resolveMediaDir resolves the media directory: flag → env → default ./data/media.
func resolveMediaDir(e *environment, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if v := e.getenv("UFOT_MEDIA_DIR"); v != "" {
		return v
	}
	return "./data/media"
}
