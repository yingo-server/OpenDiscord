package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"

	"github.com/yingo-server/OpenDiscord/server/internal/pkg/i18n"
)

// `upgrade` — one-shot maintenance plan for an existing deployment (LTS 1.4).
//
// It folds the two "manual, must-do-once" steps of a 1.0/1.1 → 1.4 upgrade
// into a single command with a dry-run-by-default plan:
//
//	1. legacy thumb_data migration  (reuses migrateThumbs — see migrate.go)
//	2. hot DB auto_vacuum conversion (PRAGMA auto_vacuum=INCREMENTAL; VACUUM;)
//
// Default (no --run) the command only READS the database and prints a
// structured plan (JSON with --json). Nothing is written, nothing is locked.
// With --run it performs the pending steps; auto_vacuum conversion requires
// the service to be stopped (VACUUM rewrites the whole DB file), enforced via
// `systemctl is-active` unless --force is given.

// upgradeStep is one planned/executed item in the upgrade plan.
type upgradeStep struct {
	ID      string `json:"id"`               // thumbs | auto_vacuum | backup
	Needed  bool   `json:"needed"`           // would this step do anything?
	Detail  string `json:"detail,omitempty"` // human-readable explanation
	Applied bool   `json:"applied"`          // executed for real (run mode)
	Err     string `json:"error,omitempty"`  // step failed (run mode)
}

type upgradePlan struct {
	DryRun     bool          `json:"dry_run"`
	HotDB      string        `json:"hot_db"`
	AutoVacuum int           `json:"auto_vacuum"`              // current PRAGMA value (0 = off)
	ServiceUp  bool          `json:"service_active,omitempty"` // unit state at plan time
	Steps      []upgradeStep `json:"steps"`
}

func cmdUpgrade(ctx context.Context, e *environment, g globals, args []string) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	run := fs.Bool("run", false, "执行计划（默认 dry-run，只读不改库）")
	force := fs.Bool("force", false, "跳过「服务必须已停止」检查（危险：仅离线维护窗口使用）")
	dbPath := fs.String("hot-db", "", "热库路径")
	mediaDir := fs.String("media-dir", "", "媒体目录（默认 OPENDISCORD_MEDIA_DIR / ./data/media）")
	batch := fs.Int("batch", 500, "thumbs 每批处理的行数")
	unit := fs.String("unit", defaultUnit, "systemd 单元名（停服检查用）")
	if err := parseFlags(fs, e, "upgrade", args, true); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usagef("upgrade: unexpected argument %q", fs.Arg(0))
	}
	if *batch <= 0 {
		return usagef("upgrade: --batch 必须 > 0")
	}

	path := hotDBPath(e, *dbPath)
	if _, err := e.stat(path); err != nil {
		return notFoundf("热库不存在: %s (%v)", path, err)
	}
	dir := resolveMediaDir(e, *mediaDir)

	// Dry-run stays read-only; a real run needs a writable handle.
	open := e.openDB
	if *run {
		open = e.openDBRW
	}
	db, err := open(path)
	if err != nil {
		return fmt.Errorf("打开热库 %s: %w", path, err)
	}
	defer db.Close()

	plan := upgradePlan{DryRun: !*run, HotDB: path}
	if err := db.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&plan.AutoVacuum); err != nil {
		return fmt.Errorf("读取 auto_vacuum: %w", err)
	}
	plan.ServiceUp = serviceActive(ctx, e, *unit)

	// --- plan steps -------------------------------------------------------

	// Step 1: legacy thumb_data migration. Count is cheap and read-only; it
	// comes free from the thumbs machinery (dry-run path of migrateThumbs).
	var thumbsRep thumbMigrateReport
	thumbsStep := upgradeStep{ID: "thumbs"}
	if n, cerr := countThumbsPending(ctx, db); cerr != nil {
		thumbsStep.Detail = fmt.Sprintf("无法统计待迁移行数: %v", cerr)
	} else {
		thumbsStep.Needed = n > 0
		thumbsStep.Detail = fmt.Sprintf("%d 行旧 thumb_data 待迁移到 %s", n, dir)
	}
	plan.Steps = append(plan.Steps, thumbsStep)

	// Step 2: auto_vacuum conversion.
	avStep := upgradeStep{ID: "auto_vacuum", Needed: plan.AutoVacuum == 0}
	if plan.AutoVacuum == 0 {
		avStep.Detail = "auto_vacuum=0 → 需转换为 INCREMENTAL（PRAGMA auto_vacuum=INCREMENTAL; VACUUM;）"
	} else {
		avStep.Detail = fmt.Sprintf("auto_vacuum=%d，无需转换", plan.AutoVacuum)
	}
	plan.Steps = append(plan.Steps, avStep)

	if *run {
		// 停服护栏：VACUUM 重写整库文件，运行中执行既慢又危险。
		if plan.ServiceUp && !*force {
			return deniedf("检测到 %s.service 正在运行：auto_vacuum 转换必须停服执行（sudo systemctl stop %s）。"+
				"确认服务已停止后重试，或离线维护窗口用 --force 跳过检查", *unit, *unit)
		}
		plan.DryRun = false
		if thumbsStep.Needed {
			thumbsRep, err = migrateThumbs(ctx, db, dir, false, *batch)
			if err != nil {
				thumbsStep.Err = err.Error()
			} else {
				thumbsStep.Applied = true
				thumbsStep.Detail = fmt.Sprintf("迁移 %d 行、写出 %d 个文件、释放 %s",
					thumbsRep.Migrated, thumbsRep.Files, humanBytes(uint64(thumbsRep.FreedBytes)))
			}
			plan.Steps[0] = thumbsStep
		}
		if avStep.Needed && thumbsStep.Err == "" {
			if _, err := db.ExecContext(ctx, `PRAGMA auto_vacuum=INCREMENTAL`); err != nil {
				avStep.Err = fmt.Sprintf("设置 auto_vacuum: %v", err)
			} else if _, err := db.ExecContext(ctx, `VACUUM`); err != nil {
				avStep.Err = fmt.Sprintf("VACUUM: %v", err)
			} else {
				avStep.Applied = true
			}
			plan.Steps[1] = avStep
		}
	}

	if g.JSON {
		return emitJSON(e, "upgrade", plan)
	}
	renderUpgrade(e, plan, thumbsRep)
	return nil
}

