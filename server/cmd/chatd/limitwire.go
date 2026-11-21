package main

// limitwire.go: bridge data-layer policy (policy.json rate_limit) into the
// user gateway's limiter (P0-2).
//
// ── Mapping semantics (design note; also reflected in docs) ────────────
//
// policy.json's rate_limit section is a USER-LEVEL AGGREGATE budget
// (per user, across all their devices): msg_per_user_per_min=60 means one
// user may send at most 60 messages per minute in total. limiter.DefaultConfig
// (docs/security-spec.md §1.2.2) is a CONNECTION-LEVEL baseline: MSG_SEND
// {Rate: 20/s, Burst: 40} per connection. The two semantics are different
// axes — the limiter enforces BOTH levels on every frame:
//
//	verdict = connLevelOK(user, kind, conn) AND userLevelOK(user, kind)
//
// Wiring policy into the connection-level table would make no sense (it would
// either loosen 20/s to 1/s — breaking the security baseline — or tighten
// something that is already tighter). So the assembly converges ONLY the
// user-level MSG_SEND rule:
//
//	userRate  = msg_per_user_per_min / 60          (per-second mean)
//	userBurst = msg_per_user_per_min / 2           (half a minute of budget)
//	each field is min(policy-derived value, security-spec baseline) — the
//	limiter never exceeds the documented baseline from this path, but stays
//	fully responsive to policy changes in both directions (the anchor is the
//	frozen baseline in limit.DefaultConfig, not the running value).
//
// All other kinds keep the security-spec §1.2.2 baseline untouched — wiring
// login_per_ip_per_min is out of scope here (authsvc/HTTP login limiting is
// a different mechanism with its own policy surface; noted for a later pass).
//
// This is deliberately conservative: the goal is that an operator raising or
// lowering msg_per_user_per_min in policy.json has a real effect on the
// user-level budget, without ever weakening the connection-level baseline.

import (
	"github.com/yingo-server/OpenDiscord/server/internal/data/policy"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/kinds"
	"github.com/yingo-server/OpenDiscord/server/internal/user/net/limit"
)

// limitFromPolicy returns cur with the MSG_SEND rule converged onto the
// policy's user-level aggregate budget (see the file header for semantics).
// It never loosens: the resulting rate/burst/capacity for MSG_SEND is the
// minimum of the policy-derived value and the current rule's value.
//
// The result applies symmetrically to the connection-level and user-level
// paths inside the limiter — the limiter multiplies the user-level bucket by
// the live device count (capped at UserDeviceCap=3), which is what keeps a
// 3-device user within 3× the policy budget while each single connection
// still respects this same (tighter) per-conn rate.
func limitFromPolicy(p policy.Policy, cur limit.Config) limit.Config {
	n := float64(p.RateLimit.MsgPerUserPerMin)
	if n < 1 {
		// Unvalidated policy never reaches here (policy.Validate rejects <1),
		// but a zero value must not zero out the limiter: keep current.
		return cur
	}
	rate := n / 60.0 // per-minute budget → per-second mean
	burst := n / 2.0 // half a minute of budget may burst
	capacity := n    // a fully idle user may bank one minute
	if capacity < 1 {
		capacity = 1
	}

	// The anchor is the security-spec BASELINE (limit.DefaultConfig), not the
	// currently running rule: anchoring on the running value would freeze the
	// tightest-ever setting and make later reloads ineffective in both
	// directions. Anchoring on the baseline means a policy change always takes
	// effect (tighter or looser than the previous reload), while the limiter
	// never exceeds the documented baseline.
	base := limit.DefaultConfig().Rules[uint32(kinds.KindMsgSend)]
	r, ok := cur.Rules[uint32(kinds.KindMsgSend)]
	if !ok {
		// No MSG_SEND rule in the current snapshot: nothing sane to converge.
		return cur
	}
	if rate < base.Rate {
		r.Rate = rate
	} else {
		r.Rate = base.Rate
	}
	if burst < base.Burst {
		r.Burst = burst
	} else {
		r.Burst = base.Burst
	}
	if capacity < base.Capacity {
		r.Capacity = capacity
	} else {
		r.Capacity = base.Capacity
	}
	// MaxPayload/Silent stay exactly as the security-spec baseline: policy
	// has no say over frame size or degradation behavior.
	cur.Rules[uint32(kinds.KindMsgSend)] = r
	// V2-0 准入容量：policy.capacity.admission_qps（0=禁用）→ limiter 全局桶。
	// capacity 配置即启用准入；capacity_admission_enabled flag 只控制审计日志，
	// 不改变准入行为（见 docs/capacity-contract.md）。SIGHUP 热改经 ReloadPolicy 生效。
	cur.GlobalQPS = float64(p.Capacity.AdmissionQPS)
	return cur
}
