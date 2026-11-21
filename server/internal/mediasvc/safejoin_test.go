package mediasvc

import (
	"path/filepath"
	"testing"
)

// TestSafeJoin is the table-driven guard: legitimate sharded paths resolve
// under the base, everything that could escape is refused.
func TestSafeJoin(t *testing.T) {
	base := "/var/lib/ufot/media"

	ok := []struct {
		rel  string
		want string
	}{
		{"ab/cd" + "/deadbeef", "/var/lib/ufot/media/ab/cd/deadbeef"},
		{"thumbs/ab/abcd/x.thumb", "/var/lib/ufot/media/thumbs/ab/abcd/x.thumb"},
		{"a/b/c", "/var/lib/ufot/media/a/b/c"},
		{"flat", "/var/lib/ufot/media/flat"},
		{"a/./b", "/var/lib/ufot/media/a/b"}, // a "." component is harmless
	}
	for _, c := range ok {
		got, err := safeJoin(base, c.rel)
		if err != nil {
			t.Fatalf("safeJoin(%q) unexpected error: %v", c.rel, err)
		}
		if got != c.want {
			t.Fatalf("safeJoin(%q) = %q, want %q", c.rel, got, c.want)
		}
	}

	bad := []string{
		"../etc/passwd",
		"/etc/passwd",
		"a/../../b",
		"../../root",
		"a/../..",
		"..",
		"",
		"   ",
		"foo/../../../etc/shadow",
	}
	for _, rel := range bad {
		if got, err := safeJoin(base, rel); err == nil {
			t.Fatalf("safeJoin(%q) = %q, want an error (must not escape base)", rel, got)
		}
	}
}

// TestSafeJoinPrefixBoundary makes sure a sibling directory whose name merely
// shares a string prefix with the base (e.g. media_evil vs media) is not
// accepted — the guard is separator-aware, not a bare HasPrefix.
func TestSafeJoinPrefixBoundary(t *testing.T) {
	// A relative "." resolves to the base itself: allowed (equal, not escape).
	if got, err := safeJoin("/var/media", "."); err != nil || filepath.Clean(got) != "/var/media" {
		t.Fatalf("safeJoin(base, \".\") = %q, %v; want the base", got, err)
	}
}
