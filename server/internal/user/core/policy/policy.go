// Package policy 是 usergw 的策略数据面（docs/user-frontend-design.md §1.3）：
// 限流参数、本地处理白名单、心跳参数等全部数据驱动，SIGHUP 热更原子替换。
//
// 机制（codec/acceptor/forwarder）写死；策略（本包）可换。
package policy

import (
	"sync/atomic"
	"time"

	"github.com/yingo-server/OpenDiscord/server/internal/proto/kinds"
	"github.com/yingo-server/OpenDiscord/server/internal/user/net/limit"
)

// Heartbeat 心跳参数（policy 可热更：HELLO 超时从 10s 调 3s 无需改代码）。
type Heartbeat struct {
	// Interval 服务端 Ping 间隔，默认 25s。
	Interval time.Duration
	// ReadTimeout 读超时（无 pong 续期），默认 60s。
	ReadTimeout time.Duration
}

// Config 是 usergw 全量策略快照。发布后不可变（Replace 时整体替换）。
type Config struct {
	// Limits 限流参数表（security-spec §1.2.2 落点）。
	Limits limit.Config
	// LocalKinds 本地处理白名单：这些 kind 不进桥接（Hello/Ping/Auth/Resume/Pong/
	// FeatureFlags 订阅）。其余一律 Wrap 内部帧转发。
	LocalKinds map[uint32]bool
	// Heartbeat 心跳参数。
	Heartbeat Heartbeat
	// MaxFrameBytes WS 单帧上限，默认 64KB。
	MaxFrameBytes int
	// WriteChanCap 每连接写缓冲容量，默认 64。
	WriteChanCap int
	// MaxConnsPerUser 单 uid 并发连接上限，默认 3（security-spec §2.3 红线）。
	MaxConnsPerUser int
	// TombstoneTTL 连接 id tombstone 保留时长，默认 30s。
	TombstoneTTL time.Duration
	// ForwardQueueCap forwarder 降级重试队列上限，默认 10000。
	ForwardQueueCap int
	// DrainTimeout 优雅退出等待 forwarder 排空上限，默认 10s。
	DrainTimeout time.Duration
	// WriteFlushTimeout 优雅退出每连接写缓冲排空上限，默认 2s。
	WriteFlushTimeout time.Duration
	// Upgrade 段：/ws upgrade 的按 IP 限流 + 失败熔断（security-spec E-3/E-4）。
	// 零值由 ws.NewUpgradeLimiter 取默认（10/min、burst 20、阈值 8、窗口 5min、
	// 封禁 15min）。策略层提供这些字段以便热更，装配时透传给 ws.Config。
	UpgradeRatePerMin       float64
	UpgradeBurst            float64
	UpgradeFailureThreshold int
	UpgradeFailureWindow    time.Duration
	UpgradeBanFor           time.Duration
}

// Default 返回默认策略（参数全部来自 docs/security-spec.md）。
func Default() Config {
	return Config{
		Limits: limit.DefaultConfig(),
		LocalKinds: map[uint32]bool{
			kinds.KindHello:        true,
			kinds.KindClientCaps:   true,
			kinds.KindAuth:         true,
			kinds.KindPing:         true,
			kinds.KindPong:         true,
			kinds.KindResume:       true,
			kinds.KindFeatureFlags: true,
		},
		Heartbeat: Heartbeat{
			Interval:    25 * time.Second,
			ReadTimeout: 60 * time.Second,
		},
		MaxFrameBytes:     64 << 10,
		WriteChanCap:      64,
		MaxConnsPerUser:   3,
		TombstoneTTL:      30 * time.Second,
		ForwardQueueCap:   10000,
		DrainTimeout:      10 * time.Second,
		WriteFlushTimeout: 2 * time.Second,

		UpgradeRatePerMin:       10,
		UpgradeBurst:            20,
		UpgradeFailureThreshold: 8,
		UpgradeFailureWindow:    5 * time.Minute,
		UpgradeBanFor:           15 * time.Minute,
	}
}

// Policy 是原子可替换的策略快照持有者。零值不可用，用 New。
type Policy struct {
	cur atomic.Pointer[Config]
}

// New 用给定配置构造。
func New(cfg Config) *Policy {
	p := &Policy{}
	p.cur.Store(&cfg)
	return p
}

// Snapshot 返回当前策略（只读；Config 内的 map 视作不可变）。
func (p *Policy) Snapshot() Config {
	if c := p.cur.Load(); c != nil {
		return *c
	}
	return Default()
}

// Replace 原子替换策略（SIGHUP 热更）。返回旧配置供 diff 日志。
func (p *Policy) Replace(cfg Config) Config {
	old := p.Snapshot()
	p.cur.Store(&cfg)
	return old
}

// IsLocal 报告 kind 是否本地处理（不进桥接）。
func (p *Policy) IsLocal(kind uint32) bool {
	c := p.cur.Load()
	if c == nil {
		return Default().LocalKinds[kind]
	}
	return c.LocalKinds[kind]
}

// Rule 返回 kind 对应的限流参数。
func (p *Policy) Rule(kind uint32) limit.Rule {
	c := p.Snapshot()
	if r, ok := c.Limits.Rules[kind]; ok {
		return r
	}
	return c.Limits.DefaultRule
}
