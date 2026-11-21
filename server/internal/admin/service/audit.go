package service

import (
	"context"
	"fmt"

	"github.com/yingo-server/UFOT/server/internal/admin/auditshim"
	"github.com/yingo-server/UFOT/server/internal/admin/identity"
)

// AuditService serves the append-only journal as a sequential cursor stream
// (journalctl semantics, docs §6): the ONLY pagination mechanism is
// since_id=<last id seen>. No offset, no random access, no mutation.
type AuditService struct {
	reader AuditReader
	audit  *auditshim.Shim
}

// NewAuditService builds the service.
func NewAuditService(reader AuditReader, audit *auditshim.Shim) *AuditService {
	return &AuditService{reader: reader, audit: audit}
}

// List returns events with id > sinceID in ascending id order.
func (s *AuditService) List(ctx context.Context, actor *identity.Admin, sinceID int64, limit int) (AuditPage, error) {
	if s.reader == nil {
		return AuditPage{}, ErrNotConfigured
	}
	if sinceID < 0 {
		return AuditPage{}, fmt.Errorf("%w: since_id must be >= 0", ErrInvalid)
	}
	limit = normalizeLimit(limit, 500, 1000)
	// Ask for one extra row to learn has_more without a second query.
	rows, err := s.reader.ListAudit(ctx, sinceID, limit+1)
	if err != nil {
		return AuditPage{}, err
	}
	prev := sinceID
	for i, r := range rows {
		if r.ID <= prev {
			return AuditPage{}, fmt.Errorf("audit: journal order violated at index %d (id %d after %d)", i, r.ID, prev)
		}
		prev = r.ID
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	next := sinceID
	if len(rows) > 0 {
		next = rows[len(rows)-1].ID
	}
	// Auditing the audit read is mandatory (security-spec §5.5): who read
	// what filter and how many rows.
	if s.audit != nil {
		s.audit.Record(ctx, auditshim.Meta{
			Actor: actorName(actor), Action: "audit.read", Subject: "audit",
			Detail: map[string]any{
				"since_id": sinceID, "limit": limit,
				"count": len(rows), "has_more": hasMore,
			},
		})
	}
	return AuditPage{Items: rows, NextSinceID: next, HasMore: hasMore}, nil
}
