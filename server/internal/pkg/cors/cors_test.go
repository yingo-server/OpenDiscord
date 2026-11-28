package cors

import (
	"errors"
	"testing"
)

// Unit-level coverage of the policy resolution and the Allow decision table.
// The integration behaviour (headers on real responses) is covered by the
// authsvc-level cors_test.go.

func TestNewModeNormalization(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ModeAllowAny},
		{"allow_any", ModeAllowAny},
		{"ALLOW_LIST", ModeAllowList},
		{"  off  ", ModeOff},
	}
	for _, tc := range cases {
		p, err := New(tc.in, "https://x.example", nil)
		if err != nil {
			t.Fatalf("New(%q): %v", tc.in, err)
		}
		if p.Mode != tc.want {
			t.Fatalf("New(%q).Mode = %q, want %q", tc.in, p.Mode, tc.want)
		}
	}
}

func TestNewUnknownModeIsError(t *testing.T) {
	// A security switch must fail closed on a typo, not fall back to the
	// most permissive mode.
	for _, bad := range []string{"of", "deny", "any", "allow-all", "allow_any "} {
		_ = bad // "allow_any " trims to a valid value; see below
	}
	for _, bad := range []string{"of", "deny", "any", "allow-all", "allow_any\tX"} {
		_, err := New(bad, "", nil)
		if !errors.Is(err, ErrUnknownMode) {
			t.Fatalf("New(%q) err = %v, want ErrUnknownMode", bad, err)
		}
	}
	// The trimmed valid value still works.
	if _, err := New("allow_any ", "", nil); err != nil {
		t.Fatalf("New(trimmed allow_any): %v", err)
	}
}

func TestAllowsDecisionTable(t *testing.T) {
	type tc struct {
		name   string
		p      Policy
		origin string
		want   bool
	}
	cases := []tc{
		{"empty origin never allowed", Policy{Mode: ModeAllowAny}, "", false},
		{"https ok", Policy{Mode: ModeAllowAny}, "https://c.example", true},
		{"http ok (dev)", Policy{Mode: ModeAllowAny}, "http://localhost:5173", true},
		{"null rejected", Policy{Mode: ModeAllowAny}, "null", false},
		{"garbage rejected", Policy{Mode: ModeAllowAny}, "not-a-url", false},
		{"ftp rejected", Policy{Mode: ModeAllowAny}, "ftp://x", false},
		{"list: public origin", Policy{Mode: ModeAllowList, PublicOrigin: "https://ui.example"}, "https://ui.example", true},
		{"list: extra origin", Policy{Mode: ModeAllowList, PublicOrigin: "https://ui.example", Origins: []string{"https://alt.example"}}, "https://alt.example", true},
		{"list: unknown origin", Policy{Mode: ModeAllowList, PublicOrigin: "https://ui.example"}, "https://evil.example", false},
		{"list: case-insensitive", Policy{Mode: ModeAllowList, PublicOrigin: "https://ui.example"}, "https://UI.EXAMPLE", true},
		{"list: empty extra entries ignored", Policy{Mode: ModeAllowList, PublicOrigin: "https://ui.example", Origins: []string{"", " "}}, "https://ui.example", true},
		{"off: everything denied", Policy{Mode: ModeOff, PublicOrigin: "https://ui.example"}, "https://ui.example", false},
	}
	for _, c := range cases {
		if got := c.p.Allows(c.origin); got != c.want {
			t.Fatalf("%s: Allows(%q) = %v, want %v", c.name, c.origin, got, c.want)
		}
	}
}
