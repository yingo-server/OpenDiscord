package prod

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/yingo-server/UFOT/server/internal/admin/service"
	"github.com/yingo-server/UFOT/server/internal/data/store"
)

// Reports implements service.ReportRepository over the hot sys_reports table
// (migration hot/sys/0002_reports.sql).
//
// Why hot and not cold: the cold sys.reports projection has no Go adapter yet
// and冷库在降级部署中可能不存在。本适配器让审核队列在任何部署形态下都可用：
// 状态机（pending→handling→resolved/rejected）与审计由 service 层强制，
// 数据层的 960 report.handle 帧负责把权威状态写回冷库（接入后本表是本地投影）。
type Reports struct{ db *sql.DB }

// NewReports builds the adapter; nil db = not configured (503).
func NewReports(db *sql.DB) *Reports { return &Reports{db: db} }

const reportColumnsSQL = `id, reporter, target_msg, target_user, reason, status,
       COALESCE(handled_by, ''), COALESCE(note, ''), at`

func reportWhere(f service.ReportFilter) (string, []any) {
	if f.Status < 0 {
		return " WHERE 1=1", nil
	}
	return " WHERE status = ?", []any{f.Status}
}

// ListReports returns one page ordered newest-first.
func (r *Reports) ListReports(ctx context.Context, f service.ReportFilter) (service.ReportPage, error) {
	if r == nil || r.db == nil {
		return service.ReportPage{}, service.ErrNotConfigured
	}
	where, args := reportWhere(f)
	limit, offset := clampPage(f.Limit, 50, 200, f.Offset)

	var total int64
	if err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sys_reports"+where, args...).Scan(&total); err != nil {
		return service.ReportPage{}, fmt.Errorf("admin/prod: count reports: %w", err)
	}
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+reportColumnsSQL+" FROM sys_reports"+where+
			" ORDER BY at DESC, id DESC LIMIT ? OFFSET ?",
		append(args, limit, offset)...)
	if err != nil {
		return service.ReportPage{}, fmt.Errorf("admin/prod: list reports: %w", err)
	}
	defer rows.Close()

	items := make([]service.Report, 0, limit)
	for rows.Next() {
		rep, err := scanReport(rows)
		if err != nil {
			return service.ReportPage{}, fmt.Errorf("admin/prod: scan report: %w", err)
		}
		items = append(items, rep)
	}
	if err := rows.Err(); err != nil {
		return service.ReportPage{}, fmt.Errorf("admin/prod: report rows: %w", err)
	}
	return service.ReportPage{Items: items, Total: total, Limit: limit, Offset: offset}, nil
}

// GetReport returns one report or store.ErrNotFound.
func (r *Reports) GetReport(ctx context.Context, id int64) (*service.Report, error) {
	if r == nil || r.db == nil {
		return nil, service.ErrNotConfigured
	}
	if id <= 0 {
		return nil, fmt.Errorf("%w: report id must be positive", service.ErrInvalid)
	}
	rep, err := scanReport(r.db.QueryRowContext(ctx,
		"SELECT "+reportColumnsSQL+" FROM sys_reports WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("admin/prod: report %d: %w", id, store.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("admin/prod: get report %d: %w", id, err)
	}
	return &rep, nil
}

const updateReportSQL = `UPDATE sys_reports SET status = ?, handled_by = ?, note = ?
WHERE id = ? AND status = ?`

// UpdateReport performs the atomic compare-and-set transition. A row that is
// not in `from` yields service.ErrConflict (409) — the service state machine
// already decided the transition is legal, so a mismatch means a concurrent
// reviewer won the race.
func (r *Reports) UpdateReport(ctx context.Context, id int64, from, to int, handledBy, note string) (*service.Report, error) {
	if r == nil || r.db == nil {
		return nil, service.ErrNotConfigured
	}
	if id <= 0 {
		return nil, fmt.Errorf("%w: report id must be positive", service.ErrInvalid)
	}
	cur, err := r.GetReport(ctx, id)
	if err != nil {
		return nil, err
	}
	if cur.Status != from {
		return nil, fmt.Errorf("%w: report %d is in status %d, not %d",
			service.ErrConflict, id, cur.Status, from)
	}
	res, err := r.db.ExecContext(ctx, updateReportSQL, to, handledBy, note, id, from)
	if err != nil {
		return nil, fmt.Errorf("admin/prod: update report %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("%w: report %d changed concurrently", service.ErrConflict, id)
	}
	return r.GetReport(ctx, id)
}

func scanReport(s scanner) (service.Report, error) {
	var rep service.Report
	var targetMsg, targetUser sql.NullInt64
	var reason sql.NullString
	err := s.Scan(&rep.ID, &rep.Reporter, &targetMsg, &targetUser, &reason,
		&rep.Status, &rep.HandledBy, &rep.Note, &rep.At)
	if err != nil {
		return rep, err
	}
	rep.TargetMsg = targetMsg.Int64
	rep.TargetUser = targetUser.Int64
	rep.Reason = reason.String
	return rep, nil
}
