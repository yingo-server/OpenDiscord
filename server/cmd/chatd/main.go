// Command chatd is the embedded-mode assembly of UFOT: one process, four
// layers wired through the in-process transport, plus the user authentication
// HTTP API and the management frame injector.
//
// This file only parses arguments, owns the signal loop and maps exit codes.
// All wiring lives in assemble.go / wire.go / layers.go / admin.go / config.go
// (docs/architecture.md: "main.go 只做接线").
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"

	"github.com/yingo-server/UFOT/server/internal/pkg/i18n"

	"github.com/yingo-server/UFOT/server/internal/pkg/exitcodes"
	"github.com/yingo-server/UFOT/server/internal/pkg/log"
	"github.com/yingo-server/UFOT/server/internal/pkg/signalset"
)

const usage = `chatd — UFOT embedded server

usage: chatd [--config PATH] [--tui] [--lang zh-CN|en]   (default config: config/chatd.toml)

  --tui           full-screen ANSI dashboard, 2s refresh, hotkeys q/r/d
  --lang          UI language for the TUI (zh-CN / en, default auto)

signals:
  SIGTERM/SIGINT  graceful drain: usergw -> data pipe -> bridge, exit 0
  SIGHUP          reload policy.json + re-publish feature flags
  SIGUSR1         status dump (one JSON line on stdout)

exit codes: 0 ok, 1 config invalid, 2 assembly failed, 75 listen/transient`

// stty helpers are overridable in tests (no real terminal in CI).
var (
	lookStty = func() (string, error) { return exec.LookPath("stty") }
	sttyCmd  = func(name string, args ...string) *exec.Cmd { return exec.Command(name, args...) }
)

func main() { os.Exit(run()) }

func run() int {
	cfgPath := "config/chatd.toml"
	tui := false
	nonInteractive := false
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config", "-config", "-c":
			if i+1 < len(args) {
				cfgPath = args[i+1]
				i++
			}
		case "--tui":
			tui = true
		case "--non-interactive":
			// Dual mode: non-TTY (CI/pipes) already implies CLI mode; this
			// flag additionally forces it on a TTY. Flags are the truth.
			nonInteractive = true
		case "--lang":
			if i+1 < len(args) {
				i18n.SetLang(i18n.DetectLang(args[i+1]))
				i++
			}
		case "--help", "-h":
			fmt.Fprintln(os.Stdout, usage)
			return exitcodes.OK
		default:
			fmt.Fprintf(os.Stderr, "chatd: unknown argument %q\n%s\n", args[i], usage)
			return exitcodes.ConfigInvalid
		}
	}

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "chatd: config:", err)
		return exitcodes.ConfigInvalid
	}
	setLogLevel(cfg.LogLevel)
	lg := log.For("chatd")
	lg.Info("starting", "mode", cfg.Mode, "config", cfgPath, "listen", cfg.Listen, "dev_otp", cfg.DevOTP)

	if cfg.Mode != "" && cfg.Mode != "embedded" {
		fmt.Fprintln(os.Stderr, "chatd: unsupported mode (this build is embedded-only):", cfg.Mode)
		return exitcodes.ConfigInvalid
	}

	a, err := newAssembly(cfg, lg)
	if err != nil {
		lg.Error("assembly failed", "err", err.Error())
		return exitcodes.AssemblyFailed
	}
	defer a.close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// chatd is the single signal owner: the bridge supervisor was handed an
	// empty channel so the ordered cross-layer drain below always wins.
	sigCh, stop := signalset.NotifyAll(ctx)
	defer stop()
	var once sync.Once
	quit := func() {
		once.Do(func() {
			a.shutdown()
			cancel()
		})
	}
	watch := signalset.NewSet()
	watch.Watch(ctx, sigCh, func(ev signalset.SignalEvent) {
		switch ev.Semantics {
		case signalset.Quit:
			lg.Info("signal received: draining", "signal", fmt.Sprint(ev.Signal))
			quit()
		case signalset.Reload:
			lg.Info("signal received: reload", "signal", fmt.Sprint(ev.Signal))
			a.reload(ctx)
		case signalset.Snapshot:
			lg.Info("signal received: status dump", "signal", fmt.Sprint(ev.Signal))
			a.dump(os.Stdout)
		}
	})

	// TUI mode: replace the plain blocking run with the dashboard loop; the
	// hotkeys drive the SAME quit/reload/dump callbacks as the signals above.
	if tui {
		if nonInteractive {
			fmt.Fprintln(os.Stderr, "chatd: --tui conflicts with --non-interactive")
			return exitcodes.ConfigInvalid
		}
		if !i18n.StdinIsTTY() {
			// Dual-mode rule: no TTY → no dashboard; behave exactly as
			// without --tui (systemd/CI safe).
			tui = false
		}
	}
	if tui {
		go func() { _ = a.run(ctx) }()
		return runTUI(ctx, a, quit, func() { a.reload(ctx) }, func() { a.dump(os.Stdout) })
	}

	runErr := a.run(ctx)
	// quit() is idempotent AND blocks while an in-progress drain finishes
	// (sync.Once), so the process never exits before "drain complete" — the
	// final SQLite group commit must land before the DB handles are closed.
	quit()
	if runErr != nil {
		lg.Error("chatd stopped with error", "err", runErr.Error())
		return exitcodes.TempFail
	}
	lg.Info("chatd stopped")
	return exitcodes.OK
}

// setLogLevel maps the config string onto the process-wide level.
func setLogLevel(s string) {
	switch s {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "warn":
		log.SetLevel(log.WarnLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	default:
		log.SetLevel(log.InfoLevel)
	}
}
