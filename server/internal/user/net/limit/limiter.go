// Package limit 实现 usergw 两级令牌桶（docs/security-spec.md §1.2）。
//
// 纯判定：Allow 只返回 Verdict，不断连、不写队列、不发帧——
// 执行策略（NACK/丢弃/断连）由 router 决定。
//
// 两级粒度：
//   - 连接级：每 conn 一个 kind→bucket 数组（单 goroutine 语义，无锁）
//   - 用户级：uid → 本进程聚合桶，桶值 × 设备数（上限 3）
package limit

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/yingo-server/OpenDiscord/server/internal/proto/kinds"
)

// Verdict 限流判决。
type Verdict int

const (
	// OK 放行。
	OK Verdict = iota
	// Drop 静默丢弃（typing/presence 类 L1 软超限）。
	Drop
	// Throttle 降级/回 NACK（MSG 类 L2，或用户级 L3 高优降级）。
	Throttle
	// Kill 断连（L4：conn 内 60s 超限占比 > 50%，或用户级连续惩罚）。
	Kill
)

func (v Verdict) String() string {
	switch v {
	case OK:
		return "OK"
	case Drop:
		return "DROP"
	case Throttle:
		return "THROTTLE"
	case Kill:
		return "KILL"
	}
	return "UNKNOWN"
}

// KindPriority 是「高优帧」的合成 kind：Envelope.Flags 置 PriorityFlag 时
// router 用本 kind 查桶（security-spec §1.2.2 MSG_PRIORITY 行）。
// 1000 以上不与 kinds 包任何真实号段冲突。
const KindPriority uint32 = 1_000_000

// PriorityFlag 是 Envelope.Flags 中的高优标志位。
const PriorityFlag uint32 = 1 << 0

// Rule 单个 kind 的令牌桶参数（security-spec §1.2.2 表一行）。
type Rule struct {
	Rate     float64 // 令牌/秒
	Burst    float64 // 突发（初始/上限令牌数）
	Capacity float64 // 桶容量上限（长期空闲后的最大积累）

	// MaxPayload 是历史遗留的 TOML 驱动单帧上限（user/app 配置解析用；
	// 0 = 不限制）。保留以兼容 user/app 的 limitRule 装配；判定见 AllowAt。
	MaxPayload int

	// PayloadCap 是显式的单帧 payload 上限（字节；0 = 不限制）。
	// 语义明确：>0 即硬上限，超出即 Drop。security-spec §1.2.2 最后一列里
	// 写 "0 B（无 payload）" 的四个 kind（TYPING / PRESENCE_*）在这里被设为
	// 一个很小的值（64 字节，足够容纳 {"conv_id":N}），从而把 spec 的
	// "无 payload" 约束真正落地——0 表示"不限制"，而不是"禁止带 payload"。
	PayloadCap int

	Silent bool // true = 超限静默丢弃（L1）；false = NACK（L2）
}

// Config 限流器参数（可由 policy 热更，原子替换）。
type Config struct {
	Rules map[uint32]Rule
	// DefaultRule 未列出的 kind 的兜底规则。
	DefaultRule Rule
	// UserDeviceCap 用户级聚合的设备数上限（%），默认 3（security-spec §1.2.1）。
	UserDeviceCap int
	// KillWindow 统计「超限帧占比」的滑动窗口，默认 60s。
	KillWindow time.Duration
	// KillRatio 超限占比阈值，默认 0.5（> 50%）。
	KillRatio float64
	// KillMinFrames 窗口内最少帧数才触发 Kill（避免 1/1 误杀），默认 20。
	KillMinFrames int64
	// ExemptKinds 完全豁免的 kind（PING/PONG 独立计数，永不限流）。
	ExemptKinds map[uint32]bool

	// --- 用户级惩罚（security-spec §1.2.1：连续 3 窗口超限 → 预算减半 10min；
	// 连续 10 窗口 → 断全部连接 60s）---
	//
	// UserHalfWindows 连续超限窗口数达到此值 → 该 uid 用户级预算减半。
	UserHalfWindows int
	// UserKillWindows 连续超限窗口数达到此值 → 返回 Kill（断全部连接）。
	UserKillWindows int
	// UserHalfDuration 预算减半的持续时间。
	UserHalfDuration time.Duration
	// UserKillDuration 惩罚断连的持续时间。
	UserKillDuration time.Duration

	// GlobalQPS 是全局（进程级）准入 QPS 上限（V2-0 容量契约，
	// docs/capacity-contract.md）。>0 时每帧先消耗共享全局桶的一个令牌
	// （在连接级/用户级判定之前），取不到令牌返回 Throttle（→ kind 13
	// RATE_LIMITED，带 ack_id 配对）。0 = 禁用（与主干逐字节一致）。
	GlobalQPS float64
}

