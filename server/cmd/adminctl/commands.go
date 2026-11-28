package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

func newFlagSet(name string, errw io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(errw)
	return fs
}

func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// The flag package already printed the flag list to fs' output;
			// report a clean exit rather than a usage error.
			return errHelpRequested
		}
		return &usageError{msg: err.Error()}
	}
	return nil
}

// boolFlag is implemented by flag.Value types that take no argument
// (flag.Bool). It mirrors the stdlib's internal interface.
type boolFlag interface{ IsBoolFlag() bool }

// reorderFlags hoists every flag token (and its value, when it takes one)
// ahead of the positional arguments. Go's flag package stops parsing at the
// first non-flag token, so without this `users ban 42 --level 2 --days 7`
// would silently keep the defaults (the documented invocation form in
// docs/deploy.md). Unknown tokens are treated as bare flags, matching Parse.
func reorderFlags(fs *flag.FlagSet, args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if len(a) > 1 && a[0] == '-' {
			if strings.Contains(a, "=") {
				flags = append(flags, a)
				continue
			}
			name := strings.TrimLeft(a, "-")
			f := fs.Lookup(name)
			flags = append(flags, a)
			takesValue := f != nil
			if takesValue {
				if bf, ok := f.Value.(boolFlag); ok && bf.IsBoolFlag() {
					takesValue = false
				}
			}
			if takesValue && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		} else {
			pos = append(pos, a)
		}
	}
	return append(flags, pos...)
}

func requireArgs(fs *flag.FlagSet, n int, what string) error {
	if fs.NArg() < n {
		return &usageError{msg: "missing " + what}
	}
	return nil
}

func getJSON(c *client, path string, out io.Writer, output string) error {
	raw, err := c.do("GET", path, nil, false)
	if err != nil {
		return err
	}
	return render(out, raw, output)
}

func postJSON(c *client, path string, body any, out io.Writer, output string) error {
	raw, err := c.do("POST", path, body, true)
	if err != nil {
		return err
	}
	return render(out, raw, output)
}

// putJSON is the idempotent-write sibling of postJSON. The admin route table
// registers PUT /api/v1/flags (docs/admin-data-design.md §8: writes are
// idempotent), so a POST here would 405.
func putJSON(c *client, path string, body any, out io.Writer, output string) error {
	raw, err := c.do("PUT", path, body, true)
	if err != nil {
		return err
	}
	return render(out, raw, output)
}

func cmdWhoami(g globals, args []string, out io.Writer) error {
	fs := newFlagSet("whoami", stderrW)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	c, err := newClient(g)
	if err != nil {
		return err
	}
	return getJSON(c, "/api/v1/whoami", out, g.Output)
}

func cmdUsers(g globals, args []string, out io.Writer) error {
	if len(args) == 0 {
		return &usageError{msg: "users requires list|show|ban|unban"}
	}
	c, err := newClient(g)
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		fs := newFlagSet("users list", stderrW)
		q := fs.String("q", "", "search query")
		status := fs.Int("status", -1, "status filter (-1 all)")
		limit := fs.Int("limit", 50, "page size")
		offset := fs.Int("offset", 0, "offset")
		if err := parseFlags(fs, args[1:]); err != nil {
			return err
		}
		path := queryPath("/api/v1/users", map[string]string{
			"q": *q, "status": strconv.Itoa(*status),
			"limit": strconv.Itoa(*limit), "offset": strconv.Itoa(*offset),
		})
		return getJSON(c, path, out, g.Output)
	case "show":
		fs := newFlagSet("users show", stderrW)
		if err := parseFlags(fs, args[1:]); err != nil {
			return err
		}
		if err := requireArgs(fs, 1, "uid"); err != nil {
			return err
		}
		uid, err := parseUID(fs.Arg(0))
		if err != nil {
			return err
		}
		return getJSON(c, fmt.Sprintf("/api/v1/users/%d", uid), out, g.Output)
	case "ban":
		fs := newFlagSet("users ban", stderrW)
		level := fs.Int("level", 1, "ban level 1..3")
		days := fs.Int("days", 0, "duration in days (0 = permanent)")
		until := fs.Int64("until", 0, "absolute unix expiry")
		reason := fs.String("reason", "", "reason")
		if err := parseFlags(fs, args[1:]); err != nil {
			return err
		}
		if err := requireArgs(fs, 1, "uid"); err != nil {
			return err
		}
		uid, err := parseUID(fs.Arg(0))
		if err != nil {
			return err
		}
		return postJSON(c, fmt.Sprintf("/api/v1/users/%d/ban", uid), map[string]any{
			"level": *level, "days": *days, "until": *until, "reason": *reason,
		}, out, g.Output)
	case "unban":
		fs := newFlagSet("users unban", stderrW)
		reason := fs.String("reason", "", "reason")
		if err := parseFlags(fs, args[1:]); err != nil {
			return err
		}
		if err := requireArgs(fs, 1, "uid"); err != nil {
			return err
		}
		uid, err := parseUID(fs.Arg(0))
		if err != nil {
			return err
		}
		return postJSON(c, fmt.Sprintf("/api/v1/users/%d/unban", uid), map[string]any{"reason": *reason}, out, g.Output)
	case "kick":
		return cmdKick(g, args[1:], out)
	default:
		return &usageError{msg: "users requires list|show|ban|unban"}
	}
}

