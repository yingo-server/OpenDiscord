package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// backupReport is the `backup` payload.
type backupReport struct {
	Mode       string `json:"mode"` // script | builtin
	DryRun     bool   `json:"dry_run"`
	Script     string `json:"script,omitempty"`
	DB         string `json:"db,omitempty"`
	SizeBytes  int64  `json:"size_bytes,omitempty"`
	Target     string `json:"target,omitempty"`
	Output     string `json:"output,omitempty"`
	Encryption string `json:"encryption_hint,omitempty"`
}

func cmdBackup(ctx context.Context, e *environment, g globals, args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "只校验环境，不写文件")
	dbPath := fs.String("db", "", "热库路径")
	script := fs.String("script", "", "备份脚本路径")
	to := fs.String("to", "", "备份目录（内置模式）")
	if err := parseFlags(fs, e, "backup", args, false); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usagef("backup: unexpected argument %q", fs.Arg(0))
	}

	rep := backupReport{DryRun: *dry}

	// Preferred path: the deployment's own script (policy-aware, encrypts,
	// ships off-site). deploy/backup.sh is the canonical one.
	scriptPath, err := findBackupScript(e, *script)
	if err != nil {
		return err
	}
	if scriptPath != "" {
		rep.Mode, rep.Script = "script", scriptPath
		argv := []string{scriptPath}
		if *dry {
			argv = append(argv, "--dry-run")
		}
		out, runErr := e.runCmd(ctx, "bash", argv...)
		rep.Output = strings.TrimRight(string(out), "\n")
		if g.JSON {
			if err := emitJSON(e, "backup", rep); err != nil {
				return err
			}
		} else {
			e.printf("%s\n", rep.Output)
		}
		if runErr != nil {
			return fmt.Errorf("备份脚本失败: %w", runErr)
		}
		return nil
	}

	// Built-in fallback: sqlite online backup + an explicit encryption hint.
	rep.Mode = "builtin"
	path := hotDBPath(e, *dbPath)
	fi, statErr := e.stat(path)
	if statErr != nil {
		return notFoundf("热库不存在: %s (%v)", path, statErr)
	}
	dir := *to
	if dir == "" {
		dir = e.getenv("UFOT_BACKUP_DIR")
	}
	if dir == "" {
		dir = "/var/lib/ufot/backup"
	}
	dst := filepath.Join(dir, "ufot-"+e.now().UTC().Format("20060102T150405Z")+".db")
	rep.DB, rep.Target, rep.SizeBytes = path, dst, fi.Size()
	rep.Encryption = "建议加密后异地存放：age -r <recipient> -o " + dst + ".age " + dst +
		" | openssl enc -aes-256-cbc -pbkdf2 -iter 200000 -salt -in " + dst + " -out " + dst + ".enc"

	if *dry {
		if g.JSON {
			return emitJSON(e, "backup", rep)
		}
		e.printf("backup dry-run（内置模式）\n")
		e.printf("  db      : %s (%s)\n", rep.DB, humanBytes(uint64(rep.SizeBytes)))
		e.printf("  target  : %s\n", rep.Target)
		e.printf("  %s\n", rep.Encryption)
		return nil
	}
	if mkErr := mkdirAll(dir); mkErr != nil {
		return fmt.Errorf("创建备份目录 %s: %w", dir, mkErr)
	}
	if err := e.backupDB(path, dst); err != nil {
		return fmt.Errorf("sqlite 备份失败: %w", err)
	}
	if g.JSON {
		return emitJSON(e, "backup", rep)
	}
	e.printf("backup ok\n")
	e.printf("  db      : %s (%s)\n", rep.DB, humanBytes(uint64(rep.SizeBytes)))
	e.printf("  target  : %s\n", rep.Target)
	e.printf("  %s\n", rep.Encryption)
	return nil
}

// findBackupScript resolves the backup script. An explicitly requested script
// must exist (a typo must not silently fall back to the built-in path).
func findBackupScript(e *environment, explicit string) (string, error) {
	if explicit != "" {
		if _, err := e.stat(explicit); err != nil {
			return "", notFoundf("备份脚本不存在: %s (%v)", explicit, err)
		}
		return explicit, nil
	}
	for _, c := range []string{
		e.getenv("UFOT_BACKUP_SCRIPT"),
		"deploy/backup.sh",
		"/usr/local/lib/ufot/backup.sh",
		"/usr/share/ufot/backup.sh",
	} {
		if c == "" {
			continue
		}
		if _, err := e.stat(c); err == nil {
			return c, nil
		}
	}
	return "", nil
}

// mkdirAll is a package var so tests can run without touching the filesystem.
var mkdirAll = func(dir string) error { return os.MkdirAll(dir, 0o700) }

// backupSQLiteDefault copies the database using the sqlite3 CLI when present
// (.backup is online-safe even while chatd is writing) and VACUUM INTO
// otherwise.
func backupSQLiteDefault(src, dst string) error {
	var cliErr error
	if bin, err := exec.LookPath("sqlite3"); err == nil {
		out, err := exec.Command(bin, src, ".backup "+sqliteQuote(dst)).CombinedOutput()
		if err == nil {
			return nil
		}
		cliErr = fmt.Errorf("sqlite3 .backup: %v: %s", err, strings.TrimSpace(string(out)))
	}
	db, err := sql.Open("sqlite3", "file:"+src+"?_busy_timeout=5000")
	if err != nil {
		return fmt.Errorf("%v; open: %w", cliErr, err)
	}
	defer db.Close()
	if _, err := db.Exec("VACUUM INTO ?", dst); err != nil {
		if cliErr != nil {
			return fmt.Errorf("%v; vacuum into: %w", cliErr, err)
		}
		return fmt.Errorf("vacuum into: %w", err)
	}
	return nil
}

// sqliteQuote quotes a string literal for the sqlite3 CLI dot-command.
func sqliteQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
