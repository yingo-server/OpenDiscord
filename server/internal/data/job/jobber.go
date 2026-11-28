// Package job: the data layer's cron-style job scheduler and units.
//
// jobber is a deliberately SIMPLE scheduler (docs §1.1: 按 policy 中的 cron
// 调度，不含具体作业逻辑). It does NOT implement full cron. Three trigger
// forms are supported:
//
//	@daily HH:MM   — every day at HH:MM (local time)
//	interval N     — every N seconds
//
// AND it accepts (but reduces) classic 5-field cron expressions: the policy
// document's cron fields use the `0 4 * * *` form (docs policy 示例), and
// ParseCronKind takes ONLY the hour/minute fields from it — the day-of-month /
// month / day-of-week fields are parsed for shape and ignored (daily
// semantics). Conversion of arbitrary schedules is the ASSEMBLY layer's job;
// see policy types comments.
package job

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CronKind is the parsed trigger form.
type CronKind int

const (
	CronDaily    CronKind = iota // @daily HH:MM (or a 5-field cron reduced to daily)
	CronInterval                 // every N seconds
)

func (k CronKind) String() string {
	switch k {
	case CronDaily:
		return "daily"
	case CronInterval:
		return "interval"
	}
	return "unknown"
}

// Spec is a parsed schedule.
type Spec struct {
	Kind     CronKind
	Hour     int // daily: fire hour
	Min      int // daily: fire minute
	Interval time.Duration
}

// ParseCronKind parses the three accepted forms. Input is the policy cron
// string. Accepted:
//
//	"@daily 04:00" | "@daily 5:30" — CronDaily
//	"interval 60"  | "60"          — CronInterval (seconds)
//	"0 4 * * *"    | "30 5 * * *"  — 5-field cron: Hour/Min from fields 1-2
//	  (day/month/dow fields must be present; "*/n" and lists are REJECTED —
//	  simplify in the assembly layer or extend here deliberately)
func ParseCronKind(s string) (kind CronKind, hour int, min int, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, 0, fmt.Errorf("job: empty cron")
	}
	if strings.HasPrefix(s, "@daily") {
		rest := strings.TrimSpace(strings.TrimPrefix(s, "@daily"))
		hm := strings.Split(rest, ":")
		if len(hm) != 2 {
			return 0, 0, 0, fmt.Errorf("job: bad @daily form %q (want @daily HH:MM)", s)
		}
		h, e1 := strconv.Atoi(hm[0])
		m, e2 := strconv.Atoi(hm[1])
		if e1 != nil || e2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			return 0, 0, 0, fmt.Errorf("job: bad @daily time in %q", s)
		}
		return CronDaily, h, m, nil
	}
	if strings.HasPrefix(s, "interval") {
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(s, "interval")))
		if err != nil || n <= 0 {
			return 0, 0, 0, fmt.Errorf("job: bad interval in %q", s)
		}
		return CronInterval, 0, 0, nil
	}
	// plain integer = seconds
	if n, err := strconv.Atoi(s); err == nil {
		if n <= 0 {
			return 0, 0, 0, fmt.Errorf("job: interval must be > 0")
		}
		return CronInterval, 0, 0, nil
	}
	// 5-field cron: take hour/minute, require the rest to be simple.
	f := strings.Fields(s)
	if len(f) != 5 {
		return 0, 0, 0, fmt.Errorf("job: unsupported cron %q (want @daily HH:MM, interval N, or 5-field)", s)
	}
	// Classic cron order: minute hour day-of-month month day-of-week.
	m, e1 := parseField(f[0], 0, 59)
	h, e2 := parseField(f[1], 0, 23)
	if e1 != nil || e2 != nil {
		return 0, 0, 0, fmt.Errorf("job: bad hour/minute in %q (want plain integers, ranges/sets unsupported)", s)
	}
	// dom/month must be "*": a restricted dom/month is a schedule the jobber
	// cannot express, and silently ignoring it would run on the wrong days.
	for _, dom := range f[2:4] {
		if dom != "*" {
			return 0, 0, 0, fmt.Errorf("job: cron %q: field %q unsupported (only daily schedules; simplify at the assembly layer)", s, dom)
		}
	}
	// dow: "*" means every day (expressible); anything else is a schedule the
	// jobber cannot honour, so it fails fast rather than running on wrong days.
	if f[4] != "*" {
		return 0, 0, 0, fmt.Errorf("job: cron %q: day-of-week %q unsupported (only daily schedules)", s, f[4])
	}
	return CronDaily, h, m, nil
}

// parseField accepts a plain integer field.
func parseField(s string, lo, hi int) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("bad field %q", s)
	}
	return n, nil
}

