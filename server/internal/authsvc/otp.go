package authsvc

// otp.go: phone OTP and email magic link issue/verify, including the
// anti-SMS-bombing limits of docs/security-spec.md §1.5.2.

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

const magicLinkTTL = 10 * time.Minute

// SMSSender delivers one OTP code. NOT wired in this batch: without it the
// service still issues challenges (dev builds accept the fixed 123456 code),
// but nothing reaches a handset. See the "not wired" list in the report.
type SMSSender interface {
	SendOTP(ctx context.Context, phone, code string) error
}

type phoneCodeRequest struct {
	Phone        string `json:"phone"`
	CaptchaToken string `json:"captcha_token,omitempty"`
	// Captcha is accepted as an alias (the password provider sends `captcha`).
	Captcha string `json:"captcha,omitempty"`
}

type phoneVerifyRequest struct {
	Phone       string `json:"phone"`
	Code        string `json:"code"`
	ChallengeID string `json:"challenge_id,omitempty"`
	Token       string `json:"token,omitempty"` // alias for challenge_id
	DeviceID    string `json:"device_id,omitempty"`
}

type emailLinkRequest struct {
	Email string `json:"email"`
}

type emailPollRequest struct {
	Email       string `json:"email"`
	ChallengeID string `json:"challenge_id,omitempty"`
	Token       string `json:"token,omitempty"`
	DeviceID    string `json:"device_id,omitempty"`
}

// handlePhoneCode issues an OTP challenge for a phone number.
func (s *Service) handlePhoneCode(w http.ResponseWriter, r *http.Request) *apiError {
	if !s.methodAllowed("phone") {
		return errMethodDisabled("phone")
	}
	var req phoneCodeRequest
	if e := s.decode(w, r, &req); e != nil {
		return e
	}
	phone := normalizePhone(req.Phone)
	if !phoneRe.MatchString(phone) {
		return errBadRequest("invalid phone number")
	}
	ctx := r.Context()
	now := s.cfg.Now()
	index := s.idIndex(IDTypePhone, phone)
	ipHash := s.idIndex(IDTypeIP, s.clientIP(r))

	if e := s.checkOTPSendAllowed(ctx, IDTypePhone, index, ipHash, req.CaptchaToken+req.Captcha, now); e != nil {
		return e
	}

	code := randomCode()
	if s.cfg.DevOTP {
		code = devOTPCode
	}
	challengeID := newID(challengeIDBytes)
	row := &otpRow{
		ChallengeID: challengeID,
		IDType:      IDTypePhone,
		Subject:     index,
		CodeHash:    s.codeDigest(challengeID, code),
		MaxAttempts: s.cfg.OTPMaxAttempts,
		CreatedAt:   now.Unix(),
		ExpiresAt:   now.Add(s.cfg.OTPTTL).Unix(),
		IPHash:      ipHash,
	}
	if err := s.st.insertOTP(ctx, row); err != nil {
		s.lg.Error("insert otp failed", "err", err.Error())
		return errInternal()
	}
	if s.sms != nil && !s.cfg.DevOTP {
		sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if err := s.sms.SendOTP(sctx, phone, code); err != nil {
			s.lg.Error("sms send failed", "sub", subjectShort(index), "err", err.Error())
		}
		cancel()
	}
	s.lg.Info("otp issued", "type", "phone", "sub", subjectShort(index), "challenge", shortToken(challengeID))
	s.writeJSON(w, http.StatusOK, challengeResponse{
		OK:          true,
		ChallengeID: challengeID,
		Channel:     "sms",
		ExpiresIn:   int64(s.cfg.OTPTTL / time.Second),
		ResendAfter: int64(s.cfg.ResendInterval / time.Second),
	})
	return nil
}

