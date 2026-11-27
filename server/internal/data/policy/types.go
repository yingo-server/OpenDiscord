// Package policy loads, validates and snapshots the operational policy
// document (policy.json). It is the SINGLE source of policy truth for the data
// layer: components ask "what is the current retention?", never hardcode
// numbers. It never executes policy — that is each job component's job.
package policy

// Policy is the wire format of /etc/chat/policy.json (docs §4.2).
// All time/duration units are integer seconds/days; crons are 5-field cron.
type Policy struct {
	// HotRetentionDays: messages older than this many days are archivable.
	// The archiver itself never decides how old is old — it reads this.
	HotRetentionDays int `json:"hot_retention_days"`

	// Archive controls the hot->cold copy job (mechanism: jobber+archiver).
	Archive ArchiveConfig `json:"archive"`

	// Backup controls the hot-DB backup job (VACUUM INTO given dir).
	Backup BackupConfig `json:"backup"`

	// Trim lists tables the trimmer deletes expired rows from.
	// Table names are validated against a whitelist — never arbitrary SQL.
	Trim []TrimRule `json:"trim"`

	// RateLimit holds per-bucket thresholds. The rate_limit table provides
	// the counting primitive; these are the limits.
	RateLimit RateLimitConfig `json:"rate_limit"`

	// WritePath tunes the group-commit write pipeline (flusher).
	WritePath WritePathConfig `json:"write_path"`

	// Capacity holds the V2-0 admission capacity contract (docs/capacity-contract.md).
	// Unlike rate_limit (per-user token buckets), these are GLOBAL axes:
	// process-wide concurrent connections and process-wide admission QPS.
	// 0 = disabled (the historical behavior); negative values are rejected by
	// Validate and treated as disabled by consumers.
	Capacity CapacityConfig `json:"capacity"`

	// Auth holds the authentication surface switches.
	//
	// Every login method is DISABLED by default (Go zero value = false): a
	// deployment must opt in explicitly. The public endpoint
	// GET /api/auth/methods (unauthenticated, but rate limited) reports the
	// live state so a client can render only the tabs that actually work.
	Auth AuthConfig `json:"auth"`

	// Group holds the group-conversation limits.
	//
	// MaxMembers is the hard ceiling on the member count of a group
	// conversation. The bound exists because every membership change
	// broadcasts a FULL 202 CONV_PAYLOAD snapshot to every member: at
	// ~60 bytes per member, a group of more than ~1090 members produces a
	// frame larger than the 64KB WS frame limit and the codec REJECTS it.
	// The ceiling keeps the snapshot inside the frame budget.
	Group GroupConfig `json:"group"`
}

// AuthConfig holds the authentication surface switches.
//
// The four login methods are independent. A disabled method rejects its
// requests with 403 METHOD_DISABLED before any parameter is parsed, so a
// disabled method leaks nothing about its internals.
type AuthConfig struct {
	// Methods gates the login entry points.
	Methods AuthMethodsConfig `json:"methods"`
	// MethodsProbePerIPPerMin is the per-IP budget of the public discovery
	// endpoint (GET /api/auth/methods). 0 = use the built-in default (60/min).
	MethodsProbePerIPPerMin int `json:"methods_probe_per_ip_per_min"`
}

// AuthMethodsConfig is the per-method login switch set.
//
// All four default to false: a deployment that ships without an explicit
// "auth" section has NO usable login method. That is deliberate — the
// surface is opt-in, not opt-out (fail-closed).
//
// NOTE: the built-in defaults used when policy.json is missing also leave
// every method off. The shipped configuration templates enable guest and
// password so a fresh install is not locked out of its own server.
type AuthMethodsConfig struct {
	// Phone gates POST /api/auth/phone/code and /api/auth/phone/verify.
	Phone bool `json:"phone"`
	// Email gates POST /api/auth/email/link and /api/auth/email/poll.
	Email bool `json:"email"`
	// Password gates POST /api/auth/password (login; it does NOT gate
	// /api/auth/password/set, which is an authenticated operation).
	Password bool `json:"password"`
	// Guest gates POST /api/auth/guest.
	Guest bool `json:"guest"`
}

// AnyEnabled reports whether at least one login method is usable. The
// assembler logs a prominent warning when it is false: the server is up but
// nobody can log in.
func (m AuthMethodsConfig) AnyEnabled() bool {
	return m.Phone || m.Email || m.Password || m.Guest
}

// ArchiveConfig is the archive job policy.
type ArchiveConfig struct {
	Cron string `json:"cron"` // e.g. "0 4 * * *"
	// NOTE (jobber contract): the job scheduler does NOT implement full cron.
	// It accepts exactly three forms: "@daily HH:00", "@daily HH:MM" and
	// "interval N" (seconds). Classic 5-field values like "0 4 * * *" are
	// ALSO accepted by job.ParseCronKind but reduced to their hour:minute
	// (daily semantics); fields with */n or lists are rejected at parse time.
	// Complex schedules must be simplified by the ASSEMBLY layer into the
	// "@daily HH:MM" form before registering the job unit.
	BatchSize    int `json:"batch_size"`     // rows per copy transaction, 1..50000
	BatchPauseMS int `json:"batch_pause_ms"` // pause between batches, 0 allowed
	// ActiveExempt: conversations with recent activity are skipped this run.
	ActiveExempt bool `json:"active_exempt"`
}

// BackupConfig is the backup job policy.
type BackupConfig struct {
	Cron     string `json:"cron"`
	Dir      string `json:"dir"`
	KeepDays int    `json:"keep_days"`
}

