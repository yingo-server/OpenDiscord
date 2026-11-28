package policy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writePolicy(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const goodPolicy = `{
  "hot_retention_days": 30,
  "archive": {"cron": "0 4 * * *", "batch_size": 500, "batch_pause_ms": 50, "active_exempt": true},
  "backup": {"cron": "0 3 * * *", "dir": "/var/backups", "keep_days": 7},
  "trim": [{"table": "sys_rate_limit", "retention_days": 7}],
  "rate_limit": {"msg_per_user_per_min": 60, "login_per_ip_per_min": 10}
}`

func TestLoadGood(t *testing.T) {
	p, err := Load([]byte(goodPolicy))
	if err != nil {
		t.Fatal(err)
	}
	if p.HotRetentionDays != 30 || p.Archive.BatchSize != 500 || !p.Archive.ActiveExempt {
		t.Fatalf("unexpected parse: %+v", p)
	}
	if p.Backup.Dir != "/var/backups" || len(p.Trim) != 1 || p.Trim[0].Table != "sys_rate_limit" {
		t.Fatalf("unexpected parse: %+v", p)
	}
}

func TestValidate(t *testing.T) {
	if errs := Validate(Default()); len(errs) != 0 {
		t.Fatalf("Default() must validate, got %v", errs)
	}
	bad := Default()
	bad.HotRetentionDays = 0
	bad.Archive.BatchSize = 100001
	bad.Archive.Cron = ""
	bad.Backup.KeepDays = 0
	bad.Trim = append(bad.Trim, TrimRule{Table: "msg_messages_hot", RetentionDays: 1})
	bad.RateLimit.MsgPerUserPerMin = 0
	errs := Validate(bad)
	if len(errs) < 6 {
		t.Fatalf("expected >=6 field errors, got %d: %v", len(errs), errs)
	}
	msg := errors.Join(errs...).Error()
	for _, want := range []string{"hot_retention_days", "archive.batch_size", "archive.cron", "backup.keep_days", "whitelist", "msg_per_user_per_min"} {
		if !contains(msg, want) {
			t.Errorf("missing field error mentioning %q in:\n%s", want, msg)
		}
	}
	// ttl/retention exclusivity
	errs = Validate(Policy{HotRetentionDays: 1,
		Archive:   ArchiveConfig{Cron: "x", BatchSize: 1},
		Backup:    BackupConfig{Cron: "x", Dir: "d", KeepDays: 1},
		RateLimit: RateLimitConfig{MsgPerUserPerMin: 1, LoginPerIPPerMin: 1},
		Trim:      []TrimRule{{Table: "sys_rate_limit", TTLHours: 5, RetentionDays: 5}}})
	if len(errs) != 1 {
		t.Fatalf("expected exactly 1 trim error, got %v", errs)
	}
	errs = Validate(Policy{HotRetentionDays: 1,
		Archive:   ArchiveConfig{Cron: "x", BatchSize: 1},
		Backup:    BackupConfig{Cron: "x", Dir: "d", KeepDays: 1},
		RateLimit: RateLimitConfig{MsgPerUserPerMin: 1, LoginPerIPPerMin: 1},
		Trim:      []TrimRule{{Table: "sys_rate_limit"}}})
	if len(errs) != 1 {
		t.Fatalf("expected exactly 1 trim error (missing ttl/retention), got %v", errs)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestStoreKeepOldOnBadReload(t *testing.T) {
	path := writePolicy(t, goodPolicy)
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	before := s.Current()
	if before.Version != 1 {
		t.Fatalf("initial version = %d, want 1", before.Version)
	}

	// 1. bad JSON
	if err := os.WriteFile(path, []byte(`{ not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reload(); err == nil {
		t.Fatal("bad JSON reload must fail")
	}
	if cur := s.Current(); cur.Version != 1 || !reflect.DeepEqual(cur.Policy, before.Policy) {
		t.Fatal("snapshot must be unchanged after failed reload")
	}

	// 2. invalid policy (retention 0, bad trim table)
	bad := map[string]any{"hot_retention_days": 0, "trim": []map[string]any{{"table": "evil", "retention_days": 1}}}
	body, _ := json.Marshal(bad)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reload(); err == nil {
		t.Fatal("invalid policy reload must fail")
	}
	if cur := s.Current(); cur.Version != 1 || !reflect.DeepEqual(cur.Policy, before.Policy) {
		t.Fatal("snapshot must be unchanged after invalid reload")
	}

	// 3. good reload bumps version and reports the changed key
	newBody := goodPolicy[:14] + `"hot_retention_days": 45,` + goodPolicy[14:]
	_ = newBody
	changed := `{"hot_retention_days": 45}`
	if err := os.WriteFile(path, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	changedKeys, err := s.Reload()
	if err != nil {
		t.Fatal(err)
	}
	cur := s.Current()
	if cur.Version != 2 {
		t.Fatalf("version = %d, want 2", cur.Version)
	}
	if cur.Policy.HotRetentionDays != 45 {
		t.Fatalf("hot_retention_days = %d, want 45 (partial doc must merge into defaults)", cur.Policy.HotRetentionDays)
	}
	// A partial doc is a full declarative truth: unspecified keys fall back to
	// defaults, so Diff may also report keys the file omitted. What matters is
	// that the changed key is reported and nothing else is silently lost.
	found := false
	for _, k := range changedKeys {
		if k == "hot_retention_days" {
			found = true
		}
	}
	if !found {
		t.Fatalf("changed keys = %v, want to contain hot_retention_days", changedKeys)
	}

	// 4. New() on a bad file fails outright
	badPath := writePolicy(t, `}`)
	if _, err := New(badPath); err == nil {
		t.Fatal("New on invalid file must fail")
	}
}

// TestWritePathPolicy (module 3): the write_path section must load with
// defaults when absent, parse explicit values, and reject out-of-range ones.
func TestWritePathPolicy(t *testing.T) {
	// absent → defaults (the document is merged onto Default())
	p, err := Load([]byte(goodPolicy))
	if err != nil {
		t.Fatal(err)
	}
	if p.WritePath.BatchSize != DefaultWriteBatchSize || p.WritePath.FlushIntervalMS != DefaultWriteFlushIntervalMS {
		t.Fatalf("absent write_path must default: %+v", p.WritePath)
	}
	// explicit values round-trip
	body := `{"write_path": {"batch_size": 2000, "flush_interval_ms": 25}}`
	p, err = Load([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if p.WritePath.BatchSize != 2000 || p.WritePath.FlushIntervalMS != 25 {
		t.Fatalf("explicit write_path lost: %+v", p.WritePath)
	}
	// A value of 0 means "unset → use the built-in default" (the flusher treats
	// >0 as set, and Load merges onto Default()), so 0 is legal and must not be
	// rejected. Only genuinely out-of-range values are errors.
	for _, ok := range []string{
		`{"write_path": {"batch_size": 0}}`,
		`{"write_path": {"flush_interval_ms": 0}}`,
	} {
		if _, err := Load([]byte(ok)); err != nil {
			t.Errorf("write_path %q should be accepted (0 = use default): %v", ok, err)
		}
	}
	// out-of-range values are rejected
	for _, bad := range []string{
		`{"write_path": {"batch_size": 99999}}`,
		`{"write_path": {"batch_size": -1}}`,
		`{"write_path": {"flush_interval_ms": 5000}}`,
	} {
		if _, err := Load([]byte(bad)); err == nil {
			t.Errorf("write_path %q must be rejected", bad)
		}
	}
}

// TestWritePathDiff: Diff reports "write_path" when the tunables change.
func TestWritePathDiff(t *testing.T) {
	a := Default()
	b := Default()
	b.WritePath.BatchSize = 2000
	if got := Diff(a, b); !reflect.DeepEqual(got, []string{"write_path"}) {
		t.Fatalf("Diff = %v, want [write_path]", got)
	}
	if got := Diff(a, a); len(got) != 0 {
		t.Fatalf("Diff of identical policies = %v, want empty", got)
	}
}
