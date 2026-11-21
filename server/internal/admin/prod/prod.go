// Package prod assembles the management plane's PRODUCTION adapters: the
// service-layer read ports (users/reports/messages/audit/archive/reach/flags)
// implemented on top of the real stores, plus first-run admin bootstrapping.
//
// Layering note (docs/admin-data-design.md §10 红线 1 «store 包外无 SQL»):
// this package IS a store-side adapter package — it is the only place outside
// internal/{admin/store,data/store} that is allowed to hold SQL literals. Every
// statement here is parameterized; dynamic fragments are built from fixed
// strings only (no caller input ever reaches the SQL text).
//
// Degradation contract (no cold PostgreSQL available):
//
//	Users        hot SQLite identity_* tables           — always available
//	Reports      hot SQLite sys_reports (new migration) — always available
//	Messages     hot SQLite msg_messages_hot            — cid + seq range +
//	             client_msg_id prefix; full-text search needs cold pg_trgm
//	AuditReader  cold sys.audit_log                     — empty page + WARN
//	Archive      cold sys.archive_runs                  — "not_configured"
//	Reach        hot presence/user counts               — always available
//	Flags        hot sys_feature_flags                  — always available
//
// The single assembly entry point is NewAdminServer; cmd/chatd must call it
// with the exact argument order below (another agent owns that call site).
package prod

import (
	"database/sql"
	"errors"
	"os"
	"time"

	"github.com/yingo-server/UFOT/server/internal/admin"
	"github.com/yingo-server/UFOT/server/internal/admin/service"
	"github.com/yingo-server/UFOT/server/internal/data/policy"
	"github.com/yingo-server/UFOT/server/internal/data/store"
	"github.com/yingo-server/UFOT/server/internal/data/store/pg"
	"github.com/yingo-server/UFOT/server/internal/pkg/log"
)

// Options carries the seams tests and specialized deployments need. The zero
// value is the production configuration.
type Options struct {
	// IDHasher maps a raw login identifier (phone/email/username) to its
	// candidate HMAC index values stored in identity_identities.id_value.
	// prod.IDIndexHasher builds one that matches the auth path; when nil, the
	// q argument of Users.List is matched against id_value verbatim (callers
	// may pass an already-hashed value).
	IDHasher func(string) []string
	// Online reports the current number of online connections. When nil the
	// adapter counts sys_presence rows with online=1.
	Online func() (int64, error)
	// DisableBanJournal turns off the identity_bans back-fill that runs after
	// a 960 ban/unban frame is accepted by the bridge.
	DisableBanJournal bool
	// SkipBootstrap disables first-run admin creation (tests, migrations).
	SkipBootstrap bool
	// AuditJournal overrides the audit journal (tests). Defaults to the cold
	// store, or a log-only journal when no cold store is configured.
	AuditJournal store.AuditJournal
	// Getenv overrides os.Getenv (tests).
	Getenv func(string) string
	// Now overrides time.Now.
	Now func() time.Time
}

// NewAdminServer is THE assembly entry point for the management plane.
//
//	cfg       admin server configuration (listen address, cookies, TTLs)
//	hot       hot SQLite handle (identity_*/msg_*/conv_*/sys_* tables)
//	adminDB   standalone admin.db handle (migrated here if needed)
//	cold      cold PostgreSQL handle; nil = degraded mode (see package doc)
//	dispatch  bridge dispatcher every mutation goes through (bridgeproxy.Proxy
//	          or any admin.Dispatcher implementation)
//	pol       policy.json snapshot store (retention days, archive cron); nil
//	          falls back to built-in defaults
//	lg        logger; nil uses log.For("admin.prod")
//
// It migrates admin.db, wires every read port, runs the first-run bootstrap
// (UFOT_ADMIN_USER / UFOT_ADMIN_PASSWORD, argon2id + TOTP) and returns
// a runnable *admin.Server. It never starts listening — the caller runs it.
func NewAdminServer(cfg admin.Config, hot *sql.DB, adminDB *sql.DB, cold *pg.ColdDB, dispatch admin.Dispatcher, pol *policy.Store, lg *log.Logger) (*admin.Server, error) {
	return NewAdminServerWithOptions(cfg, hot, adminDB, cold, dispatch, pol, lg, Options{})
}

// NewAdminServerWithOptions is NewAdminServer plus the Options seam.
func NewAdminServerWithOptions(cfg admin.Config, hot *sql.DB, adminDB *sql.DB, cold *pg.ColdDB, dispatch admin.Dispatcher, pol *policy.Store, lg *log.Logger, opts Options) (*admin.Server, error) {
	if adminDB == nil {
		return nil, errors.New("admin/prod: admin.db handle is required")
	}
	if dispatch == nil {
		return nil, errors.New("admin/prod: dispatcher is required")
	}
	if lg == nil {
		lg = log.For("admin.prod")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}

	journal := opts.AuditJournal
	if journal == nil {
		if cold != nil && cold.DB != nil {
			journal = cold // store.AuditJournal: append-only hash chain
		} else {
			journal = &logJournal{lg: lg}
			lg.Warn("审计冷库未配置：审计事件只写日志、不落盘（生产部署必须配置 cold DB）")
		}
	}

	users := NewUsers(hot, UsersConfig{Hasher: opts.IDHasher, Now: opts.Now})
	reports := NewReports(hot)
	messages := NewMessages(hot)
	flags := NewFlags(hot)
	reach := NewReach(hot, ReachConfig{Online: opts.Online})
	auditReader := NewAuditReader(cold, lg)
	archive := NewArchive(hot, cold, pol, opts.Now)

	disp := dispatch
	if !opts.DisableBanJournal {
		disp = &banRecorder{next: dispatch, users: users, lg: lg}
	}

	deps := admin.Deps{
		DB:           adminDB,
		AuditJournal: journal,
		Dispatch:     disp,
		Users:        users,
		Reports:      reports,
		Messages:     messages,
		Flags:        flags,
		Reach:        reach,
		AuditReader:  auditReader,
		Archive:      archive,
	}

	srv, err := admin.New(cfg, deps)
	if err != nil {
		return nil, err
	}
	if !opts.SkipBootstrap {
		if err := BootstrapAdmin(adminDB, cfg, opts, lg); err != nil {
			// A broken bootstrap must not take the console down: operators can
			// still log in with an existing account. It is loud on purpose.
			lg.Error("admin bootstrap failed", "err", err.Error())
		}
	}
	lg.Info("admin production adapters wired",
		"read_ports", "users,reports,messages,flags,reach,audit,archive",
		"cold", cold != nil)
	return srv, nil
}

// compile-time proof that every port is satisfied by the production adapters.
var (
	_ service.UserRepository    = (*Users)(nil)
	_ service.ReportRepository  = (*Reports)(nil)
	_ service.MessageRepository = (*Messages)(nil)
	_ service.AuditReader       = (*AuditReader)(nil)
	_ service.ArchiveRepository = (*Archive)(nil)
	_ service.ReachCounter      = (*Reach)(nil)
	_ service.FlagStore         = (*Flags)(nil)

	_ store.AuditJournal = (*logJournal)(nil)
	_ admin.Dispatcher   = (*banRecorder)(nil)
)
