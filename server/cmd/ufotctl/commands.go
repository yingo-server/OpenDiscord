package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- version

func cmdVersion(_ context.Context, e *environment, g globals, args []string) error {
	if len(args) > 0 {
		return usagef("version: unexpected argument %q", args[0])
	}
	info := map[string]string{
		"version": version,
		"commit":  commit,
		"date":    date,
		"go":      runtime.Version(),
		"os":      runtime.GOOS,
		"arch":    runtime.GOARCH,
	}
	if g.JSON {
		return emitJSON(e, "version", info)
	}
	e.printf("ufotctl %s (commit %s, built %s)\n", version, commit, date)
	e.printf("%s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return nil
}

// ---------------------------------------------------------------- archive

func cmdArchive(ctx context.Context, e *environment, g globals, args []string) error {
	fs := flag.NewFlagSet("archive", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "只估算影响，不触发")
	run := fs.Bool("run", false, "触发一次归档")
	if err := parseFlags(fs, e, "archive", args, false); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usagef("archive: unexpected argument %q", fs.Arg(0))
	}
	if *dry == *run {
		return usagef("archive: 需要且只能指定 --dry-run 或 --run")
	}

	g.resolveAdmin(e)
	client, err := newAPIClient(e, g)
	if err != nil {
		// Management actions never bypass the admin API (CLI 是 API 的第二皮肤).
		e.printf("提示: archive 是管理动作，请使用 adminctl archive status|run [--dry-run]\n")
		return err
	}
	path := "/api/v1/archive/run"
	if *dry {
		path += "?dry_run=true"
	}
	raw, err := client.doJSON(ctx, http.MethodPost, path, map[string]any{"dry_run": *dry})
	if err != nil {
		return classifyAPIError(err)
	}
	return renderAPIResult(e, g, "archive", raw)
}

// ------------------------------------------------------------------ flags

func cmdFlags(ctx context.Context, e *environment, g globals, args []string) error {
	if len(args) == 0 {
		return usagef("flags: 需要 list | set KEY=VALUE")
	}
	clientFor := func() (*apiClient, error) {
		g.resolveAdmin(e)
		return newAPIClient(e, g)
	}
	switch args[0] {
	case "list":
		client, err := clientFor()
		if err != nil {
			return err
		}
		raw, err := client.doJSON(ctx, http.MethodGet, "/api/v1/flags", nil)
		if err != nil {
			return classifyAPIError(err)
		}
		return renderAPIResult(e, g, "flags", raw)
	case "set":
		// Validate the assignment BEFORE touching credentials: a syntax error
		// must be a usage error (64), not "not logged in" (77).
		key, value, err := parseFlagAssignment(args[1:])
		if err != nil {
			return err
		}
		if !json.Valid([]byte(value)) {
			b, _ := json.Marshal(value) // bare strings become JSON strings
			value = string(b)
		}
		client, err := clientFor()
		if err != nil {
			return err
		}
		raw, err := client.doJSON(ctx, http.MethodPut, "/api/v1/flags",
			map[string]any{"key": key, "value": value})
		if err != nil {
			return classifyAPIError(err)
		}
		return renderAPIResult(e, g, "flags", raw)
	default:
		return usagef("flags: unknown subcommand %q (want list|set)", args[0])
	}
}

// parseFlagAssignment accepts `set k=v` and `set k v`.
func parseFlagAssignment(args []string) (key, value string, err error) {
	if len(args) == 0 {
		return "", "", usagef("flags set: 需要 KEY=VALUE")
	}
	if k, v, ok := strings.Cut(args[0], "="); ok {
		if strings.TrimSpace(k) == "" {
			return "", "", usagef("flags set: 空 key")
		}
		return strings.TrimSpace(k), v, nil
	}
	if len(args) >= 2 {
		return strings.TrimSpace(args[0]), args[1], nil
	}
	return "", "", usagef("flags set: 需要 KEY=VALUE")
}

// -------------------------------------------------------------- ban/unban

func cmdBan(ctx context.Context, e *environment, g globals, args []string, unban bool) error {
	name := "ban"
	if unban {
		name = "unban"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	days := fs.Int("days", 0, "封禁天数（0=永久）")
	level := fs.Int("level", 1, "封禁等级 1..3")
	until := fs.Int64("until", 0, "绝对到期时间（unix 秒）")
	reason := fs.String("reason", "", "原因")
	if err := parseFlags(fs, e, name, args, true); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return usagef("%s: 需要 uid", name)
	}
	uid, err := parseUID(fs.Arg(0))
	if err != nil {
		return err
	}
	if *days < 0 || *until < 0 || (*days > 0 && *until > 0) {
		return usagef("%s: --days 与 --until 互斥且不能为负", name)
	}

	g.resolveAdmin(e)
	client, err := newAPIClient(e, g)
	if err != nil {
		return err
	}
	var raw []byte
	if unban {
		raw, err = client.doJSON(ctx, http.MethodPost,
			fmt.Sprintf("/api/v1/users/%d/unban", uid), map[string]any{"reason": *reason})
	} else {
		if *level < 1 || *level > 3 {
			return usagef("ban: --level 必须为 1..3")
		}
		raw, err = client.doJSON(ctx, http.MethodPost,
			fmt.Sprintf("/api/v1/users/%d/ban", uid), map[string]any{
				"level": *level, "days": *days, "until": *until, "reason": *reason,
			})
	}
	if err != nil {
		return classifyAPIError(err)
	}
	return renderAPIResult(e, g, name, raw)
}

// parseUID parses a positive snowflake/int id.
func parseUID(raw string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, usagef("invalid uid %q", raw)
	}
	return id, nil
}

// --------------------------------------------------------------- rendering

// renderAPIResult prints an admin API response: pretty JSON under --json, a
// sorted key/value listing otherwise.
func renderAPIResult(e *environment, g globals, command string, raw []byte) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		e.printf("%s\n", strings.TrimSpace(string(raw)))
		return nil
	}
	if g.JSON {
		return emitJSON(e, command, v)
	}
	switch val := v.(type) {
	case map[string]any:
		printObject(e, val)
	case []any:
		for _, item := range val {
			if obj, ok := item.(map[string]any); ok {
				printObject(e, obj)
				e.printf("\n")
				continue
			}
			e.printf("%v\n", item)
		}
	default:
		e.printf("%v\n", val)
	}
	return nil
}

// printObject prints a flat JSON object with sorted keys.
func printObject(e *environment, obj map[string]any) {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	width := 0
	for _, k := range keys {
		if len(k) > width {
			width = len(k)
		}
	}
	for _, k := range keys {
		e.printf("%-*s : %s\n", width, k, formatValue(obj[k]))
	}
}

func formatValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "-"
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprintf("%v", x)
		}
		return string(b)
	}
}

// boolFlag is implemented by flag.Value types that take no argument.
type boolFlag interface{ IsBoolFlag() bool }

// reorderFlags hoists flag tokens (and their values) ahead of positional
// arguments. Go's flag package stops at the first non-flag token, so without
// this `ban 42 --days 7 --level 2` would keep the defaults even though the
// usage text advertises that order.
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
