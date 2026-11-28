package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestEvalIntegrity(t *testing.T) {
	if st, detail := evalIntegrity("ok\n"); st != checkOK || detail != "ok" {
		t.Fatalf("ok => %s/%s", st, detail)
	}
	if st, _ := evalIntegrity("  "); st != checkWarn {
		t.Fatalf("empty => %s", st)
	}
	st, detail := evalIntegrity("database disk image is malformed\nrow 3 missing\n")
	if st != checkFail {
		t.Fatalf("corruption => %s", st)
	}
	if !strings.Contains(detail, "malformed") || !strings.Contains(detail, "+1 more") {
		t.Fatalf("detail = %q", detail)
	}
}

func TestEvalDiskFree(t *testing.T) {
	cases := []struct {
		free, total uint64
		want        string
	}{
		{128 << 20, 64 << 30, checkFail}, // < 256 MiB
		{512 << 20, 64 << 30, checkWarn}, // < 1 GiB
		{2 << 30, 100 << 30, checkWarn},  // 2% of the filesystem
		{32 << 30, 64 << 30, checkOK},    // plenty
		{10 << 30, 100 << 30, checkOK},   // 10%
		{diskFailBelow - 1, 10 << 30, checkFail},
		{diskFailBelow, 10 << 30, checkWarn},
		{diskWarnBelow, 100 << 30, checkWarn}, // 1 GiB free = 1% of the disk
		{diskWarnBelow, 10 << 30, checkOK},    // 10% of the disk
	}
	for _, c := range cases {
		if got, _ := evalDiskFree(c.free, c.total); got != c.want {
			t.Fatalf("evalDiskFree(%d,%d) = %s, want %s", c.free, c.total, got, c.want)
		}
	}
}

func TestParsePGAddr(t *testing.T) {
	cases := []struct {
		dsn     string
		addr    string
		network string
		ok      bool
	}{
		{"postgres://u:p@db.internal:5433/ufot?sslmode=disable", "db.internal:5433", "tcp", true},
		{"postgresql://db.internal/ufot", "db.internal:5432", "tcp", true},
		{"host=10.0.0.5 port=6543 user=oc dbname=oc", "10.0.0.5:6543", "tcp", true},
		{"host=10.0.0.5 dbname=oc", "10.0.0.5:5432", "tcp", true},
		{"host=/var/run/postgresql dbname=oc", "/var/run/postgresql", "unix", true},
		{"", "", "", false},
		{"sslmode=disable", "", "", false},
		{"postgres:///db", "", "", false},
	}
	for _, c := range cases {
		addr, network, ok := parsePGAddr(c.dsn)
		if ok != c.ok || addr != c.addr || network != c.network {
			t.Fatalf("parsePGAddr(%q) = %q,%q,%v want %q,%q,%v",
				c.dsn, addr, network, ok, c.addr, c.network, c.ok)
		}
	}
}

func TestCountLines(t *testing.T) {
	if got := countLines([]byte("a\n\n b \n")); got != 2 {
		t.Fatalf("countLines = %d", got)
	}
	if got := countLines(nil); got != 0 {
		t.Fatalf("countLines(nil) = %d", got)
	}
}

func TestDoctorAllGreen(t *testing.T) {
	hot := makeHotDB(t, 2)
	fs := &fakeSystem{resp: []cmdResp{{prefix: "journalctl", out: ""}}}
	e, out, _ := newTestEnv(t, fs)
	code := run([]string{"--json", "doctor", "--hot-db", hot, "--unit", "ufot"}, e)
	if code != exitOK {
		t.Fatalf("exit = %d stderr=%s stdout=%s", code, e.stderr, out.String())
	}
	rep := decodeDoctor(t, out.String())
	if !rep.OK {
		t.Fatalf("report = %+v", rep)
	}
	byName := map[string]check{}
	for _, c := range rep.Checks {
		byName[c.Name] = c
	}
	if byName["hot_db.integrity"].Status != checkOK {
		t.Fatalf("integrity = %+v", byName["hot_db.integrity"])
	}
	if byName["disk"].Status != checkOK {
		t.Fatalf("disk = %+v", byName["disk"])
	}
	if byName["logs.error"].Status != checkOK {
		t.Fatalf("logs = %+v", byName["logs.error"])
	}
	// No UFOT_COLD_DSN ⇒ explicit degraded SKIP, never a failure.
	if byName["cold_db"].Status != checkSkip {
		t.Fatalf("cold = %+v", byName["cold_db"])
	}
}

