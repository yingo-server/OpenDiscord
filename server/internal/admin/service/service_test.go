package service

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/yingo-server/UFOT/server/internal/admin/auditshim"
	"github.com/yingo-server/UFOT/server/internal/admin/bridgeproxy"
	"github.com/yingo-server/UFOT/server/internal/admin/identity"
	"github.com/yingo-server/UFOT/server/internal/data/store"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
)

type dispatchCall struct {
	Kind    uint32
	Target  bridgeproxy.Target
	Payload []byte
}

type fakeDispatcher struct {
	mu    sync.Mutex
	calls []dispatchCall
	err   error
}

func (f *fakeDispatcher) InjectFrame(_ context.Context, kind uint32, target bridgeproxy.Target, payload []byte) (bridgeproxy.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return bridgeproxy.Result{}, f.err
	}
	f.calls = append(f.calls, dispatchCall{Kind: kind, Target: target, Payload: append([]byte(nil), payload...)})
	return bridgeproxy.Result{StatusCode: 202, Topic: "sys.admin", Kind: kind}, nil
}

func (f *fakeDispatcher) snapshot() []dispatchCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dispatchCall(nil), f.calls...)
}

type fakeJournal struct {
	mu     sync.Mutex
	events []store.AuditEvent
	fail   bool
}

func (j *fakeJournal) Append(_ context.Context, ev store.AuditEvent) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.fail {
		return context.DeadlineExceeded
	}
	j.events = append(j.events, ev)
	return nil
}

func (j *fakeJournal) snapshot() []store.AuditEvent {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]store.AuditEvent(nil), j.events...)
}

type fakeUserRepo struct {
	users map[int64]UserRecord
}

func (r *fakeUserRepo) ListUsers(_ context.Context, f UserFilter) (UserPage, error) {
	var out []UserRecord
	for _, u := range r.users {
		out = append(out, u)
	}
	return UserPage{Items: out, Total: int64(len(out))}, nil
}

func (r *fakeUserRepo) GetUser(_ context.Context, uid int64) (*UserRecord, error) {
	u, ok := r.users[uid]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &u, nil
}

func admin(role identity.Role) *identity.Admin {
	return &identity.Admin{AdminID: 1, Username: "tester", Role: role}
}

func TestBanInjectsSecurityFrameAndAudits(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	journal := &fakeJournal{}
	disp := &fakeDispatcher{}
	svc := NewUserService(&fakeUserRepo{users: map[int64]UserRecord{
		42: {UID: 42, DisplayName: "victim", Phone: "13812341234"},
	}}, disp, auditshim.New(journal))
	svc.SetClock(func() time.Time { return now })

	res, err := svc.Ban(context.Background(), admin(identity.RoleOps), 42, BanInput{
		Level: 2, Reason: "spam", Days: 7,
	})
	if err != nil {
		t.Fatalf("ban: %v", err)
	}
	if !res.Banned || res.Until != now.Add(7*24*time.Hour).Unix() {
		t.Fatalf("unexpected result: %+v", res)
	}
	calls := disp.snapshot()
	if len(calls) != 1 {
		t.Fatalf("dispatch calls = %d, want 1", len(calls))
	}
	call := calls[0]
	if call.Kind != kinds.KindSecurityRequest {
		t.Fatalf("frame kind = %d, want %d (SECURITY_REQUEST)", call.Kind, kinds.KindSecurityRequest)
	}
	if call.Target.UserID != 42 {
		t.Fatalf("frame target uid = %d, want 42", call.Target.UserID)
	}
	var payload map[string]any
	if err := json.Unmarshal(call.Payload, &payload); err != nil {
		t.Fatalf("payload json: %v", err)
	}
	if payload["op"] != "ban" || payload["uid"].(float64) != 42 || payload["level"].(float64) != 2 {
		t.Fatalf("payload = %v", payload)
	}
	evs := journal.snapshot()
	if len(evs) != 1 || evs[0].Action != "user.ban" || evs[0].Subject != "user:42" {
		t.Fatalf("audit events = %+v", evs)
	}
}

func TestBanUnknownUserNoDispatch(t *testing.T) {
	disp := &fakeDispatcher{}
	svc := NewUserService(&fakeUserRepo{users: map[int64]UserRecord{}}, disp, auditshim.New(&fakeJournal{}))
	_, err := svc.Ban(context.Background(), admin(identity.RoleOps), 99, BanInput{Level: 1})
	if err == nil {
		t.Fatal("expected not found")
	}
	if len(disp.snapshot()) != 0 {
		t.Fatal("dispatched for unknown user")
	}
}

func TestAuditAppendFailureDoesNotFailBan(t *testing.T) {
	journal := &fakeJournal{fail: true}
	disp := &fakeDispatcher{}
	shim := auditshim.New(journal)
	svc := NewUserService(&fakeUserRepo{users: map[int64]UserRecord{1: {UID: 1}}}, disp, shim)
	if _, err := svc.Kick(context.Background(), admin(identity.RoleOps), 1, "test"); err != nil {
		t.Fatalf("kick failed due to audit: %v", err)
	}
	if len(disp.snapshot()) != 1 {
		t.Fatal("primary operation did not run")
	}
	if shim.Failures() != 1 {
		t.Fatalf("audit failure counter = %d, want 1", shim.Failures())
	}
}

func TestShowMasksPhone(t *testing.T) {
	svc := NewUserService(&fakeUserRepo{users: map[int64]UserRecord{
		7: {UID: 7, Phone: "13812341234", Email: "alice@example.com"},
	}}, &fakeDispatcher{}, auditshim.New(&fakeJournal{}))
	u, err := svc.Show(context.Background(), admin(identity.RoleReview), 7)
	if err != nil {
		t.Fatal(err)
	}
	if u.Phone != "138****1234" || u.Email != "a***@example.com" {
		t.Fatalf("masking failed: %+v", u)
	}
}

