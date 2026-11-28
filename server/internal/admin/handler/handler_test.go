package handler_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"github.com/yingo-server/UFOT/server/internal/admin/auditshim"
	"github.com/yingo-server/UFOT/server/internal/admin/bridgeproxy"
	"github.com/yingo-server/UFOT/server/internal/admin/handler"
	"github.com/yingo-server/UFOT/server/internal/admin/identity"
	"github.com/yingo-server/UFOT/server/internal/admin/middleware"
	"github.com/yingo-server/UFOT/server/internal/admin/service"
	"github.com/yingo-server/UFOT/server/internal/admin/totp"
	"github.com/yingo-server/UFOT/server/internal/data/store"
	"github.com/yingo-server/UFOT/server/internal/pkg/log"
	"github.com/yingo-server/UFOT/server/internal/user/core/authn"
)

var testNow = time.Unix(1_700_000_000, 0)

const (
	rootToken = "root-token-0123456789"
	opsToken  = "ops-token-0123456789"
	revToken  = "rev-token-0123456789"
	rootCSRF  = "root-csrf-token"
	opsCSRF   = "ops-csrf-token"
	revCSRF   = "rev-csrf-token"
	rootPass  = "correct horse battery staple"
	totpSeed  = "12345678901234567890"
)

type fakeJournal struct {
	mu     sync.Mutex
	events []store.AuditEvent
}

func (j *fakeJournal) Append(_ context.Context, ev store.AuditEvent) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.events = append(j.events, ev)
	return nil
}

func (j *fakeJournal) snapshot() []store.AuditEvent {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]store.AuditEvent(nil), j.events...)
}

type dispatchCall struct {
	Kind    uint32
	Target  bridgeproxy.Target
	Payload []byte
}

type fakeDispatcher struct {
	mu    sync.Mutex
	calls []dispatchCall
}

func (f *fakeDispatcher) InjectFrame(_ context.Context, kind uint32, target bridgeproxy.Target, payload []byte) (bridgeproxy.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, dispatchCall{Kind: kind, Target: target, Payload: append([]byte(nil), payload...)})
	return bridgeproxy.Result{StatusCode: 202, Kind: kind}, nil
}

func (f *fakeDispatcher) snapshot() []dispatchCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dispatchCall(nil), f.calls...)
}

type fakeAdmins struct {
	byName map[string]*service.AdminAccount
	byID   map[int64]*service.AdminAccount
}

func (f *fakeAdmins) GetAdminByUsername(_ context.Context, username string) (*service.AdminAccount, error) {
	a, ok := f.byName[username]
	if !ok {
		return nil, store.ErrNotFound
	}
	return a, nil
}

func (f *fakeAdmins) GetAdmin(_ context.Context, id int64) (*service.AdminAccount, error) {
	a, ok := f.byID[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return a, nil
}

type fakeSessions struct {
	mu   sync.Mutex
	rows map[string]*service.AdminSession
}

func (f *fakeSessions) CreateSession(_ context.Context, s *service.AdminSession) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[s.SID] = s
	return nil
}

func (f *fakeSessions) GetSession(_ context.Context, sid string) (*service.AdminSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.rows[sid]
	if !ok {
		return nil, store.ErrNotFound
	}
	return s, nil
}

func (f *fakeSessions) DeleteSession(_ context.Context, sid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, sid)
	return nil
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

type fakeUsers struct {
	rows map[int64]service.UserRecord
}

func (f *fakeUsers) ListUsers(_ context.Context, _ service.UserFilter) (service.UserPage, error) {
	out := make([]service.UserRecord, 0, len(f.rows))
	for _, u := range f.rows {
		out = append(out, u)
	}
	return service.UserPage{Items: out, Total: int64(len(out))}, nil
}

func (f *fakeUsers) GetUser(_ context.Context, uid int64) (*service.UserRecord, error) {
	u, ok := f.rows[uid]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &u, nil
}

type fakeReports struct {
	rows map[int64]service.Report
}