// TrimRule is one table's expiry rule. Exactly one of TTLHours /
// RetentionDays must be set (>=1).
type TrimRule struct {
	Table         string `json:"table"` // must be in the trim whitelist
	TTLHours      int    `json:"ttl_hours,omitempty"`
	RetentionDays int    `json:"retention_days,omitempty"`
}

// RateLimitConfig holds bucket thresholds.
type RateLimitConfig struct {
	MsgPerUserPerMin int `json:"msg_per_user_per_min"`
	LoginPerIPPerMin int `json:"login_per_ip_per_min"`
}

// CapacityConfig is the explicit admission capacity contract (V2-0,
// docs/capacity-contract.md). Both axes are optional and independent:
//   - MaxConcurrentConns: global connection ceiling — RESERVED for a later
//     phase; connection-count admission is currently the deployment layer's
//     responsibility. Validation accepts >=0; no runtime enforcement yet.
//   - AdmissionQPS: global admission QPS ceiling, enforced by the limiter's
//     shared global bucket on MSG_SEND. Over-limit frames take the usual
//     Throttle path (kind 13 with ack_id). 0 = disabled.
//
// Negative values are rejected at load time; a consumer that still sees one
// (hand-built Config in tests) treats it as disabled.
type CapacityConfig struct {
	MaxConcurrentConns int `json:"max_concurrent_conns"`
	AdmissionQPS       int `json:"admission_qps"`
}

// WritePathConfig tunes the group-commit write pipeline (flusher): how many
// messages ride one transaction, and how long a partial batch waits before it
// is committed. Both are pure performance knobs — they never change the
// all-or-nothing / dedup semantics of a batch.
type WritePathConfig struct {
	// BatchSize is the maximum number of messages committed per transaction.
	// Default 1000. Range 1..8000: larger batches raise throughput but also
	// the worst-case commit latency and peak memory, so the ceiling is well
	// below the point where a single transaction stalls readers.
	BatchSize int `json:"batch_size"`
	// FlushIntervalMS is the maximum time a message waits for batch mates
	// before the partial batch is committed. Default 50ms. Range 5..2000:
	// smaller is lower latency but more commits; larger amortises fsyncs at
	// the cost of p50/p95 write ACK latency.
	FlushIntervalMS int `json:"flush_interval_ms"`
}

// MaxGroupMembersHardLimit is the absolute ceiling a policy may set for
// group.max_members. The full CONV_PAYLOAD snapshot costs ~60 bytes per member
// and the WS frame codec caps a frame at 64KB, so anything above ~1090 members
// cannot be delivered in one frame; 1000 keeps a safety margin.
const MaxGroupMembersHardLimit = 1000

// GroupConfig holds the group-conversation limits.
type GroupConfig struct {
	// MaxMembers is the ceiling on a group's member count (default 100).
	//
	// Two independent limits apply:
	//   - framing: the full CONV_PAYLOAD snapshot is ~60 bytes/member and a
	//     WS frame is capped at 64KB, so >~1090 members cannot be delivered
	//     in one frame (see MaxGroupMembersHardLimit).
	//   - capacity: every message fans out one frame per member, so a group's
	//     cost is (members x messages/s). Measured on a 2-core box the frame
	//     ceiling is ~40k frames/s; a 500-member group is therefore NOT usable
	//     on 2 cores (measured 2.2% delivery). Default 100 keeps a busy
	//     100-member group inside the 2-core budget; raise it only after
	//     sizing against the fan-out budget in docs/known-issues.md.
	MaxMembers int `json:"max_members"`
}

// WritePath defaults and validation bounds.
const (
	// DefaultWriteBatchSize is the group-commit batch size when policy omits
	// write_path.batch_size. 500 is the measured p95 optimum (docs/perf-sqlite-write-path.md:
	// batch=500 p95≈70ms vs 1000≈187ms; higher batches only worsen latency).
	DefaultWriteBatchSize = 500
	// DefaultWriteFlushIntervalMS is the group-commit flush interval when
	// policy omits write_path.flush_interval_ms.
	DefaultWriteFlushIntervalMS = 50
	// Validation bounds for write_path (see Validate).
	MinWriteBatchSize       = 1
	MaxWriteBatchSize       = 8000
	MinWriteFlushIntervalMS = 5
	MaxWriteFlushIntervalMS = 2000
)

// Snapshot pairs a Policy with its version, so callers can detect reloads.
type Snapshot struct {
	Policy  Policy
	Version int // monotonically increasing; bumps on every successful Reload
}

// Default returns the built-in baseline policy. Real deployments override
// via policy.json; defaults exist so a missing field degrades predictably.
func Default() Policy {
	return Policy{
		HotRetentionDays: 90,
		Archive: ArchiveConfig{
			Cron:         "0 4 * * *",
			BatchSize:    1000,
			BatchPauseMS: 100,
			ActiveExempt: true,
		},
		Backup: BackupConfig{
			Cron:     "0 3 * * *",
			Dir:      "/var/lib/chat/backup",
			KeepDays: 14,
		},
		Trim: []TrimRule{
			{Table: "sys_rate_limit", RetentionDays: 7},
			{Table: "identity_login_history", RetentionDays: 180},
		},
		RateLimit: RateLimitConfig{
			MsgPerUserPerMin: 60,
			LoginPerIPPerMin: 10,
		},
		WritePath: WritePathConfig{
			BatchSize:       DefaultWriteBatchSize,
			FlushIntervalMS: DefaultWriteFlushIntervalMS,
		},
		Group: GroupConfig{
			MaxMembers: 100,
		},
	}
}
