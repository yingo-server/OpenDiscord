package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/yingo-server/UFOT/server/internal/admin/auditshim"
	"github.com/yingo-server/UFOT/server/internal/admin/bridgeproxy"
	"github.com/yingo-server/UFOT/server/internal/admin/identity"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
)

// ReportService owns the report state machine:
//
//	0 pending ──claim──► 1 handling ──resolve──► 2 resolved
//	    │                    │
//	    └──────reject────────┴──────────────► 3 rejected
//
// Transitions into the current state are idempotent successes. Any other
// transition returns ErrConflict.
type ReportService struct {
	repo     ReportRepository
	dispatch Dispatcher
	audit    *auditshim.Shim
}

// NewReportService builds the service.
func NewReportService(repo ReportRepository, dispatch Dispatcher, audit *auditshim.Shim) *ReportService {
	return &ReportService{repo: repo, dispatch: dispatch, audit: audit}
}

// List returns one page of reports.
func (s *ReportService) List(ctx context.Context, actor *identity.Admin, f ReportFilter) (ReportPage, error) {
	if s.repo == nil {
		return ReportPage{}, ErrNotConfigured
	}
	f.Limit = normalizeLimit(f.Limit, 50, 200)
	f.Offset = normalizeOffset(f.Offset)
	page, err := s.repo.ListReports(ctx, f)
	if err != nil {
		return ReportPage{}, mapRepoErr(err)
	}
	page.Limit, page.Offset = f.Limit, f.Offset
	return page, nil
}

// Show returns one report.
func (s *ReportService) Show(ctx context.Context, actor *identity.Admin, id int64) (*Report, error) {
	if s.repo == nil {
		return nil, ErrNotConfigured
	}
	if id <= 0 {
		return nil, fmt.Errorf("%w: report id must be positive", ErrInvalid)
	}
	r, err := s.repo.GetReport(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return r, nil
}

// Handle applies one state-machine action ("claim"/"start", "resolve",
// "reject") and injects a 960 report.handle frame.
func (s *ReportService) Handle(ctx context.Context, actor *identity.Admin, id int64, action, note string) (*Report, error) {
	if s.repo == nil {
		return nil, ErrNotConfigured
	}
	if id <= 0 {
		return nil, fmt.Errorf("%w: report id must be positive", ErrInvalid)
	}
	action = strings.ToLower(strings.TrimSpace(action))
	target, ok := reportTarget(action)
	if !ok {
		return nil, fmt.Errorf("%w: unknown action %q", ErrInvalid, action)
	}
	cur, err := s.repo.GetReport(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if cur.Status == target {
		return cur, nil // idempotent: the desired state is already effective
	}
	if cur.Status == ReportResolved || cur.Status == ReportRejected {
		return nil, fmt.Errorf("%w: report is already closed (status %d)", ErrConflict, cur.Status)
	}
	if target == ReportHandling && cur.Status != ReportPending {
		return nil, fmt.Errorf("%w: cannot claim report in status %d", ErrConflict, cur.Status)
	}
	updated, err := s.repo.UpdateReport(ctx, id, cur.Status, target, actorName(actor), note)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	payload := map[string]any{
		"op": "report.handle", "report_id": id, "action": action,
		"from_status": cur.Status, "to_status": target,
		"note": note, "by": actorName(actor),
	}
	_, derr := auditshim.Do(ctx, s.audit, auditshim.Meta{
		Actor: actorName(actor), Action: "report.handle", Subject: "report:" + itoa(id),
		Detail: map[string]any{"from": cur.Status, "to": target, "action": action, "note_len": len(note)},
	}, func(ctx context.Context) (struct{}, error) {
		_, err := injectJSON(ctx, s.dispatch, kinds.KindSecurityRequest,
			bridgeproxy.Target{}, payload)
		return struct{}{}, err
	})
	if derr != nil {
		// Best-effort rollback so the local projection does not diverge from
		// the data layer when the bridge rejects the command.
		_, _ = s.repo.UpdateReport(context.Background(), id, target, cur.Status, cur.HandledBy, cur.Note)
		return nil, derr
	}
	return updated, nil
}

func reportTarget(action string) (int, bool) {
	switch action {
	case "claim", "start", "handling":
		return ReportHandling, true
	case "resolve", "resolved", "approve":
		return ReportResolved, true
	case "reject", "rejected", "dismiss":
		return ReportRejected, true
	default:
		return 0, false
	}
}
