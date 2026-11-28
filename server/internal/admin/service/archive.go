package service

import (
	"context"
	"fmt"

	"github.com/yingo-server/UFOT/server/internal/admin/auditshim"
	"github.com/yingo-server/UFOT/server/internal/admin/bridgeproxy"
	"github.com/yingo-server/UFOT/server/internal/admin/identity"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
)

// ArchiveService fronts the archive job ledger. The job itself is a data-layer
// job.Unit shared by cron/API/CLI; the admin plane only triggers it through a
// 980 JOB_TRIGGER frame and reads the ledger.
type ArchiveService struct {
	repo     ArchiveRepository
	dispatch Dispatcher
	audit    *auditshim.Shim
}

// NewArchiveService builds the service.
func NewArchiveService(repo ArchiveRepository, dispatch Dispatcher, audit *auditshim.Shim) *ArchiveService {
	return &ArchiveService{repo: repo, dispatch: dispatch, audit: audit}
}

// Status returns the latest ledger state.
func (s *ArchiveService) Status(ctx context.Context) (ArchiveStatus, error) {
	if s.repo == nil {
		return ArchiveStatus{}, ErrNotConfigured
	}
	return s.repo.Status(ctx)
}

// Run triggers an archive run. A non-dry-run while another run is active
// returns ErrConflict (HTTP 409). Dry-run returns the estimate and writes
// neither a frame nor an audit row.
func (s *ArchiveService) Run(ctx context.Context, actor *identity.Admin, dryRun bool) (ArchiveRun, error) {
	if s.repo == nil {
		return ArchiveRun{}, ErrNotConfigured
	}
	st, err := s.repo.Status(ctx)
	if err != nil {
		return ArchiveRun{}, err
	}
	if st.Running && !dryRun {
		return ArchiveRun{}, fmt.Errorf("%w: archive already running (run %d)", ErrConflict, st.LastRunID)
	}
	if dryRun {
		est, err := s.repo.Estimate(ctx)
		if err != nil {
			return ArchiveRun{}, err
		}
		return ArchiveRun{State: "dry_run", DryRun: true, Estimate: &est}, nil
	}
	run, err := s.repo.BeginRun(ctx)
	if err != nil {
		return ArchiveRun{}, err
	}
	return auditshim.Do(ctx, s.audit, auditshim.Meta{
		Actor: actorName(actor), Action: "archive.run", Subject: "job:archive",
		Detail: map[string]any{"run_id": run.RunID},
	}, func(ctx context.Context) (ArchiveRun, error) {
		_, derr := injectJSON(ctx, s.dispatch, kinds.KindJobTrigger, bridgeproxy.Target{}, map[string]any{
			"job": "archive", "dry_run": false, "run_id": run.RunID, "by": actorName(actor),
		})
		if derr != nil {
			_ = s.repo.FinishRun(context.Background(), run.RunID, 0, "failed")
			return ArchiveRun{}, derr
		}
		run.FrameKind = kinds.KindJobTrigger
		run.State = "running"
		return run, nil
	})
}
