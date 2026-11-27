package policy

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
)

// Store loads policy.json and serves atomic snapshots. Reads are lock-free
// (atomic.Value); a failed load or validation keeps the previous snapshot —
// the mechanism stays robust against bad policy (docs §4: 坏策略拒载继续用旧).
type Store struct {
	path string

	cur atomic.Value // Snapshot

	// mu serializes Reload (SIGHUP handler + CLI may race).
	mu sync.Mutex

	version atomic.Int64
}

// New creates a Store over path. The file must exist, parse and validate;
// otherwise New fails (startup-time strictness — 78 config error).
func New(path string) (*Store, error) {
	s := &Store{path: path}
	p, err := LoadFile(path)
	if err != nil {
		return nil, fmt.Errorf("policy: load %s: %w", path, err)
	}
	s.cur.Store(Snapshot{Policy: p, Version: 1})
	s.version.Store(1)
	return s, nil
}

// Current returns the active snapshot. Always safe to call concurrently.
func (s *Store) Current() Snapshot {
	return s.cur.Load().(Snapshot)
}

// Path returns the configured policy file path.
func (s *Store) Path() string { return s.path }

// Reload re-reads and re-validates the file. On any failure (missing file,
// bad JSON, invalid fields) it returns the error and the OLD snapshot stays
// active. On success the version increments and the changed top-level keys
// are returned for incremental frame fan-out.
func (s *Store) Reload() (changed []string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, err := LoadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("policy: reload %s: %w", s.path, err)
	}
	old := s.Current().Policy
	next := s.version.Add(1)
	s.cur.Store(Snapshot{Policy: p, Version: int(next)})
	return Diff(old, p), nil
}

// LoadFile reads, unmarshals and validates one policy file.
func LoadFile(path string) (Policy, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Policy{}, err
	}
	return Load(body)
}

// Load parses and validates raw policy bytes.
func Load(body []byte) (Policy, error) {
	p := Default()
	if err := unmarshal(body, &p); err != nil { // into defaults: partial docs merge
		return Policy{}, fmt.Errorf("policy: decode: %w", err)
	}
	if errs := Validate(p); len(errs) > 0 {
		return Policy{}, fmt.Errorf("policy: invalid: %w", errors.Join(errs...))
	}
	return p, nil
}

// Diff lists top-level keys whose values changed between two policies.
// Keys: hot_retention_days, archive, backup, trim, rate_limit.
func Diff(old, new Policy) []string {
	var out []string
	if old.HotRetentionDays != new.HotRetentionDays {
		out = append(out, "hot_retention_days")
	}
	if old.Archive != new.Archive {
		out = append(out, "archive")
	}
	if old.Backup != new.Backup {
		out = append(out, "backup")
	}
	if !equalTrim(old.Trim, new.Trim) {
		out = append(out, "trim")
	}
	if old.RateLimit != new.RateLimit {
		out = append(out, "rate_limit")
	}
	if old.WritePath != new.WritePath {
		out = append(out, "write_path")
	}
	if old.Capacity != new.Capacity {
		out = append(out, "capacity")
	}
	if old.Auth != new.Auth {
		out = append(out, "auth")
	}
	return out
}

func equalTrim(a, b []TrimRule) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
