// Package adaptive 实现普遍拉取 1.4 LTS 的自适应 AIMD 控制器（策略层）。
//
// 依据：docs/universal-pull-spec.md §2（伪代码 §2.3）、§2.4（T-AD-1~7 断言）。
//
// 纪律（§2.3 补充约束）：
//   - 纯函数式 (signals, prev) → effective，便于单测注入假时钟/假计数；
//   - 机制层只读 effective[cid] 原子快照，不调用控制器、不判拥塞、不写审计；
//   - 控制器不读写 DB、不发帧、不改 flag；审计与 dump 经接口注入；
//   - 所有策略数字（硬界、soft/firm、alpha/beta、速率限制、冷却、静态默认值）
//     一律由 Config 注入，本包代码零硬编码策略数字。
package adaptive

import (
	"fmt"
	"time"
)

// Signals 单会话输入信号快照（§2.1：全部来自已有计数器，不新增探针）。
type Signals struct {
	QueueDepth   int  // 扇出队列深度（Fanout/Egress/Forwarder 既有计数）
	DroppedDelta int  // 窗口内丢弃计数增量
	AckRatePct   int  // ACK 率（百分比 0~100，ACK Tracker / kind15 cursor 推导）
	Paused       int  // credit PAUSED / backpressured 连接数
	P95MS        int  // p95（毫秒，与 S-6 同一 p95 计数器口径）
	HasP95Base   bool // 是否有同场景稳态 p95 基线（缺失则 p95NotRegressed 视为不满足）
	P95SteadyMS  int  // 稳态窗口 p95 基线（毫秒）
	HasAckSample bool // ACK 窗口样本是否充足；不足时保持现值，不猜
	HasQueue     bool // 队列计数是否可用
}

// Global 全局（非会话）信号。
type Global struct {
	PausedConns    int // delivery.Stats().PausedConns
	Backpressured  int // delivery.Stats().Backpressured
	StaticOverride bool
}

// Effective 机制层每会话读取的原子参数快照（§0 术语）。
type Effective struct {
	Batch       int // watermark_batch ∈ [BatchMin, BatchMax]
	IntervalMS  int // watermark_interval_ms ∈ [IntervalMinMS, IntervalMaxMS]
	Overridden  bool
	FromDefault bool
}

// EventKind 审计/dump 事件类型（§2.3：可离线回放）。
type EventKind string

const (
	EventChange   EventKind = "adaptive.change"
	EventReject   EventKind = "adaptive.reject"
	EventFallback EventKind = "adaptive.fallback"
)

// Event 一次参数变更 / 拒载 / 回退的审计事件；detail 只放 id/枚举/数值，禁止消息正文。
type Event struct {
	Kind     EventKind
	CID      uint64
	Old      Effective
	New      Effective
	Reason   string
	Signals  Signals // 触发时的信号快照（仅数值）
	At       time.Time
	Version  string
	Override bool
}

// Auditor 审计事件出口（接口注入；实现层负责落 pipe.audit / status_dump，控制器不直接写 DB）。
type Auditor interface {
	AdaptiveEvent(ev Event)
}

// Dumper SIGUSR1 dump 出口（status_dump 的 adaptive 子表，接口注入）。
type Dumper interface {
	AdaptiveDump(ev Event)
}

