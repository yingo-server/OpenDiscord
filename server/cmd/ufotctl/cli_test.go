package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ------------------------------------------------------------- test doubles

type cmdResp struct {
	prefix string
	out    string
	err    error
}

type fakeSystem struct {
	resp      []cmdResp
	cmds      []string
	killed    []int
	killSigs  []syscall.Signal
	killErr   error
	dialErr   error
	diskFree  uint64
	diskTotal uint64
	env       map[string]string
	http      func(*http.Request) (*http.Response, error)
}

func newTestEnv(t *testing.T, fs *fakeSystem) (*environment, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	if fs == nil {
		fs = &fakeSystem{}
	}
	if fs.diskTotal == 0 {
		fs.diskTotal = 64 << 30
	}
	if fs.diskFree == 0 {
		fs.diskFree = 32 << 30
	}
	out, errb := &bytes.Buffer{}, &bytes.Buffer{}
	e := defaultEnv()
	e.stdout, e.stderr = out, errb
	// Isolate the credential lookup: the default env reads the real HOME, so a
	// developer machine (or a parallel test) with a leftover
	// ~/.config/ufot/adminctl.json silently changes the expected outcome.
	t.Setenv("UFOT_ADMIN_CREDENTIALS", filepath.Join(t.TempDir(), "adminctl.json"))
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	e.runCmd = func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := name + " " + strings.Join(args, " ")
		fs.cmds = append(fs.cmds, key)
		for _, r := range fs.resp {
			if strings.HasPrefix(key, r.prefix) {
				return []byte(r.out), r.err
			}
		}
		return nil, fmt.Errorf("fake: no response for %q", key)
	}
	e.kill = func(pid int, sig syscall.Signal) error {
		fs.killed = append(fs.killed, pid)
		fs.killSigs = append(fs.killSigs, sig)
		return fs.killErr
	}
	e.sleep = func(time.Duration) {}
	e.now = func() time.Time { return time.Unix(1_800_000_000, 0) }
	e.getenv = func(k string) string {
		if fs.env == nil {
			return ""
		}
		return fs.env[k]
	}
	e.diskFree = func(string) (uint64, uint64, error) { return fs.diskFree, fs.diskTotal, nil }
	e.dial = func(string, string, time.Duration) (net.Conn, error) { return nil, fs.dialErr }
	e.backupDB = func(src, dst string) error { return nil }
	if fs.http != nil {
		e.httpDo = fs.http
	} else {
		e.httpDo = func(*http.Request) (*http.Response, error) {
			return nil, errors.New("fake: no http")
		}
	}
	return e, out, errb
}

// makeHotDB creates a minimal hot-looking DB with a few messages.
func makeHotDB(t *testing.T, rows int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hot.db")
	db, err := sql.Open("sqlite3", "file:"+path+"?_journal_mode=WAL")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE msg_messages_hot (msg_id INTEGER PRIMARY KEY, sent_at INTEGER)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 1; i <= rows; i++ {
		if _, err := db.Exec(`INSERT INTO msg_messages_hot (msg_id, sent_at) VALUES (?, ?)`,
			i, 1_700_000_000+i); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	return path
}

func jsonResp(status int, body string) func(*http.Request) (*http.Response, error) {
	return func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	}
}

// ------------------------------------------------------------------- tests