// 用户级惩罚默认参数（security-spec §1.2.1）。
const (
	defaultUserHalfWindows  = 3
	defaultUserKillWindows  = 10
	defaultUserHalfDuration = 10 * time.Minute
	defaultUserKillDuration = 60 * time.Second
	// userHalfFactor 预算减半系数。
	userHalfFactor = 0.5
)

// DefaultConfig 返回 security-spec §1.2.2 的默认参数表。
func DefaultConfig() Config {
	return Config{
		Rules: map[uint32]Rule{
			// MSG_SEND（普通消息） 20/s 40 100 8KB — §1.2.2 第 1 行
			kinds.KindMsgSend: {Rate: 20, Burst: 40, Capacity: 100, MaxPayload: 8 << 10, PayloadCap: 8 << 10},
			// MSG_PRIORITY（高优帧） 5/s 10 20 4KB — §1.2.2 第 2 行
			KindPriority: {Rate: 5, Burst: 10, Capacity: 20, PayloadCap: 4 << 10},
			// TYPING 1/s 3 5 0B（无 payload）— §1.2.2 第 3 行；"0B"落地为 64B 硬上限
			kinds.KindTyping: {Rate: 1, Burst: 3, Capacity: 5, PayloadCap: 64, Silent: true},
			// PRESENCE 1/10s 3 5 0B — §1.2.2 第 4 行（update/subscribe/unsubscribe）；"0B"→64B
			kinds.KindPresenceUpdate:  {Rate: 1.0 / 10.0, Burst: 3, Capacity: 5, PayloadCap: 64, Silent: true},
			kinds.KindPresenceOnline:  {Rate: 1.0 / 10.0, Burst: 3, Capacity: 5, PayloadCap: 64, Silent: true},
			kinds.KindPresenceOffline: {Rate: 1.0 / 10.0, Burst: 3, Capacity: 5, PayloadCap: 64, Silent: true},
			// READ_ACK / DELIVER_ACK 10/s 30 60 256B — §1.2.2 第 5 行
			kinds.KindMsgRead:      {Rate: 10, Burst: 30, Capacity: 60, MaxPayload: 256, PayloadCap: 256},
			kinds.KindMsgDelivered: {Rate: 10, Burst: 30, Capacity: 60, MaxPayload: 256, PayloadCap: 256},
			// 撤回：比 MSG_SEND 更严（低频管理操作），§1.2.2 表内 256B 行派生
			kinds.KindMsgDelete: {Rate: 2, Burst: 5, Capacity: 10, MaxPayload: 256, PayloadCap: 256},
			kinds.KindMsgRecall: {Rate: 2, Burst: 5, Capacity: 10, MaxPayload: 256, PayloadCap: 256},
			// SYNC（历史拉取游标） 2/s 4 8 512B — §1.2.2 第 6 行
			kinds.KindSyncRequest: {Rate: 2, Burst: 4, Capacity: 8, MaxPayload: 512, PayloadCap: 512},
			// MEDIA_REQ（预签名申请） 1/30s 2 4 512B — §1.2.2 第 7 行
			kinds.KindMediaPresignRequest: {Rate: 1.0 / 30.0, Burst: 2, Capacity: 4, MaxPayload: 512, PayloadCap: 512},
			// CALL 信令 350-354：spec §1.2.2 未单列，但通话建连时 SDP+ICE 会突发；
			// 10/s 默认会把建连打断。给 40/s、burst 60、16KB payload（SDP 数 KB）。
			kinds.KindCallOffer:     {Rate: 40, Burst: 60, Capacity: 120, PayloadCap: 16 << 10},
			kinds.KindCallAnswer:    {Rate: 40, Burst: 60, Capacity: 120, PayloadCap: 16 << 10},
			kinds.KindCallCandidate: {Rate: 40, Burst: 60, Capacity: 120, PayloadCap: 16 << 10},
			kinds.KindCallBusy:      {Rate: 40, Burst: 60, Capacity: 120, PayloadCap: 16 << 10},
			kinds.KindCallHangup:    {Rate: 40, Burst: 60, Capacity: 120, PayloadCap: 16 << 10},
		},
		DefaultRule:      Rule{Rate: 10, Burst: 20, Capacity: 40},
		UserDeviceCap:    3,
		KillWindow:       60 * time.Second,
		KillRatio:        0.5,
		KillMinFrames:    20,
		UserHalfWindows:  defaultUserHalfWindows,
		UserKillWindows:  defaultUserKillWindows,
		UserHalfDuration: defaultUserHalfDuration,
		UserKillDuration: defaultUserKillDuration,
		ExemptKinds: map[uint32]bool{
			kinds.KindPing: true,
			kinds.KindPong: true,
		},
	}
}