// Config 全部策略参数，由 policy/装配层注入；本包零硬编码策略数字。
type Config struct {
	// 硬界（§2.2）
	BatchMin, BatchMax           int
	IntervalMinMS, IntervalMaxMS int

	// 静态默认值（控制器关闭 / panic / 旁路失败：50/2000 语义由装配给出）
	DefaultBatch, DefaultIntervalMS int

	// 拥塞 / 健康阈值（§2.3：ACK_RATE_NO_CONGESTION=98、ACK_RATE_CONGESTED=95 语义由装配给出）
	SoftQueueDepth   int // qDepth < soft
	FirmQueueDepth   int // qDepth > firm
	AckHealthyPct    int // ackRate > 98
	AckCongestedPct  int // ackRate < 95
	P95RegressFactor int // p95NotRegressed := p95 <= factor * steadyP95

	// AIMD 步长与系数（policy 配置项）
	AlphaIntervalMS  int // 加性减小步长
	AlphaBatch       int // 加性减小步长（batch 维）
	BetaNum, BetaDen int // 乘性系数 beta = BetaNum/BetaDen（>1）

	// 变更速率限制（每次调整 ≤ 当前值 25%）与冷却（1s）
	RateLimitNum, RateLimitDen int // clampRate 比例 1/4
	Cooldown                   time.Duration

	// 连续失败回退阈值（panic / 无信号旁路：连续 N 次失败回退静态默认）
	MaxConsecutiveFailures int

	// 特性开关（§2.2）
	AdaptiveEnabled bool
	Version         string // 写入审计事件的版本字段
}

// State 单会话控制器状态（每会话独立，§2.3 伪代码头部注释）。
type State struct {
	Eff        Effective // 上一次合法 effective[cid]
	LastChange time.Time // 上次变更时刻（冷却用）
	Failures   int       // 连续失败计数（panic / 越界 / 信号不足不全部计入，见 SetFail）
	HasEff     bool      // 是否已有合法 effective；无则首个输出为静态默认
}

// Controller 纯策略层。时钟经 Now 注入（假时钟可测）。
type Controller struct {
	cfg Config
	Now func() time.Time
	// PanicHook 仅用于测试/故障注入：非 nil 时 Tick 入口调用；SafeTick 捕获后走旁路回退。
	PanicHook func()
}

// New 构造控制器；时钟必须注入，nil 时使用 time.Now。
func New(cfg Config, now func() time.Time) *Controller {
	if now == nil {
		now = time.Now
	}
	return &Controller{cfg: cfg, Now: now}
}

// Defaults 返回静态默认值（确定行为：adaptive_control_enabled=false / panic / 旁路失败）。
func (c *Controller) Defaults() Effective {
	return Effective{Batch: c.cfg.DefaultBatch, IntervalMS: c.cfg.DefaultIntervalMS, FromDefault: true}
}

// Disabled 处理 adaptive_control_enabled=false 的确定回退（T-AD-5）。
// 输出静态默认值并写 adaptive.fallback 审计；不受冷却限制（回滚动作不被策略层拖住）。
func (c *Controller) Disabled(a Auditor, d Dumper, cid uint64) Effective {
	return c.fallback(a, d, cid, "adaptive_control_disabled")
}

// Fallback 旁路失败（panic / 超时 / 无信号）后的确定回退（§2.3 补充约束最后一条）。
func (c *Controller) Fallback(a Auditor, d Dumper, cid uint64, reason string) Effective {
	return c.fallback(a, d, cid, reason)
}

func (c *Controller) fallback(a Auditor, d Dumper, cid uint64, reason string) Effective {
	eff := c.Defaults()
	c.emit(a, d, Event{
		Kind: EventFallback, CID: cid, New: eff, Reason: reason,
		At: c.Now(), Version: c.cfg.Version,
	})
	return eff
}

// StaticOverride 处理 SIGHUP / 管理 API 静态覆盖（§2.2：优先于控制器输出；越界拒载；
// 生效不受冷却延迟）。raw 由调用方以 any 传入，归一化复用 ToInt 语义。
func (c *Controller) StaticOverride(a Auditor, d Dumper, cid uint64, prev State, batchRaw, intervalRaw any) (Effective, State) {
	batch, okB := ToInt(batchRaw)
	interval, okI := ToInt(intervalRaw)
	if !okB || !okI || !c.inHardBounds(batch, interval) {
		c.emit(a, d, Event{
			Kind: EventReject, CID: cid, Old: prev.Eff, New: Effective{Batch: batch, IntervalMS: interval},
			Reason: "out_of_bounds", At: c.Now(), Version: c.cfg.Version, Override: true,
		})
		return prev.Eff, prev
	}
	eff := Effective{Batch: batch, IntervalMS: interval, Overridden: true}
	c.emit(a, d, Event{
		Kind: EventChange, CID: cid, Old: prev.Eff, New: eff,
		Reason: "static_override", At: c.Now(), Version: c.cfg.Version, Override: true,
	})
	return eff, State{Eff: eff, HasEff: true} // 覆盖生效不记冷却（§2.3：覆盖/回滚不受冷却延迟）
}

