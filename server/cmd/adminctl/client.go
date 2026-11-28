package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// Exit codes follow docs/admin-data-design.md §7 (sysexits subset).
const (
	exitOK       = 0
	exitError    = 1
	exitUsage    = 64
	exitNotFound = 65
	exitDenied   = 77
)

// errHelpRequested signals that a subcommand's -h/--help was handled: the flag
// set already printed its usage, so run() maps it to exit 0 (not a syntax error).
var errHelpRequested = errors.New("help requested")

// stderrW is the writer newFlagSet attaches to subcommand flag sets so that
// `adminctl <sub> --help` prints that subcommand's flag list. run() sets it to
// the per-invocation stderr writer.
var stderrW io.Writer = os.Stderr

type globals struct {
	Server  string
	Token   string
	CSRF    string
	Output  string // table | json
	NoSave  bool
	IdemKey string
}

type credentials struct {
	Server    string `json:"server"`
	Token     string `json:"token"`
	CSRF      string `json:"csrf_token"`
	Username  string `json:"username,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
}

type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

type statusError struct {
	Status    int
	Reason    string
	RequestID string
	Body      []byte
}

func (e *statusError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Reason)
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// client is the sole HTTP transport for adminctl. There is no bypass path:
// every command goes through the same REST API as the browser console.
type client struct {
	base       string
	token      string
	csrf       string
	csrfCookie string
	idem       string
	client     *http.Client
}

func newClient(g globals) (*client, error) {
	c := &client{
		base:  strings.TrimRight(g.Server, "/"),
		token: g.Token, csrf: g.CSRF, idem: g.IdemKey,
		client: &http.Client{Timeout: 15 * time.Second},
	}
	if c.base == "" {
		return nil, &usageError{msg: "admin server URL is empty"}
	}
	u, err := url.Parse(c.base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, &usageError{msg: fmt.Sprintf("invalid --server %q", c.base)}
	}
	return c, nil
}

func (c *client) do(method, path string, body any, write bool) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.csrf != "" {
		// Double-submit requires BOTH the cookie and the header, even for
		// Bearer-authenticated CLI calls (strict admin CSRF mode).
		req.Header.Set("X-CSRF-Token", c.csrf)
		req.AddCookie(&http.Cookie{Name: c.csrfCookie, Value: c.csrf})
	}
	if write {
		key := c.idem
		if key == "" {
			key = newUUID()
		}
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		se := &statusError{Status: resp.StatusCode, Body: raw}
		var eb struct {
			Code      int    `json:"code"`
			Reason    string `json:"reason"`
			RequestID string `json:"request_id"`
		}
		if json.Unmarshal(raw, &eb) == nil {
			se.Reason, se.RequestID = eb.Reason, eb.RequestID
			if se.Reason == "" {
				se.Reason = http.StatusText(resp.StatusCode)
			}
		}
		return nil, se
	}
	return raw, nil
}

func exitCodeFor(err error) int {
	var ue *usageError
	if errors.As(err, &ue) || errors.Is(err, errUsage) {
		return exitUsage
	}
	var se *statusError
	if errors.As(err, &se) {
		switch se.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return exitDenied
		case http.StatusNotFound:
			return exitNotFound
		}
	}
	return exitError
}

func printError(w io.Writer, err error) {
	var se *statusError
	if errors.As(err, &se) {
		if se.RequestID != "" {
			fmt.Fprintf(w, "adminctl: %s (request_id=%s)\n", se.Error(), se.RequestID)
			return
		}
		fmt.Fprintf(w, "adminctl: %s\n", se.Error())
		return
	}
	fmt.Fprintf(w, "adminctl: %v\n", err)
}

// ------------------------------------------------------------ credentials

func credPath() string {
	if p := os.Getenv("UFOT_ADMIN_CREDENTIALS"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "ufot", "adminctl.json")
}

func loadCredentials() (credentials, error) {
	var c credentials
	b, err := os.ReadFile(credPath())
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("credentials file %s: %w", credPath(), err)
	}
	return c, nil
}

func saveCredentials(c credentials) error {
	path := credPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// ---------------------------------------------------------------- rendering

func render(out io.Writer, raw []byte, output string) error {
	if output == "json" {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			_, err = out.Write(raw)
			if err == nil {
				_, err = fmt.Fprintln(out)
			}
			return err
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		_, err := out.Write(raw)
		return err
	}
	switch val := v.(type) {
	case map[string]any:
		if items, ok := val["items"].([]any); ok {
			return renderRows(out, items)
		}
		return renderObject(out, val)
	case []any:
		return renderRows(out, val)
	default:
		_, err := fmt.Fprintln(out, val)
		return err
	}
}

func renderObject(out io.Writer, obj map[string]any) error {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	for _, k := range keys {
		fmt.Fprintf(tw, "%s:\t%s\n", k, formatValue(obj[k]))
	}
	return tw.Flush()
}

func renderRows(out io.Writer, rows []any) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(out, "(empty)")
		return err
	}
	keySet := map[string]bool{}
	objs := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		obj, ok := row.(map[string]any)
		if !ok {
			obj = map[string]any{"value": row}
		}
		objs = append(objs, obj)
		for k := range obj {
			keySet[k] = true
		}
	}
	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	for i, k := range keys {
		if i > 0 {
			fmt.Fprint(tw, "\t")
		}
		fmt.Fprint(tw, strings.ToUpper(k))
	}
	fmt.Fprintln(tw)
	for _, obj := range objs {
		for i, k := range keys {
			if i > 0 {
				fmt.Fprint(tw, "\t")
			}
			fmt.Fprint(tw, formatValue(obj[k]))
		}
		fmt.Fprintln(tw)
	}
	return tw.Flush()
}

func formatValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "-"
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%v", x)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprintf("%v", x)
		}
		return string(b)
	}
}

// ------------------------------------------------------------------ helpers

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func queryPath(base string, params map[string]string) string {
	q := url.Values{}
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	if len(q) == 0 {
		return base
	}
	return base + "?" + q.Encode()
}
