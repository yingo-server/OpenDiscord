package job

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/yingo-server/UFOT/server/internal/data/policy"
	"github.com/yingo-server/UFOT/server/internal/data/store"
)

// TrimRuleSource is the policy seam: it yields the trim rules to execute.
// The policy store satisfies it (policy.Store.Current().Policy.Trim).
type TrimRuleSource interface {
	TrimRules() []policy.TrimRule
}

// Trimmer deletes expired rows from the tables policy declares.
//
// Mechanism/policy split: the TABLES and their TTLs come from policy.trim
// (validated against policy.TrimWhitelist at load time); the DELETE
// statements are fixed in the store layer and looked up by table name. No
// policy value is ever concatenated into SQL.
//
// Two rule families:
//   - policy-driven: sys_rate_limit (window_start), identity_login_history
//     (at) — retention_days / ttl_hours from policy.trim.
//   - built-in: identity_refresh (expires_at), media_media
//     (created_at AND ref_count = 0) — always applied; they are expiry
//     semantics, not tunable retention policy.
//
// The trimmer needs NO cold store: every table it touches lives in the hot
// (SQLite) DB, so a skeleton deployment still runs it.
type Trimmer struct {
	T store.TrimTarget // the hot store (SQLite)

	// Rules returns the policy trim rules; nil means "no policy rules" and
	// only the built-in rules run.
	Rules TrimRuleSource

	// Now is the clock seam (unix seconds); nil = time.Now.
	Now func() int64

	// Vacuum, when set, is called after the deletes to reclaim free pages
	// (PRAGMA incremental_vacuum). Optional.
	Vacuum func(ctx context.Context) error

	// Log is an optional progress sink (the jobber logger).
	Log Logger
}

// Name implements Unit.
func (t *Trimmer) Name() string { return "trim" }

// builtInTrimRules are the always-on expiry rules. identity_refresh tokens
// expire (expires_at); media rows with ref_count = 0 are unreferenced garbage
// (created_at). Both live in the hot DB.
var builtInTrimRules = []builtInRule{
	{table: "identity_refresh", column: "expires_at", ttl: 0}, // ttl 0 = use the row's own expiry
	{table: "media_media", column: "created_at", ttl: 0},      // ttl 0 = mediaRetentionDays
}

// builtInRule is one built-in expiry rule.
type builtInRule struct {
	table  string
	column string
	ttl    int64 // seconds; 0 = special meaning per rule
}

// mediaRetentionDays is how long an unreferenced media row (ref_count = 0)
// is kept before the trimmer deletes it. A media row that is still referenced
// by a message is NEVER deleted (ref_count = 0 is part of the DELETE).
const mediaRetentionDays = 7

// Run executes one trim pass over every rule. A failing table does not abort
// the pass: the remaining tables are still trimmed and the first error is
// returned at the end (a trimmer must be robust — one missing table must not
// stop the rest).
func (t *Trimmer) Run(ctx context.Context, dryRun bool) (Result, error) {
	if t.T == nil {
		return Result{}, fmt.Errorf("trimmer: no trim target wired (assembly must set T)")
	}
	now := time.Now().Unix()
	if t.Now != nil {
		if n := t.Now(); n != 0 {
			now = n
		}
	}

	type target struct {
		table string
		until int64
	}
	var targets []target

	// policy-driven rules (retention_days / ttl_hours).
	for _, r := range t.policyRules() {
		if !policy.TrimWhitelist[r.Table] {
			continue // defense in depth: the policy loader already rejects this
		}
		ttl := int64(r.RetentionDays) * 24 * 3600
		if r.TTLHours >= 1 {
			ttl = int64(r.TTLHours) * 3600
		}
		if ttl <= 0 {
			continue
		}
		targets = append(targets, target{table: r.Table, until: now - ttl})
	}
	// built-in rules.
	for _, r := range builtInTrimRules {
		switch r.table {
		case "identity_refresh":
			// expires_at is absolute: the cutoff is "now" — the DELETE drops
			// rows whose own expires_at is in the past.
			targets = append(targets, target{table: r.table, until: now})
		case "media_media":
			targets = append(targets, target{table: r.table, until: now - mediaRetentionDays*24*3600})
		}
	}

	var total int64
	var firstErr error
	for _, tg := range targets {
		if ctx.Err() != nil {
			return Result{MovedRows: total}, ctx.Err()
		}
		if dryRun {
			// A dry run reports the plan without deleting anything. The row
			// COUNT is not computed here (that would be a full scan); the plan
			// itself is reported.
			continue
		}
		n, err := t.T.TrimTable(ctx, tg.table, tg.until)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("trimmer: %s: %w", tg.table, err)
			}
			if t.Log != nil {
				t.Log.Error("trim failed", "table", tg.table, "err", err.Error())
			}
			continue
		}
		total += n
		if t.Log != nil && n > 0 {
			t.Log.Info("trimmed table", "table", tg.table, "rows", n)
		}
	}

	if t.Vacuum != nil && !dryRun {
		if err := t.Vacuum(ctx); err != nil {
			// Space reclamation failure is not a trim failure — report it but
			// keep the deleted rows counted.
			if t.Log != nil {
				t.Log.Warn("incremental vacuum failed", "err", err.Error())
			}
		}
	}

	detail := fmt.Sprintf("trimmed %d rows across %d tables", total, len(targets))
	if dryRun {
		detail = fmt.Sprintf("would trim %d tables", len(targets))
	}
	return Result{MovedRows: total, ConvsTouched: len(targets), DryRun: dryRun, Detail: detail}, firstErr
}

// policyRules resolves the policy rules, tolerating a nil source.
func (t *Trimmer) policyRules() []policy.TrimRule {
	if t.Rules == nil {
		return nil
	}
	return t.Rules.TrimRules()
}

// PolicyRules adapts a policy snapshot to the TrimRuleSource port.
type PolicyRules struct {
	Rules []policy.TrimRule
}

// TrimRules implements TrimRuleSource.
func (p PolicyRules) TrimRules() []policy.TrimRule { return p.Rules }

// TrimTableNames returns the sorted list of tables the trimmer may touch
// (policy whitelist + built-ins). Useful for diagnostics and tests.
func TrimTableNames() []string {
	out := make([]string, 0, len(policy.TrimWhitelist)+len(builtInTrimRules))
	for t := range policy.TrimWhitelist {
		out = append(out, t)
	}
	for _, r := range builtInTrimRules {
		out = append(out, r.table)
	}
	sort.Strings(out)
	return out
}

// TrimTableNamesString renders TrimTableNames for logs.
func TrimTableNamesString() string { return strings.Join(TrimTableNames(), ",") }