// bucket 单个令牌桶。非并发安全：连接级桶只被该连接读循环使用，
// 用户级桶由 Limiter.mu 保护。
type bucket struct {
	tokens float64
	last   time.Time
}

func (b *bucket) allow(now time.Time, r Rule) bool {
	if b.last.IsZero() {
		b.tokens = r.Burst
		b.last = now
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * r.Rate
		cap := r.Capacity
		if cap < r.Burst {
			cap = r.Burst
		}
		if b.tokens > cap {
			b.tokens = cap
		}
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// connState 连接级状态（每连接独立，无锁使用）。
type connState struct {
	uid     int64
	buckets map[uint32]*bucket
	// 窗口内超限统计（用于 L4 判决）。
	winStart time.Time
	frames   int64
	over     int64
	// 心跳独立计数（豁免但仍可观测）。
	pings uint64
}

// userState 用户级聚合状态（Limiter.mu 保护）。
type userState struct {
	buckets map[uint32]*bucket
	conns   int

	// --- 连续超限窗口惩罚状态（L3/L4，security-spec §1.2.1）---
	overWindows  int       // 连续超限窗口计数
	winStart     time.Time // 当前用户级窗口起点
	winOver      bool      // 当前窗口内是否发生过用户级超限
	penaltyUntil time.Time // 预算减半生效截止时间
	killUntil    time.Time // 惩罚断连生效截止时间
}

// Limiter 两级令牌桶限流器。零值不可用，用 New。
type Limiter struct {
	cfg atomic.Pointer[Config]

	mu    sync.Mutex
	conns map[uint64]*connState
	users map[int64]*userState
	// global 是 V2-0 准入容量契约的共享全局桶（cfg.GlobalQPS>0 时启用，mu 保护）。
	// 只对 MSG_SEND 等写操作消耗；心跳/豁免帧不消耗（过载下也不拒心跳）。
	global bucket
}

// New 构造限流器。
func New(cfg Config) *Limiter {
	l := &Limiter{
		conns: make(map[uint64]*connState),
		users: make(map[int64]*userState),
	}
	l.cfg.Store(&cfg)
	return l
}

// SetConfig 原子替换参数（SIGHUP 热更）。
func (l *Limiter) SetConfig(cfg Config) { l.cfg.Store(&cfg) }

// Config 返回当前参数快照。
func (l *Limiter) Config() Config { return *l.cfg.Load() }

// AddConn 登记一条连接（connID 唯一；uid 用于用户级聚合）。
func (l *Limiter) AddConn(connID uint64, uid int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.conns[connID]; ok {
		return
	}
	l.conns[connID] = &connState{uid: uid, buckets: make(map[uint32]*bucket), winStart: time.Now()}
	u := l.users[uid]
	if u == nil {
		u = &userState{buckets: make(map[uint32]*bucket)}
		l.users[uid] = u
	}
	u.conns++
}

// RemoveConn 注销一条连接。
func (l *Limiter) RemoveConn(connID uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cs, ok := l.conns[connID]
	if !ok {
		return
	}
	delete(l.conns, connID)
	if u := l.users[cs.uid]; u != nil {
		u.conns--
		if u.conns <= 0 {
			delete(l.users, cs.uid)
		}
	}
}

// ConnCount 返回进程内该 uid 的连接数（用户级聚合基数）。
func (l *Limiter) ConnCount(uid int64) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if u := l.users[uid]; u != nil {
		return u.conns
	}
	return 0
}

// Allow 对一帧做两级判定（time.Now 版本）。
func (l *Limiter) Allow(connID uint64, uid int64, kind uint32, payloadLen int) Verdict {
	return l.AllowAt(connID, uid, kind, payloadLen, time.Now())
}

// AllowAt 显式时间版本（测试用）。
//
// 顺序：payload 上限 → 连接桶 → 用户聚合桶 → L4 窗口判决 → 用户级 L3 惩罚。
// 心跳等豁免 kind 独立计数后直接放行。
func (l *Limiter) AllowAt(connID uint64, uid int64, kind uint32, payloadLen int, now time.Time) Verdict {
	cfg := l.cfg.Load()
	if cfg.ExemptKinds[kind] {
		l.mu.Lock()
		if cs := l.conns[connID]; cs != nil {
			cs.pings++
			cs.frames++
		}
		l.mu.Unlock()
		return OK
	}

	rule, ok := cfg.Rules[kind]
	if !ok {
		rule = cfg.DefaultRule
	}

	// V2-0 准入容量：全局 QPS 桶（cfg.GlobalQPS>0 时启用），只对消息写入
	// 这类重操作消耗令牌；flag/配置关闭时本段完全不执行（逐字节等价主干）。
	// 超限返回 Throttle → Router 下发 kind13 RATE_LIMITED（带 env.Id 配对）。
	if cfg.GlobalQPS > 0 && kind == uint32(kinds.KindMsgSend) && !l.allowGlobal(now, cfg.GlobalQPS) {
		return Throttle
	}

	// 单帧 payload 上限（security-spec §1.2.2 最后一列）。
	// MaxPayload：历史 TOML 字段（0 = 不限制）。
	// PayloadCap：显式上限（0 = 不限制；>0 = 硬上限）。
	if rule.MaxPayload > 0 && payloadLen > rule.MaxPayload {
		return Drop
	}
	if rule.PayloadCap > 0 && payloadLen > rule.PayloadCap {
		return Drop
	}

	l.mu.Lock()
	cs := l.conns[connID]
	if cs == nil {
		// 未登记的连接（测试/异常路径）：临时建立状态，仍执行判定。
		cs = &connState{uid: uid, buckets: make(map[uint32]*bucket), winStart: now}
		l.conns[connID] = cs
		u := l.users[uid]
		if u == nil {
			u = &userState{buckets: make(map[uint32]*bucket)}
			l.users[uid] = u
		}
		u.conns++
	}
	b := cs.buckets[kind]
	if b == nil {
		b = &bucket{}
		cs.buckets[kind] = b
	}
	connOK := b.allow(now, rule)
	u := l.users[cs.uid]
	mult := l.userMultiplier(cs.uid, cfg, now)
	userOK := true
	if u != nil {
		ub := u.buckets[kind]
		if ub == nil {
			ub = &bucket{}
			u.buckets[kind] = ub
		}
		userRule := rule
		userRule.Rate *= mult
		userRule.Burst *= mult
		userRule.Capacity *= mult
		userOK = ub.allow(now, userRule)
	}
	cs.frames++
	if !connOK || !userOK {
		cs.over++
	}
	kill := l.judgeKill(cs, cfg, now)
	// 用户级连续超限惩罚（L3 预算减半 / L4 断连）。
	//
	// Only overage of a *substantive* rule counts. Silent rules (typing,
	// presence) are best-effort signals that a normal client emits slightly
	// faster than the budget; letting them drive the penalty halves a user's
	// real message budget and looks like the server is broken. Measured
	// before the fix: 128 clients sending typing at 1.5/s were demoted to
	// 67% message availability purely from that false positive.
	userKill := false
	if u != nil {
		userOver := !userOK && !rule.Silent
		userKill = l.userPenalty(u, cfg, now, userOver)
	}
	l.mu.Unlock()

	if kill || userKill {
		return Kill
	}
	if connOK && userOK {
		return OK
	}
	if !userOK {
		// 用户级超限：高优帧降级（Throttle），静默类丢弃。
		if kind == KindPriority {
			return Throttle
		}
		if rule.Silent {
			return Drop
		}
		return Throttle
	}
	// 连接级超限。
	if rule.Silent {
		return Drop
	}
	return Throttle
}

// userPenalty 更新用户级「连续超限窗口」计数并实施惩罚（security-spec §1.2.1）：
//   - 每跨过一个窗口：若该窗口内有用户级超限 → overWindows++，否则清零。
//   - overWindows 达到 UserHalfWindows → 预算减半，持续 UserHalfDuration。
//   - overWindows 达到 UserKillWindows → 返回 true（调用方判 Kill，断连），
//     killUntil = now + UserKillDuration。
//
// over 表示本帧是否发生了用户级超限。返回 true 表示此刻应断连（Kill）。
func (l *Limiter) userPenalty(u *userState, cfg *Config, now time.Time, over bool) bool {
	win := cfg.KillWindow
	if win <= 0 {
		win = 60 * time.Second
	}
	halfN := cfg.UserHalfWindows
	if halfN <= 0 {
		halfN = defaultUserHalfWindows
	}
	killN := cfg.UserKillWindows
	if killN <= 0 {
		killN = defaultUserKillWindows
	}
	halfDur := cfg.UserHalfDuration
	if halfDur <= 0 {
		halfDur = defaultUserHalfDuration
	}
	killDur := cfg.UserKillDuration
	if killDur <= 0 {
		killDur = defaultUserKillDuration
	}

	if u.winStart.IsZero() {
		u.winStart = now
	}
	// 跨过任意多个完整窗口：逐窗结算。
	for now.Sub(u.winStart) >= win {
		if u.winOver {
			u.overWindows++
		} else {
			u.overWindows = 0
		}
		switch {
		case u.overWindows >= killN:
			u.killUntil = u.winStart.Add(win).Add(killDur)
		case u.overWindows >= halfN:
			u.penaltyUntil = u.winStart.Add(win).Add(halfDur)
		}
		u.winStart = u.winStart.Add(win)
		u.winOver = false
	}
	if over {
		u.winOver = true
	}
	return now.Before(u.killUntil)
}

// judgeKill 在窗口内统计超限占比，返回是否触发 L4。
func (l *Limiter) judgeKill(cs *connState, cfg *Config, now time.Time) bool {
	if cs.winStart.IsZero() {
		cs.winStart = now
		return false
	}
	if now.Sub(cs.winStart) > cfg.KillWindow {
		cs.winStart = now
		cs.frames = 0
		cs.over = 0
		return false
	}
	if cs.frames < cfg.KillMinFrames {
		return false
	}
	return float64(cs.over)/float64(cs.frames) > cfg.KillRatio
}

// userMultiplier 返回用户级桶倍率：min(本进程 conn 数, 设备上限)，至少 1；
// 处于「预算减半」惩罚期时再乘 userHalfFactor。
func (l *Limiter) userMultiplier(uid int64, cfg *Config, now time.Time) float64 {
	n := 1
	u := l.users[uid]
	if u != nil && u.conns > 1 {
		n = u.conns
	}
	if cfg.UserDeviceCap > 0 && n > cfg.UserDeviceCap {
		n = cfg.UserDeviceCap
	}
	mult := float64(n)
	if u != nil && now.Before(u.penaltyUntil) {
		mult *= userHalfFactor
	}
	return mult
}

// Stats 可观测快照（指标采集用）。
// allowGlobal 消耗一个全局准入令牌（V2-0 容量契约）。桶参数 {Rate,Burst,Capacity}=qps，
// 即突发不超过 1 秒的量。l.mu 与 conns/users 共用，锁粒度可接受（每 msg_send 一次）。
func (l *Limiter) allowGlobal(now time.Time, qps float64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.global.allow(now, Rule{Rate: qps, Burst: qps, Capacity: qps})
}

type Stats struct {
	Conns      int
	Users      int
	PingCount  uint64
	KillWindow map[uint64]int64 // connID → 窗口内超限帧数（调试用）
}

// Snapshot 返回当前统计。
func (l *Limiter) Snapshot() Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := Stats{Conns: len(l.conns), Users: len(l.users), KillWindow: make(map[uint64]int64, len(l.conns))}
	for id, cs := range l.conns {
		st.PingCount += cs.pings
		if cs.over > 0 {
			st.KillWindow[id] = cs.over
		}
	}
	return st
}
