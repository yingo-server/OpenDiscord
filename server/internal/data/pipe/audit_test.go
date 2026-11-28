package pipe

// audit_test.go: coverage for the §5.5 audit-trail writes wired into pipe
// (recall, membership mutation, call start/end). A FakeAudit stands in for the
// pg ColdDB journal so tests can assert BOTH "an event was appended" and "no
// PII/SDP leaked into detail", and inject a failing journal to prove the
// best-effort path never blocks the destructive operation.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yingo-server/UFOT/server/internal/data/pipe/pipefakes"
	"github.com/yingo-server/UFOT/server/internal/data/pipe/wire"
	"github.com/yingo-server/UFOT/server/internal/data/store"
	"github.com/yingo-server/UFOT/server/internal/pkg/log"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
)

// FakeAudit implements store.AuditJournal, recording events and optionally
// failing (err) to exercise the swallow-and-log degradation.
type FakeAudit struct {
	mu     sync.Mutex
	events []store.AuditEvent
	err    error
}

func (f *FakeAudit) Append(ctx context.Context, ev store.AuditEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	return f.err
}

func (f *FakeAudit) snapshot() []store.AuditEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.AuditEvent(nil), f.events...)
}

// auditHot wraps hotBundle but resolves identities so conv addMember's
// checkedUser gate passes (the base fake only returns ErrNotFound).
type auditHot struct {
	hotBundle
	users map[int64]*store.User
}

func (h auditHot) GetUser(ctx context.Context, uid int64) (*store.User, error) {
	if u, ok := h.users[uid]; ok {
		return u, nil
	}
	return nil, store.ErrNotFound
}

// newAuditPipe assembles a pipe whose cold deps carry the given FakeAudit.
func newAuditPipe(t *testing.T, fa *FakeAudit) (*Pipe, *pipefakes.FakeMsgStore, *pipefakes.FakeConvStore, *pipefakes.FakeSysStore) {
	t.Helper()
	msgs := pipefakes.NewFakeMsgStore()
	convs := pipefakes.NewFakeConvStore()
	sys := pipefakes.NewFakeSysStore()
	hot := auditHot{
		hotBundle: hotBundle{w: msgs, r: msgs, conv: convs, sys: sys},
		users:     map[int64]*store.User{8: {UID: 8, Status: 0}},
	}
	p := NewStores(nil, hot, ColdDeps{Audit: fa}, log.For("data-test"))
	p.fl.interval = 5 * time.Millisecond
	return p, msgs, convs, sys
}

// findAudit returns the first event whose Action matches, or nil.
func findAudit(events []store.AuditEvent, action string) *store.AuditEvent {
	for i := range events {
		if events[i].Action == action {
			return &events[i]
		}
	}
	return nil
}

// detailMap decodes an event's JSON detail into a generic map.
func detailMap(t *testing.T, ev *store.AuditEvent) map[string]any {
	t.Helper()
	if ev == nil {
		t.Fatal("event is nil")
	}
	var m map[string]any
	if err := json.Unmarshal(ev.Detail, &m); err != nil {
		t.Fatalf("detail decode: %v (%s)", err, ev.Detail)
	}
	return m
}

// TestRecallWritesAudit: a successful recall appends one message.recall event
// carrying msg_id/conv_id and — critically — NOT the message body.
func TestRecallWritesAudit(t *testing.T) {
	fa := &FakeAudit{}
	p, msgs, convs, _ := newAuditPipe(t, fa)
	ctx := context.Background()
	cid, _ := convs.CreateConv(ctx, &store.ConvMeta{CType: 2, CreatedBy: 7, CreatedAt: 1}, 7)
	const body = "topsecret-message-body"
	if err := msgs.AppendMessages(ctx, []store.PendingMsg{{
		MsgID: 55, Cid: cid, Seq: 1, SenderID: 7, ClientMsgID: "m1",
		Content: []byte(body), SentAt: nowSec(),
	}}); err != nil {
		t.Fatalf("seed message: %v", err)
	}

	p.dispatchReq(ctx, mkReq(uint32(kinds.KindMsgRecall), cid, 7,
		`{"msg_id":55,"conv_id":`+itoa(cid)+`}`))

	ev := findAudit(fa.snapshot(), "message.recall")
	if ev == nil {
		t.Fatal("no message.recall audit event")
	}
	m := detailMap(t, ev)
	if int64(m["msg_id"].(float64)) != 55 || int64(m["conv_id"].(float64)) != cid {
		t.Fatalf("detail missing ids: %v", m)
	}
	if int64(m["target_sender"].(float64)) != 7 {
		t.Fatalf("target_sender wrong: %v", m)
	}
	if strings.Contains(string(ev.Detail), body) {
		t.Fatalf("audit detail leaked message body: %s", ev.Detail)
	}
}