func TestDoctorFailsOnMissingDB(t *testing.T) {
	e, out, _ := newTestEnv(t, &fakeSystem{})
	code := run([]string{"--json", "doctor", "--hot-db", "/nonexistent/hot.db"}, e)
	if code != exitError {
		t.Fatalf("exit = %d, want 1", code)
	}
	rep := decodeDoctor(t, out.String())
	if rep.OK {
		t.Fatal("missing DB reported as OK")
	}
}

func TestDoctorWarnOnLogs(t *testing.T) {
	hot := makeHotDB(t, 1)
	fs := &fakeSystem{resp: []cmdResp{{prefix: "journalctl", out: "abc\ndef\n"}}}
	e, out, _ := newTestEnv(t, fs)
	if code := run([]string{"--json", "doctor", "--hot-db", hot}, e); code != exitOK {
		t.Fatalf("warnings must still exit 0, got %d", code)
	}
	rep := decodeDoctor(t, out.String())
	for _, c := range rep.Checks {
		if c.Name == "logs.error" {
			if c.Status != checkWarn {
				t.Fatalf("logs = %+v", c)
			}
			return
		}
	}
	t.Fatal("logs check missing")
}

func TestDoctorColdConnectivity(t *testing.T) {
	hot := makeHotDB(t, 1)
	fs := &fakeSystem{
		env:     map[string]string{"UFOT_COLD_DSN": "postgres://u:p@127.0.0.1:5432/oc"},
		dialErr: errors.New("connection refused"),
	}
	e, out, _ := newTestEnv(t, fs)
	if code := run([]string{"--json", "doctor", "--hot-db", hot}, e); code != exitError {
		t.Fatalf("exit = %d, want 1", code)
	}
	rep := decodeDoctor(t, out.String())
	for _, c := range rep.Checks {
		if c.Name == "cold_db" {
			if c.Status != checkFail || !strings.Contains(c.Detail, "127.0.0.1:5432") {
				t.Fatalf("cold = %+v", c)
			}
			return
		}
	}
	t.Fatal("cold check missing")
}

func TestDoctorTextOutput(t *testing.T) {
	hot := makeHotDB(t, 1)
	fs := &fakeSystem{resp: []cmdResp{{prefix: "journalctl", out: ""}}}
	e, out, _ := newTestEnv(t, fs)
	if code := run([]string{"doctor", "--hot-db", hot}, e); code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out.String(), "结果: OK") {
		t.Fatalf("stdout = %q", out.String())
	}
}

func decodeDoctor(t *testing.T, raw string) doctorReport {
	t.Helper()
	var env struct {
		OK      bool         `json:"ok"`
		Command string       `json:"command"`
		Data    doctorReport `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("json: %v (%s)", err, raw)
	}
	if env.Command != "doctor" {
		t.Fatalf("command = %q", env.Command)
	}
	return env.Data
}

func TestDoctorAuditStore(t *testing.T) {
	hot := makeHotDB(t, 1)

	// Empty UFOT_COLD_DSN ⇒ audit store degraded (WARN, not fail).
	fs := &fakeSystem{resp: []cmdResp{{prefix: "journalctl", out: ""}}}
	e, out, _ := newTestEnv(t, fs)
	if code := run([]string{"--json", "doctor", "--hot-db", hot}, e); code != exitOK {
		t.Fatalf("empty-DSN exit = %d, want 0", code)
	}
	rep := decodeDoctor(t, out.String())
	var got *check
	for i := range rep.Checks {
		if rep.Checks[i].Name == "audit_store" {
			got = &rep.Checks[i]
		}
	}
	if got == nil {
		t.Fatal("audit_store check missing")
	}
	if got.Status != checkWarn || !strings.Contains(got.Detail, "degraded") {
		t.Fatalf("empty-DSN audit_store = %+v, want warn/degraded", got)
	}

	// Configured but unreachable ⇒ FAIL.
	fs2 := &fakeSystem{
		resp:    []cmdResp{{prefix: "journalctl", out: ""}},
		env:     map[string]string{"UFOT_COLD_DSN": "postgres://u:p@127.0.0.1:5432/oc"},
		dialErr: errors.New("connection refused"),
	}
	e2, out2, _ := newTestEnv(t, fs2)
	if code := run([]string{"--json", "doctor", "--hot-db", hot}, e2); code != exitError {
		t.Fatalf("unreachable audit_store exit = %d, want 1", code)
	}
	rep2 := decodeDoctor(t, out2.String())
	for _, c := range rep2.Checks {
		if c.Name == "audit_store" {
			if c.Status != checkFail || !strings.Contains(c.Detail, "127.0.0.1:5432") {
				t.Fatalf("unreachable audit_store = %+v, want fail", c)
			}
			return
		}
	}
	t.Fatal("audit_store check missing on unreachable case")
}
