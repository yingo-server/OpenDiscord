// Command ufotctl is the SYSTEM-LEVEL operations CLI for UFOT.
//
// Division of labour (docs/admin-data-design.md §7):
//
//	adminctl      speaks the management REST API (users/reports/flags/…)
//	ufotctl   speaks to the LOCAL machine: systemd unit state, signals,
//	              the hot SQLite file, backups, doctor checks. It never opens a
//	              new network port and never bypasses the admin API for
//	              management actions — the few mutating commands (archive, flags,
//	              ban/unban) go through the same loopback REST API as adminctl.
//
// Exit codes (sysexits subset, matching adminctl):
//
//	0   ok
//	1   error / doctor found a FAIL
//	64  usage
//	65  target not found (REST 404 / missing local path) — admin-data §7
//	77  permission denied or admin token not configured (run `adminctl login`)
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3" // hot.db read-only diagnostics

	"github.com/yingo-server/UFOT/server/internal/pkg/i18n"
)

// Exit codes.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 64
	// exitNotFound: the referenced target does not exist — e.g. a REST call
	// answered 404, or a local path (hot db, backup script) is missing.
	// Aligned with adminctl (docs/admin-data-design.md §7).
	exitNotFound = 65
	exitDenied   = 77
)

// Build metadata, overridable with -ldflags "-X main.version=…".
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// Default paths. Every one of them is overridable by flag or environment
// variable so the same binary works in containers and on dev machines.
const (
	defaultUnit    = "ufot"
	defaultHotDB   = "/var/lib/ufot/hot.db"
	fallbackHotDB  = "/var/lib/ufot/chatd.db" // deploy/backup.sh default
	defaultTimeout = 20 * time.Second
)

// environment is the process seam: everything the CLI touches (files, exec,
// signals, clock, sqlite) is injectable so the pure command logic is testable
// without systemd, root or a live database.
type environment struct {
	stdout io.Writer
	stderr io.Writer

	getenv   func(string) string
	runCmd   func(ctx context.Context, name string, args ...string) ([]byte, error)
	readFile func(string) ([]byte, error)
	stat     func(string) (os.FileInfo, error)
	openDB   func(path string) (*sql.DB, error)
	openDBRW func(path string) (*sql.DB, error)
	diskFree func(path string) (free, total uint64, err error)
	dial     func(network, addr string, timeout time.Duration) (net.Conn, error)
	backupDB func(src, dst string) error
	kill     func(pid int, sig syscall.Signal) error
	sleep    func(time.Duration)
	now      func() time.Time
	httpDo   func(req *http.Request) (*http.Response, error)
}

func defaultEnv() *environment {
	hc := &http.Client{Timeout: 15 * time.Second}
	return &environment{
		stdout: os.Stdout,
		stderr: os.Stderr,
		getenv: os.Getenv,
		runCmd: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			return out, err
		},
		readFile: os.ReadFile,
		stat:     os.Stat,
		openDB:   openSQLiteReadOnly,
		openDBRW: openSQLiteReadWrite,
		diskFree: diskFreeBytes,
		dial: func(network, addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout(network, addr, timeout)
		},
		backupDB: backupSQLiteDefault,
		kill:     syscall.Kill,
		sleep:    time.Sleep,
		now:      time.Now,
		httpDo:   hc.Do,
	}
}

// openSQLiteReadOnly opens the hot DB without ever writing it.
func openSQLiteReadOnly(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?mode=ro&_busy_timeout=2000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// openSQLiteReadWrite opens the hot DB for a mutating maintenance command
// (media thin migration). It honours WAL and waits briefly for a lock rather
// than failing immediately when chatd is live.
func openSQLiteReadWrite(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?_busy_timeout=5000&_journal_mode=WAL"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// ------------------------------------------------------------------ errors

type usageErr struct{ msg string }

func (e *usageErr) Error() string { return e.msg }

// errHelpRequested signals that a subcommand's -h/--help was already handled
// (its flag list was printed): run() maps it to exit 0, not a syntax error.
var errHelpRequested = errors.New("help requested")

// parseFlags parses a subcommand flag set. Output goes to e.stderr so that
// `--help` prints the subcommand's flag list; ErrHelp becomes errHelpRequested
// (exit 0) instead of a usage error.
func parseFlags(fs *flag.FlagSet, e *environment, name string, args []string, reorder bool) error {
	fs.SetOutput(e.stderr)
	a := args
	if reorder {
		a = reorderFlags(fs, args)
	}
	if err := fs.Parse(a); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return errHelpRequested
		}
		return usagef("%s: %v", name, err)
	}
	return nil
}

// deniedErr maps to exit 77 (permission / not configured).
type deniedErr struct{ msg string }

func (e *deniedErr) Error() string { return e.msg }

// notFoundErr maps to exit 65 (target does not exist; admin-data §7).
type notFoundErr struct{ msg string }

func (e *notFoundErr) Error() string { return e.msg }

func usagef(format string, a ...any) error { return &usageErr{msg: fmt.Sprintf(format, a...)} }

func deniedf(format string, a ...any) error { return &deniedErr{msg: fmt.Sprintf(format, a...)} }