func cmdKick(g globals, args []string, out io.Writer) error {
	fs := newFlagSet("kick", stderrW)
	reason := fs.String("reason", "", "reason")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireArgs(fs, 1, "uid"); err != nil {
		return err
	}
	uid, err := parseUID(fs.Arg(0))
	if err != nil {
		return err
	}
	c, err := newClient(g)
	if err != nil {
		return err
	}
	return postJSON(c, fmt.Sprintf("/api/v1/users/%d/kick", uid), map[string]any{"reason": *reason}, out, g.Output)
}

func cmdReports(g globals, args []string, out io.Writer) error {
	if len(args) == 0 {
		return &usageError{msg: "reports requires list|show|handle"}
	}
	c, err := newClient(g)
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		fs := newFlagSet("reports list", stderrW)
		status := fs.Int("status", -1, "status filter (-1 all)")
		limit := fs.Int("limit", 50, "page size")
		offset := fs.Int("offset", 0, "offset")
		if err := parseFlags(fs, args[1:]); err != nil {
			return err
		}
		return getJSON(c, queryPath("/api/v1/reports", map[string]string{
			"status": strconv.Itoa(*status), "limit": strconv.Itoa(*limit), "offset": strconv.Itoa(*offset),
		}), out, g.Output)
	case "show":
		fs := newFlagSet("reports show", stderrW)
		if err := parseFlags(fs, args[1:]); err != nil {
			return err
		}
		if err := requireArgs(fs, 1, "report id"); err != nil {
			return err
		}
		id, err := parseUID(fs.Arg(0))
		if err != nil {
			return err
		}
		return getJSON(c, fmt.Sprintf("/api/v1/reports/%d", id), out, g.Output)
	case "handle":
		fs := newFlagSet("reports handle", stderrW)
		action := fs.String("action", "", "claim|resolve|reject")
		note := fs.String("note", "", "handling note")
		if err := parseFlags(fs, args[1:]); err != nil {
			return err
		}
		if err := requireArgs(fs, 1, "report id"); err != nil {
			return err
		}
		if *action == "" {
			return &usageError{msg: "--action is required"}
		}
		id, err := parseUID(fs.Arg(0))
		if err != nil {
			return err
		}
		return postJSON(c, fmt.Sprintf("/api/v1/reports/%d/handle", id), map[string]any{
			"action": *action, "note": *note,
		}, out, g.Output)
	default:
		return &usageError{msg: "reports requires list|show|handle"}
	}
}

func cmdMessages(g globals, args []string, out io.Writer) error {
	if len(args) == 0 {
		return &usageError{msg: "messages requires search|recall"}
	}
	c, err := newClient(g)
	if err != nil {
		return err
	}
	switch args[0] {
	case "search":
		fs := newFlagSet("messages search", stderrW)
		q := fs.String("q", "", "query text")
		conv := fs.Int64("conv", 0, "conversation id")
		limit := fs.Int("limit", 50, "page size")
		offset := fs.Int("offset", 0, "offset")
		if err := parseFlags(fs, args[1:]); err != nil {
			return err
		}
		if *q == "" && *conv == 0 {
			return &usageError{msg: "messages search requires --q or --conv"}
		}
		return getJSON(c, queryPath("/api/v1/messages/search", map[string]string{
			"q": *q, "conv_id": strconv.FormatInt(*conv, 10),
			"limit": strconv.Itoa(*limit), "offset": strconv.Itoa(*offset),
		}), out, g.Output)
	case "recall":
		fs := newFlagSet("messages recall", stderrW)
		reason := fs.String("reason", "", "reason")
		if err := parseFlags(fs, args[1:]); err != nil {
			return err
		}
		if err := requireArgs(fs, 1, "message id"); err != nil {
			return err
		}
		id, err := parseUID(fs.Arg(0))
		if err != nil {
			return err
		}
		return postJSON(c, fmt.Sprintf("/api/v1/messages/%d/recall", id), map[string]any{"reason": *reason}, out, g.Output)
	default:
		return &usageError{msg: "messages requires search|recall"}
	}
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func cmdBroadcast(g globals, args []string, out io.Writer) error {
	if len(args) == 0 {
		return &usageError{msg: "broadcast requires preview|send"}
	}
	mode := args[0]
	if mode != "preview" && mode != "send" {
		return &usageError{msg: "broadcast requires preview|send"}
	}
	fs := newFlagSet("broadcast "+mode, stderrW)
	text := fs.String("text", "", "broadcast text")
	var convs stringList
	fs.Var(&convs, "conv", "conversation id (repeatable)")
	expires := fs.Int64("expires-at", 0, "unix expiry")
	if err := parseFlags(fs, args[1:]); err != nil {
		return err
	}
	if *text == "" {
		return &usageError{msg: "--text is required"}
	}
	ids := make([]int64, 0, len(convs))
	for _, s := range convs {
		id, err := parseUID(s)
		if err != nil {
			return err
		}
		ids = append(ids, id)
	}
	body := map[string]any{"text": *text, "conv_ids": ids, "expires_at": *expires}
	c, err := newClient(g)
	if err != nil {
		return err
	}
	path := "/api/v1/broadcast"
	if mode == "preview" {
		body["dry_run"] = true
		path = queryPath(path, map[string]string{"dry_run": "true"})
	}
	return postJSON(c, path, body, out, g.Output)
}

func cmdFlags(g globals, args []string, out io.Writer) error {
	if len(args) == 0 {
		return &usageError{msg: "flags requires list|set"}
	}
	c, err := newClient(g)
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		fs := newFlagSet("flags list", stderrW)
		if err := parseFlags(fs, args[1:]); err != nil {
			return err
		}
		return getJSON(c, "/api/v1/flags", out, g.Output)
	case "set":
		fs := newFlagSet("flags set", stderrW)
		if err := parseFlags(fs, args[1:]); err != nil {
			return err
		}
		if err := requireArgs(fs, 2, "key and value"); err != nil {
			return err
		}
		key, value := fs.Arg(0), fs.Arg(1)
		if !json.Valid([]byte(value)) {
			b, _ := json.Marshal(value) // convenience: bare strings are quoted
			value = string(b)
		}
		return putJSON(c, "/api/v1/flags", map[string]any{"key": key, "value": value}, out, g.Output)
	default:
		return &usageError{msg: "flags requires list|set"}
	}
}

