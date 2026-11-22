// Package migrate runs per-domain SQL migrations against a single database.
//
// Mechanism only: it applies whatever ordered SQL files the migration tree
// provides. Retention values, cron schedules and other tunables are policy
// (policy.json / feature flags) and never appear here.
package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrChecksumMismatch is returned when an already-applied migration file has
// been modified since it was applied. The process must refuse to start — a
// tampered history is a corruption signal, not something to heal.
var ErrChecksumMismatch = errors.New("migrate: applied migration checksum mismatch")

// Dialect names.
const (
	DialectSQLite   = "sqlite"
	DialectPostgres = "postgres"
)

// Root directories inside the migrations tree, per dialect.
const (
	DirHot   = "hot"   // SQLite, hot DB
	DirCold  = "cold"  // PostgreSQL, cold DB
	DirAdmin = "admin" // SQLite, standalone admin.db
)

const migrTable = "_migrations"

// Migration is one parsed SQL file.
type Migration struct {
	Domain   string // e.g. "identity"
	Version  int    // 1 for 0001_*.sql
	Name     string // file base name, e.g. "0001_init.sql"
	Path     string // full path within the FS
	SQL      string
	Checksum string // hex SHA256 of file content
}

// DomainStatus is the applied-version state of one domain.
type DomainStatus struct {
	Domain      string
	Version     int    // highest applied version, 0 if none
	LastApplied string // name of the highest applied file, "" if none
}

// Runner applies migrations for one database.
//
// Dialect selects the SQL sub-tree root (hot/cold/admin) unless Root is set
// explicitly. Dir is the migration filesystem; embed.FS from
// server/store/migrations satisfies it, and tests may supply any fs.FS.
type Runner struct {
	Dialect string // "sqlite" or "postgres"
	Dir     fs.FS
	DB      *sql.DB

	// Root overrides the directory derived from Dialect ("hot", "cold", "admin").
	Root string
}

func (r *Runner) rootDir() string {
	if r.Root != "" {
		return r.Root
	}
	switch r.Dialect {
	case DialectPostgres:
		return DirCold
	default:
		return DirHot
	}
}

func (r *Runner) quote(id string) string {
	return `"` + strings.ReplaceAll(id, `"`, `""`) + `"`
}

