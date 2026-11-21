// Command adminctl is the management-plane CLI. It is a pure API client:
// every operation goes through the same HTTP/鉴权 path as the browser console
// (docs/admin-data-design.md §7: CLI 是 API 的第二皮肤，不是旁路).
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/yingo-server/OpenDiscord/server/internal/pkg/i18n"
)

var errUsage = errors.New("usage")

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, in io.Reader, out, errw io.Writer) int {
	stderrW = errw
	g, rest, err := parseGlobals(args)
	if err != nil {
		fmt.Fprintf(errw, "adminctl: %v\n\n", err)
		printUsage(errw)
		return exitUsage
	}
	if len(rest) == 0 {
		printUsage(errw)
		return exitUsage
	}
	switch rest[0] {
	case "help", "-h", "--help":
		printUsage(out)
		return exitOK
	case "completion":
		if err := cmdCompletion(rest[1:], out); err != nil {
			printError(errw, err)
			return exitCodeFor(err)
		}
		return exitOK
	}
	if err := resolveGlobals(&g); err != nil {
		printError(errw, err)
		return exitCodeFor(err)
	}

	switch rest[0] {
	case "login":
		err = cmdLogin(g, rest[1:], in, out, errw)
	case "whoami":
		err = cmdWhoami(g, rest[1:], out)
	case "users":
		err = cmdUsers(g, rest[1:], out)
	case "kick":
		err = cmdKick(g, rest[1:], out)
	case "reports":
		err = cmdReports(g, rest[1:], out)
	case "messages":
		err = cmdMessages(g, rest[1:], out)
	case "broadcast":
		err = cmdBroadcast(g, rest[1:], out)
	case "flags":
		err = cmdFlags(g, rest[1:], out)
	case "logout":
		err = cmdLogout(g, rest[1:], out, errw)
	case "metrics":
		err = cmdMetrics(g, rest[1:], out)
	case "audit":
		err = cmdAudit(g, rest[1:], out)
	case "archive":
		err = cmdArchive(g, rest[1:], out)
	default:
		fmt.Fprintf(errw, "adminctl: unknown command %q\n\n", rest[0])
		printUsage(errw)
		return exitUsage
	}
	if err != nil {
		if errors.Is(err, errHelpRequested) {
			return exitOK
		}
		printError(errw, err)
		return exitCodeFor(err)
	}
	return exitOK
}

// parseGlobals extracts global flags wherever they appear; everything else is
// preserved for the subcommand parser.
func parseGlobals(args []string) (globals, []string, error) {
	var g globals
	var rest []string
	var langSeen string
	// Language resolution: explicit --lang wins, else LC_ALL/LANG (i18n.DetectLang).
	if langSeen == "" {
		i18n.SetLang(i18n.DetectLang(""))
	}
	take := func(i *int, inline, name string) (string, error) {
		if inline != "" {
			return inline, nil
		}
		if *i+1 >= len(args) {
			return "", &usageError{msg: "missing value for " + name}
		}
		*i++
		return args[*i], nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		var val string
		var err error
		switch {
		case a == "--server" || a == "-server":
			g.Server, err = take(&i, "", a)
		case strings.HasPrefix(a, "--server=") || strings.HasPrefix(a, "-server="):
			g.Server = a[strings.IndexByte(a, '=')+1:]
		case a == "--token":
			g.Token, err = take(&i, "", a)
		case strings.HasPrefix(a, "--token="):
			g.Token = a[len("--token="):]
		case a == "--csrf-token":
			g.CSRF, err = take(&i, "", a)
		case strings.HasPrefix(a, "--csrf-token="):
			g.CSRF = a[len("--csrf-token="):]
		case a == "--output" || a == "-o":
			g.Output, err = take(&i, "", a)
		case strings.HasPrefix(a, "--output=") || strings.HasPrefix(a, "-o="):
			val = a[strings.IndexByte(a, '=')+1:]
			g.Output = val
		case a == "--idempotency-key":
			g.IdemKey, err = take(&i, "", a)
		case strings.HasPrefix(a, "--idempotency-key="):
			g.IdemKey = a[len("--idempotency-key="):]
		case a == "--no-save":
			g.NoSave = true
		case a == "--lang" || strings.HasPrefix(a, "--lang="):
			v, lerr := take(&i, a, "--lang")
			if lerr != nil {
				return g, nil, lerr
			}
			i18n.SetLang(i18n.DetectLang(v))
			langSeen = v
		default:
			rest = append(rest, a)
		}
		if err != nil {
			return g, nil, err
		}
	}
	if g.Output == "" || g.Output == "table" {
		g.Output = "table"
	}
	if g.Output != "table" && g.Output != "json" && g.Output != "text" {
		return g, nil, &usageError{msg: "invalid --output (want json, table or text)"}
	}
	return g, rest, nil
}

func resolveGlobals(g *globals) error {
	// A malformed/absent credentials file must not block explicit flags or
	// commands that do not need auth; treat it as empty.
	creds, _ := loadCredentials()
	if g.Server == "" {
		g.Server = os.Getenv("OPENDISCORD_ADMIN_URL")
	}
	if g.Server == "" {
		g.Server = creds.Server
	}
	if g.Server == "" {
		// Built-in default matches config/chatd.toml admin_listen; override with
		// --server or OPENDISCORD_ADMIN_URL.
		g.Server = "http://127.0.0.1:7780"
	}
	if g.Token == "" {
		g.Token = os.Getenv("OPENDISCORD_ADMIN_TOKEN")
	}
	if g.Token == "" {
		g.Token = creds.Token
	}
	if g.CSRF == "" {
		g.CSRF = os.Getenv("OPENDISCORD_ADMIN_CSRF")
	}
	if g.CSRF == "" {
		g.CSRF = creds.CSRF
	}
	return nil
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `adminctl — OpenDiscord management API client

usage: adminctl [global flags] <command> [args]

Commands:
  login                       Administrators: obtain a bearer token (interactive password + TOTP)
  whoami                      Show the current admin identity
  users list|show|ban|unban   User administration: list, show, ban, unban
  kick <uid>                  Kick: disconnect all of a user's sessions
  reports list|show|handle    Report administration: list, show, handle
  messages search|recall      Message administration: search, recall
  broadcast preview|send      Broadcast: preview (dry run) / send
  flags list|set              Feature flags / policy-as-flag: list, set
  metrics                     Show Prometheus metrics (GET /api/v1/metrics, raw text)
  logout                      Log out (POST /api/v1/logout) and delete credentials
  audit --since-id <id>       Stream the sequential audit cursor
  archive status|run          Archive: job ledger / run a job (--dry-run previews)
  completion bash             Print bash completion

Global flags:
  --server <url>              Admin API base URL (env OPENDISCORD_ADMIN_URL)
  --token <token>             Bearer token (env OPENDISCORD_ADMIN_TOKEN)
  --csrf-token <token>        CSRF token (env OPENDISCORD_ADMIN_CSRF)
  --output json|table         Output format (default table)
  --idempotency-key <key>     Explicit key for a write operation
  --no-save                   login only: do not persist credentials

Run 'adminctl <command> --help' for command-specific flags.
Exit codes: 0 ok, 1 error, 64 usage, 65 not found, 77 permission denied.
`)
}
