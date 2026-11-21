package prod

import (
	"context"
	"encoding/base32"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yingo-server/UFOT/server/internal/admin"
	"github.com/yingo-server/UFOT/server/internal/admin/identity"
	adminstore "github.com/yingo-server/UFOT/server/internal/admin/store"
	"github.com/yingo-server/UFOT/server/internal/user/core/authn"
)

func adminDB(t *testing.T) *adminstore.DB {
	t.Helper()
	db, err := adminstore.Open(filepath.Join(t.TempDir(), "admin.db"))
	if err != nil {
		t.Fatalf("open admin.db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate admin.db: %v", err)
	}
	return db
}

func TestAdminBootstrapVars(t *testing.T) {
	env := map[string]string{EnvAdminUser: " root ", EnvAdminPassword: "p@ssw0rd-long-enough"}
	user, pass := adminBootstrapVars(func(k string) string { return env[k] })
	if user != "root" || pass != "p@ssw0rd-long-enough" {
		t.Fatalf("vars = %q/%q", user, pass)
	}
	if u, p := adminBootstrapVars(nil); u != "" || p != "" {
		t.Fatal("nil getenv must yield empty vars")
	}
}

func TestBootstrapWithoutEnvCreatesNothing(t *testing.T) {
	db := adminDB(t)
	if err := BootstrapAdmin(db.DB, admin.Config{}, Options{
		Getenv: func(string) string { return "" },
		Now:    fixedNow,
	}, discardLogger(t)); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	var n int64
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM adm_admins`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("admins = %d, want 0 (no weak default account)", n)
	}
}

func TestBootstrapRejectsWeakPassword(t *testing.T) {
	db := adminDB(t)
	env := map[string]string{EnvAdminUser: "root", EnvAdminPassword: "short"}
	err := BootstrapAdmin(db.DB, admin.Config{PasswordMinLen: 14}, Options{
		Getenv: func(k string) string { return env[k] },
		Now:    fixedNow,
	}, discardLogger(t))
	if err == nil {
		t.Fatal("weak bootstrap password accepted")
	}
	if !strings.Contains(err.Error(), "too short") {
		t.Fatalf("err = %v", err)
	}
	var n int64
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM adm_admins`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("admins = %d, want 0", n)
	}
}

func TestBootstrapCreatesSuperAdmin(t *testing.T) {
	db := adminDB(t)
	const password = "correct-horse-battery-staple"
	env := map[string]string{EnvAdminUser: "root", EnvAdminPassword: password}
	opts := Options{Getenv: func(k string) string { return env[k] }, Now: fixedNow}
	lg := discardLogger(t)

	if err := BootstrapAdmin(db.DB, admin.DefaultConfig(), opts, lg); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	acc, err := db.GetAdminByUsername(context.Background(), "root")
	if err != nil {
		t.Fatalf("get admin: %v", err)
	}
	if acc.Role != int(identity.RoleSuper) {
		t.Fatalf("role = %d, want super (%d)", acc.Role, int(identity.RoleSuper))
	}
	if acc.CreatedAt != fixedNow().Unix() {
		t.Fatalf("created_at = %d", acc.CreatedAt)
	}
	if !strings.HasPrefix(acc.PWHash, "$argon2id$") {
		t.Fatalf("hash = %q, want $argon2id$...", acc.PWHash)
	}
	ok, err := authn.NewHasher(authn.DefaultArgon2Params()).Verify(password, acc.PWHash)
	if err != nil || !ok {
		t.Fatalf("verify password: ok=%v err=%v", ok, err)
	}
	// TOTP secret must be usable (base32, 160 bits) — login requires it.
	if len(acc.TOTPSecret) != 32 {
		t.Fatalf("totp secret len = %d, want 32", len(acc.TOTPSecret))
	}
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(acc.TOTPSecret)
	if err != nil || len(raw) != 20 {
		t.Fatalf("totp secret decode: len=%d err=%v", len(raw), err)
	}
	if uri := otpauthURI("root", acc.TOTPSecret); !strings.HasPrefix(uri, "otpauth://totp/UFOT:root?secret=") {
		t.Fatalf("otpauth uri = %q", uri)
	}

	// Idempotent: a second start never creates a second account.
	env[EnvAdminUser] = "root2"
	if err := BootstrapAdmin(db.DB, admin.DefaultConfig(), opts, lg); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	var n int64
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM adm_admins`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("admins = %d, want 1", n)
	}
}
