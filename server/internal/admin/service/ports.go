// Package service implements the admin domain operations. Handlers parse and
// render; services enforce business rules and talk to two kinds of ports:
//
//   - repositories for reads (users/reports/messages/audit/archive)
//   - the bridge Dispatcher for every mutation (translated to 5xx/96x frames)
//
// SQL lives exclusively in the admin store / data store packages.
package service

import (
	"context"
	"errors"

	"github.com/yingo-server/UFOT/server/internal/admin/bridgeproxy"
)

// Sentinel errors. Handlers map these onto HTTP status codes / error bodies.
var (
	ErrNotFound      = errors.New("service: not found")
	ErrConflict      = errors.New("service: conflict")
	ErrInvalid       = errors.New("service: invalid argument")
	ErrUnauthorized  = errors.New("service: unauthorized")
	ErrForbidden     = errors.New("service: forbidden")
	ErrUnavailable   = errors.New("service: unavailable")
	ErrNotConfigured = errors.New("service: repository not configured")
)

// Dispatcher is the service-facing bridge proxy seam. bridgeproxy.Proxy
// satisfies it; tests use a recording fake.
type Dispatcher interface {
	InjectFrame(ctx context.Context, kind uint32, target bridgeproxy.Target, payload []byte) (bridgeproxy.Result, error)
}

// ---------------------------------------------------------------- users

// UserFilter is a normalized user query.
type UserFilter struct {
	Query  string
	Status int // -1 = all
	Limit  int
	Offset int
}

// BanInfo is the effective ban shown on a user record.
type BanInfo struct {
	Level     int    `json:"level"`
	Reason    string `json:"reason,omitempty"`
	Until     int64  `json:"until"`
	CreatedBy string `json:"created_by,omitempty"`
	CreatedAt int64  `json:"created_at,omitempty"`
}

// UserRecord is the admin-side user projection. Phone/Email are masked by the
// service before they reach a handler or audit event (PII red line).
type UserRecord struct {
	UID         int64    `json:"uid"`
	DisplayName string   `json:"display_name"`
	Status      int      `json:"status"`
	CreatedAt   int64    `json:"created_at"`
	Ban         *BanInfo `json:"ban,omitempty"`
	Phone       string   `json:"phone,omitempty"`
	Email       string   `json:"email,omitempty"`
}

// UserPage is one page of users.
type UserPage struct {
	Items  []UserRecord `json:"items"`
	Total  int64        `json:"total"`
	Limit  int          `json:"limit"`
	Offset int          `json:"offset"`
}

// UserRepository is the read port for user lookups.
type UserRepository interface {
	ListUsers(ctx context.Context, f UserFilter) (UserPage, error)
	GetUser(ctx context.Context, uid int64) (*UserRecord, error)
}

// BanInput is a ban request. Days and Until are mutually exclusive; neither
// set means permanent (Until=0).
type BanInput struct {
	Level  int    `json:"level"`
	Reason string `json:"reason"`
	Days   int    `json:"days"`
	Until  int64  `json:"until"`
}

// BanResult reports what the bridge was asked to do.
type BanResult struct {
	UID       int64  `json:"uid"`
	Banned    bool   `json:"banned"`
	Until     int64  `json:"until"`
	FrameKind uint32 `json:"frame_kind"`
}

// --------------------------------------------------------------- reports

// Report status state machine (policy: statuses are mechanism, decisions are
// the reviewer's).
const (
	ReportPending  = 0
	ReportHandling = 1
	ReportResolved = 2
	ReportRejected = 3
)

// ReportFilter is a normalized report query.
type ReportFilter struct {
	Status int // -1 = all
	Limit  int
	Offset int
}