// countThumbsPending counts legacy rows cheaply (read-only).
func countThumbsPending(ctx context.Context, db *sql.DB) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM media_media WHERE thumb_data != ''`).Scan(&n)
	return n, err
}

// serviceActive reports whether the systemd unit is currently running.
// Unknown environments (no systemctl) count as NOT running — containers and
// dev boxes, where the caller is responsible for the stop anyway.
func serviceActive(ctx context.Context, e *environment, unit string) bool {
	out, err := e.runCmd(ctx, "systemctl", "is-active", "--quiet", unit)
	_ = out
	return err == nil
}

func renderUpgrade(e *environment, plan upgradePlan, thumbs thumbMigrateReport) {
	if i18n.StdinIsTTY() {
		e.printf("+--------------------------------------------------\n")
		defer e.printf("+--------------------------------------------------\n")
	}
	mode := "dry-run（未改库；加 --run 执行）"
	if !plan.DryRun {
		mode = "已执行"
	}
	e.printf("upgrade: %s\n", mode)
	e.printf("  hot_db      : %s (auto_vacuum=%d)\n", plan.HotDB, plan.AutoVacuum)
	if plan.ServiceUp {
		e.printf("  ⚠ 服务正在运行：执行前必须 systemctl stop\n")
	}
	for _, s := range plan.Steps {
		mark := "-"
		switch {
		case s.Err != "":
			mark = "✗"
		case s.Applied:
			mark = "✓"
		case s.Needed:
			mark = "!"
		}
		e.printf("  [%s] %-12s %s\n", mark, s.ID, s.Detail)
		if s.ID == "thumbs" && thumbs.Batch > 0 && !plan.DryRun {
			e.printf("        thumb_data: %s → %s (释放 %s, 跳过 %d)\n",
				humanBytes(uint64(thumbs.BeforeBytes)), humanBytes(uint64(thumbs.AfterBytes)),
				humanBytes(uint64(thumbs.FreedBytes)), thumbs.Skipped)
		}
	}
	e.printf("图例: ! 待执行  ✓ 已执行  - 无需  ✗ 失败\n")
}