// checkOTPSendAllowed enforces §1.5.2: 60s interval, 5/hour + 10/day per
// number, 5/hour per IP, captcha from the 2nd request of the day, and the
// 24h freeze when a number is hit from >= 3 distinct IPs in 24h.
func (s *Service) checkOTPSendAllowed(ctx context.Context, idType int, index, ipHash, captchaToken string, now time.Time) *apiError {
	if left, err := s.otpFreezeRemaining(ctx, index, penaltyOTPSend); err != nil {
		return errInternal()
	} else if left > 0 {
		return errRateLimited("OTP_FROZEN", left)
	}

	last, err := s.st.lastOTPAt(ctx, idType, index)
	if err != nil {
		return errInternal()
	}
	if last > 0 {
		if elapsed := now.Unix() - last; elapsed < int64(s.cfg.ResendInterval/time.Second) {
			return errRateLimited("OTP_RESEND_TOO_SOON", s.cfg.ResendInterval-time.Duration(elapsed)*time.Second)
		}
	}

	daily, err := s.st.countOTPSince(ctx, idType, index, now.Add(-24*time.Hour).Unix())
	if err != nil {
		return errInternal()
	}
	if daily >= s.cfg.PhoneDailyCap {
		return errRateLimited("OTP_DAILY_LIMIT", 24*time.Hour)
	}
	hourly, err := s.st.countOTPSince(ctx, idType, index, now.Add(-time.Hour).Unix())
	if err != nil {
		return errInternal()
	}
	if hourly >= s.cfg.PhoneHourlyCap {
		return errRateLimited("OTP_HOURLY_LIMIT", time.Hour)
	}

	// >= 3 distinct source IPs within the freeze window -> freeze sending 24h
	// (docs §1.5.2). Notification to the account owner is not wired (no
	// channel yet) — TODO-NOTIFY in the report.
	distinct, err := s.st.countOTPDistinctIPsSince(ctx, idType, index, now.Add(-s.cfg.FreezeWindow).Unix())
	if err != nil {
		return errInternal()
	}
	if distinct >= s.cfg.FreezeDistinctIPs {
		until := now.Add(24 * time.Hour)
		if err := s.sys.SetPenalty(ctx, penaltyFor(index, penaltyOTPSend, until.Unix())); err != nil {
			s.lg.Warn("freeze otp failed", "err", err.Error())
		}
		s.lg.Warn("otp send frozen: too many source IPs", "sub", subjectShort(index), "ips", distinct)
		return errRateLimited("OTP_FROZEN", 24*time.Hour)
	}

	ipCount, err := s.st.countOTPByIPSince(ctx, ipHash, now.Add(-time.Hour).Unix())
	if err != nil {
		return errInternal()
	}
	if ipCount >= s.cfg.IPHourlyCap {
		return errRateLimited("IP_RATE_LIMIT", time.Hour)
	}

	// Captcha from the 2nd request of the last 24h (§1.5.2 红线).
	if daily >= 1 {
		if captchaToken == "" {
			return &apiError{Status: http.StatusBadRequest, Code: "CAPTCHA_REQUIRED", Message: "captcha required"}
		}
		if s.cfg.DevOTP {
			// Dev builds have no captcha provider; any non-empty token passes.
			return nil
		}
		if s.captcha == nil {
			// Fail closed: never silently accept an unverifiable captcha.
			return &apiError{Status: http.StatusBadRequest, Code: "CAPTCHA_REQUIRED", Message: "captcha verification is not configured"}
		}
		if err := s.captcha.Verify(ctx, captchaToken, ""); err != nil {
			return &apiError{Status: http.StatusBadRequest, Code: "CAPTCHA_INVALID", Message: "captcha verification failed"}
		}
	}
	return nil
}