func TestParseGlobals(t *testing.T) {
	g, rest, err := parseGlobals([]string{"--json", "ban", "--token=abc", "42", "--timeout", "5s"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !g.JSON || g.Token != "abc" || g.Timeout != 5*time.Second {
		t.Fatalf("globals = %+v", g)
	}
	if strings.Join(rest, " ") != "ban 42" {
		t.Fatalf("rest = %v", rest)
	}

	if _, _, err := parseGlobals([]string{"--token"}); err == nil {
		t.Fatal("missing value accepted")
	}
	if _, _, err := parseGlobals([]string{"--timeout", "soon"}); err == nil {
		t.Fatal("bad timeout accepted")
	}
}

func TestRunExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no args", nil, exitUsage},
		{"unknown command", []string{"frobnicate"}, exitUsage},
		{"help", []string{"help"}, exitOK},
		{"version", []string{"version"}, exitOK},
		{"version json", []string{"--json", "version"}, exitOK},
		{"ban without token", []string{"ban", "42"}, exitDenied},
		{"unban without token", []string{"unban", "42"}, exitDenied},
		{"ban bad uid", []string{"ban", "0"}, exitUsage},
		{"ban no uid", []string{"ban"}, exitUsage},
		{"archive no mode", []string{"archive"}, exitUsage},
		{"archive both modes", []string{"archive", "--run", "--dry-run"}, exitUsage},
		{"archive run no token", []string{"archive", "--run"}, exitDenied},
		{"flags no sub", []string{"flags"}, exitUsage},
		{"flags bad sub", []string{"flags", "nuke"}, exitUsage},
		{"flags set missing value", []string{"flags", "set", "k"}, exitUsage},
		{"doctor missing db", []string{"doctor", "--hot-db", "/nonexistent/hot.db"}, exitError},
		{"backup missing script", []string{"backup", "--script", "/nonexistent/backup.sh"}, exitNotFound},
	}
	for _, c := range cases {
		e, _, _ := newTestEnv(t, &fakeSystem{})
		if got := run(c.args, e); got != c.want {
			t.Fatalf("%s: exit = %d, want %d (stdout=%q stderr=%q)",
				c.name, got, c.want, e.stdout, e.stderr)
		}
	}
}

func TestBanWithoutTokenHintsAdminctlLogin(t *testing.T) {
	e, _, errb := newTestEnv(t, &fakeSystem{})
	code := run([]string{"ban", "42", "--days", "3"}, e)
	if code != exitDenied {
		t.Fatalf("exit = %d, want 77", code)
	}
	if !strings.Contains(errb.String(), "adminctl login") {
		t.Fatalf("stderr = %q, want an adminctl login hint", errb.String())
	}
}

func TestVersionJSON(t *testing.T) {
	e, out, _ := newTestEnv(t, &fakeSystem{})
	if code := run([]string{"--json", "version"}, e); code != exitOK {
		t.Fatalf("exit = %d stderr=%s", code, e.stderr)
	}
	var env struct {
		OK      bool              `json:"ok"`
		Command string            `json:"command"`
		Data    map[string]string `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("json: %v (%s)", err, out.String())
	}
	if !env.OK || env.Command != "version" || env.Data["version"] == "" {
		t.Fatalf("envelope = %+v", env)
	}
}

func TestStatusSendsUSR1AndReadsJournal(t *testing.T) {
	hot := makeHotDB(t, 3)
	fs := &fakeSystem{
		resp: []cmdResp{
			{prefix: "systemctl is-active ufot", out: "active\n"},
			{prefix: "systemctl show -p MainPID --value ufot", out: "4242\n"},
			{prefix: "journalctl -u ufot ", out: "boot ok\n{\"component\":\"bridge\",\"msg\":\"dump\",\"egress\":{\"q\":0}}\n"},
		},
	}
	e, out, _ := newTestEnv(t, fs)
	if code := run([]string{"--json", "status", "--hot-db", hot}, e); code != exitOK {
		t.Fatalf("exit = %d stderr=%s", code, e.stderr)
	}
	var env struct {
		Data statusReport `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("json: %v (%s)", err, out.String())
	}
	rep := env.Data
	if rep.State != "active" || !rep.Active || rep.PID != 4242 {
		t.Fatalf("status = %+v", rep)
	}
	if rep.Signal != "SIGUSR1" {
		t.Fatalf("signal = %q, want SIGUSR1", rep.Signal)
	}
	if len(fs.killed) != 1 || fs.killed[0] != 4242 || fs.killSigs[0] != syscall.SIGUSR1 {
		t.Fatalf("killed = %v sigs = %v", fs.killed, fs.killSigs)
	}
	if !strings.Contains(rep.Dump, "dump") {
		t.Fatalf("dump = %q", rep.Dump)
	}
	if !rep.HotDB.Exists || rep.HotDB.Messages != 3 || rep.HotDB.SizeBytes <= 0 {
		t.Fatalf("hot stats = %+v", rep.HotDB)
	}
}

