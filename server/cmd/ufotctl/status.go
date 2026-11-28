package main

import (
	"context"
	"database/sql"
	"flag"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yingo-server/UFOT/server/internal/pkg/i18n"
)

// statusReport is the `status` payload. Field names double as the JSON keys.
type statusReport struct {
	Unit       string   `json:"unit"`
	State      string   `json:"state"`
	Active     bool     `json:"active"`
	PID        int      `json:"pid,omitempty"`
	Signal     string   `json:"signal,omitempty"`
	SignalSent bool     `json:"signal_sent,omitempty"`
	SignErr    string   `json:"signal_error,omitempty"`
	Dump       string   `json:"dump,omitempty"`
	HotDB      hotStats `json:"hot_db"`
}

// hotStats is the read-only hot database summary.
type hotStats struct {
	Path          string `json:"path"`
	Exists        bool   `json:"exists"`
	SizeBytes     int64  `json:"size_bytes"`
	Messages      int64  `json:"messages"`
	LastMessageAt int64  `json:"last_message_at,omitempty"`
	Error         string `json:"error,omitempty"`
}

func cmdStatus(ctx context.Context, e *environment, g globals, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	unit := fs.String("unit", defaultUnit, "systemd unit")
	hotPath := fs.String("hot-db", "", "hot SQLite path")
	sigName := fs.String("signal", "usr1", "signal to send: usr1|usr2|none")
	noJournal := fs.Bool("no-journal", false, "skip journalctl")
	journalLines := fs.Int("journal-lines", 50, "journal lines to scan")
	if err := parseFlags(fs, e, "status", args, false); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usagef("status: unexpected argument %q", fs.Arg(0))
	}

	sig, sigLabel, err := parseSignal(*sigName)
	if err != nil {
		return err
	}
	rep := statusReport{Unit: *unit, Signal: sigLabel}

	// 1. unit state (a non-zero exit from `systemctl is-active` is normal)
	out, _ := e.runCmd(ctx, "systemctl", "is-active", *unit)
	rep.State = firstLine(string(out))
	if rep.State == "" {
		rep.State = "unknown"
	}
	rep.Active = rep.State == "active"

	// 2. SIGUSR1 dump (signalset: SIGUSR1 = Snapshot, SIGUSR2 = Custom)
	if rep.Active && sig != 0 {
		pidOut, _ := e.runCmd(ctx, "systemctl", "show", "-p", "MainPID", "--value", *unit)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(pidOut)))
		if pid > 0 {
			rep.PID = pid
			if kerr := e.kill(pid, sig); kerr != nil {
				rep.SignErr = kerr.Error()
			} else {
				rep.SignalSent = true
				// Give the process a beat to emit its dump before reading the
				// journal; the read still works if the line lands later.
				e.sleep(150 * time.Millisecond)
			}
		}
	}

	// 3. last dump line from the journal
	if !*noJournal {
		rep.Dump = lastDumpLine(ctx, e, *unit, *journalLines)
	}

	// 4. hot.db statistics (read-only)
	rep.HotDB = readHotStats(e, hotDBPath(e, *hotPath))

	if g.JSON {
		return emitJSON(e, "status", rep)
	}
	renderStatus(e, rep)
	return nil
}

func renderStatus(e *environment, rep statusReport) {
	// TTY → ASCII table frame; non-TTY (pipes/CI) → plain text as before.
	if i18n.StdinIsTTY() {
		e.printf("+--------------------------------------------------\n")
		defer e.printf("+--------------------------------------------------\n")
	}
	e.printf("unit        : %s\n", rep.Unit)
	state := rep.State
	if rep.PID > 0 {
		state += " (pid " + itoaI(int64(rep.PID)) + ")"
	}
	e.printf("state       : %s\n", state)
	if rep.Signal != "" {
		e.printf("signal      : %s", rep.Signal)
		switch {
		case rep.SignErr != "":
			e.printf(" (发送失败: %s)", rep.SignErr)
		case rep.SignalSent:
			e.printf(" (已发送)")
		case !rep.Active:
			e.printf(" (未发送: unit 未运行)")
		default:
			e.printf(" (未发送: 无 MainPID)")
		}
		e.printf("\n")
	}
	if rep.Dump != "" {
		e.printf("dump        : %s\n", rep.Dump)
	} else {
		e.printf("dump        : (journal 中未找到 dump 行)\n")
	}
	e.printf("hot.db      : %s", rep.HotDB.Path)
	if rep.HotDB.Exists {
		e.printf("  %s\n", humanBytes(uint64(rep.HotDB.SizeBytes)))
		e.printf("messages    : %d", rep.HotDB.Messages)
		if rep.HotDB.LastMessageAt > 0 {
			e.printf(" (last %s)", time.Unix(rep.HotDB.LastMessageAt, 0).UTC().Format(time.RFC3339))
		}
		e.printf("\n")
	} else {
		e.printf("  (不存在)\n")
	}
	if rep.HotDB.Error != "" {
		e.printf("hot.db err  : %s\n", rep.HotDB.Error)
	}
}

// parseSignal maps the CLI spelling onto a signal number.
// SIGUSR1 is the Snapshot/dump trigger in pkg/signalset's default mapping;
// SIGUSR2 is the emergency/custom one. "none" skips signalling entirely.
func parseSignal(name string) (syscall.Signal, string, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "usr1", "sigusr1", "1":
		return syscall.SIGUSR1, "SIGUSR1", nil
	case "usr2", "sigusr2", "2":
		return syscall.SIGUSR2, "SIGUSR2", nil
	case "none", "off", "0":
		return 0, "", nil
	default:
		return 0, "", usagef("invalid --signal %q (want usr1|usr2|none)", name)
	}
}

// lastDumpLine reads the unit's journal and returns the last line that looks
// like a dump summary. Falls back to "<unit>-bridge" because split deployments
// run the bridge as its own unit and that is where the dump is emitted.
func lastDumpLine(ctx context.Context, e *environment, unit string, lines int) string {
	if lines <= 0 {
		lines = 50
	}
	units := []string{unit}
	if !strings.HasSuffix(unit, "-bridge") {
		units = append(units, unit+"-bridge")
	}
	for _, u := range units {
		out, _ := e.runCmd(ctx, "journalctl", "-u", u,
			"-n", strconv.Itoa(lines), "--no-pager", "-o", "cat")
		if line := pickDumpLine(string(out)); line != "" {
			return line
		}
	}
	return ""
}

// pickDumpLine returns the LAST line mentioning "dump" (case-insensitive).
func pickDumpLine(journal string) string {
	var found string
	for _, line := range strings.Split(journal, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.Contains(strings.ToLower(line), "dump") {
			found = line
		}
	}
	return found
}

// readHotStats opens the hot DB read-only and collects counters. Every failure
// is reported in hotStats.Error — status never dies because of the database.
func readHotStats(e *environment, path string) hotStats {
	st := hotStats{Path: path}
	fi, err := e.stat(path)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Exists = true
	st.SizeBytes = fi.Size()

	db, err := e.openDB(path)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	defer db.Close()
	if err := db.QueryRow("SELECT COUNT(*) FROM msg_messages_hot").Scan(&st.Messages); err != nil {
		st.Error = err.Error()
		return st
	}
	var last sql.NullInt64
	if err := db.QueryRow("SELECT MAX(sent_at) FROM msg_messages_hot").Scan(&last); err == nil {
		st.LastMessageAt = last.Int64
	}
	return st
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