// NextFire computes the next time strictly after now per spec.
func (sp Spec) NextFire(now time.Time) time.Time {
	switch sp.Kind {
	case CronInterval:
		return now.Add(sp.Interval)
	case CronDaily:
		next := time.Date(now.Year(), now.Month(), now.Day(), sp.Hour, sp.Min, 0, 0, now.Location())
		if !next.After(now) {
			next = next.Add(24 * time.Hour)
		}
		return next
	}
	return now
}

// Unit is one schedulable job (docs §5): Run is fully equivalent across the
// three trigger forms (cron / manual API / CLI), dryRun is a first-class mode.
type Unit interface {
	Name() string
	Run(ctx context.Context, dryRun bool) (Result, error)
}

// Result is a job outcome (dry-run reports carry estimates).
type Result struct {
	MovedRows    int64
	ConvsTouched int
	DryRun       bool
	Detail       string
}

// Jobber schedules Units per Specs. Single-flight per job: a unit still
// running never re-enters (manual triggers see ErrRunning).
type Jobber struct {
	mu    sync.Mutex
	units map[string]entry
	lg    Logger
	now   func() time.Time
}

type entry struct {
	unit Unit
	spec Spec
}

// ErrRunning is returned by Trigger for an in-flight job (API answers 409).
var ErrRunning = fmt.Errorf("job: already running")

// Logger is the minimal logging surface (log.Logger satisfies it).
type Logger interface {
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
	Error(msg string, kv ...any)
}

// NewJobber creates the scheduler.
func NewJobber(lg Logger) *Jobber {
	if lg == nil {
		lg = nopLogger{}
	}
	return &Jobber{units: map[string]entry{}, lg: lg, now: time.Now}
}

type nopLogger struct{}

func (nopLogger) Info(string, ...any)  {}
func (nopLogger) Warn(string, ...any)  {}
func (nopLogger) Error(string, ...any) {}

// Register adds a unit with its schedule spec.
func (j *Jobber) Register(u Unit, spec Spec) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.units[u.Name()] = entry{unit: u, spec: spec}
}

// Trigger runs a unit once, single-flight. dryRun passes through.
func (j *Jobber) Trigger(ctx context.Context, name string, dryRun bool) (Result, error) {
	j.mu.Lock()
	e, ok := j.units[name]
	j.mu.Unlock()
	if !ok {
		return Result{}, fmt.Errorf("job: unknown job %q", name)
	}
	res, err := runSingleFlight(ctx, name, e.unit, dryRun, j.lg)
	return res, err
}

// running guards single-flight across Trigger and Run loops.
var (
	runMu   sync.Mutex
	running = map[string]bool{}
)

func runSingleFlight(ctx context.Context, name string, u Unit, dryRun bool, lg Logger) (Result, error) {
	runMu.Lock()
	if running[name] {
		runMu.Unlock()
		return Result{}, ErrRunning
	}
	running[name] = true
	runMu.Unlock()
	defer func() {
		runMu.Lock()
		delete(running, name)
		runMu.Unlock()
	}()

	// 作业失败≠进程失败 (docs §6): errors return to the caller; the process
	// keeps running.
	//
	// A PANIC in a job unit must be even less fatal than an error: one broken
	// unit must never take the whole process (and every other job) down.
	// The panic is recovered here, logged as an error, and turned into an
	// error return so the caller sees a failed job instead of a dead daemon.
	res, err := runUnit(ctx, name, u, dryRun)
	if err != nil {
		lg.Error("job failed", "job", name, "err", err.Error())
	}
	return res, err
}

// runUnit runs a job unit with a panic guard. The recovered value is turned
// into an error (with the panic text) so the caller sees a normal failure.
func runUnit(ctx context.Context, name string, u Unit, dryRun bool) (res Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			res = Result{}
			err = fmt.Errorf("job %q panicked: %v", name, r)
		}
	}()
	return u.Run(ctx, dryRun)
}

// Run is the scheduler loop: fires each unit at its NextFire. Blocks until
// ctx is done. Interval units fire from registration + interval.
func (j *Jobber) Run(ctx context.Context) {
	type next struct {
		name string
		at   time.Time
	}
	for {
		j.mu.Lock()
		nexts := make([]next, 0, len(j.units))
		now := j.now()
		for name, e := range j.units {
			nexts = append(nexts, next{name, e.spec.NextFire(now)})
		}
		j.mu.Unlock()
		if len(nexts) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		// earliest
		earliest := nexts[0]
		for _, n := range nexts[1:] {
			if n.at.Before(earliest.at) {
				earliest = n
			}
		}
		wait := time.Until(earliest.at)
		if wait < 0 {
			wait = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
			j.mu.Lock()
			e, ok := j.units[earliest.name]
			spec := e.spec
			j.mu.Unlock()
			if !ok {
				continue
			}
			runSingleFlight(ctx, earliest.name, e.unit, false, j.lg)
			// advance: for interval the next fire derives from now; for daily,
			// NextFire(now) naturally skips ahead.
			_ = spec
		}
	}
}