// TestMemberChangeWritesAudit: adding a member appends a conv.member.add event
// with conv_id/target_uid/role.
func TestMemberChangeWritesAudit(t *testing.T) {
	fa := &FakeAudit{}
	p, _, convs, _ := newAuditPipe(t, fa)
	ctx := context.Background()
	cid, _ := convs.CreateConv(ctx, &store.ConvMeta{CType: 2, CreatedBy: 7, CreatedAt: 1}, 7)

	body, _ := wire.Marshal(wire.MemberOp{UserIDs: []uint64{8}, Role: 0})
	pl, _ := wire.Marshal(map[string]any{"op": uint32(convOpAddMember), "convId": uint64(cid), "body": body})
	p.dispatchReq(ctx, Req{Kind: 930, ConvID: uint64(cid), UserID: 7, Payload: pl})

	ev := findAudit(fa.snapshot(), "conv.member.add")
	if ev == nil {
		t.Fatal("no conv.member.add audit event")
	}
	m := detailMap(t, ev)
	if int64(m["conv_id"].(float64)) != cid || int64(m["target_uid"].(float64)) != 8 {
		t.Fatalf("conv.member.add detail wrong: %v", m)
	}
}

// TestCallStartEndWritesAudit: OFFER appends call.start, HANGUP appends
// call.end, and neither detail carries SDP/candidate payload.
func TestCallStartEndWritesAudit(t *testing.T) {
	fa := &FakeAudit{}
	p, _, _, sys := newAuditPipe(t, fa)
	enableCalls(t, sys)
	ctx := context.Background()
	cid := twoMemberConv(t, p, p.hot)

	offer, _ := wire.Marshal(wire.CallSignal{
		CallID: "call-x", ConvID: uint64(cid), ToUserID: 8, FromUserID: 7,
		SDP: "v=0 SECRET-SDP", Candidate: "cand-secret", Media: "audio",
	})
	p.dispatchReq(ctx, mkReq(uint32(kinds.KindCallOffer), cid, 7, string(offer)))

	start := findAudit(fa.snapshot(), "call.start")
	if start == nil {
		t.Fatal("no call.start audit event")
	}
	sm := detailMap(t, start)
	if sm["call_id"].(string) != "call-x" || int64(sm["peer_uid"].(float64)) != 8 {
		t.Fatalf("call.start detail wrong: %v", sm)
	}
	if strings.Contains(string(start.Detail), "SECRET-SDP") || strings.Contains(string(start.Detail), "cand-secret") {
		t.Fatalf("call.start leaked SDP/candidate: %s", start.Detail)
	}

	hangup, _ := wire.Marshal(wire.CallSignal{
		CallID: "call-x", ConvID: uint64(cid), Reason: "hangup", FromUserID: 7,
	})
	p.dispatchReq(ctx, mkReq(uint32(kinds.KindCallHangup), cid, 7, string(hangup)))

	end := findAudit(fa.snapshot(), "call.end")
	if end == nil {
		t.Fatal("no call.end audit event")
	}
	em := detailMap(t, end)
	if em["reason"].(string) != "hangup" || int64(em["peer_uid"].(float64)) != 8 {
		t.Fatalf("call.end detail wrong: %v", em)
	}
	if _, ok := em["duration_ms"]; !ok {
		t.Fatalf("call.end missing duration_ms: %v", em)
	}
	if strings.Contains(string(end.Detail), "SECRET-SDP") {
		t.Fatalf("call.end leaked SDP: %s", end.Detail)
	}
}

