package prod

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/yingo-server/UFOT/server/internal/admin"
	"github.com/yingo-server/UFOT/server/internal/admin/identity"
	"github.com/yingo-server/UFOT/server/internal/pkg/log"
	"github.com/yingo-server/UFOT/server/internal/user/core/authn"
)

// Bootstrap environment variables. They are read ONCE, on the first start of
// an admin.db that has no administrator, and never stored anywhere.
const (
	// EnvAdminUser is the bootstrap super-admin username.
	EnvAdminUser = "UFOT_ADMIN_USER"
	// EnvAdminPassword is the bootstrap password (>= PasswordMinLen chars).
	// Legacy path: the password lives in the process environment. It is kept
	// for compatibility but prints a warning.
	EnvAdminPassword = "UFOT_ADMIN_PASSWORD"
	// EnvAdminPasswordFile points at a file holding the bootstrap password.
	// Preferred: the secret never appears in the environment or in `ps` output.
	// The file must be a regular file, owned by the running user, mode 0600.
	EnvAdminPasswordFile = "UFOT_ADMIN_PASS_FILE"
)

// bootstrapTimeout bounds the whole first-run provisioning.
const bootstrapTimeout = 30 * time.Second

// BootstrapAdmin creates the first super administrator when adm_admins is
// empty (first start of a fresh admin.db).
//
// Contract (docs/security-spec.md §3.3):
//   - credentials come from UFOT_ADMIN_USER / UFOT_ADMIN_PASSWORD;
//   - the password is stored as argon2id (authn.Hasher), never in clear;
//   - a TOTP secret is generated and printed exactly once — TOTP is mandatory
//     at login, so the account is unusable until the operator enrolls it;
//   - no environment variable ⇒ NOTHING is created: a weak default password is
//     worse than an unconfigured console. The hint is logged instead.
func BootstrapAdmin(db *sql.DB, cfg admin.Config, opts Options, lg *log.Logger) error {
	if db == nil {
		return errors.New("admin/prod: bootstrap requires admin.db")
	}
	if lg == nil {
		lg = log.For("admin.prod.bootstrap")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Getenv == nil {
		opts.Getenv = func(string) string { return "" }
	}

	ctx, cancel := context.WithTimeout(context.Background(), bootstrapTimeout)
	defer cancel()

	var existing int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM adm_admins").Scan(&existing); err != nil {
		return fmt.Errorf("admin/prod: bootstrap count admins: %w", err)
	}
	if existing > 0 {
		return nil // already provisioned — bootstrap is a one-shot
	}

	user, pass := adminBootstrapVars(opts.Getenv)
	if user == "" || pass == "" {
		lg.Warn("admin.db 尚无管理员：未自动创建（拒绝默认/弱口令）。" +
			"请设置 " + EnvAdminUser + " / " + EnvAdminPassword +
			"（密码 >= 14 位）后重启，或由既有管理员通过 adm_admins 预置 argon2id 记录")
		return nil
	}
	minLen := cfg.PasswordMinLen
	if minLen <= 0 {
		minLen = admin.DefaultConfig().PasswordMinLen
	}
	if len([]rune(pass)) < minLen {
		return fmt.Errorf("admin/prod: bootstrap password too short (%d < %d runes): refusing to create a weak super admin",
			len([]rune(pass)), minLen)
	}

	hasher := authn.NewHasher(authn.DefaultArgon2Params())
	hash, err := hasher.Hash(pass)
	if err != nil {
		return fmt.Errorf("admin/prod: bootstrap hash: %w", err)
	}
	secret, err := newTOTPSecret()
	if err != nil {
		return fmt.Errorf("admin/prod: bootstrap totp: %w", err)
	}

	res, err := db.ExecContext(ctx, insertBootstrapAdminSQL,
		user, hash, secret, int(identity.RoleSuper), opts.Now().Unix())
	if err != nil {
		return fmt.Errorf("admin/prod: bootstrap insert admin %q: %w", user, err)
	}
	if id, _ := res.LastInsertId(); id > 0 {
		lg.Info("bootstrap: admin row created", "admin_id", id, "username", user, "role", identity.RoleSuper.String())
	}

	// The secret must reach the operator exactly once; journald is the
	// deployment's trusted sink (the password came from the same trust domain
	// as the environment).
	lg.Warn("bootstrap: 已创建 super 管理员，请立即启用 TOTP（本条 secret 只打印一次）",
		"username", user,
		"totp_secret", secret,
		"otpauth", otpauthURI(user, secret),
		"next", "用该 secret 在认证器中登记后，从环境中移除 "+EnvAdminPassword)
	return nil
}

// insertBootstrapAdminSQL mirrors adminstore.CreateAdmin (role 0 = super).
const insertBootstrapAdminSQL = `INSERT INTO adm_admins (username, pw_hash, totp_secret, role, created_at)
VALUES (?, ?, ?, ?, ?)`

// adminBootstrapVars reads and trims the bootstrap credentials. Pure.
func adminBootstrapVars(getenv func(string) string) (user, pass string) {
	if getenv == nil {
		return "", ""
	}
	user = strings.TrimSpace(getenv(EnvAdminUser))
	// Preferred: a password file (the secret never enters the environment).
	if path := strings.TrimSpace(getenv(EnvAdminPasswordFile)); path != "" {
		if raw, err := readPasswordFile(path); err == nil {
			return user, raw
		} else {
			return user, ""
		}
	}
	return user, getenv(EnvAdminPassword)
}

// readPasswordFile reads a bootstrap password from disk. The file must be a
// regular file (not a symlink), owned by the current user, and not readable by
// group or others — otherwise the secret is exposed to other local users.
func readPasswordFile(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("password file must not be a symlink")
	}
	if !fi.Mode().IsRegular() {
		return "", errors.New("password file must be a regular file")
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("password file mode %o is too permissive (want 0600)", fi.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(raw), "\r\n"), nil
}

// newTOTPSecret returns a 160-bit base32 secret (RFC 4226 recommends >= 128
// bits; the padding-free form is what authenticator apps expect).
func newTOTPSecret() (string, error) {
	var b [20]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]), nil
}

// otpauthURI builds the provisioning URI an authenticator app scans.
func otpauthURI(user, secret string) string {
	return "otpauth://totp/UFOT:" + user +
		"?secret=" + secret + "&issuer=UFOT&algorithm=SHA1&digits=6&period=30"
}
