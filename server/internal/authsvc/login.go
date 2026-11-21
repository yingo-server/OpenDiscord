package authsvc

// login.go: account classification, user resolution (auto-registration on
// first OTP / magic-link / guest login), ban checks and the persistent
// login-failure escalator.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yingo-server/UFOT/server/internal/data/store"
)

// actionLoginFail is the cumulative counter action for login failures
// (window_start = 0 → one bucket per subject, reset on success).
const actionLoginFail = "login_fail"

// penaltyLogin / penaltyOTPSend / penaltyOTPVerify are penalty names in
// sys_rate_limit_penalty.
const (
	penaltyLogin     = "login"
	penaltyOTPSend   = "otp_send"
	penaltyOTPVerify = "otp_verify"
)

// classifyAccount guesses the identity type of a password-login account:
// phone / email / username (in that order).
func classifyAccount(account string) (idType int, normalized string, e *apiError) {
	raw := account
	if len(raw) > maxAccountLen {
		return 0, "", errBadRequest("account too long")
	}
	if p := normalizePhone(raw); phoneRe.MatchString(p) {
		return IDTypePhone, p, nil
	}
	if m := normalizeEmail(raw); emailRe.MatchString(m) {
		return IDTypeEmail, m, nil
	}
	if u := normalizeUsername(raw); usernameRe.MatchString(u) {
		return IDTypeUsername, u, nil
	}
	return 0, "", errBadRequest("account must be a phone number, an email or a username")
}

// displayNameFor derives a non-PII default display name. It never contains the
// phone/email (that would leak PII into every profile read).
func displayNameFor(idType int, normalized string) string {
	switch idType {
	case IDTypePhone:
		return "用户" + normalized[max(0, len(normalized)-4):]
	case IDTypeEmail:
		return "用户" + shortToken(tokenHash(normalized))
	case IDTypeGuest:
		return "游客" + shortToken(tokenHash(normalized))
	default:
		return normalized
	}
}

// resolveUser looks the identity index up and creates the user on first sight
// (security-spec §2.5: 首次 OTP 验证成功即注册).
func (s *Service) resolveUser(ctx context.Context, idType int, normalized string, forceCreate bool, displayName string) (uid int64, isNew bool, displayNameOut string, e *apiError) {
	index := s.idIndex(idType, normalized)
	uid, err := s.ident.LookupByIdentity(ctx, idType, index)
	switch {
	case err == nil:
		if u, gerr := s.ident.GetUser(ctx, uid); gerr == nil && u != nil {
			return uid, false, u.DisplayName, nil
		}
		return uid, false, "", nil
	case errors.Is(err, store.ErrNotFound):
		if !forceCreate {
			return 0, false, "", nil
		}
	default:
		s.lg.Error("identity lookup failed", "type", idType, "sub", subjectShort(index), "err", err.Error())
		return 0, false, "", errInternal()
	}

	if displayName == "" {
		displayName = displayNameFor(idType, normalized)
	}
	newUID, err := s.st.createUser(ctx, displayName, idType, index, s.cfg.Now().Unix())
	if err != nil {
		// A concurrent request may have created the same identity between the
		// lookup and the insert; re-resolve once instead of failing the login.
		if uid2, lerr := s.ident.LookupByIdentity(ctx, idType, index); lerr == nil {
			return uid2, false, "", nil
		}
		s.lg.Error("create user failed", "type", idType, "sub", subjectShort(index), "err", err.Error())
		return 0, false, "", errInternal()
	}
	s.lg.Info("user registered", "uid", newUID, "type", idType, "sub", subjectShort(index))
	return newUID, true, displayName, nil
}

