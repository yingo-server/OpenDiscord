package prod

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/yingo-server/UFOT/server/internal/admin/service"
	"github.com/yingo-server/UFOT/server/internal/data/store"
	"github.com/yingo-server/UFOT/server/internal/data/store/pg"
	"github.com/yingo-server/UFOT/server/internal/pkg/log"
)

// AuditReader adapts the cold journal (sys.audit_log) to the admin read port.
//
// Degraded mode: without a cold PostgreSQL the journal simply does not exist
// locally. Rather than failing the console, the port returns an EMPTY page and
// logs one WARN per process — reads are diagnostic, writes remain the red line
// (see logJournal).
type AuditReader struct {
	reader   *pg.AuditReader
	lg       *log.Logger
	warnOnce sync.Once
}

// NewAuditReader builds the adapter. cold may be nil.
func NewAuditReader(cold *pg.ColdDB, lg *log.Logger) *AuditReader {
	if lg == nil {
		lg = log.For("admin.prod.audit")
	}
	return &AuditReader{reader: pg.NewAuditReader(cold), lg: lg}
}

// ListAudit returns journal rows with id > sinceID (ascending).
func (a *AuditReader) ListAudit(ctx context.Context, sinceID int64, limit int) ([]service.AuditRecord, error) {
	if a == nil || a.reader == nil {
		if a != nil {
			a.warnOnce.Do(func() {
				a.lg.Warn("审计冷库未配置：audit 读端口返回空页（配置 cold DB 后自动启用）")
			})
		}
		return nil, nil
	}
	rows, err := a.reader.ListAudit(ctx, sinceID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]service.AuditRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, service.AuditRecord{
			ID: r.ID, Actor: r.Actor, Action: r.Action,
			Subject: r.Subject, Detail: r.Detail, At: r.At,
		})
	}
	return out, nil
}

// logJournal is the degraded audit journal: no cold store, nowhere to persist.
//
// Every append emits one WARN with the event's semantic fields (never Detail —
// it may carry PII) and increments Dropped. Mutations still succeed: an
// admin-plane outage caused by a missing cold DB would be worse than a logged
// gap, and the assembly screams about the misconfiguration at startup.
type logJournal struct {
	lg      *log.Logger
	once    sync.Once
	dropped atomic.Uint64
}

// Append implements store.AuditJournal.
func (j *logJournal) Append(_ context.Context, ev store.AuditEvent) error {
	if j == nil {
		return nil
	}
	if j.lg != nil {
		j.once.Do(func() {
			j.lg.Warn("审计事件不落盘：cold DB 未配置（生产必须配置；详见 docs/security-spec.md §5.5）")
		})
		j.lg.Warn("audit dropped",
			"actor", ev.Actor, "action", ev.Action, "subject", ev.Subject)
	}
	j.dropped.Add(1)
	return nil
}

// Dropped reports how many audit events were not persisted.
func (j *logJournal) Dropped() uint64 {
	if j == nil {
		return 0
	}
	return j.dropped.Load()
}