// Report mirrors sys.reports.
type Report struct {
	ID         int64  `json:"id"`
	Reporter   int64  `json:"reporter"`
	TargetMsg  int64  `json:"target_msg,omitempty"`
	TargetUser int64  `json:"target_user,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Status     int    `json:"status"`
	HandledBy  string `json:"handled_by,omitempty"`
	Note       string `json:"note,omitempty"`
	At         int64  `json:"at"`
}

// ReportPage is one page of reports.
type ReportPage struct {
	Items  []Report `json:"items"`
	Total  int64    `json:"total"`
	Limit  int      `json:"limit"`
	Offset int      `json:"offset"`
}

// ReportRepository is the report state store (cold sys.reports projection).
type ReportRepository interface {
	ListReports(ctx context.Context, f ReportFilter) (ReportPage, error)
	GetReport(ctx context.Context, id int64) (*Report, error)
	// UpdateReport applies an atomic state transition. It must return
	// ErrConflict when the row's current status != from.
	UpdateReport(ctx context.Context, id int64, from, to int, handledBy, note string) (*Report, error)
}

// -------------------------------------------------------------- messages

// MessageQuery is a normalized message search.
type MessageQuery struct {
	Query  string
	ConvID int64
	Limit  int
	Offset int
}

// MessageRecord is one search hit. Content is an excerpt for the admin UI;
// audit events never include it.
type MessageRecord struct {
	MsgID    int64  `json:"msg_id"`
	Cid      int64  `json:"cid"`
	Seq      int64  `json:"seq"`
	SenderID int64  `json:"sender_id"`
	Text     string `json:"text,omitempty"`
	SentAt   int64  `json:"sent_at"`
	Recalled bool   `json:"recalled"`
}

// MessagePage is one page of search results.
type MessagePage struct {
	Items  []MessageRecord `json:"items"`
	Total  int64           `json:"total"`
	Limit  int             `json:"limit"`
	Offset int             `json:"offset"`
}

// MessageRepository is the read port for archive/hot message search.
type MessageRepository interface {
	SearchMessages(ctx context.Context, q MessageQuery) (MessagePage, error)
}

// ----------------------------------------------------------------- flags

// Flag is one feature flag / policy-as-flag row.
type Flag struct {
	Key     string `json:"key"`
	Value   string `json:"value"`
	Version int    `json:"version"`
}

// FlagStore is the policy-flag port (store.SysStore satisfies the adapter in
// NewSysFlagStore; a fake is used in tests).
type FlagStore interface {
	ListFlags(ctx context.Context) ([]Flag, error)
	SetFlag(ctx context.Context, key, value string) error
}

// ------------------------------------------------------------- broadcast

// BroadcastInput is a broadcast request.
type BroadcastInput struct {
	Text      string  `json:"text"`
	ConvIDs   []int64 `json:"conv_ids,omitempty"`
	ExpiresAt int64   `json:"expires_at,omitempty"`
	DryRun    bool    `json:"dry_run,omitempty"`
}

// BroadcastPreview is the dry-run estimate. No audit event is written for a
// preview (dry-run is not an action).
type BroadcastPreview struct {
	EstimatedReach int64 `json:"estimated_reach"`
	ConvCount      int   `json:"conv_count"`
	DryRun         bool  `json:"dry_run"`
}

// BroadcastResult is the send response.
type BroadcastResult struct {
	Sent           bool   `json:"sent"`
	EstimatedReach int64  `json:"estimated_reach"`
	FrameKind      uint32 `json:"frame_kind"`
}

// ReachCounter estimates how many users a broadcast reaches.
type ReachCounter interface {
	CountReachable(ctx context.Context, convIDs []int64) (int64, error)
}

// ----------------------------------------------------------------- audit

// AuditRecord is one audit-journal row as exposed by the read API.
type AuditRecord struct {
	ID      int64  `json:"id"`
	Actor   string `json:"actor"`
	Action  string `json:"action"`
	Subject string `json:"subject"`
	Detail  []byte `json:"detail,omitempty"`
	At      int64  `json:"at"`
}

// AuditPage is a cursor page: next call uses NextSinceID.
type AuditPage struct {
	Items       []AuditRecord `json:"items"`
	NextSinceID int64         `json:"next_since_id"`
	HasMore     bool          `json:"has_more"`
}

// AuditReader is the sequential-cursor read side of the journal. It has no
// write/update/delete methods: append lives only on store.AuditJournal.
type AuditReader interface {
	ListAudit(ctx context.Context, sinceID int64, limit int) ([]AuditRecord, error)
}

// --------------------------------------------------------------- archive

// ArchiveStatus mirrors sys.archive_runs' latest state.
type ArchiveStatus struct {
	State     string `json:"state"`
	Running   bool   `json:"running"`
	LastRunID int64  `json:"last_run_id,omitempty"`
	LastRunAt int64  `json:"last_run_at,omitempty"`
	LastMoved int64  `json:"last_moved,omitempty"`
	NextRunAt int64  `json:"next_run_at,omitempty"`
}

// ArchiveEstimate is the dry-run projection.
type ArchiveEstimate struct {
	Rows    int64 `json:"rows"`
	Convs   int64 `json:"convs"`
	Minutes int64 `json:"minutes"`
}

// ArchiveRun identifies one manual run.
type ArchiveRun struct {
	RunID     int64            `json:"run_id"`
	State     string           `json:"state"`
	DryRun    bool             `json:"dry_run"`
	FrameKind uint32           `json:"frame_kind,omitempty"`
	Estimate  *ArchiveEstimate `json:"estimate,omitempty"`
}

// ArchiveRepository is the job ledger port.
type ArchiveRepository interface {
	Status(ctx context.Context) (ArchiveStatus, error)
	Estimate(ctx context.Context) (ArchiveEstimate, error)
	BeginRun(ctx context.Context) (ArchiveRun, error)
	FinishRun(ctx context.Context, runID int64, moved int64, status string) error
}
