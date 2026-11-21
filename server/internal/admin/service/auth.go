package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yingo-server/UFOT/server/internal/admin/identity"
	"github.com/yingo-server/UFOT/server/internal/admin/totp"
	"github.com/yingo-server/UFOT/server/internal/data/store"
	"github.com/yingo-server/UFOT/server/internal/user/core/authn"
)

// AdminAccount is the admin identity row (adm_admins projection).
type AdminAccount struct {
	ID           int64
	Username     string
	PasswordHash string
	TOTPSecret   string
	Role         int
	CreatedAt    int64
}

// AdminRepository is the admin account read port.
type AdminRepository interface {
	GetAdminByUsername(ctx context.Context, username string) (*AdminAccount, error)
	GetAdmin(ctx context.Context, id int64) (*AdminAccount, error)
}

// AdminSession is a server-side session row. SID is a SHA-256 hex of the
// opaque token, never the token itself.
type AdminSession struct {
	SID       string
	AdminID   int64
	ExpiresAt int64
	CSRF      string
}

// SessionRepository is the session store port.
type SessionRepository interface {
	CreateSession(ctx context.Context, s *AdminSession) error
	GetSession(ctx context.Context, sid string) (*AdminSession, error)
	DeleteSession(ctx context.Context, sid string) error
}

// AuthConfig controls admin login mechanics.
type AuthConfig struct {
	SessionTTL     time.Duration
	PasswordMinLen int
	RequireTOTP    bool
	Now            func() time.Time
}

// DefaultAuthConfig returns the security-spec baseline (15 min idle, 14
// character admin passwords, mandatory TOTP).
func DefaultAuthConfig() AuthConfig {
	return AuthConfig{
		SessionTTL:     15 * time.Minute,
		PasswordMinLen: 14,
		RequireTOTP:    true,
		Now:            time.Now,
	}
}

// LoginResult is returned once, at login.
type LoginResult struct {
	Token     string          `json:"token"`
	CSRF      string          `json:"csrf_token"`
	ExpiresAt int64           `json:"expires_at"`
	Admin     *identity.Admin `json:"admin"`
}

// AuthService owns admin credential/session mechanics. Lockout escalation is
// middleware's job; this service only verifies and issues sessions.
type AuthService struct {
	admins   AdminRepository
	sessions SessionRepository
	hasher   *authn.Hasher
	cfg      AuthConfig
}

// NewAuthService builds the auth service.
func NewAuthService(admins AdminRepository, sessions SessionRepository, hasher *authn.Hasher, cfg AuthConfig) *AuthService {
	if hasher == nil {
		hasher = authn.NewHasher(authn.DefaultArgon2Params())
	}
	def := DefaultAuthConfig()
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = def.SessionTTL
	}
	if cfg.PasswordMinLen <= 0 {
		cfg.PasswordMinLen = def.PasswordMinLen
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &AuthService{admins: admins, sessions: sessions, hasher: hasher, cfg: cfg}
}

// HashPassword hashes an admin password with argon2id, enforcing the
// management-plane minimum length (security-spec §2.4: 14).
func (s *AuthService) HashPassword(password string) (string, error) {
	if len(password) < s.cfg.PasswordMinLen {
		return "", fmt.Errorf("%w: password must be at least %d characters", ErrInvalid, s.cfg.PasswordMinLen)
	}
	if len(password) > 1024 {
		return "", fmt.Errorf("%w: password too long", ErrInvalid)
	}
	return s.hasher.Hash(password)
}

// DummyVerify burns a full argon2 computation. The login handler calls it
// when the account is locked or unknown so timing does not reveal account
// existence or lock state (security-spec §2.4 / A-2).
func (s *AuthService) DummyVerify(password string) {
	_ = s.hasher.VerifyOrFake(password)
}

// Login verifies password + TOTP and creates a session. All verification
// failures return ErrUnauthorized with no detail (the handler renders one
// uniform 401 body).
func (s *AuthService) Login(ctx context.Context, username, password, totpCode string) (*LoginResult, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		s.hasher.VerifyOrFake(password)
		return nil, ErrUnauthorized
	}
	acc, err := s.admins.GetAdminByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.hasher.VerifyOrFake(password)
			return nil, ErrUnauthorized
		}
		return nil, fmt.Errorf("auth: lookup admin: %w", err)
	}
	ok, verr := s.hasher.Verify(password, acc.PasswordHash)
	if verr != nil || !ok {
		return nil, ErrUnauthorized
	}
	if s.cfg.RequireTOTP || acc.TOTPSecret != "" {
		if acc.TOTPSecret == "" || !totp.Validate(acc.TOTPSecret, totpCode, s.cfg.Now()) {
			return nil, ErrUnauthorized
		}
	}
	token, err := randomToken(32)
	if err != nil {
		return nil, fmt.Errorf("auth: token: %w", err)
	}
	csrf, err := randomToken(24)
	if err != nil {
		return nil, fmt.Errorf("auth: csrf: %w", err)
	}
	expires := s.cfg.Now().Add(s.cfg.SessionTTL).Unix()
	sess := &AdminSession{SID: tokenHash(token), AdminID: acc.ID, ExpiresAt: expires, CSRF: csrf}
	if err := s.sessions.CreateSession(ctx, sess); err != nil {
		return nil, fmt.Errorf("auth: create session: %w", err)
	}
	role := identity.RoleFromInt(acc.Role)
	return &LoginResult{
		Token:     token,
		CSRF:      csrf,
		ExpiresAt: expires,
		Admin: &identity.Admin{
			AdminID:   acc.ID,
			Username:  acc.Username,
			Role:      role,
			SessionID: sess.SID,
			CSRF:      csrf,
			ExpiresAt: expires,
		},
	}, nil
}

// Resolve implements middleware.SessionResolver.
func (s *AuthService) Resolve(ctx context.Context, token string) (*identity.Admin, error) {
	if token == "" || len(token) > 512 {
		return nil, ErrUnauthorized
	}
	sid := tokenHash(token)
	sess, err := s.sessions.GetSession(ctx, sid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrUnauthorized
		}
		return nil, fmt.Errorf("auth: get session: %w", err)
	}
	if s.cfg.Now().Unix() >= sess.ExpiresAt {
		_ = s.sessions.DeleteSession(ctx, sid)
		return nil, ErrUnauthorized
	}
	acc, err := s.admins.GetAdmin(ctx, sess.AdminID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			_ = s.sessions.DeleteSession(ctx, sid)
			return nil, ErrUnauthorized
		}
		return nil, fmt.Errorf("auth: get admin: %w", err)
	}
	return &identity.Admin{
		AdminID:   acc.ID,
		Username:  acc.Username,
		Role:      identity.RoleFromInt(acc.Role),
		SessionID: sess.SID,
		CSRF:      sess.CSRF,
		ExpiresAt: sess.ExpiresAt,
	}, nil
}

// Logout revokes the session. Unknown tokens are a no-op (idempotent).
func (s *AuthService) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	err := s.sessions.DeleteSession(ctx, tokenHash(token))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("auth: logout: %w", err)
	}
	return nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomToken(n int) (string, error) {
	if n <= 0 {
		n = 32
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
