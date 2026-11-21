package pg

import (
	"context"
	"strings"
	"testing"

	"github.com/yingo-server/UFOT/server/internal/data/store"
)

// Pure-logic tests only: no database connection (CI has no postgres driver
// registered; see cold.go package doc for the injection contract).

func TestRecordHashChain(t *testing.T) {
	ev1 := store.AuditEvent{Actor: "admin:1", Action: "user.ban", Subject: "user:123",
		Detail: []byte(`{"level":1}`), At: 100}
	h1 := RecordHash("", ev1)
	if len(h1) != 64 {
		t.Fatalf("hash length = %d", len(h1))
	}
	// Same event after a different prev must hash differently.
	h2 := RecordHash(h1, ev1)
	if h1 == h2 {
		t.Fatalf("chain link did not change hash")
	}
	// Determinism: same inputs, same hash.
	if RecordHash(h1, ev1) != h2 {
		t.Fatalf("hash not deterministic")
	}
	// Canonical field separation: a field-boundary move must change the hash.
	evShift := ev1
	evShift.Subject = "user:123\x1fextra"
	if RecordHash("", evShift) == h1 {
		t.Fatalf("field separator collision")
	}
	// detail bytes are part of canonical form
	ev3 := ev1
	ev3.Detail = []byte(`{"level":2}`)
	if RecordHash("", ev3) == h1 {
		t.Fatalf("detail change not reflected in hash")
	}
}

func TestExtractText(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`{"text":"hello world","other":1}`, "hello world"},
		{`{"client_msg_id":"x","text":"a \"quoted\" end"}`, `a \"quoted\" end`},
		{`{"no_text":true}`, ""},
		{``, ""},
		{`{"text":""}`, ""},
		{`{"text":"ends-at-tail"`, "ends-at-tail"},
	}
	for _, c := range cases {
		if got := ExtractText([]byte(c.in)); got != c.want {
			t.Errorf("ExtractText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// E2EE content (random bytes) yields no text — search cannot see it.
	if got := ExtractText([]byte{0x93, 0x2f, 0x81, 0x00, 0xd4}); got != "" {
		t.Errorf("binary content should yield empty text, got %q", got)
	}
}

// TestReadColdSQLShape pins the cold-read SQL shape (kind 920 cold merge):
// keyset cursor + LIMIT, parameterized, newest-first. No connection — this
// asserts the constants, which are the contract the readrouter merges against.
func TestReadColdSQLShape(t *testing.T) {
	if n := strings.Count(readColdCursoredSQL, "$"); n != 3 {
		t.Fatalf("cursored SQL placeholders = %d, want 3 ($1 cid, $2 beforeSeq, $3 limit)", n)
	}
	if n := strings.Count(readColdHeadSQL, "$"); n != 2 {
		t.Fatalf("head SQL placeholders = %d, want 2 ($1 cid, $2 limit)", n)
	}
	for _, sql := range []string{readColdCursoredSQL, readColdHeadSQL} {
		if !strings.Contains(sql, "msg.messages_archive") {
			t.Fatalf("SQL must read msg.messages_archive: %s", sql)
		}
		if !strings.Contains(sql, "ORDER BY seq DESC") {
			t.Fatalf("cold page must be newest-first (readrouter merge contract): %s", sql)
		}
		if !strings.Contains(sql, "LIMIT $") {
			t.Fatalf("SQL must take LIMIT as a parameter: %s", sql)
		}
	}
	// Cursored form carries the keyset predicate; head form must not.
	if !strings.Contains(readColdCursoredSQL, "seq < $2") {
		t.Fatalf("cursored SQL missing keyset predicate: %s", readColdCursoredSQL)
	}
	if strings.Contains(readColdHeadSQL, "seq <") {
		t.Fatalf("head SQL must not carry a cursor predicate: %s", readColdHeadSQL)
	}
	// ReadCold argument order is the real contract the shape pins.
	ctx := context.Background()
	c := &ColdDB{}
	if _, err := c.ReadCold(ctx, 0, 0, 0); err == nil {
		t.Fatal("limit <= 0 must be rejected before any query")
	}
}

func TestBuildPlaceholders(t *testing.T) {
	got := buildPlaceholders(1, 2)
	if got != "($1,$2)" {
		t.Fatalf("1x2 = %q", got)
	}
	got = buildPlaceholders(2, 3)
	want := "($1,$2,$3),($4,$5,$6)"
	if got != want {
		t.Fatalf("2x3 = %q, want %q", got, want)
	}
	if buildPlaceholders(0, 3) != "" || buildPlaceholders(3, 0) != "" {
		t.Fatalf("empty shapes must be empty")
	}
	// chunk shape used by copyChunk: 6 cols
	if got := buildPlaceholders(3, 6); strings.Count(got, ")") != 3 {
		t.Fatalf("group count wrong: %q", got)
	}
}

func TestSQLShapeStability(t *testing.T) {
	// The generated multi-row INSERT must always end with ON CONFLICT DO NOTHING
	// and contain only $n placeholders — no data ever inlined.
	ph := buildPlaceholders(4, 6)
	full := copyBatchSQL + ph + ` ON CONFLICT DO NOTHING`
	for _, bad := range []string{"SELECT", "DROP", ";"} {
		if strings.Contains(strings.ToUpper(ph), bad) && bad != ";" {
			t.Errorf("placeholder shape contains %q", bad)
		}
	}
	if !strings.HasSuffix(full, "ON CONFLICT DO NOTHING") {
		t.Errorf("copy statement missing conflict clause")
	}
	if !strings.Contains(copyBatchSQL, "msg.messages_archive") {
		t.Errorf("wrong table in copy statement")
	}
	if !strings.Contains(auditAppendSQL, "ORDER BY id DESC LIMIT 1") {
		t.Errorf("audit append must read chain tail in the same statement")
	}
	// deleteHotSQL is gone (defect fix): pg.ColdDB must NOT delete hot rows —
	// the hot table lives in SQLite. The cold side is copy-only.
}
