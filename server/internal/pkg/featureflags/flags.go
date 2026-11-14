// Package featureflags 提供原子快照读写的 feature flag 集。
// flags 经 FEATURE_FLAGS 帧（kind 911）广播到 sys.>，SIGHUP 白名单热重载可写。
// 读多写少：Snapshot() 走 atomic.Value 零拷贝读，Apply() 写整份新副本。
package featureflags

import (
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// 默认 flag 集合常量：同名 key 的默认值。
const (
	KeyVideoEnabled = "video_enabled"
	KeyE2EE         = "e2ee"
	KeyRegistration = "registration"
	KeyReadonly     = "readonly"
	KeyCallEnabled  = "call_enabled"

	// 拉取模式（docs/pull-mode-spec-v1.2.md §3.3）。
	//
	// KeyPullModeThreshold 默认 100：单阶段直接启用（v1.2 定调）。
	// 与主干逐字节一致（规范 §6.4）。0 = 全量 inline，回滚即改此值。
	KeyPullModeThreshold = "pull_mode_threshold"
	// KeyWatermarkBatch 是水位线的推进阈值：最新 seq 与上次水位线相差
	// >= 该条数即发（§1.3）。
	KeyWatermarkBatch = "watermark_batch"
	// KeyWatermarkIntervalMS 是水位线的时间兜底间隔（毫秒，§1.3）。
	KeyWatermarkIntervalMS = "watermark_interval_ms"
	// KeyWatermarkJitterMS 是客户端抖动上界（毫秒）。服务端不参与逻辑，
	// 仅随 911 FEATURE_FLAGS 下发供压测/诊断读取（§1.3/§2.2）。
	KeyWatermarkJitterMS = "watermark_jitter_ms"

	// v3 消息总线灰度（docs/v3-bus-design.md §6）。
	//
	// KeyUniversalPullEnabled 是 1.4 CORE-2 的普遍拉取总开关（docs/universal-pull-spec.md
	// §5 开关 1）。默认 false：中间态铁律。true 时取消 pull_mode_threshold 的
	// 交付分流语义——所有会话统一走水位线分支（inline 仅作回滚路径，逐字节一致）。
	KeyUniversalPullEnabled = "universal_pull_enabled"
	// KeyAdaptiveControlEnabled 是 1.4 CORE-2 策略层开关（spec §5 开关 3）。
	// 默认 false：机制层回退静态默认值 watermark_batch=50 / watermark_interval_ms=2000。
	KeyAdaptiveControlEnabled = "adaptive_control_enabled"

	// KeyV3BusEnabled 默认 false：主干合入时旧路径为唯一投递路径（§9 中间态铁律），
	// T-SV-5 常驻锁定旧路径行为逐字节同主干。热回滚 = SIGHUP 改此 flag。
	KeyV3BusEnabled = "v3_bus_enabled"
	// KeyV3BusPercent 是 v3 灰度百分比（0-100，按 uid 哈希分桶，非按连接随机，§7.3 T-SV-7）。
	KeyV3BusPercent = "v3_bus_percent"

	// KeyMsgDomainV2 是 FE-1a 消息域补齐的总开关（106 编辑 / 109 转发元数据 /
	// @ 提及定向通知 / 453 举报入库）。默认 false：关闭时这些 kind 的上行走
	// 原有 default 分支（debug 日志后丢弃），现有路径逐字节不变。
	KeyMsgDomainV2 = "msg_domain_v2_enabled"
)

// Defaults 返回默认 flag 集合的副本。
func Defaults() map[string]any {
	return map[string]any{
		KeyVideoEnabled: false,
		KeyE2EE:         false,
		KeyRegistration: true,
		KeyReadonly:     false,
		KeyCallEnabled:  false,

		KeyPullModeThreshold:   100, // >100 成员走水位线拉取；<=100 保持 inline
		KeyWatermarkBatch:      50,
		KeyWatermarkIntervalMS: 2000,
		KeyWatermarkJitterMS:   2000,

		// 1.4 转生产：四项实验性功能全部默认开启，由 CI 的 S-1~S-7 扫测与压测
		// 守门；回滚 = SIGHUP 热改对应 flag 为 false / percent=0（S-7 逐字节基线）。
		KeyUniversalPullEnabled:   true, // 普遍拉取：所有会话走水位线（无 kind14 能力的旧客户端自动保持 inline）
		KeyAdaptiveControlEnabled: true, // AIMD 自适应：effective[cid] 由控制循环写入

		KeyV3BusEnabled: true, // v3 总线：mailbox + credit 流控为默认投递路径
		KeyV3BusPercent: 100,  // 全量（按 uid 哈希分桶，100 = 所有 uid 在桶内）

		KeyMsgDomainV2: true, // FE-1a 消息域（106 编辑 / 109 转发 / @ / 453 举报）默认启用
	}
}

// state 内部不可变快照（发布后不再修改）。
type state struct {
	flags   map[string]any
	version uint64
}

// Flags 并发安全的 flag 存储。零值不可用，用 New()。
type Flags struct {
	cur atomic.Value // *state
	mu  sync.Mutex   // 只串行化 Apply
}

// New 用默认集合构造。
func New() *Flags {
	f := &Flags{}
	f.cur.Store(&state{flags: Defaults(), version: 0})
	return f
}

// NewFromMap 用指定集合构造。
func NewFromMap(m map[string]any) *Flags {
	f := &Flags{}
	cp := make(map[string]any, len(m))
	for k, v := range m {
		cp[k] = v
	}
	f.cur.Store(&state{flags: cp, version: 0})
	return f
}

// Snapshot 返回当前集合的只读副本（调用方可安全修改，不影响存储）。
func (f *Flags) Snapshot() map[string]any {
	s := f.cur.Load().(*state)
	cp := make(map[string]any, len(s.flags))
	for k, v := range s.flags {
		cp[k] = v
	}
	return cp
}

// Version 返回当前版本号（每次 Apply 递增）。
func (f *Flags) Version() uint64 {
	return f.cur.Load().(*state).version
}

// Apply 原子合并 delta（仅覆盖出现的 key），返回新版本号。
func (f *Flags) Apply(delta map[string]any) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	old := f.cur.Load().(*state)
	next := make(map[string]any, len(old.flags)+len(delta))
	for k, v := range old.flags {
		next[k] = v
	}
	for k, v := range delta {
		if v == nil {
			delete(next, k) // nil 语义 = 删除该 flag 回退默认
			continue
		}
		next[k] = v
	}
	newVer := old.version + 1
	f.cur.Store(&state{flags: next, version: newVer})
	return newVer
}

// Get 返回单个 flag 值；不存在返回 (nil, false)。
func (f *Flags) Get(key string) (any, bool) {
	s := f.cur.Load().(*state)
	v, ok := s.flags[key]
	return v, ok
}

// Int 便捷读取整数 flag：不存在或类型不符返回 def。
//
// flag 值的来源不统一（Go 默认值是 int，JSON/DB 装载是 float64，911 下发
// 是字符串），这里统一归一化，避免每个调用方各写一份类型断言。
func (f *Flags) Int(key string, def int) int {
	if v, ok := f.Get(key); ok {
		if n, ok := ToInt(v); ok {
			return n
		}
	}
	return def
}

// ToInt 把 flag 值归一化为 int。支持 bool（true=1）与数字字符串。
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
		return int(n), true
	case float64:
		return int(n), true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil {
			return 0, false
		}
		return i, true
	}
	return 0, false
}
func (f *Flags) Bool(key string, def bool) bool {
	if v, ok := f.Get(key); ok {
		if b, isB := v.(bool); isB {
			return b
		}
	}
	return def
}
