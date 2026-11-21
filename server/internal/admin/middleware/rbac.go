package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/yingo-server/UFOT/server/internal/admin/identity"
	"github.com/yingo-server/UFOT/server/internal/data/store"
	"github.com/yingo-server/UFOT/server/internal/pkg/log"
)

// Permission is one admin capability. Route definitions reference these; the
// role matrix below is the single authorization truth for the admin plane.
type Permission string

const (
	PermUserRead      Permission = "user.read"
	PermUserBan       Permission = "user.ban"
	PermUserKick      Permission = "user.kick"
	PermReportRead    Permission = "report.read"
	PermReportHandle  Permission = "report.handle"
	PermMessageSearch Permission = "message.search"
	PermMessageRecall Permission = "message.recall"
	PermFlagRead      Permission = "flag.read"
	PermFlagWrite     Permission = "flag.write"
	PermBroadcast     Permission = "broadcast.send"
	PermAuditRead     Permission = "audit.read"
	PermArchiveRead   Permission = "archive.read"
	PermArchiveRun    Permission = "archive.run"
	PermMetricsRead   Permission = "metrics.read"
)

// AllPermissions is the canonical, sorted permission list.
func AllPermissions() []Permission {
	out := []Permission{
		PermUserRead, PermUserBan, PermUserKick,
		PermReportRead, PermReportHandle,
		PermMessageSearch, PermMessageRecall,
		PermFlagRead, PermFlagWrite,
		PermBroadcast, PermAuditRead,
		PermArchiveRead, PermArchiveRun, PermMetricsRead,
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// rolePermissions is the evaluable matrix. Notes:
//   - super: everything.
//   - ops: day-to-day operations; policy writes (flag.write) stay super-only.
//   - review: content governance (reports/messages/read-only user lookup),
//     no broadcast, no flags, no archive/ops actions.
var rolePermissions = map[identity.Role]map[Permission]bool{
	identity.RoleSuper: {
		PermUserRead: true, PermUserBan: true, PermUserKick: true,
		PermReportRead: true, PermReportHandle: true,
		PermMessageSearch: true, PermMessageRecall: true,
		PermFlagRead: true, PermFlagWrite: true,
		PermBroadcast: true, PermAuditRead: true,
		PermArchiveRead: true, PermArchiveRun: true, PermMetricsRead: true,
	},
	identity.RoleOps: {
		PermUserRead: true, PermUserBan: true, PermUserKick: true,
		PermReportRead: true, PermReportHandle: true,
		PermMessageSearch: true, PermMessageRecall: true,
		PermFlagRead:  true,
		PermBroadcast: true, PermAuditRead: true,
		PermArchiveRead: true, PermArchiveRun: true, PermMetricsRead: true,
	},
	identity.RoleReview: {
		PermUserRead:   true,
		PermReportRead: true, PermReportHandle: true,
		PermMessageSearch: true, PermMessageRecall: true,
		PermAuditRead: true,
	},
}

// Allowed is the single role→permission decision function.
func Allowed(role identity.Role, p Permission) bool {
	m, ok := rolePermissions[role]
	if !ok {
		return false
	}
	return m[p]
}

// PermissionsFor lists granted permissions (sorted) for docs/CLI display.
func PermissionsFor(role identity.Role) []Permission {
	var out []Permission
	for p := range rolePermissions[role] {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// RBAC enforces route permissions and records denied attempts in the audit
// journal (deny events are not optional; failure to append is logged ERROR).
type RBAC struct {
	Journal store.AuditJournal
	Logger  *log.Logger
	Now     func() time.Time
}

// NewRBAC builds the guard.
func NewRBAC(journal store.AuditJournal, logger *log.Logger) *RBAC {
	if logger == nil {
		logger = log.For("admin.rbac")
	}
	return &RBAC{Journal: journal, Logger: logger, Now: time.Now}
}

// Guard returns a middleware for one permission.
func (a *RBAC) Guard(p Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			admin := IdentityFrom(r.Context())
			if admin == nil {
				WriteError(w, r, http.StatusUnauthorized, "unauthenticated")
				return
			}
			if p == "" { // authenticated-only route (whoami/logout)
				next.ServeHTTP(w, r)
				return
			}
			if !Allowed(admin.Role, p) {
				a.recordDenied(r, admin, p)
				WriteError(w, r, http.StatusForbidden, "permission_denied")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (a *RBAC) recordDenied(r *http.Request, admin *identity.Admin, p Permission) {
	detail, _ := json.Marshal(map[string]string{
		"permission": string(p),
		"role":       admin.Role.String(),
		"method":     r.Method,
	})
	ev := store.AuditEvent{
		Actor:   admin.Username,
		Action:  "admin.rbac.denied",
		Subject: r.Method + " " + r.URL.Path,
		Detail:  detail,
		At:      a.Now().Unix(),
	}
	if a.Journal == nil {
		a.Logger.Error("rbac denied audit skipped (no journal)", "permission", string(p), "admin_id", admin.AdminID)
		return
	}
	if err := a.Journal.Append(context.Background(), ev); err != nil {
		a.Logger.Error("rbac denied audit append failed", "permission", string(p), "err", err.Error())
	}
}
