package policy

import (
	"fmt"
	"strings"
)

// TrimWhitelist is the closed set of tables the trimmer may ever touch.
// Anything else in policy.trim is rejected at load time — the trimmer only
// ever deletes with a fixed (table, expiry-column, TTL) plan, no dynamic SQL.
var TrimWhitelist = map[string]bool{
	"sys_rate_limit":         true,
	"identity_login_history": true,
}

// Validate checks semantic legality of a Policy and returns one error per
// offending field, in stable order. Empty result means valid.
func Validate(p Policy) []error {
	var errs []error

	if p.HotRetentionDays < 1 {
		errs = append(errs, fieldErr("hot_retention_days", "must be >= 1 (0 would archive everything immediately)"))
	}

	// archive
	if strings.TrimSpace(p.Archive.Cron) == "" {
		errs = append(errs, fieldErr("archive.cron", "must not be empty"))
	}
	if p.Archive.BatchSize < 1 || p.Archive.BatchSize > 50000 {
		errs = append(errs, fieldErr("archive.batch_size", "must be within 1..50000"))
	}
	if p.Archive.BatchPauseMS < 0 {
		errs = append(errs, fieldErr("archive.batch_pause_ms", "must be >= 0"))
	}

	// backup
	if strings.TrimSpace(p.Backup.Cron) == "" {
		errs = append(errs, fieldErr("backup.cron", "must not be empty"))
	}
	if strings.TrimSpace(p.Backup.Dir) == "" {
		errs = append(errs, fieldErr("backup.dir", "must not be empty"))
	}
	if p.Backup.KeepDays < 1 {
		errs = append(errs, fieldErr("backup.keep_days", "must be >= 1"))
	}

	// trim: whitelist + exactly-one-of ttl_hours/retention_days
	for i, r := range p.Trim {
		if !TrimWhitelist[r.Table] {
			errs = append(errs, fieldErr(fmt.Sprintf("trim[%d].table", i),
				fmt.Sprintf("table %q is not in the trim whitelist", r.Table)))
			continue
		}
		ttl, days := r.TTLHours >= 1, r.RetentionDays >= 1
		switch {
		case ttl && days:
			errs = append(errs, fieldErr(fmt.Sprintf("trim[%d]", i),
				"ttl_hours and retention_days are mutually exclusive"))
		case !ttl && !days:
			errs = append(errs, fieldErr(fmt.Sprintf("trim[%d]", i),
				"one of ttl_hours / retention_days is required (>=1)"))
		case r.TTLHours < 0 || r.RetentionDays < 0:
			errs = append(errs, fieldErr(fmt.Sprintf("trim[%d]", i), "negative value"))
		}
	}

	// rate_limit: non-positive means "no limit", which must be explicit policy,
	// not an accident of a missing field — reject.
	if p.RateLimit.MsgPerUserPerMin < 1 {
		errs = append(errs, fieldErr("rate_limit.msg_per_user_per_min", "must be >= 1"))
	}
	if p.RateLimit.LoginPerIPPerMin < 1 {
		errs = append(errs, fieldErr("rate_limit.login_per_ip_per_min", "must be >= 1"))
	}

	// write_path: batch size and flush interval only affect performance, but
	// out-of-range values must never slip in silently (a 0 batch would stall
	// every commit; a huge interval would blow up write ACK latency).
	// 0 means "unset → use Default()" (Load merges the document onto defaults),
	// so only an explicit out-of-range value is an error. Without this a bare
	// Policy literal (e.g. constructed in tests) would fail validation.
	if p.WritePath.BatchSize != 0 &&
		(p.WritePath.BatchSize < MinWriteBatchSize || p.WritePath.BatchSize > MaxWriteBatchSize) {
		errs = append(errs, fieldErr("write_path.batch_size",
			fmt.Sprintf("must be within %d..%d", MinWriteBatchSize, MaxWriteBatchSize)))
	}
	if p.WritePath.FlushIntervalMS != 0 &&
		(p.WritePath.FlushIntervalMS < MinWriteFlushIntervalMS || p.WritePath.FlushIntervalMS > MaxWriteFlushIntervalMS) {
		errs = append(errs, fieldErr("write_path.flush_interval_ms",
			fmt.Sprintf("must be within %d..%d", MinWriteFlushIntervalMS, MaxWriteFlushIntervalMS)))
	}

	// group: the member ceiling must stay inside the 64KB frame budget.
	// The full CONV_PAYLOAD snapshot is ~60 bytes per member; above ~1090
	// members the frame is rejected by the codec.
	// 0 means "unset → use Default()" (the document is merged onto defaults),
	// so only a negative or out-of-range explicit value is an error.
	if p.Group.MaxMembers < 0 {
		errs = append(errs, fieldErr("group.max_members", "must be >= 1 when set"))
	}
	if p.Group.MaxMembers > MaxGroupMembersHardLimit {
		errs = append(errs, fieldErr("group.max_members",
			fmt.Sprintf("must be <= %d (a larger group would overflow the 64KB WS frame limit)", MaxGroupMembersHardLimit)))
	}

	// capacity: global admission axes (docs/capacity-contract.md). 0 means
	// "unset → disabled" (the historical behavior); a negative value is a
	// policy typo and is rejected so it can never silently disable a limit
	// the operator meant to set.
	if p.Capacity.MaxConcurrentConns < 0 {
		errs = append(errs, fieldErr("capacity.max_concurrent_conns",
			"negative value (0 = disabled; set >= 1 to enable)"))
	}
	if p.Capacity.AdmissionQPS < 0 {
		errs = append(errs, fieldErr("capacity.admission_qps",
			"negative value (0 = disabled; set >= 1 to enable)"))
	}

	return errs
}

// fieldErr formats a single field error for Validate's result.
func fieldErr(field, msg string) error {
	return fmt.Errorf("policy: %s: %s", field, msg)
}
