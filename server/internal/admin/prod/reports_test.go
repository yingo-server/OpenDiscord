package prod

import (
	"context"
	"errors"
	"testing"

	"github.com/yingo-server/UFOT/server/internal/admin/service"
	"github.com/yingo-server/UFOT/server/internal/data/store"
)

func TestReportsLifecycle(t *testing.T) {
	db := hotDB(t)
	mustExec(t, db, `INSERT INTO sys_reports (reporter, target_msg, target_user, reason, status, at)
VALUES (10, 900, 0, 'spam', 0, 100)`)
	mustExec(t, db, `INSERT INTO sys_reports (reporter, target_msg, target_user, reason, status, at)
VALUES (11, 0, 55, 'abuse', 1, 200)`)

	r := NewReports(db)
	ctx := context.Background()

	page, err := r.ListReports(ctx, service.ReportFilter{Status: -1, Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("total=%d items=%d", page.Total, len(page.Items))
	}
	if page.Items[0].ID != 2 {
		t.Fatalf("newest first: got id %d", page.Items[0].ID)
	}

	pending, err := r.ListReports(ctx, service.ReportFilter{Status: service.ReportPending, Limit: 10})
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if pending.Total != 1 || pending.Items[0].Reporter != 10 || pending.Items[0].Reason != "spam" {
		t.Fatalf("pending page = %+v", pending)
	}

	rep, err := r.GetReport(ctx, 1)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if rep.Status != service.ReportPending || rep.TargetMsg != 900 {
		t.Fatalf("report = %+v", rep)
	}

	// happy path: pending → handling, note recorded
	updated, err := r.UpdateReport(ctx, 1, service.ReportPending, service.ReportHandling, "ops1", "looking")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Status != service.ReportHandling || updated.HandledBy != "ops1" || updated.Note != "looking" {
		t.Fatalf("updated = %+v", updated)
	}

	// stale transition: compare-and-set refuses
	if _, err := r.UpdateReport(ctx, 1, service.ReportPending, service.ReportResolved, "ops2", ""); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("stale transition err = %v, want ErrConflict", err)
	}
	if _, err := r.GetReport(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing report err = %v, want ErrNotFound", err)
	}
	if _, err := r.UpdateReport(ctx, 999, service.ReportPending, service.ReportResolved, "", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing report update err = %v, want ErrNotFound", err)
	}

	// finish the state machine
	done, err := r.UpdateReport(ctx, 1, service.ReportHandling, service.ReportResolved, "ops1", "resolved")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if done.Status != service.ReportResolved {
		t.Fatalf("status = %d", done.Status)
	}
}

func TestReportsNilDBIsNotConfigured(t *testing.T) {
	r := NewReports(nil)
	if _, err := r.ListReports(context.Background(), service.ReportFilter{Status: -1}); !errors.Is(err, service.ErrNotConfigured) {
		t.Fatalf("err = %v", err)
	}
	if _, err := r.GetReport(context.Background(), 1); !errors.Is(err, service.ErrNotConfigured) {
		t.Fatalf("err = %v", err)
	}
}

func TestReportsMigrationIsAdditive(t *testing.T) {
	// The new hot migration must not disturb existing sys tables.
	db := hotDB(t)
	for _, table := range []string{"sys_presence", "sys_rate_limit", "sys_feature_flags", "sys_reports"} {
		var name string
		err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name = ?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("table %s: %v", table, err)
		}
	}
}
