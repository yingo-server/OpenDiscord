package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yingo-server/UFOT/server/internal/pkg/i18n"
)

// Check statuses (doctor vocabulary).
const (
	checkOK   = "ok"
	checkWarn = "warn"
	checkFail = "fail"
	checkSkip = "skip"
)

type check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type doctorReport struct {
	OK     bool    `json:"ok"`
	Checks []check `json:"checks"`
}

// Thresholds are constants of the mechanism, not policy knobs (they are about
// the machine being able to keep serving, not about product behaviour).
const (
	diskFailBelow = 256 << 20 // 256 MiB free is a hard fail
	diskWarnBelow = 1 << 30   // 1 GiB free is a warning
	logFailAbove  = 100       // >100 error lines/window is a fail
	logWarnAbove  = 0         // any error line is a warning
)

func cmdDoctor(ctx context.Context, e *environment, g globals, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	hotPath := fs.String("hot-db", "", "hot SQLite path")
	unit := fs.String("unit", defaultUnit, "systemd unit for log scoping")
	since := fs.String("since", "-1h", "journal time window")
	noLogs := fs.Bool("no-logs", false, "skip the journald error check")
	noCold := fs.Bool("no-cold", false, "skip the cold DB connectivity check")
	if err := parseFlags(fs, e, "doctor", args, false); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usagef("doctor: unexpected argument %q", fs.Arg(0))
	}

	path := hotDBPath(e, *hotPath)
	checks := make([]check, 0, 8)
	checks = append(checks, checkHotDB(e, path)...)
	checks = append(checks, checkDisk(e, path))
	if *noLogs {
		checks = append(checks, check{Name: "logs.error", Status: checkSkip, Detail: "--no-logs"})
	} else {
		checks = append(checks, checkLogs(ctx, e, *unit, *since))
	}
	if *noCold {
		checks = append(checks, check{Name: "cold_db", Status: checkSkip, Detail: "--no-cold"})
	} else {
		checks = append(checks, checkCold(e))
	}
	checks = append(checks, checkAuditStore(e))

	rep := doctorReport{OK: true, Checks: checks}
	fails, warns := 0, 0
	for _, c := range checks {
		switch c.Status {
		case checkFail:
			rep.OK = false
			fails++
		case checkWarn:
			warns++
		}
	}

	if g.JSON {
		if err := emitJSON(e, "doctor", rep); err != nil {
			return err
		}
	} else {
		renderDoctor(e, rep, fails, warns)
	}
	if !rep.OK {
		return fmt.Errorf("doctor: %d 项检查失败（%d warn）", fails, warns)
	}
	return nil
}

func renderDoctor(e *environment, rep doctorReport, fails, warns int) {
	// TTY → colored ✓/⚠/✗; non-TTY (pipe/CI) → plain text (existing format).
	colored := i18n.StdinIsTTY() && e.getenv("NO_COLOR_DISABLED") == ""
	sym := map[string]string{checkOK: "ok", checkWarn: "warn", checkFail: "fail", checkSkip: "skip"}
	if colored {
		sym = map[string]string{
			checkOK: "\033[32m✓\033[0m", checkWarn: "\033[33m⚠\033[0m",
			checkFail: "\033[31m✗\033[0m", checkSkip: "-",
		}
	}
	e.printf("doctor: %d checks\n", len(rep.Checks))
	for _, c := range rep.Checks {
		e.printf("  [%s] %-18s %s\n", sym[c.Status], c.Name, c.Detail)
	}
	switch {
	case fails > 0:
		e.printf("结果: FAIL (%d fail, %d warn)\n", fails, warns)
	case warns > 0:
		e.printf("结果: OK (%d warn)\n", warns)
	default:
		e.printf("结果: OK\n")
	}
}

// checkHotDB returns the file + integrity checks for the hot database.
func checkHotDB(e *environment, path string) []check {
	fi, err := e.stat(path)
	if err != nil {
		return []check{{Name: "hot_db.file", Status: checkFail, Detail: path + ": " + err.Error()}}
	}
	checks := []check{{
		Name:   "hot_db.file",
		Status: checkOK,
		Detail: path + " (" + humanBytes(uint64(fi.Size())) + ")",
	}}
	db, err := e.openDB(path)
	if err != nil {
		return append(checks, check{Name: "hot_db.integrity", Status: checkFail, Detail: err.Error()})
	}
	defer db.Close()

	var out string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&out); err != nil {
		return append(checks, check{Name: "hot_db.integrity", Status: checkFail, Detail: err.Error()})
	}
	st, detail := evalIntegrity(out)
	checks = append(checks, check{Name: "hot_db.integrity", Status: st, Detail: detail})

	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err == nil {
		mst := checkOK
		if !strings.EqualFold(mode, "wal") {
			mst = checkWarn // docs require WAL: readers must not block the writer
		}
		checks = append(checks, check{Name: "hot_db.journal", Status: mst, Detail: mode})
	}
	return checks
}

// evalIntegrity interprets PRAGMA integrity_check output. Pure.
func evalIntegrity(out string) (status, detail string) {
	s := strings.TrimSpace(out)
	if s == "" {
		return checkWarn, "integrity_check 输出为空"
	}
	if strings.EqualFold(s, "ok") {
		return checkOK, "ok"
	}
	lines := strings.Split(s, "\n")
	first := strings.TrimSpace(lines[0])
	if len(lines) > 1 {
		first += " (+" + strconv.Itoa(len(lines)-1) + " more)"
	}
	return checkFail, first
}

