package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yingo-server/UFOT/server/internal/admin/auditshim"
	"github.com/yingo-server/UFOT/server/internal/admin/bridgeproxy"
	"github.com/yingo-server/UFOT/server/internal/admin/identity"
	"github.com/yingo-server/UFOT/server/internal/data/store"
	"github.com/yingo-server/UFOT/server/internal/pkg/log"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
)

var userLog = log.For("admin.user")

// UserService implements user administration. Every mutation is a security
// frame (kind 960) injected through the bridge; the admin plane never writes
// the hot identity tables directly.
type UserService struct {
	repo     UserRepository
	dispatch Dispatcher
	audit    *auditshim.Shim
	now      func() time.Time
}

// NewUserService builds the service.
func NewUserService(repo UserRepository, dispatch Dispatcher, audit *auditshim.Shim) *UserService {
	return &UserService{repo: repo, dispatch: dispatch, audit: audit, now: time.Now}
}

// SetClock injects a test clock.
func (s *UserService) SetClock(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

// List returns one page of masked users.
func (s *UserService) List(ctx context.Context, actor *identity.Admin, f UserFilter) (UserPage, error) {
	if s.repo == nil {
		return UserPage{}, ErrNotConfigured
	}
	f.Limit = normalizeLimit(f.Limit, 50, 100)
	f.Offset = normalizeOffset(f.Offset)
	page, err := s.repo.ListUsers(ctx, f)
	if err != nil {
		return UserPage{}, mapRepoErr(err)
	}
	for i := range page.Items {
		page.Items[i] = maskUser(page.Items[i])
	}
	page.Limit, page.Offset = f.Limit, f.Offset
	return page, nil
}

// Show returns one masked user.
func (s *UserService) Show(ctx context.Context, actor *identity.Admin, uid int64) (*UserRecord, error) {
	if s.repo == nil {
		return nil, ErrNotConfigured
	}
	if uid <= 0 {
		return nil, fmt.Errorf("%w: uid must be positive", ErrInvalid)
	}
	u, err := s.repo.GetUser(ctx, uid)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	// Any diagnostic that might carry PII goes through log.Redactor.
	userLog.Debug("user shown", "uid", uid, "phone", log.RedactPhone(u.Phone))
	masked := maskUser(*u)
	return &masked, nil
}

// Ban injects a 960 SECURITY_REQUEST with op=ban and records an audit event
// only after the bridge accepted the command.
func (s *UserService) Ban(ctx context.Context, actor *identity.Admin, uid int64, in BanInput) (BanResult, error) {
	if err := s.ensureUser(ctx, uid); err != nil {
		return BanResult{}, err
	}
	if in.Level < 1 || in.Level > 3 {
		return BanResult{}, fmt.Errorf("%w: level must be 1..3", ErrInvalid)
	}
	if in.Days < 0 || in.Until < 0 || (in.Days > 0 && in.Until > 0) {
		return BanResult{}, fmt.Errorf("%w: days and until are mutually exclusive", ErrInvalid)
	}
	until := in.Until
	if in.Days > 0 {
		until = s.now().Add(time.Duration(in.Days) * 24 * time.Hour).Unix()
	}
	payload := map[string]any{
		"op": "ban", "uid": uid, "level": in.Level,
		"reason": in.Reason, "until": until, "by": actorName(actor),
	}
	return auditshim.Do(ctx, s.audit, auditshim.Meta{
		Actor: actorName(actor), Action: "user.ban", Subject: "user:" + itoa(uid),
		Detail: map[string]any{
			"level": in.Level, "until": until, "days": in.Days,
			"reason_len": len(in.Reason),
		},
	}, func(ctx context.Context) (BanResult, error) {
		if _, err := injectJSON(ctx, s.dispatch, kinds.KindSecurityRequest,
			bridgeproxy.Target{UserID: uint64(uid)}, payload); err != nil {
			return BanResult{}, err
		}
		return BanResult{UID: uid, Banned: true, Until: until, FrameKind: kinds.KindSecurityRequest}, nil
	})
}

// Unban lifts a ban via 960 op=unban.
func (s *UserService) Unban(ctx context.Context, actor *identity.Admin, uid int64, reason string) (BanResult, error) {
	if err := s.ensureUser(ctx, uid); err != nil {
		return BanResult{}, err
	}
	payload := map[string]any{"op": "unban", "uid": uid, "reason": reason, "by": actorName(actor)}
	return auditshim.Do(ctx, s.audit, auditshim.Meta{
		Actor: actorName(actor), Action: "user.unban", Subject: "user:" + itoa(uid),
		Detail: map[string]any{"reason_len": len(reason)},
	}, func(ctx context.Context) (BanResult, error) {
		if _, err := injectJSON(ctx, s.dispatch, kinds.KindSecurityRequest,
			bridgeproxy.Target{UserID: uint64(uid)}, payload); err != nil {
			return BanResult{}, err
		}
		return BanResult{UID: uid, Banned: false, FrameKind: kinds.KindSecurityRequest}, nil
	})
}

// Kick disconnects all of a user's sessions via 960 op=kick.
func (s *UserService) Kick(ctx context.Context, actor *identity.Admin, uid int64, reason string) (BanResult, error) {
	if err := s.ensureUser(ctx, uid); err != nil {
		return BanResult{}, err
	}
	payload := map[string]any{"op": "kick", "uid": uid, "reason": reason, "by": actorName(actor)}
	return auditshim.Do(ctx, s.audit, auditshim.Meta{
		Actor: actorName(actor), Action: "user.kick", Subject: "user:" + itoa(uid),
		Detail: map[string]any{"reason_len": len(reason)},
	}, func(ctx context.Context) (BanResult, error) {
		if _, err := injectJSON(ctx, s.dispatch, kinds.KindSecurityRequest,
			bridgeproxy.Target{UserID: uint64(uid)}, payload); err != nil {
			return BanResult{}, err
		}
		return BanResult{UID: uid, FrameKind: kinds.KindSecurityRequest}, nil
	})
}

func (s *UserService) ensureUser(ctx context.Context, uid int64) error {
	if uid <= 0 {
		return fmt.Errorf("%w: uid must be positive", ErrInvalid)
	}
	if s.repo == nil {
		return ErrNotConfigured
	}
	if _, err := s.repo.GetUser(ctx, uid); err != nil {
		return mapRepoErr(err)
	}
	return nil
}

func mapRepoErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	return err
}
