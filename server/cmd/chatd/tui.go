package main

// chatd --tui: a dependency-free ANSI dashboard drawn from the SAME internal
// state collector as the SIGUSR1 status dump (assembly.dump) — the TUI is a
// renderer over that snapshot, never a second source of truth.
//
// Flags remain the source of truth for configuration; the TUI only observes
// runtime state and maps hotkeys onto the same actions as signals:
//   q → graceful drain (same path as SIGTERM), r → reload (SIGHUP),
//   d → status dump (SIGUSR1).

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/yingo-server/UFOT/server/internal/pkg/i18n"
)

const tuiRefreshInterval = 2 * time.Second

// version is overridable with -ldflags "-X main.version=…".
var version = "dev"

// tuiSnapshot is the rendered projection of assembly.dump's JSON output.
type tuiSnapshot struct {
	Version    string
	Uptime     string
	Now        string
	Conns      any
	PolicyVer  any
	FlagsVer   any
	Mode       string
	Listen     any
	AdminAddr  any
	Goroutines int
	Err        string
}

// renderTUI draws one frame. Kept pure (state in, bytes out) so it is
// testable without a pty: tests assert on the returned frame text.
func renderTUI(w io.Writer, snap tuiSnapshot) {
	var b strings.Builder
	b.WriteString("\033[2J\033[H") // clear screen, cursor home
	b.WriteString("═══ UFOT chatd ════════════════════════════════════════\n")
	fmt.Fprintf(&b, " %s: %s   %s: %s   %s: %s\n",
		i18n.T("chatd.tui.version"), snap.Version,
		i18n.T("chatd.tui.uptime"), snap.Uptime,
		i18n.T("chatd.tui.now"), snap.Now)
	b.WriteString("──────────────────────────────────────────────────────────\n")
	fmt.Fprintf(&b, " %s: conns=%v  %s=%v\n",
		i18n.T("chatd.tui.health"), snap.Conns, i18n.T("chatd.tui.db"), healthLabel(snap))
	fmt.Fprintf(&b, " %s: %v   flags=%v\n",
		i18n.T("chatd.tui.policy"), snap.PolicyVer, snap.FlagsVer)
	fmt.Fprintf(&b, " mode=%s  listen=%v  admin=%v  goroutines=%d\n",
		snap.Mode, snap.Listen, snap.AdminAddr, snap.Goroutines)
	if snap.Err != "" {
		fmt.Fprintf(&b, " \033[31m✗ %s\033[0m\n", snap.Err)
	}
	b.WriteString("──────────────────────────────────────────────────────────\n")
	b.WriteString(" " + i18n.T("chatd.tui.hotkeys") + "\n")
	io.WriteString(w, b.String())
}

func healthLabel(s tuiSnapshot) string {
	if s.Err != "" {
		return i18n.T("chatd.tui.err")
	}
	return i18n.T("chatd.tui.ok")
}

// snapshotToTUI converts the raw dump JSON map into the render model.
func snapshotToTUI(version string, raw []byte) tuiSnapshot {
	var env struct {
		Status map[string]any `json:"status"`
	}
	s := tuiSnapshot{Version: version}
	if err := json.Unmarshal(raw, &env); err != nil {
		s.Err = err.Error()
		return s
	}
	get := func(k string) any { return env.Status[k] }
	s.Uptime = fmt.Sprintf("%v", get("uptime_ms"))
	if ms, ok := get("uptime_ms").(float64); ok {
		s.Uptime = (time.Duration(ms) * time.Millisecond).Round(time.Second).String()
	}
	s.Now = time.Now().Format("2006-01-02 15:04:05")
	s.Conns = get("conns")
	s.PolicyVer = get("policy_version")
	s.FlagsVer = get("flags_ver")
	s.Mode, _ = get("mode").(string)
	s.Listen = get("listen")
	s.AdminAddr = get("admin_addr")
	if g, ok := get("goroutines").(float64); ok {
		s.Goroutines = int(g)
	}
	return s
}

// collectSnapshot reuses the existing dump collector (assembly.dump) via a
// pipe — no duplicated state gathering.
func (a *assembly) collectSnapshot() tuiSnapshot {
	pr, pw, err := os.Pipe()
	if err != nil {
		return tuiSnapshot{Err: err.Error(), Version: version}
	}
	done := make(chan struct{})
	var buf strings.Builder
	go func() {
		_, _ = io.Copy(&buf, pr)
		close(done)
	}()
	a.dump(pw)
	_ = pw.Close()
	<-done
	_ = pr.Close()
	return snapshotToTUI(version, []byte(buf.String()))
}

// runTUI renders every 2s and reads hotkeys until q. quit/reload/dump are the
// same callbacks the signal loop uses (graceful drain on q → exit 0).
func runTUI(ctx context.Context, a *assembly, quit, reload func(), dump func()) int {
	restore := enableRawMode()
	defer restore()

	frames := make(chan tuiSnapshot, 1)
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(tuiRefreshInterval)
		defer t.Stop()
		frames <- a.collectSnapshot()
		for {
			select {
			case <-t.C:
				select {
				case frames <- a.collectSnapshot():
				default:
				}
			case <-stop:
				return
			}
		}
	}()
	defer close(stop)

	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			switch strings.TrimSpace(sc.Text()) {
			case "r":
				reload()
			case "d":
				dump()
			case "q":
				quit()
				return
			}
		}
	}()

	renderTUI(os.Stdout, a.collectSnapshot())
	for {
		select {
		case <-ctx.Done():
			fmt.Println("\n" + i18n.T("chatd.tui.quit_drain"))
			return 0
		case s := <-frames:
			renderTUI(os.Stdout, s)
		}
	}
}

// enableRawMode puts the terminal in cbreak mode (best-effort, `stty`), so a
// single keypress is visible without Enter; stdin that is not a tty (tests)
// is left untouched.
func enableRawMode() func() {
	stty, err := lookStty()
	if err != nil {
		return func() {}
	}
	f := os.Stdin
	off := sttyCmd(stty, "raw", "-echo")
	off.Stdin = f
	if off.Run() != nil {
		return func() {}
	}
	on := sttyCmd(stty, "sane")
	on.Stdin = f
	return func() { _ = on.Run() }
}

// sortedKeys is used by tests to assert stable rendering.
func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