type fakeAuditReader struct {
	mu    sync.Mutex
	rows  []AuditRecord
	calls []struct {
		since int64
		limit int
	}
	reverse bool
}

func (r *fakeAuditReader) ListAudit(_ context.Context, sinceID int64, limit int) ([]AuditRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, struct {
		since int64
		limit int
	}{sinceID, limit})
	var out []AuditRecord
	for _, row := range r.rows {
		if row.ID > sinceID {
			out = append(out, row)
		}
		if len(out) == limit {
			break
		}
	}
	if r.reverse {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, nil
}

func TestAuditCursorPaginationOrderAndAppendOnly(t *testing.T) {
	var rows []AuditRecord
	for i := int64(1); i <= 12; i++ {
		rows = append(rows, AuditRecord{ID: i, Actor: "a", Action: "x", Subject: "s", At: i})
	}
	reader := &fakeAuditReader{rows: rows}
	journal := &fakeJournal{}
	svc := NewAuditService(reader, auditshim.New(journal))

	var ids []int64
	since := int64(0)
	pages := 0
	for {
		page, err := svc.List(context.Background(), admin(identity.RoleReview), since, 3)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		for _, r := range page.Items {
			ids = append(ids, r.ID)
		}
		if !page.HasMore {
			if page.NextSinceID != 12 {
				t.Fatalf("final next_since_id = %d, want 12", page.NextSinceID)
			}
			break
		}
		if page.NextSinceID != page.Items[len(page.Items)-1].ID {
			t.Fatalf("next_since_id %d != last item %d", page.NextSinceID, page.Items[len(page.Items)-1].ID)
		}
		since = page.NextSinceID
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(ids) != 12 {
		t.Fatalf("ids = %v (len %d), want 1..12", ids, len(ids))
	}
	for i, id := range ids {
		if id != int64(i+1) {
			t.Fatalf("ids not strictly ascending from since_id: %v", ids)
		}
	}
	// Self-audit: one append per read, and nothing was mutated.
	evs := journal.snapshot()
	if len(evs) != pages {
		t.Fatalf("audit-read events = %d, want %d", len(evs), pages)
	}
	for _, ev := range evs {
		if ev.Action != "audit.read" {
			t.Fatalf("unexpected self-audit action %q", ev.Action)
		}
	}
	if len(reader.rows) != 12 {
		t.Fatal("reader rows mutated")
	}
}

func TestAuditRejectsOutOfOrderJournal(t *testing.T) {
	reader := &fakeAuditReader{rows: []AuditRecord{{ID: 5}, {ID: 4}}}
	svc := NewAuditService(reader, auditshim.New(&fakeJournal{}))
	if _, err := svc.List(context.Background(), admin(identity.RoleReview), 0, 10); err == nil {
		t.Fatal("out-of-order journal accepted")
	}
}

func TestAuditLimitClampedAndNegativeSinceRejected(t *testing.T) {
	reader := &fakeAuditReader{}
	svc := NewAuditService(reader, auditshim.New(&fakeJournal{}))
	if _, err := svc.List(context.Background(), admin(identity.RoleSuper), -1, 10); err == nil {
		t.Fatal("negative since_id accepted")
	}
	if _, err := svc.List(context.Background(), admin(identity.RoleSuper), 0, 5000); err != nil {
		t.Fatal(err)
	}
	if reader.calls[0].limit != 1001 {
		t.Fatalf("reader limit = %d, want 1001 (clamped 1000 + lookahead)", reader.calls[0].limit)
	}
}

func TestReportStateMachine(t *testing.T) {
	repo := &fakeReportRepo{reports: map[int64]Report{
		1: {ID: 1, Status: ReportPending},
	}}
	disp := &fakeDispatcher{}
	svc := NewReportService(repo, disp, auditshim.New(&fakeJournal{}))
	ctx := context.Background()
	actor := admin(identity.RoleReview)

	rep, err := svc.Handle(ctx, actor, 1, "claim", "")
	if err != nil || rep.Status != ReportHandling {
		t.Fatalf("claim: %+v %v", rep, err)
	}
	// Idempotent claim: no new frame.
	before := len(disp.snapshot())
	if _, err := svc.Handle(ctx, actor, 1, "claim", ""); err != nil {
		t.Fatal(err)
	}
	if len(disp.snapshot()) != before {
		t.Fatal("idempotent claim dispatched again")
	}
	rep, err = svc.Handle(ctx, actor, 1, "resolve", "ok")
	if err != nil || rep.Status != ReportResolved {
		t.Fatalf("resolve: %+v %v", rep, err)
	}
	if _, err := svc.Handle(ctx, actor, 1, "reject", "late"); err == nil {
		t.Fatal("closed report accepted a new transition")
	}
}

type fakeReportRepo struct {
	reports map[int64]Report
}

func (r *fakeReportRepo) ListReports(_ context.Context, f ReportFilter) (ReportPage, error) {
	var out []Report
	for _, rep := range r.reports {
		if f.Status < 0 || rep.Status == f.Status {
			out = append(out, rep)
		}
	}
	return ReportPage{Items: out, Total: int64(len(out))}, nil
}

func (r *fakeReportRepo) GetReport(_ context.Context, id int64) (*Report, error) {
	rep, ok := r.reports[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &rep, nil
}

func (r *fakeReportRepo) UpdateReport(_ context.Context, id int64, from, to int, handledBy, note string) (*Report, error) {
	rep, ok := r.reports[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	if rep.Status != from {
		return nil, ErrConflict
	}
	rep.Status = to
	rep.HandledBy = handledBy
	rep.Note = note
	r.reports[id] = rep
	return &rep, nil
}