func TestStatusSignalNoneAndInactive(t *testing.T) {
	hot := makeHotDB(t, 0)
	fs := &fakeSystem{resp: []cmdResp{
		{prefix: "systemctl is-active ufot", out: "inactive\n", err: errors.New("exit status 3")},
	}}
	e, out, _ := newTestEnv(t, fs)
	if code := run([]string{"--json", "status", "--hot-db", hot, "--signal", "none", "--no-journal"}, e); code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if len(fs.killed) != 0 {
		t.Fatalf("inactive unit must not be signalled: %v", fs.killed)
	}
	var env struct {
		Data statusReport `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.Active || env.Data.State != "inactive" {
		t.Fatalf("status = %+v", env.Data)
	}
}

func TestStatusInvalidSignalIsUsage(t *testing.T) {
	e, _, _ := newTestEnv(t, &fakeSystem{})
	if code := run([]string{"status", "--signal", "kthx"}, e); code != exitUsage {
		t.Fatalf("exit = %d, want 64", code)
	}
}

func TestBackupBuiltinDryRun(t *testing.T) {
	hot := makeHotDB(t, 1)
	fs := &fakeSystem{env: map[string]string{"UFOT_BACKUP_DIR": "/tmp/backups"}}
	e, out, _ := newTestEnv(t, fs)
	// Neutralise script discovery: run from a temp dir without deploy/backup.sh.
	chdir(t, t.TempDir())
	if code := run([]string{"--json", "backup", "--dry-run", "--db", hot}, e); code != exitOK {
		t.Fatalf("exit = %d stderr=%s", code, e.stderr)
	}
	var env struct {
		Data backupReport `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("json: %v (%s)", err, out.String())
	}
	if env.Data.Mode != "builtin" || !env.Data.DryRun || env.Data.DB != hot {
		t.Fatalf("report = %+v", env.Data)
	}
	if !strings.HasPrefix(env.Data.Target, "/tmp/backups/ufot-") {
		t.Fatalf("target = %q", env.Data.Target)
	}
	if !strings.Contains(env.Data.Encryption, "age") {
		t.Fatalf("encryption hint = %q", env.Data.Encryption)
	}
}