// checkBan returns a 403 when the account is banned (identity_bans).
func (s *Service) checkBan(ctx context.Context, uid int64) *apiError {
	ban, err := s.ident.GetBan(ctx, uid)
	if err != nil {
		// A ban lookup failure must not silently admit a banned user: fail
		// closed with a 500 (the caller can retry).
		s.lg.Error("ban lookup failed", "uid", uid, "err", err.Error())
		return errInternal()
	}
	if ban == nil {
		return nil
	}
	return &apiError{Status: 403, Code: "BANNED", Message: fmt.Sprintf("account banned (level %d)", ban.Level)}
}

// ------------------------------------------------------------- login guard

func accountSubject(index string) string { return "acct:" + index }
func ipSubject(ipHash string) string     { return "ip:" + ipHash }

// lockRemaining reports the longest active lock across the given subjects.
func (s *Service) lockRemaining(ctx context.Context, subjects ...string) (time.Duration, *apiError) {
	now := s.cfg.Now().Unix()
	var worst time.Duration
	for _, sub := range subjects {
		p, err := s.sys.GetPenalty(ctx, sub, penaltyLogin)
		if err != nil {
			s.lg.Error("penalty lookup failed", "err", err.Error())
			return 0, errInternal()
		}
		if p == nil || p.Until <= now {
			continue
		}
		if d := time.Duration(p.Until-now) * time.Second; d > worst {
			worst = d
		}
	}
	if worst > 0 {
		return worst, nil
	}
	return 0, nil
}

// noteFailure increments the failure counter of each subject and escalates to
// a lock at every Threshold-th failure: 5 -> 15m, 10 -> 1h, 15+ -> 24h.
func (s *Service) noteFailure(ctx context.Context, subjects ...string) (time.Duration, error) {
	now := s.cfg.Now()
	var worst time.Duration
	for _, sub := range subjects {
		win, err := s.sys.RateLimitIncr(ctx, sub, actionLoginFail, 0)
		if err != nil {
			return 0, fmt.Errorf("authsvc: login counter: %w", err)
		}
		if win.Count%int64(s.cfg.LockThreshold) != 0 {
			continue
		}
		stage := int(win.Count / int64(s.cfg.LockThreshold))
		idx := stage - 1
		if idx >= len(s.cfg.LockDurations) {
			idx = len(s.cfg.LockDurations) - 1
		}
		if idx < 0 {
			idx = 0
		}
		d := s.cfg.LockDurations[idx]
		until := now.Add(d)
		if err := s.sys.SetPenalty(ctx, &store.Penalty{Subject: sub, Penalty: penaltyLogin, Until: until.Unix()}); err != nil {
			return 0, fmt.Errorf("authsvc: set login penalty: %w", err)
		}
		if d > worst {
			worst = d
		}
	}
	return worst, nil
}

// clearFailures resets the counters and lifts the lock after a success.
func (s *Service) clearFailures(ctx context.Context, subjects ...string) {
	for _, sub := range subjects {
		if err := s.st.resetCounter(ctx, sub, actionLoginFail); err != nil {
			s.lg.Warn("reset login counter failed", "err", err.Error())
		}
		if err := s.sys.SetPenalty(ctx, &store.Penalty{Subject: sub, Penalty: penaltyLogin, Until: 0}); err != nil {
			s.lg.Warn("clear login penalty failed", "err", err.Error())
		}
	}
}

// otpLocked reports the otp_send / otp_verify freeze for a phone subject.
func (s *Service) otpFreezeRemaining(ctx context.Context, subject, penalty string) (time.Duration, error) {
	p, err := s.sys.GetPenalty(ctx, subject, penalty)
	if err != nil {
		return 0, err
	}
	if p == nil {
		return 0, nil
	}
	now := s.cfg.Now().Unix()
	if p.Until <= now {
		return 0, nil
	}
	return time.Duration(p.Until-now) * time.Second, nil
}

// penaltyFor builds the sys_rate_limit_penalty row (store primitive).
func penaltyFor(subject, penalty string, until int64) *store.Penalty {
	return &store.Penalty{Subject: subject, Penalty: penalty, Until: until}
}
