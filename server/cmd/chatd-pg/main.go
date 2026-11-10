// main.go — chatd-pg: the chatd cold-store (PostgreSQL) build variant.
//
// The main chatd binary deliberately carries NO postgres driver (architecture
// red line, server/cmd/chatd/assemble.go step 8): the deployment layer injects
// it. This variant IS that deployment layer. It:
//
//  1. registers the "postgres" driver via a blank import of pgx/v5/stdlib;
//  2. reads the same config as chatd (chatd.toml — the openchat.toml
//     equivalent; unknown keys are ignored, see server/cmd/chatd/config.go);
//  3. runs the startup self-check:
//     - cold_dsn configured but "postgres" NOT registered → hard, explicit
//     startup failure (never a silent hot-only fallback here: this binary
//     exists precisely to supply the driver);
//     - cold_dsn configured + driver present → Ping the cold DB and Append a
//     startup audit event to sys.audit_log (hash-chained) to prove the audit
//     journal is writable end-to-end before chatd starts;
//  4. optionally hands off to the standard chatd binary (CHATD_BIN) with the
//     driver guarantee already established; if CHATD_BIN is unset this entry
//     runs the self-check only and exits 0 (CI / smoke-check mode).
//
// No go.sum churn: golang.org/x/crypto is pinned to v0.24.0 to match the main
// module (deploy/chatd-pg is a separate module with a replace to ../..).
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers database/sql driver "postgres"

	"github.com/yingo-server/OpenDiscord/server/internal/data/store"
	"github.com/yingo-server/OpenDiscord/server/internal/data/store/pg"
)

// config is the minimal chatd.toml subset this entry needs. It deliberately
// re-implements the tiny TOML parser contract (comments, sections, key=value,
// quoted strings) instead of parsing full TOML: chatd owns the file and
// unknown keys must not break a deployment.
type config struct {
	ColdDSN         string
	RequireAuditSto bool
	OtherKeys       int // diagnostics: keys parsed but not interpreted
}

// loadConfig parses path as the chatd config subset. Missing file is not an
// error (defaults apply, cold_dsn empty → self-check is a no-op pass).
func loadConfig(path string) (*config, error) {
	cfg := &config{}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return nil, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		if i := strings.Index(v, " #"); i >= 0 { // trailing comment after unquoted value
			v = strings.TrimSpace(v[:i])
		}
		switch k {
		case "cold_dsn":
			cfg.ColdDSN = v
		case "require_audit_store":
			b, err := strconv.ParseBool(v)
			if err == nil {
				cfg.RequireAuditSto = b
			}
		default:
			cfg.OtherKeys++
		}
	}
	return cfg, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "chatd-pg: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := os.Getenv("CHATD_CONFIG")
	if cfgPath == "" {
		cfgPath = "config/chatd.toml"
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("read config %s: %w", cfgPath, err)
	}

	// Driver registration self-check (deliverable 4, first half).
	registered := false
	for _, d := range sql.Drivers() {
		if d == "postgres" {
			registered = true
			break
		}
	}
	fmt.Printf("chatd-pg: config=%s cold_dsn_set=%v driver_registered=%v (drivers: %v)\n",
		cfgPath, cfg.ColdDSN != "", registered, sql.Drivers())

	if cfg.ColdDSN == "" {
		fmt.Println("chatd-pg: cold_dsn empty — nothing to verify; startup self-check PASSED (no-op)")
	} else if !registered {
		// Explicit, loud failure. Cannot happen with the pgx blank import
		// above, which is the point: if a future refactor drops it, this
		// binary must refuse to start instead of silently degrading chatd.
		return fmt.Errorf("cold_dsn is configured but the \"postgres\" driver is NOT registered — "+
			"the pgx/stdlib blank import in deploy/chatd-pg/main.go is missing or broken; "+
			"refusing to start (require_audit_store=%v)", cfg.RequireAuditSto)
	} else {
		if err := verifyCold(cfg.ColdDSN); err != nil {
			return fmt.Errorf("cold store self-check FAILED: %w", err)
		}
		fmt.Println("chatd-pg: cold store self-check PASSED (ping + audit Append OK)")
	}

	// Hand off to the standard chatd binary if requested; the driver is
	// guaranteed registered in this process tree, so assemble.go step 8 will
	// open the cold store instead of logging the driver-not-registered ERROR.
	if bin := os.Getenv("CHATD_BIN"); bin != "" {
		fmt.Printf("chatd-pg: handing off to %s (args: %v)\n", bin, os.Args[1:])
		return execChatd(bin, os.Args[1:])
	}
	fmt.Println("chatd-pg: CHATD_BIN unset — self-check only, exiting 0 (CI/smoke mode)")
	return nil
}

// verifyCold pings the cold DB and appends one startup audit event through the
// production write path (pg.ColdDB.Append → sys.audit_log, hash-chained).
func verifyCold(dsn string) error {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer db.Close()
	cold := &pg.ColdDB{DB: db, DriverName: "postgres"}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := cold.Ping(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	ev := store.AuditEvent{
		Actor:   "system",
		Action:  "ops.startup_selfcheck",
		Subject: "cold",
		Detail:  []byte(`{"variant":"chatd-pg","driver":"pgx/v5/stdlib"}`),
		At:      time.Now().Unix(),
	}
	if err := cold.Append(ctx, ev); err != nil {
		return fmt.Errorf("audit Append to sys.audit_log: %w", err)
	}
	return nil
}