func TestBackupRunsScriptWhenPresent(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	script := filepath.Join(dir, "backup.sh")
	if err := os.WriteFile(script, []byte("#!/usr/bin/env bash\necho script-ran\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	fs := &fakeSystem{resp: []cmdResp{{prefix: "bash " + script, out: "script-ran\n"}}}
	e, out, _ := newTestEnv(t, fs)
	if code := run([]string{"backup", "--dry-run", "--script", script}, e); code != exitOK {
		t.Fatalf("exit = %d stderr=%s", code, e.stderr)
	}
	if !strings.Contains(out.String(), "script-ran") {
		t.Fatalf("stdout = %q", out.String())
	}
	if len(fs.cmds) != 1 || !strings.HasSuffix(fs.cmds[0], "--dry-run") {
		t.Fatalf("cmds = %v", fs.cmds)
	}
}

func TestBanPostsToAdminAPI(t *testing.T) {
	var gotBody string
	fs := &fakeSystem{http: func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/users/42/ban" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusAccepted,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"uid":42,"banned":true,"until":0}`)),
		}, nil
	}}
	e, out, _ := newTestEnv(t, fs)
	code := run([]string{"--json", "--token", "t0ken", "ban", "42", "--level", "2", "--days", "3", "--reason", "spam"}, e)
	if code != exitOK {
		t.Fatalf("exit = %d stderr=%s", code, e.stderr)
	}
	var env struct {
		Command string         `json:"command"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("json: %v (%s)", err, out.String())
	}
	if env.Command != "ban" || env.Data["banned"] != true {
		t.Fatalf("envelope = %+v", env)
	}
	// The request body must carry the parsed flag values (regression guard:
	// a flag-order bug silently sent the defaults).
	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("request body not json: %v (%s)", err, gotBody)
	}
	if body["reason"] != "spam" {
		t.Fatalf("reason = %v, want spam (body=%s)", body["reason"], gotBody)
	}
	if lvl, _ := body["level"].(float64); lvl != 2 {
		t.Fatalf("level = %v, want 2 (body=%s)", body["level"], gotBody)
	}
	if d, _ := body["days"].(float64); d != 3 {
		t.Fatalf("days = %v, want 3 (body=%s)", body["days"], gotBody)
	}
}

func TestHelpExitsZero(t *testing.T) {
	for _, cmd := range []string{"status", "doctor", "backup", "archive"} {
		e, out, errb := newTestEnv(t, &fakeSystem{})
		if code := run([]string{cmd, "--help"}, e); code != exitOK {
			t.Fatalf("%s --help exit = %d, want 0 (stderr=%s)", cmd, code, errb.String())
		}
		if out.Len()+errb.Len() == 0 {
			t.Fatalf("%s --help printed nothing", cmd)
		}
	}
}

func TestBanUnauthorizedMapsTo77(t *testing.T) {
	fs := &fakeSystem{http: jsonResp(http.StatusUnauthorized, `{"code":401,"reason":"unauthenticated"}`)}
	e, _, errb := newTestEnv(t, fs)
	if code := run([]string{"--token", "stale", "ban", "42"}, e); code != exitDenied {
		t.Fatalf("exit = %d, want 77", code)
	}
	if !strings.Contains(errb.String(), "adminctl login") {
		t.Fatalf("stderr = %q", errb.String())
	}
}

func TestFlagsSetSendsJSONValue(t *testing.T) {
	var gotBody string
	fs := &fakeSystem{http: func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/flags" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"key":"chat.x","value":"1"}`)),
		}, nil
	}}
	e, _, _ := newTestEnv(t, fs)
	if code := run([]string{"--token", "t", "flags", "set", "chat.x=1"}, e); code != exitOK {
		t.Fatalf("exit = %d stderr=%s", code, e.stderr)
	}
	if !strings.Contains(gotBody, `"key":"chat.x"`) || !strings.Contains(gotBody, `"value":"1"`) {
		t.Fatalf("body = %s", gotBody)
	}
}

func TestParseFlagAssignment(t *testing.T) {
	if k, v, err := parseFlagAssignment([]string{"a=1"}); err != nil || k != "a" || v != "1" {
		t.Fatalf("k=%q v=%q err=%v", k, v, err)
	}
	if k, v, err := parseFlagAssignment([]string{"a", "1"}); err != nil || k != "a" || v != "1" {
		t.Fatalf("k=%q v=%q err=%v", k, v, err)
	}
	if _, _, err := parseFlagAssignment([]string{"=1"}); err == nil {
		t.Fatal("empty key accepted")
	}
	if _, _, err := parseFlagAssignment([]string{"a"}); err == nil {
		t.Fatal("missing value accepted")
	}
}

func TestParseSignal(t *testing.T) {
	cases := map[string]syscall.Signal{"usr1": syscall.SIGUSR1, "SIGUSR2": syscall.SIGUSR2, "none": 0, "": syscall.SIGUSR1}
	for in, want := range cases {
		got, _, err := parseSignal(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != want {
			t.Fatalf("%q: got %v want %v", in, got, want)
		}
	}
	if _, _, err := parseSignal("hup"); err == nil {
		t.Fatal("unsupported signal accepted")
	}
}

func TestPickDumpLine(t *testing.T) {
	journal := "start\nplain\n{\"msg\":\"dump\",\"n\":1}\nother\n{\"msg\":\"DUMP\",\"n\":2}\ntail\n"
	if got := pickDumpLine(journal); !strings.Contains(got, `"n":2`) {
		t.Fatalf("pickDumpLine = %q, want the last dump line", got)
	}
	if got := pickDumpLine("a\nb\n"); got != "" {
		t.Fatalf("pickDumpLine = %q, want empty", got)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[uint64]string{
		512:             "512 B",
		2048:            "2.0 KiB",
		5 << 20:         "5.0 MiB",
		3 << 30:         "3.0 GiB",
		0:               "0 B",
		10<<20 + 512000: "10.4 MiB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Fatalf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// test helpers

func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}