// Discover lists all migrations for every domain under the runner's root, in
// (domain, version) order. It errors on bad layout or naming, or duplicate
// versions within a domain.
func (r *Runner) Discover() ([]Migration, error) {
	root := r.rootDir()
	var files []string
	err := fs.WalkDir(r.Dir, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".sql") {
			return nil
		}
		files = append(files, p)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("migrate: walk %s: %w", root, err)
	}

	byDomain := map[string][]Migration{}
	for _, p := range files {
		// fs.FS paths are slash-separated and rooted at the FS root; derive
		// the relative path by trimming the walk root prefix (fs.Rel does not
		// exist in the stdlib).
		rel := strings.TrimPrefix(p, root+"/")
		parts := strings.Split(rel, "/") // domain/file.sql (root already trimmed)
		if len(parts) != 2 {
			return nil, fmt.Errorf("migrate: unexpected layout %q (want %s/{domain}/{NNNN}_{name}.sql)", rel, root)
		}
		domain, base := parts[0], parts[1]
		version, ok := parseVersion(base)
		if !ok {
			return nil, fmt.Errorf("migrate: bad file name %q (want NNNN_name.sql)", base)
		}
		body, rerr := fs.ReadFile(r.Dir, p)
		if rerr != nil {
			return nil, fmt.Errorf("migrate: read %s: %w", p, rerr)
		}
		sum := sha256.Sum256(body)
		byDomain[domain] = append(byDomain[domain], Migration{
			Domain:   domain,
			Version:  version,
			Name:     base,
			Path:     p,
			SQL:      string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}

	domains := make([]string, 0, len(byDomain))
	for d := range byDomain {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	var out []Migration
	for _, d := range domains {
		ms := byDomain[d]
		sort.Slice(ms, func(i, j int) bool { return ms[i].Version < ms[j].Version })
		for i := 1; i < len(ms); i++ {
			if ms[i].Version == ms[i-1].Version {
				return nil, fmt.Errorf("migrate: duplicate version %04d in domain %s", ms[i].Version, d)
			}
		}
		out = append(out, ms...)
	}
	return out, nil
}

// MigrationKey identifies a migration row in the applied map.
func MigrationKey(domain string, version int) string {
	return fmt.Sprintf("%s/%04d", domain, version)
}

// VerifyChecksum checks one migration against an applied map (domain/version
// key -> stored checksum). Not-applied verifies trivially.
func VerifyChecksum(applied map[string]string, m Migration) bool {
	cs, ok := applied[MigrationKey(m.Domain, m.Version)]
	return !ok || cs == m.Checksum
}

// SortMigrations orders by (domain asc, version asc) and is pure.
func SortMigrations(ms []Migration) []Migration {
	out := append([]Migration(nil), ms...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Domain != out[j].Domain {
			return out[i].Domain < out[j].Domain
		}
		return out[i].Version < out[j].Version
	})
	return out
}

// parseVersion accepts names like "0001_init.sql" -> (1, true).
func parseVersion(base string) (int, bool) {
	if len(base) < 9 || base[len(base)-4:] != ".sql" || base[4] != '_' {
		return 0, false
	}
	for i := 0; i < 4; i++ {
		if base[i] < '0' || base[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(base[:4])
	if err != nil {
		return 0, false
	}
	return n, true
}

// ensureTable creates the bookkeeping table:
//
//	_migrations(domain TEXT, version INT, name TEXT, applied_at INT,
//	            checksum TEXT, PRIMARY KEY(domain, version))
func (r *Runner) ensureTable(ctx context.Context) error {
	q := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
	domain     TEXT    NOT NULL,
	version    INTEGER NOT NULL,
	name       TEXT    NOT NULL,
	applied_at INTEGER NOT NULL,
	checksum   TEXT    NOT NULL,
	PRIMARY KEY (domain, version)
)`, r.quote(migrTable))
	_, err := r.DB.ExecContext(ctx, q)
	return err
}

func (r *Runner) applied(ctx context.Context) (map[string]string, error) {
	rows, err := r.DB.QueryContext(ctx,
		fmt.Sprintf("SELECT domain, version, checksum FROM %s", r.quote(migrTable)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var domain string
		var version int
		var checksum string
		if err := rows.Scan(&domain, &version, &checksum); err != nil {
			return nil, err
		}
		out[MigrationKey(domain, version)] = checksum
	}
	return out, rows.Err()
}

// Up applies all not-yet-applied migrations across every domain, each in its
// own transaction. Applied files are checksum-verified first: any mismatch
// fails the whole run with ErrChecksumMismatch before new SQL executes.
func (r *Runner) Up(ctx context.Context) error {
	if err := r.ensureTable(ctx); err != nil {
		return fmt.Errorf("migrate: create %s: %w", migrTable, err)
	}
	all, err := r.Discover()
	if err != nil {
		return err
	}
	done, err := r.applied(ctx)
	if err != nil {
		return fmt.Errorf("migrate: read %s: %w", migrTable, err)
	}

	// Integrity pass first: refuse to touch anything on tampered history.
	for _, m := range all {
		if !VerifyChecksum(done, m) {
			return fmt.Errorf("%w: domain %s version %04d (%s)", ErrChecksumMismatch, m.Domain, m.Version, m.Name)
		}
	}

	for _, m := range all {
		if _, ok := done[MigrationKey(m.Domain, m.Version)]; ok {
			continue
		}
		if err := r.applyOne(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) applyOne(ctx context.Context, m Migration) error {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate: begin tx for %s: %w", m.Name, err)
	}
	defer tx.Rollback() // no-op after commit

	// PostgreSQL (pgx) does not accept a whole multi-statement SQL file as one
	// ExecContext the way mattn/sqlite3 does, and uses $n (not ?) placeholders.
	// Split on semicolons and execute each statement individually. Comments
	// lines are kept attached to the following statement (harmless to PG).
	if r.Dialect == DialectPostgres {
		// Strip -- line comments first: a semicolon inside a comment (e.g.
		// "indexable; full-text") must not split a statement. Then split on
		// semicolons and run each statement in the same transaction.
		clean := strings.Builder{}
		for _, line := range strings.Split(m.SQL, "\n") {
			if idx := strings.Index(line, "--"); idx >= 0 {
				line = line[:idx]
			}
			clean.WriteString(line)
			clean.WriteString("\n")
		}
		for _, stmt := range strings.Split(clean.String(), ";") {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("migrate: exec %s: %w", m.Name, err)
			}
		}
	} else if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return fmt.Errorf("migrate: exec %s: %w", m.Name, err)
	}
	// Placeholder style is dialect-specific: SQLite accepts ?, PostgreSQL
	// only $n. Using ? on PG is the classic "syntax error near ," source.
	vals := []any{m.Domain, m.Version, m.Name, timeNowUnix(), m.Checksum}
	ins := ""
	if r.Dialect == DialectPostgres {
		ins = fmt.Sprintf("INSERT INTO %s (domain, version, name, applied_at, checksum) VALUES ($1, $2, $3, $4, $5)", r.quote(migrTable))
	} else {
		ins = fmt.Sprintf("INSERT INTO %s (domain, version, name, applied_at, checksum) VALUES (?, ?, ?, ?, ?)", r.quote(migrTable))
	}
	if _, err := tx.ExecContext(ctx, ins, vals...); err != nil {
		return fmt.Errorf("migrate: record %s: %w", m.Name, err)
	}
	return tx.Commit()
}

// Status returns the current applied version of every domain discovered under
// the runner's root, sorted by domain name. Domains with no applied migration
// report Version 0.
func (r *Runner) Status(ctx context.Context) ([]DomainStatus, error) {
	if err := r.ensureTable(ctx); err != nil {
		return nil, fmt.Errorf("migrate: create %s: %w", migrTable, err)
	}
	all, err := r.Discover()
	if err != nil {
		return nil, err
	}
	applied, err := r.applied(ctx)
	if err != nil {
		return nil, fmt.Errorf("migrate: read %s: %w", migrTable, err)
	}

	latest := map[string]Migration{}
	for _, m := range all {
		if _, ok := applied[MigrationKey(m.Domain, m.Version)]; ok {
			if prev, ok := latest[m.Domain]; !ok || m.Version > prev.Version {
				latest[m.Domain] = m
			}
		}
	}
	domains := make([]string, 0, len(latest))
	for d := range latest {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	out := make([]DomainStatus, 0, len(domains))
	for _, d := range domains {
		m := latest[d]
		out = append(out, DomainStatus{Domain: d, Version: m.Version, LastApplied: m.Name})
	}
	return out, nil
}

// timeNowUnix is a test seam; the package avoids broader time mocking.
var timeNowUnix = func() int64 { return time.Now().Unix() }