// TestAuditFailureDoesNotBlockRecall: a journal that errors must NOT stop the
// recall from succeeding (the row is still soft-deleted).
func TestAuditFailureDoesNotBlockRecall(t *testing.T) {
	fa := &FakeAudit{err: errors.New("audit journal down")}
	p, msgs, convs, _ := newAuditPipe(t, fa)
	ctx := context.Background()
	cid, _ := convs.CreateConv(ctx, &store.ConvMeta{CType: 2, CreatedBy: 7, CreatedAt: 1}, 7)
	if err := msgs.AppendMessages(ctx, []store.PendingMsg{{
		MsgID: 9, Cid: cid, Seq: 1, SenderID: 7, ClientMsgID: "m2",
		Content: []byte("x"), SentAt: nowSec(),
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	p.dispatchReq(ctx, mkReq(uint32(kinds.KindMsgRecall), cid, 7,
		`{"msg_id":9,"conv_id":`+itoa(cid)+`}`))

	rows, err := msgs.ReadHot(ctx, cid, 0, 10, false)
	if err != nil || len(rows) != 1 {
		t.Fatalf("read after recall: %v rows=%d", err, len(rows))
	}
	if rows[0].Recalled == 0 {
		t.Fatal("recall was blocked by a failing audit journal")
	}
}

// TestAuditSkippedWithoutCold: Audit == nil (no cold store) → recall still
// works, no panic, no nil deref.
func TestAuditSkippedWithoutCold(t *testing.T) {
	p, msgs, convs, _ := newTestPipe(t) // ColdDeps{} → Audit nil
	ctx := context.Background()
	cid, _ := convs.CreateConv(ctx, &store.ConvMeta{CType: 2, CreatedBy: 7, CreatedAt: 1}, 7)
	if err := msgs.AppendMessages(ctx, []store.PendingMsg{{
		MsgID: 3, Cid: cid, Seq: 1, SenderID: 7, ClientMsgID: "m3",
		Content: []byte("x"), SentAt: nowSec(),
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	p.dispatchReq(ctx, mkReq(uint32(kinds.KindMsgRecall), cid, 7,
		`{"msg_id":3,"conv_id":`+itoa(cid)+`}`))

	rows, err := msgs.ReadHot(ctx, cid, 0, 10, false)
	if err != nil || len(rows) != 1 || rows[0].Recalled == 0 {
		t.Fatalf("recall with nil audit: err=%v rows=%d", err, len(rows))
	}
}

// TestRecallIdempotentSecondAttempt: recalling the SAME message twice must
// NOT surface as 404 on the retry — findMsg located the row, so MarkRecalled's
// ErrNotFound means "already recalled". The second request gets a success
// reply on the MSG_RECALL kind with already=true (and no re-broadcast).
func TestRecallIdempotentSecondAttempt(t *testing.T) {
	fa := &FakeAudit{}
	p, msgs, convs, _ := newAuditPipe(t, fa)
	ctx := context.Background()
	cid, _ := convs.CreateConv(ctx, &store.ConvMeta{CType: 2, CreatedBy: 7, CreatedAt: 1}, 7)
	if err := msgs.AppendMessages(ctx, []store.PendingMsg{{
		MsgID: 77, Cid: cid, Seq: 1, SenderID: 7, ClientMsgID: "m-idem",
		Content: []byte("x"), SentAt: nowSec(),
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	req := mkReq(uint32(kinds.KindMsgRecall), cid, 7,
		`{"msg_id":77,"conv_id":`+itoa(cid)+`}`)

	// First recall: row soft-deleted; the tombstone is enqueued on the fan-out
	// plane (not started here — the unit tests drive dispatch directly).
	p.dispatchReq(ctx, req)
	rows, err := msgs.ReadHot(ctx, cid, 0, 10, false)
	if err != nil || len(rows) != 1 || rows[0].Recalled == 0 {
		t.Fatalf("first recall did not soft-delete: err=%v rows=%v", err, rows)
	}

	// Second recall of the same msg_id with a store whose MarkRecalled refuses
	// already-recalled rows (the real sqlite/pg semantics: `WHERE recalled=0`
	// misses → ErrNotFound).
	strict := &strictRecallStore{HotStores: p.hot, rows: msgs}
	prev := p.hot
	p.hot = strict
	defer func() { p.hot = prev }()

	p.dispatchReq(ctx, req)
	second := waitEnv(t, p.Outbound())
	if second.Kind != uint32(kinds.KindMsgRecall) {
		t.Fatalf("second recall reply kind = %d, want %d (got error? payload=%s)",
			second.Kind, kinds.KindMsgRecall, second.Payload)
	}
	var res struct {
		MsgID   uint64 `json:"msg_id"`
		ConvID  uint64 `json:"conv_id"`
		Already bool   `json:"already"`
	}
	if err := wire.Unmarshal(second.Payload, &res); err != nil {
		t.Fatalf("already-payload decode: %v (%s)", err, second.Payload)
	}
	if !res.Already || res.MsgID != 77 {
		t.Fatalf("second recall payload = %+v, want already=true msg_id=77", res)
	}

	// Exactly ONE message.recall audit event total (no duplicate audit on the
	// idempotent replay).
	n := 0
	for _, ev := range fa.snapshot() {
		if ev.Action == "message.recall" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("message.recall audit events = %d, want 1", n)
	}
	// No second tombstone broadcast.
	select {
	case extra := <-p.OutboundFan():
		t.Fatalf("unexpected second tombstone broadcast: kind=%d", extra.Kind)
	default:
	}
}

// strictRecallStore mirrors the real store semantics: MarkRecalled returns
// ErrNotFound when `WHERE recalled=0` matches nothing (already recalled).
type strictRecallStore struct {
	HotStores
	rows *pipefakes.FakeMsgStore
}

func (s *strictRecallStore) MarkRecalled(ctx context.Context, cid, msgID, senderID, at int64) (*store.HotMsg, error) {
	rows, err := s.rows.ReadHot(ctx, cid, 0, 100, false)
	if err != nil {
		return nil, err
	}
	for _, m := range rows {
		if m.MsgID == msgID && m.SenderID == senderID {
			if m.Recalled != 0 {
				return nil, fmt.Errorf("strict: msg %d: %w", msgID, store.ErrNotFound)
			}
			return s.rows.MarkRecalled(ctx, cid, msgID, senderID, at)
		}
	}
	return nil, fmt.Errorf("strict: msg %d: %w", msgID, store.ErrNotFound)
}
