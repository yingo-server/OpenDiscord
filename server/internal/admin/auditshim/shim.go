// Package auditshim is the declarative audit decorator for admin services
// (docs/admin-data-design.md §2): service methods wrap their side effect in
// auditshim.Do and an AuditEvent{Actor,Action,Subject,Detail,At} is appended
// to the cold audit journal automatically.
//
// Failure policy: an audit append failure must NEVER fail or roll back the
// primary operation. It is logged at ERROR level and counted in metrics so
// the audit-chain alarm can fire separately.
package auditshim

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/yingo-server/UFOT/server/internal/data/store"
	"github.com/yingo-server/UFOT/server/internal/pkg/log"
)

var lg = log.For("admin.auditshim")

// Meta describes the audit event for one service method invocation.
type Meta struct {
	Actor   string // admin username or "system"
	Action  string // e.g. "user.ban"
	Subject string // e.g. "user:123"
	Detail  any    // JSON-marshalable; MUST NOT contain PII, tokens or content
	// DryRun suppresses the audit append: dry-run is an estimate, not an
	// action, per docs §8 ("不产审计动作行").
	DryRun bool
}

// Shim appends audit events. It is safe for concurrent use.
type Shim struct {
	journal  store.AuditJournal
	now      func() time.Time
	failures atomic.Uint64
}

// New builds a shim. A nil journal is tolerated (degraded mode) but every
// append then records an ERROR and increments the failure counter.
func New(journal store.AuditJournal) *Shim {
	return &Shim{journal: journal, now: time.Now}
}

// SetClock injects a test clock.
func (s *Shim) SetClock(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

// Failures returns the number of failed audit appends (monotonic).
func (s *Shim) Failures() uint64 {
	if s == nil {
		return 0
	}
	return s.failures.Load()
}

// Do runs fn and, only when fn succeeds and this is not a dry run, appends an
// audit event. The return values of fn are passed through unchanged; an audit
// failure is logged but never returned.
func Do[T any](ctx context.Context, s *Shim, meta Meta, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	if fn == nil {
		return zero, fmt.Errorf("auditshim: nil operation")
	}
	out, err := fn(ctx)
	if err != nil {
		return out, err
	}
	if s != nil && !meta.DryRun {
		s.Record(ctx, meta)
	}
	return out, nil
}

// Record appends one event. It never returns an error to the caller: audit
// failure is deliberately non-fatal (see package doc). Exposed for events that
// are not wrapped around a service method (login attempts, RBAC denials,
// audit reads).
func (s *Shim) Record(ctx context.Context, meta Meta) {
	if s == nil || meta.DryRun {
		return
	}
	if meta.Actor == "" {
		meta.Actor = "system"
	}
	if meta.Subject == "" {
		meta.Subject = "-"
	}
	detail := []byte(`{}`)
	if meta.Detail != nil {
		switch d := meta.Detail.(type) {
		case json.RawMessage:
			if len(d) > 0 {
				detail = d
			}
		case []byte:
			if len(d) > 0 && json.Valid(d) {
				detail = d
			}
		default:
			b, err := json.Marshal(d)
			if err != nil {
				lg.Error("audit detail marshal failed", "action", meta.Action, "err", err.Error())
				b = []byte(`{"marshal_error":true}`)
			}
			detail = b
		}
	}
	if s.journal == nil {
		s.failures.Add(1)
		lg.Error("audit journal not configured", "action", meta.Action, "subject", meta.Subject)
		return
	}
	ev := store.AuditEvent{
		Actor:   meta.Actor,
		Action:  meta.Action,
		Subject: meta.Subject,
		Detail:  detail,
		At:      s.now().Unix(),
	}
	if err := s.journal.Append(ctx, ev); err != nil {
		s.failures.Add(1)
		lg.Error("audit append failed (primary operation kept)", "action", meta.Action, "subject", meta.Subject, "err", err.Error())
	}
}