// checkDisk evaluates free space on the filesystem holding the hot DB.
func checkDisk(e *environment, path string) check {
	dir := filepath.Dir(path)
	free, total, err := e.diskFree(dir)
	if err != nil {
		return check{Name: "disk", Status: checkWarn, Detail: "statfs " + dir + ": " + err.Error()}
	}
	st, reason := evalDiskFree(free, total)
	return check{
		Name:   "disk",
		Status: st,
		Detail: dir + ": free " + humanBytes(free) + " / " + humanBytes(total) + " (" + reason + ")",
	}
}

// evalDiskFree applies the thresholds. Pure.
func evalDiskFree(free, total uint64) (status, reason string) {
	switch {
	case free < diskFailBelow:
		return checkFail, "free < 256 MiB"
	case free < diskWarnBelow:
		return checkWarn, "free < 1 GiB"
	case total > 0 && free*100/total < 5:
		return checkWarn, "free < 5% of the filesystem"
	default:
		return checkOK, "ok"
	}
}

// checkLogs counts journald error lines in the window (docs: journalctl -p err
// --since -1h | wc -l).
func checkLogs(ctx context.Context, e *environment, unit, since string) check {
	args := []string{"-p", "err", "--since", since, "--no-pager", "-q"}
	if unit != "" {
		args = append(args, "-u", unit)
	}
	out, err := e.runCmd(ctx, "journalctl", args...)
	if err != nil && len(out) == 0 {
		return check{Name: "logs.error", Status: checkSkip, Detail: "journalctl 不可用: " + err.Error()}
	}
	n := countLines(out)
	detail := strconv.Itoa(n) + " error lines (" + since + ")"
	switch {
	case n > logFailAbove:
		return check{Name: "logs.error", Status: checkFail, Detail: detail}
	case n > logWarnAbove:
		return check{Name: "logs.error", Status: checkWarn, Detail: detail}
	default:
		return check{Name: "logs.error", Status: checkOK, Detail: detail}
	}
}

// countLines counts non-empty lines (journalctl -q still emits blanks).
func countLines(b []byte) int {
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// checkCold does a TCP (or unix-socket) reachability probe of the cold DB. The
// CLI deliberately links no postgres driver: reachability is what an operator
// can verify without credentials.
func checkCold(e *environment) check {
	dsn := strings.TrimSpace(e.getenv("UFOT_COLD_DSN"))
	if dsn == "" {
		return check{
			Name:   "cold_db",
			Status: checkSkip,
			Detail: "未配置 UFOT_COLD_DSN（降级模式：audit/archive 读端口不可用）",
		}
	}
	addr, network, ok := parsePGAddr(dsn)
	if !ok {
		return check{Name: "cold_db", Status: checkWarn, Detail: "无法从 DSN 解析 host:port"}
	}
	conn, err := e.dial(network, addr, 2*time.Second)
	if err != nil {
		return check{Name: "cold_db", Status: checkFail, Detail: addr + ": " + err.Error()}
	}
	_ = conn.Close()
	return check{Name: "cold_db", Status: checkOK, Detail: addr + " 可达"}
}

// checkAuditStore verifies the audit journal has a durable backing store. The
// audit store lives in the cold DB (PostgreSQL); without it audit events are
// not persisted. Empty UFOT_COLD_DSN ⇒ degraded (WARN): the process runs
// but audits are lost. A configured DSN is probed for TCP reachability.
func checkAuditStore(e *environment) check {
	dsn := strings.TrimSpace(e.getenv("UFOT_COLD_DSN"))
	if dsn == "" {
		return check{
			Name:   "audit_store",
			Status: checkWarn,
			Detail: "degraded: 未配置 UFOT_COLD_DSN，audit 事件不会持久化",
		}
	}
	addr, network, ok := parsePGAddr(dsn)
	if !ok {
		return check{Name: "audit_store", Status: checkWarn, Detail: "degraded: 无法从 UFOT_COLD_DSN 解析 host:port"}
	}
	conn, err := e.dial(network, addr, 2*time.Second)
	if err != nil {
		return check{Name: "audit_store", Status: checkFail, Detail: addr + ": " + err.Error()}
	}
	if conn != nil {
		_ = conn.Close()
	}
	return check{Name: "audit_store", Status: checkOK, Detail: addr + " 可达"}
}

// parsePGAddr extracts a dialable address from a PostgreSQL DSN.
//
// Supported forms:
//
//	postgres://user:pass@host:5432/db?sslmode=disable
//	postgresql://host/db
//	host=127.0.0.1 port=5432 user=ufot dbname=ufot
//	host=/var/run/postgresql dbname=ufot      (unix socket)
func parsePGAddr(dsn string) (addr, network string, ok bool) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return "", "", false
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", "", false
		}
		host := u.Hostname()
		if host == "" {
			return "", "", false
		}
		port := u.Port()
		if port == "" {
			port = "5432"
		}
		return net.JoinHostPort(host, port), "tcp", true
	}
	var host, port string
	for _, f := range strings.Fields(dsn) {
		k, v, has := strings.Cut(f, "=")
		if !has {
			continue
		}
		switch k {
		case "host", "hostaddr":
			host = v
		case "port":
			port = v
		}
	}
	if host == "" {
		return "", "", false
	}
	if strings.HasPrefix(host, "/") {
		return host, "unix", true
	}
	if port == "" {
		port = "5432"
	}
	return net.JoinHostPort(host, port), "tcp", true
}