// Tick 执行一个控制周期（§2.3 伪代码 onControlTick）。
// 纯函数：以 (signals, prev) 为输入，返回新的 effective 与状态，不触达任何外部状态。
func (c *Controller) Tick(a Auditor, d Dumper, cid uint64, sig Signals, g Global, prev State) (Effective, State) {
	if c.PanicHook != nil {
		c.PanicHook()
	}
	if !c.cfg.AdaptiveEnabled {
		return c.Disabled(a, d, cid), State{Eff: c.Defaults(), HasEff: true, Failures: prev.Failures}
	}
	if g.StaticOverride {
		return prev.Eff, prev // 覆盖优先于控制器输出；覆盖本身经 StaticOverride 写入
	}
	if !prev.HasEff {
		// 首个控制周期：从静态默认值起控（确定行为，不写变更事件）
		return c.Defaults(), State{Eff: c.Defaults(), HasEff: true, Failures: prev.Failures}
	}
	if c.Now().Sub(prev.LastChange) < c.cfg.Cooldown {
		return prev.Eff, prev // 每会话冷却，防振荡（T-AD-3）
	}

	// 信号不足：保持现值，不猜（§2.3 补充约束第 1 条）
	if !sig.HasQueue || !sig.HasAckSample {
		return prev.Eff, prev
	}

	now := c.Now()
	old := prev.Eff

	congested := sig.QueueDepth > c.cfg.FirmQueueDepth ||
		sig.DroppedDelta > 0 ||
		sig.AckRatePct < c.cfg.AckCongestedPct ||
		(g.PausedConns+g.Backpressured) > 0

	p95OK := !sig.HasP95Base || (c.cfg.P95RegressFactor > 0 && sig.P95MS <= c.cfg.P95RegressFactor*sig.P95SteadyMS)
	healthy := sig.QueueDepth < c.cfg.SoftQueueDepth &&
		sig.AckRatePct > c.cfg.AckHealthyPct &&
		sig.DroppedDelta == 0 &&
		(g.PausedConns+g.Backpressured) == 0 &&
		p95OK

	var interval, batch int
	switch {
	case congested: // M：乘性增大（大群聚合）
		interval = ceilDiv(old.IntervalMS*c.cfg.BetaNum, c.cfg.BetaDen)
		batch = ceilDiv(old.Batch*c.cfg.BetaNum, c.cfg.BetaDen)
	case healthy: // A：加性减小（小群趋近实时）
		interval = old.IntervalMS - c.cfg.AlphaIntervalMS
		batch = old.Batch - c.cfg.AlphaBatch
	default:
		return prev.Eff, prev // 中间带/信号不足：保持，不振荡
	}

	// 速率限制先于硬界 clamp（§2.3 补充约束第 2 条）；两维分别判。
	// clampRate 结果 ≤1 视为「无有效移动」→ 保持旧值（1/4 步长在 <4 的值上会归零）。
	newInterval := clampRate(interval, old.IntervalMS, c.cfg.RateLimitNum, c.cfg.RateLimitDen)
	newBatch := clampRate(batch, old.Batch, c.cfg.RateLimitNum, c.cfg.RateLimitDen)
	if old.IntervalMS-newInterval > 0 || newInterval-old.IntervalMS > 0 {
		interval = newInterval
	}
	if old.Batch-newBatch > 0 || newBatch-old.Batch > 0 {
		batch = newBatch
	}
	interval = clamp(interval, c.cfg.IntervalMinMS, c.cfg.IntervalMaxMS)
	batch = clamp(batch, c.cfg.BatchMin, c.cfg.BatchMax)

	if interval == old.IntervalMS && batch == old.Batch {
		return prev.Eff, prev
	}

	next := Effective{Batch: batch, IntervalMS: interval}
	if !c.inHardBounds(batch, interval) { // 越界拒载（防御性；clamp 后不应到达）
		c.emit(a, d, Event{
			Kind: EventReject, CID: cid, Old: old, New: next,
			Reason: "out_of_bounds", Signals: sig, At: now, Version: c.cfg.Version,
		})
		return prev.Eff, prev
	}

	st := State{Eff: next, LastChange: now, HasEff: true, Failures: prev.Failures}
	c.emit(a, d, Event{
		Kind: EventChange, CID: cid, Old: old, New: next,
		Reason: aimdReason(congested), Signals: sig, At: now, Version: c.cfg.Version,
	})
	return next, st
}