func cmdAudit(g globals, args []string, out io.Writer) error {
	fs := newFlagSet("audit", stderrW)
	since := fs.Int64("since-id", 0, "cursor: last id already seen")
	limit := fs.Int("limit", 500, "page size (max 1000)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	c, err := newClient(g)
	if err != nil {
		return err
	}
	return getJSON(c, queryPath("/api/v1/audit", map[string]string{
		"since_id": strconv.FormatInt(*since, 10), "limit": strconv.Itoa(*limit),
	}), out, g.Output)
}

func cmdArchive(g globals, args []string, out io.Writer) error {
	if len(args) == 0 {
		return &usageError{msg: "archive requires status|run"}
	}
	c, err := newClient(g)
	if err != nil {
		return err
	}
	switch args[0] {
	case "status":
		fs := newFlagSet("archive status", stderrW)
		if err := parseFlags(fs, args[1:]); err != nil {
			return err
		}
		return getJSON(c, "/api/v1/archive/status", out, g.Output)
	case "run":
		fs := newFlagSet("archive run", stderrW)
		dry := fs.Bool("dry-run", false, "estimate only")
		if err := parseFlags(fs, args[1:]); err != nil {
			return err
		}
		path := "/api/v1/archive/run"
		if *dry {
			path = queryPath(path, map[string]string{"dry_run": "true"})
		}
		return postJSON(c, path, map[string]any{"dry_run": *dry}, out, g.Output)
	default:
		return &usageError{msg: "archive requires status|run"}
	}
}

func parseUID(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, &usageError{msg: fmt.Sprintf("invalid id %q", raw)}
	}
	return id, nil
}

// cmdLogout invalidates the session on the server (POST /api/v1/logout) and
// then removes the local credentials file. A missing file is not an error:
// exiting 0 keeps `adminctl logout` idempotent.
func cmdLogout(g globals, args []string, out, errw io.Writer) error {
	fs := newFlagSet("logout", stderrW)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	c, err := newClient(g)
	if err != nil {
		return err
	}
	raw, err := c.do("POST", "/api/v1/logout", nil, true)
	if err != nil {
		return err
	}
	if err := os.Remove(credPath()); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(errw, "adminctl: warning: could not remove credentials: %v\n", err)
	}
	if g.Output == "json" && len(raw) > 0 {
		return render(out, raw, "json")
	}
	fmt.Fprintln(out, "logged out")
	return nil
}

// cmdMetrics fetches Prometheus exposition text from GET /api/v1/metrics and
// writes it verbatim (no JSON wrapper); with --output json it wraps the raw
// text as {"raw": "..."} for callers that want a machine envelope.
func cmdMetrics(g globals, args []string, out io.Writer) error {
	fs := newFlagSet("metrics", stderrW)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	c, err := newClient(g)
	if err != nil {
		return err
	}
	raw, err := c.do("GET", "/api/v1/metrics", nil, false)
	if err != nil {
		return err
	}
	if g.Output == "json" {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]string{"raw": string(raw)})
	}
	if _, err := out.Write(raw); err != nil {
		return err
	}
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		_, err = fmt.Fprintln(out)
	}
	return err
}