func notFoundf(format string, a ...any) error { return &notFoundErr{msg: fmt.Sprintf(format, a...)} }

func exitCodeFor(err error) int {
	if err == nil {
		return exitOK
	}
	var ue *usageErr
	if errors.As(err, &ue) {
		return exitUsage
	}
	var de *deniedErr
	if errors.As(err, &de) {
		return exitDenied
	}
	var ne *notFoundErr
	if errors.As(err, &ne) {
		return exitNotFound
	}
	return exitError
}

func (e *environment) printf(format string, a ...any) {
	fmt.Fprintf(e.stdout, format, a...)
}

func (e *environment) errorf(format string, a ...any) {
	fmt.Fprintf(e.stderr, "ufotctl: "+format+"\n", a...)
}

// ---------------------------------------------------------------- gl flags

// globals are flags accepted anywhere on the command line.
type globals struct {
	JSON     bool
	AdminURL string
	Token    string
	CSRF     string
	Timeout  time.Duration
}

// parseGlobals extracts global flags and returns the remaining argv. Values
// may be given as "--flag value" or "--flag=value".
func parseGlobals(args []string) (globals, []string, error) {
	g := globals{Timeout: defaultTimeout}
	rest := make([]string, 0, len(args))
	take := func(i *int, a, name string) (string, error) {
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			return a[eq+1:], nil
		}
		if *i+1 >= len(args) {
			return "", usagef("missing value for %s", name)
		}
		*i++
		return args[*i], nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json" || a == "-json":
			g.JSON = true
		case a == "--admin-url" || strings.HasPrefix(a, "--admin-url="):
			v, err := take(&i, a, "--admin-url")
			if err != nil {
				return g, nil, err
			}
			g.AdminURL = v
		case a == "--token" || strings.HasPrefix(a, "--token="):
			v, err := take(&i, a, "--token")
			if err != nil {
				return g, nil, err
			}
			g.Token = v
		case a == "--csrf-token" || strings.HasPrefix(a, "--csrf-token="):
			v, err := take(&i, a, "--csrf-token")
			if err != nil {
				return g, nil, err
			}
			g.CSRF = v
		case a == "--timeout" || strings.HasPrefix(a, "--timeout="):
			v, err := take(&i, a, "--timeout")
			if err != nil {
				return g, nil, err
			}
			d, perr := time.ParseDuration(v)
			if perr != nil || d <= 0 {
				return g, nil, usagef("invalid --timeout %q", v)
			}
			g.Timeout = d
		case a == "--lang" || strings.HasPrefix(a, "--lang="):
			v, err := take(&i, a, "--lang")
			if err != nil {
				return g, nil, err
			}
			i18n.SetLang(i18n.DetectLang(v))
		default:
			rest = append(rest, a)
		}
	}
	return g, rest, nil
}

// resolveAdmin fills the admin API connection from flags → env → adminctl's
// credentials file, so `adminctl login` has to be run only once.
func (g *globals) resolveAdmin(e *environment) {
	if g.AdminURL == "" {
		g.AdminURL = e.getenv("UFOT_ADMIN_URL")
	}
	if g.AdminURL == "" {
		g.AdminURL = e.getenv("UFOT_ADMIN_CREDENTIALS_SERVER")
	}
	if g.Token == "" {
		g.Token = e.getenv("UFOT_ADMIN_TOKEN")
	}
	if g.CSRF == "" {
		g.CSRF = e.getenv("UFOT_ADMIN_CSRF")
	}
	creds := loadAdminctlCredentials(e)
	if g.AdminURL == "" {
		g.AdminURL = creds.Server
	}
	if g.Token == "" {
		g.Token = creds.Token
	}
	if g.CSRF == "" {
		g.CSRF = creds.CSRF
	}
	if g.AdminURL == "" {
		// Built-in default matches config/chatd.toml admin_listen; override with
		// --admin-url or UFOT_ADMIN_URL.
		g.AdminURL = "http://127.0.0.1:7780"
	}
}

// ---------------------------------------------------------------- output

// emitJSON writes the unified --json envelope.
func emitJSON(e *environment, command string, data any) error {
	env := map[string]any{"ok": true, "command": command, "data": data}
	b, err := jsonMarshalIndent(env)
	if err != nil {
		return err
	}
	e.printf("%s\n", b)
	return nil
}

// hotDBPath resolves the hot database path: explicit flag → env → defaults,
// preferring an existing file.
func hotDBPath(e *environment, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if p := e.getenv("UFOT_HOT_DB"); p != "" {
		return p
	}
	if _, err := e.stat(defaultHotDB); err == nil {
		return defaultHotDB
	}
	if _, err := e.stat(fallbackHotDB); err == nil {
		return fallbackHotDB
	}
	return defaultHotDB
}

// adminctlCredPath mirrors adminctl's credential location exactly (one login
// for both CLIs).
func adminctlCredPath(e *environment) string {
	if p := e.getenv("UFOT_ADMIN_CREDENTIALS"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = filepath.Join(e.getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "ufot", "adminctl.json")
}