// SafeTick 带 panic 旁路的控制周期：panic 时按 §2.3 回退（连续失败回退静态默认），
// 绝不因策略层异常丢数据帧或改 seq（机制层继续用最后一次合法 effective）。
func (c *Controller) SafeTick(a Auditor, d Dumper, cid uint64, sig Signals, g Global, prev State) (eff Effective, st State) {
	defer func() {
		if r := recover(); r != nil {
			nf := prev.Failures + 1
			if nf >= c.cfg.MaxConsecutiveFailures {
				eff = c.fallback(a, d, cid, fmt.Sprintf("panic_consecutive:%v", r))
				st = State{Eff: eff, HasEff: true}
				return
			}
			// 单次 panic：保持最后一次合法 effective
			c.emit(a, d, Event{
				Kind: EventFallback, CID: cid, Old: prev.Eff, New: prev.Eff,
				Reason: fmt.Sprintf("panic_hold:%v", r), At: c.Now(), Version: c.cfg.Version,
			})
			eff, st = prev.Eff, State{Eff: prev.Eff, LastChange: prev.LastChange, HasEff: prev.HasEff, Failures: nf}
		}
	}()
	return c.Tick(a, d, cid, sig, g, prev)
}

func (c *Controller) emit(a Auditor, d Dumper, ev Event) {
	if a != nil {
		a.AdaptiveEvent(ev)
	}
	if d != nil {
		d.AdaptiveDump(ev)
	}
}

func (c *Controller) inHardBounds(batch, interval int) bool {
	return batch >= c.cfg.BatchMin && batch <= c.cfg.BatchMax &&
		interval >= c.cfg.IntervalMinMS && interval <= c.cfg.IntervalMaxMS
}

func aimdReason(congested bool) string {
	if congested {
		return "aimd_multiplicative_increase"
	}
	return "aimd_additive_decrease"
}

// clampRate 单次变更 ≤ old*num/den（双向生效；在硬界处允许只移动一维由调用方 clamp 承担）。
// 返回钳制后数值与是否有任何一维真正移动。
func clampRate(v, old, num, den int) int {
	maxDelta := old * num / den
	lo, hi := old-maxDelta, old+maxDelta
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func ceilDiv(a, b int) int {
	if b == 0 {
		return a
	}
	return (a + b - 1) / b
}

// ToInt 复用 featureflags.ToInt 语义（§2.2「类型无法归一化为 int」）；包内复制以避免
// 对 internal/pkg 之外的装配耦合——语义与 server/internal/pkg/featureflags.ToInt 一致。
func ToInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int8:
		return int(n), true
	case int16:
		return int(n), true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case uint:
		return int(n), true
	case uint8:
		return int(n), true
	case uint16:
		return int(n), true
	case uint32:
		return int(n), true
	case uint64:
		return int(n), true
	case float32:
		if n == float32(int64(n)) {
			return int(n), true
		}
		return 0, false
	case float64:
		if n == float64(int64(n)) {
			return int(n), true
		}
		return 0, false
	case string:
		return 0, false
	default:
		return 0, false
	}
}