// handlePhoneVerify exchanges an OTP for a session (auto-registers on first use).
func (s *Service) handlePhoneVerify(w http.ResponseWriter, r *http.Request) *apiError {
	if !s.methodAllowed("phone") {
		return errMethodDisabled("phone")
	}
	var req phoneVerifyRequest
	if e := s.decode(w, r, &req); e != nil {
		return e
	}
	phone := normalizePhone(req.Phone)
	if !phoneRe.MatchString(phone) {
		return errBadRequest("invalid phone number")
	}
	if req.Code == "" {
		return errBadRequest("code required")
	}
	ctx := r.Context()
	index := s.idIndex(IDTypePhone, phone)
	if left, err := s.otpFreezeRemaining(ctx, index, penaltyOTPVerify); err != nil {
		return errInternal()
	} else if left > 0 {
		return errRateLimited("OTP_VERIFY_LOCKED", left)
	}

	challengeID := req.ChallengeID
	if challengeID == "" {
		challengeID = req.Token
	}
	var row *otpRow
	var err error
	if challengeID != "" {
		row, err = s.st.otp(ctx, challengeID)
	} else {
		row, err = s.st.latestOTP(ctx, IDTypePhone, index)
	}
	if err != nil {
		// Unknown challenge: same answer as a wrong code (no existence oracle).
		return errUnauthorized("OTP_INVALID")
	}
	if row.IDType != IDTypePhone || row.Subject != index {
		return errUnauthorized("OTP_INVALID")
	}
	if e := s.verifyOTP(ctx, row, req.Code, penaltyOTPVerify); e != nil {
		return e
	}

	deviceID := req.DeviceID
	if !validDeviceID(deviceID) {
		deviceID = issueDeviceID()
	}
	uid, isNew, name, e := s.resolveUser(ctx, IDTypePhone, phone, true, "")
	if e != nil {
		return e
	}
	if e := s.checkBan(ctx, uid); e != nil {
		return e
	}
	if err := s.st.upsertDevice(ctx, deviceID, uid, "phone", s.cfg.Now().Unix()); err != nil {
		s.lg.Warn("device upsert failed", "err", err.Error())
	}
	s.lg.Info("login ok", "method", "phone", "uid", uid, "new", isNew)
	return s.issueSession(w, ctx, uid, deviceID, "", false, isNew, name)
}

// verifyOTP performs the constant-time compare plus single-use consume. On the
// 5th wrong attempt the challenge is revoked and the subject is locked for
// VerifyLock (docs §2.5).
func (s *Service) verifyOTP(ctx context.Context, row *otpRow, code, penalty string) *apiError {
	now := s.cfg.Now()
	if row.Revoked || row.Attempts >= row.MaxAttempts {
		return errUnauthorized("OTP_LOCKED")
	}
	if row.ExpiresAt <= now.Unix() {
		return errUnauthorized("OTP_EXPIRED")
	}
	if !hmacEqual(row.CodeHash, s.codeDigest(row.ChallengeID, code)) {
		if err := s.st.incrOTPAttempts(ctx, row.ChallengeID); err != nil {
			s.lg.Warn("otp attempt counter failed", "err", err.Error())
		}
		if row.Attempts+1 >= row.MaxAttempts {
			if err := s.st.revokeOTP(ctx, row.ChallengeID); err != nil {
				s.lg.Warn("otp revoke failed", "err", err.Error())
			}
			until := now.Add(s.cfg.VerifyLock).Unix()
			if err := s.sys.SetPenalty(ctx, penaltyFor(row.Subject, penalty, until)); err != nil {
				s.lg.Warn("otp verify penalty failed", "err", err.Error())
			}
			s.lg.Warn("otp challenge locked after max attempts", "sub", subjectShort(row.Subject))
			return errRateLimited("OTP_LOCKED", s.cfg.VerifyLock)
		}
		return errUnauthorized("OTP_INVALID")
	}
	ok, err := s.st.consumeOTP(ctx, row.ChallengeID, now.Unix())
	if err != nil {
		return errInternal()
	}
	if !ok {
		return errUnauthorized("OTP_USED")
	}
	return nil
}

// handleEmailLink issues a magic-link challenge. Without a Mailer nothing is
// delivered (reported as "not wired"); dev mode returns the link in the body.
func (s *Service) handleEmailLink(w http.ResponseWriter, r *http.Request) *apiError {
	if !s.methodAllowed("email") {
		return errMethodDisabled("email")
	}
	var req emailLinkRequest
	if e := s.decode(w, r, &req); e != nil {
		return e
	}
	email := normalizeEmail(req.Email)
	if !emailRe.MatchString(email) {
		return errBadRequest("invalid email")
	}
	ctx := r.Context()
	now := s.cfg.Now()
	index := s.idIndex(IDTypeEmail, email)
	ipHash := s.idIndex(IDTypeIP, s.clientIP(r))
	if e := s.checkOTPSendAllowed(ctx, IDTypeEmail, index, ipHash, "magic-link", now); e != nil {
		return e
	}

	challengeID := newID(challengeIDBytes)
	token := newSecret(magicLinkTokenBytes)
	row := &otpRow{
		ChallengeID: challengeID,
		IDType:      IDTypeEmail,
		Subject:     index,
		CodeHash:    s.codeDigest(challengeID, token),
		MaxAttempts: s.cfg.OTPMaxAttempts,
		CreatedAt:   now.Unix(),
		ExpiresAt:   now.Add(magicLinkTTL).Unix(),
		IPHash:      ipHash,
	}
	if err := s.st.insertOTP(ctx, row); err != nil {
		s.lg.Error("insert magic link failed", "err", err.Error())
		return errInternal()
	}
	link := s.magicLink(challengeID, token)
	if s.mailer != nil {
		mctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := s.mailer.SendMagicLink(mctx, email, link); err != nil {
			s.lg.Error("magic link send failed", "sub", subjectShort(index), "err", err.Error())
		}
		cancel()
	}
	s.lg.Info("magic link issued", "sub", subjectShort(index), "challenge", shortToken(challengeID))
	resp := challengeResponse{
		OK:          true,
		ChallengeID: challengeID,
		Channel:     "email",
		ExpiresIn:   int64(magicLinkTTL / time.Second),
		ResendAfter: int64(s.cfg.ResendInterval / time.Second),
	}
	if s.cfg.DevOTP {
		resp.Link = link
		resp.Hint = "dev mode: use the returned link"
	}
	s.writeJSON(w, http.StatusOK, resp)
	return nil
}

