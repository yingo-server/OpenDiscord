package pipe

// audit.go: the pipe's audit-trail write helper (security review P1, §5.5).
//
// The audit journal lives on the COLD store (store.AuditJournal, hash-chained
// append-only log). It is OPTIONAL: a pipe assembled without a cold store has
// p.cold.Audit == nil and every audit call is a no-op. Auditing is also
// strictly BEST-EFFORT — a failed Append is logged and swallowed, never
// propagated to (or blocking) the destructive operation it records. Losing an
// audit row can never mean "the user cannot recall the message".
//
// PII red line: callers pass only ids/enums/counts in detail. Never message
// bodies, phone numbers, or SDP/candidate payloads.

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/yingo-server/UFOT/server/internal/data/pipe/wire"
	"github.com/yingo-server/UFOT/server/internal/data/store"
)

// audit appends one event to the cold audit journal. actor is the acting uid
// (formatted "user:<id>"); subject names the affected resource; detail is
// JSON-serialized verbatim into AuditEvent.Detail. A nil journal or a failing
// Append is logged and ignored (best-effort; never blocks the caller).
func (p *Pipe) audit(ctx context.Context, action string, actor int64, subject string, detail map[string]any) {
	if auditJournalNil(p.cold.Audit) {
		return // no cold store: auditing disabled (assembly logs the absent-cold WARN)
	}
	var payload []byte
	if detail != nil {
		b, err := json.Marshal(detail)
		if err != nil {
			// detail is always ids/enums we control; a marshal failure is a bug,
			// not a reason to block the operation.
			p.lg.Warn("pipe: audit detail marshal failed", "action", action, "err", err.Error())
			return
		}
		payload = b
	}
	ev := store.AuditEvent{
		Actor:   userActor(actor),
		Action:  action,
		Subject: subject,
		Detail:  payload,
		At:      nowSec(),
	}
	if err := p.cold.Audit.Append(ctx, ev); err != nil {
		p.lg.Warn("pipe: audit append failed", "action", action, "subject", subject, "err", err.Error())
	}
}

// auditConvMember records a successful conversation membership mutation
// (op add/remove/setRole). One event per target uid; detail carries only
// conv_id/target_uid/role — never any message content.
func (p *Pipe) auditConvMember(ctx context.Context, actor int64, op uint32, cid int64, body []byte) {
	var action string
	switch op {
	case convOpAddMember:
		action = "conv.member.add"
	case convOpRemoveMember:
		action = "conv.member.remove"
	case convOpSetRole:
		action = "conv.member.role"
	default:
		return
	}
	var m wire.MemberOp
	if err := wire.Unmarshal(body, &m); err != nil {
		return
	}
	if cid == 0 {
		cid = int64(m.ConvID)
	}
	for _, rawUID := range m.UserIDs {
		p.audit(ctx, action, actor, "conv:"+itoa(cid), map[string]any{
			"conv_id":    cid,
			"target_uid": int64(rawUID),
			"role":       int(m.Role),
		})
	}
}

// userActor formats a uid as the audit actor id ("user:123").
func userActor(uid int64) string {
	if uid <= 0 {
		return "user:0"
	}
	return "user:" + itoa(uid)
}

// auditJournalNil reports whether j is unusable (absent). It treats BOTH an
// untyped nil interface AND a typed-nil pointer (e.g. a (*pg.ColdDB)(nil)
// stashed into the interface when the cold store is unavailable) as absent —
// otherwise calling Append on the typed nil would panic instead of degrading.
func auditJournalNil(j store.AuditJournal) bool {
	if j == nil {
		return true
	}
	v := reflect.ValueOf(j)
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return v.IsNil()
	}
	return false
}