func (f *fakeReports) ListReports(_ context.Context, _ service.ReportFilter) (service.ReportPage, error) {
	out := make([]service.Report, 0, len(f.rows))
	for _, r := range f.rows {
		out = append(out, r)
	}
	return service.ReportPage{Items: out, Total: int64(len(out))}, nil
}

func (f *fakeReports) GetReport(_ context.Context, id int64) (*service.Report, error) {
	r, ok := f.rows[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &r, nil
}

func (f *fakeReports) UpdateReport(_ context.Context, id int64, from, to int, handledBy, note string) (*service.Report, error) {
	r, ok := f.rows[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	if r.Status != from {
		return nil, service.ErrConflict
	}
	r.Status, r.HandledBy, r.Note = to, handledBy, note
	f.rows[id] = r
	return &r, nil
}

type fakeMessages struct{}

func (fakeMessages) SearchMessages(_ context.Context, q service.MessageQuery) (service.MessagePage, error) {
	return service.MessagePage{Items: []service.MessageRecord{{MsgID: 9, Cid: 3, SenderID: 42, Text: "hello", SentAt: 1}}, Total: 1}, nil
}

type fakeFlags struct {
	flags map[string]string
}

func (f *fakeFlags) ListFlags(_ context.Context) ([]service.Flag, error) {
	out := make([]service.Flag, 0, len(f.flags))
	for k, v := range f.flags {
		out = append(out, service.Flag{Key: k, Value: v, Version: 1})
	}
	return out, nil
}

func (f *fakeFlags) SetFlag(_ context.Context, key, value string) error {
	f.flags[key] = value
	return nil
}

type fakeReach struct{ n int64 }

func (f fakeReach) CountReachable(_ context.Context, _ []int64) (int64, error) { return f.n, nil }

type fakeArchive struct {
	running bool
	runID   int64
}

func (f *fakeArchive) Status(_ context.Context) (service.ArchiveStatus, error) {
	return service.ArchiveStatus{Running: f.running, LastRunID: f.runID, State: "ok"}, nil
}

func (f *fakeArchive) Estimate(_ context.Context) (service.ArchiveEstimate, error) {
	return service.ArchiveEstimate{Rows: 120, Convs: 4, Minutes: 2}, nil
}

func (f *fakeArchive) BeginRun(_ context.Context) (service.ArchiveRun, error) {
	f.runID++
	f.running = true
	return service.ArchiveRun{RunID: f.runID, State: "running"}, nil
}

func (f *fakeArchive) FinishRun(_ context.Context, _ int64, _ int64, _ string) error { return nil }

type fakeAuditReader struct{ rows []service.AuditRecord }

func (f *fakeAuditReader) ListAudit(_ context.Context, sinceID int64, limit int) ([]service.AuditRecord, error) {
	var out []service.AuditRecord
	for _, r := range f.rows {
		if r.ID > sinceID {
			out = append(out, r)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

type testEnv struct {
	api      *handler.API
	journal  *fakeJournal
	dispatch *fakeDispatcher
	users    *fakeUsers
	reports  *fakeReports
	flags    *fakeFlags
	archive  *fakeArchive
	reach    *fakeReach
	auth     *service.AuthService
	limiter  *middleware.LoginLimiter
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	log.SetLevel(log.ErrorLevel)
	params := authn.Argon2Params{Memory: 8 * 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}
	hasher := authn.NewHasher(params)
	rootHash, err := hasher.Hash(rootPass)
	if err != nil {
		t.Fatal(err)
	}
	secret := totp.SecretFromASCII(totpSeed)
	admins := &fakeAdmins{byName: map[string]*service.AdminAccount{}, byID: map[int64]*service.AdminAccount{}}
	add := func(id int64, name, hash string, role int) {
		a := &service.AdminAccount{ID: id, Username: name, PasswordHash: hash, TOTPSecret: secret, Role: role, CreatedAt: testNow.Unix()}
		admins.byName[name] = a
		admins.byID[id] = a
	}
	add(1, "root", rootHash, int(identity.RoleSuper))
	add(2, "ops", rootHash, int(identity.RoleOps))
	add(3, "rev", rootHash, int(identity.RoleReview))

	sessions := &fakeSessions{rows: map[string]*service.AdminSession{}}
	for tok, pair := range map[string]struct {
		id   int64
		csrf string
	}{
		rootToken: {1, rootCSRF}, opsToken: {2, opsCSRF}, revToken: {3, revCSRF},
	} {
		sessions.rows[hashToken(tok)] = &service.AdminSession{
			SID: hashToken(tok), AdminID: pair.id, ExpiresAt: testNow.Add(time.Hour).Unix(), CSRF: pair.csrf,
		}
	}
	authSvc := service.NewAuthService(admins, sessions, hasher, service.AuthConfig{
		SessionTTL: 15 * time.Minute, PasswordMinLen: 14, RequireTOTP: true, Now: func() time.Time { return testNow },
	})
	journal := &fakeJournal{}
	disp := &fakeDispatcher{}
	shim := auditshim.New(journal)

	users := &fakeUsers{rows: map[int64]service.UserRecord{
		1:  {UID: 1, DisplayName: "alice", Status: 0, CreatedAt: 1, Phone: "13812341234"},
		42: {UID: 42, DisplayName: "bob", Status: 1, CreatedAt: 1, Ban: &service.BanInfo{Level: 2, Until: 0}},
	}}
	reports := &fakeReports{rows: map[int64]service.Report{
		1: {ID: 1, Reporter: 42, TargetMsg: 9, Reason: "spam", Status: service.ReportPending, At: 1},
	}}
	flags := &fakeFlags{flags: map[string]string{"registration.enabled": "true"}}
	archive := &fakeArchive{}
	reach := &fakeReach{n: 1000}
	auditReader := &fakeAuditReader{rows: []service.AuditRecord{
		{ID: 1, Actor: "root", Action: "user.ban", Subject: "user:42", At: 1},
		{ID: 2, Actor: "root", Action: "user.unban", Subject: "user:42", At: 2},
		{ID: 3, Actor: "root", Action: "flag.set", Subject: "flag:x", At: 3},
	}}

	loginLimiter := middleware.NewLoginLimiter(middleware.DefaultLockPolicy())
	loginLimiter.SetClock(func() time.Time { return testNow })
	opLimiter := middleware.NewOpLimiter(middleware.OpLimiterConfig{PerMinute: 600, HighRiskPerMinute: 100, Window: time.Minute})
	opLimiter.SetClock(func() time.Time { return testNow })

	broadcast := service.NewBroadcastService(reach, disp, shim)
	broadcast.SetClock(func() time.Time { return testNow })

	api := handler.New(handler.Config{
		CookieName: "oc_admin_session", CSRFCookieName: "oc_admin_csrf", CSRFHeaderName: "X-CSRF-Token",
	}, handler.Deps{
		Auth:          authSvc,
		Users:         service.NewUserService(users, disp, shim),
		Reports:       service.NewReportService(reports, disp, shim),
		Messages:      service.NewMessageService(fakeMessages{}, disp, shim),
		Flags:         service.NewFlagService(flags, disp, shim),
		Broadcast:     broadcast,
		Audit:         service.NewAuditService(auditReader, shim),
		Archive:       service.NewArchiveService(archive, disp, shim),
		Metrics:       service.NewMetrics(),
		Authenticator: middleware.NewAuthenticator(authSvc, "oc_admin_session"),
		CSRF:          middleware.NewCSRF("oc_admin_csrf", "X-CSRF-Token"),
		RBAC:          middleware.NewRBAC(journal, log.For("test.rbac")),
		LoginLimiter:  loginLimiter,
		OpLimiter:     opLimiter,
		AuditShim:     shim,
		Logger:        log.For("test.http"),
	})
	return &testEnv{api: api, journal: journal, dispatch: disp, users: users, reports: reports,
		flags: flags, archive: archive, reach: reach, auth: authSvc, limiter: loginLimiter}
}