// magicLink builds the SPA landing URL. It deliberately carries no email
// address (URLs end up in logs/history; security-spec §5.3).
func (s *Service) magicLink(challengeID, token string) string {
	base := s.cfg.PublicOrigin
	if base == "" {
		base = ""
	}
	q := url.Values{}
	q.Set("email_challenge", challengeID)
	q.Set("token", token)
	return base + "/login?" + q.Encode()
}

// handleEmailPoll answers either "pending" (202) or the session, once the
// magic link token is presented.
func (s *Service) handleEmailPoll(w http.ResponseWriter, r *http.Request) *apiError {
	if !s.methodAllowed("email") {
		return errMethodDisabled("email")
	}
	var req emailPollRequest
	if e := s.decode(w, r, &req); e != nil {
		return e
	}
	email := normalizeEmail(req.Email)
	if !emailRe.MatchString(email) {
		return errBadRequest("invalid email")
	}
	ctx := r.Context()
	index := s.idIndex(IDTypeEmail, email)
	challengeID := req.ChallengeID
	token := req.Token
	var row *otpRow
	var err error
	if challengeID != "" {
		row, err = s.st.otp(ctx, challengeID)
	} else {
		row, err = s.st.latestOTP(ctx, IDTypeEmail, index)
	}
	if err != nil || row.Subject != index {
		return s.writePending(w)
	}
	if row.ConsumedAt != 0 {
		return errUnauthorized("EMAIL_TOKEN_USED")
	}
	if token == "" {
		return s.writePending(w)
	}
	if e := s.verifyOTP(ctx, row, token, penaltyOTPVerify); e != nil {
		// Never reveal challenge state to an unauthenticated poller: expired /
		// revoked / wrong token all look like "still pending".
		return s.writePending(w)
	}
	deviceID := req.DeviceID
	if !validDeviceID(deviceID) {
		deviceID = issueDeviceID()
	}
	uid, isNew, name, e := s.resolveUser(ctx, IDTypeEmail, email, true, "")
	if e != nil {
		return e
	}
	if e := s.checkBan(ctx, uid); e != nil {
		return e
	}
	if err := s.st.upsertDevice(ctx, deviceID, uid, "email", s.cfg.Now().Unix()); err != nil {
		s.lg.Warn("device upsert failed", "err", err.Error())
	}
	s.lg.Info("login ok", "method", "email", "uid", uid, "new", isNew)
	return s.issueSession(w, ctx, uid, deviceID, "", false, isNew, name)
}

// writePending is the "magic link not clicked yet" answer.
//
// Status choice: 409 Conflict, NOT 202. The client's shared helper
// (createHttpAuthApi) only raises AuthError for !response.ok, and
// EmailAuthProvider.verify documents "未点击链接时 → AuthError(code=PENDING)".
// A 202 would be treated as a success and then fail mapAuthSession with
// BAD_RESPONSE instead of the retryable PENDING the UI expects.
func (s *Service) writePending(w http.ResponseWriter) *apiError {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusConflict)
	_ = jsonEncode(w, errBody{Status: "pending", Code: "PENDING", Error: "pending"})
	return nil
}
