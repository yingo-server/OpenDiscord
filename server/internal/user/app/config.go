// Package app 组装 usergw（docs/user-frontend-design.md §1.2 app 层）：
// config + server.go。main 只做接线，任何业务逻辑不在这里。
package app

import (
	"net/http"
	"time"

	"github.com/yingo-server/UFOT/server/internal/pkg/featureflags"
	"github.com/yingo-server/UFOT/server/internal/user/core/policy"
	"github.com/yingo-server/UFOT/server/internal/user/net/limit"
)

func limitRule(rate, burst, capacity float64, maxPayload int, silent bool) limit.Rule {
	return limit.Rule{Rate: rate, Burst: burst, Capacity: capacity, MaxPayload: maxPayload, Silent: silent}
}

// Config 是 usergw 的装配配置（监听/桥接/限流/日志）。
// 可热更字段见 policy.Config（SIGHUP 由批 3 的 sig 包调用 Server.ReloadPolicy）。
type Config struct {
	// ListenAddr WS 监听地址，如 ":8081"。
	ListenAddr string
	// PublicOrigin 允许的浏览器 Origin（如 https://chat.example.com）；
	// 为空时仅做 Host 同源比较。
	PublicOrigin string
	// AllowAnyOrigin 放行任意合法 Origin（独立前端部署默认 true）；
	// false 时回退到 PublicOrigin / 同 Host 校验。
	AllowAnyOrigin bool
	// CORSOrigins 与 HTTP 面 cors_origins 同源（WS 握手白名单对齐 HTTP 判据）。
	CORSOrigins []string
	// BridgeAddr 桥接地址（TCP/UDS）。本批次 Peer 由装配层注入，仅记录与健康展示。
	BridgeAddr string
	// CookieName 承载 access token 的 Cookie 名，默认 access_token。
	CookieName string
	// LogLevel "debug"/"info"/"warn"/"error"，默认 info。
	LogLevel string

	// 以下为零值即取 policy 默认值的覆盖项。
	HeartbeatInterval time.Duration
	ReadTimeout       time.Duration
	MaxFrameBytes     int
	WriteChanCap      int
	MaxConnsPerUser   int
	DrainTimeout      time.Duration
	// SendTick 透传给 ws.Config.SendTick：写循环可选兜底 tick。默认 0 =
	// 不启用（消除每连接 ~66 次/秒的空转唤醒）。
	SendTick time.Duration

	// Upgrade 限流覆盖（/ws upgrade 按 IP 令牌桶 + 失败熔断，E-3/E-4）。
	// 零值取 policy 默认（10/min、burst 20、阈值 8、窗口 5min、封禁 15min）。
	UpgradeRatePerMin       float64
	UpgradeBurst            float64
	UpgradeFailureThreshold int
	UpgradeFailureWindow    time.Duration
	UpgradeBanFor           time.Duration

	// RateLimits 覆盖默认限流规则（key=kind）。nil 表示使用 security-spec 默认表。
	RateLimits map[uint32]RateLimit

	// EnableRateLimit 为 false 时所有规则置为极大值（仅测试/压测使用）。
	EnableRateLimit bool

	// ExtraRoutes 是 /ws 之外同一 HTTP 端口上的处理器（/api/auth/*、
	// /healthz、SPA 静态资源）。nil = 非 /ws 路径返回 404。
	ExtraRoutes http.Handler

	// Flags 是进程级 feature flag 集；nil 时 New 自建一份默认集合。
	// 装配层注入共享实例后，SIGHUP 重下发的 flag 对用户层立即可见。
	Flags *featureflags.Flags
}

// RateLimit 单 kind 限流参数覆盖（对应 security-spec §1.2.2 一行）。
type RateLimit struct {
	Rate       float64
	Burst      float64
	Capacity   float64
	MaxPayload int
	Silent     bool
}

// DefaultConfig 返回一个可直接监听本机端口的默认配置（测试/开发）。
func DefaultConfig() Config {
	return Config{
		ListenAddr:        "127.0.0.1:8081",
		PublicOrigin:      "",
		CookieName:        "access_token",
		LogLevel:          "info",
		HeartbeatInterval: 25 * time.Second,
		ReadTimeout:       60 * time.Second,
		MaxFrameBytes:     64 << 10,
		WriteChanCap:      64,
		MaxConnsPerUser:   3,
		DrainTimeout:      10 * time.Second,
		EnableRateLimit:   true,
	}
}

// Policy 把 app.Config 展开成 policy.Config（默认值 + 覆盖）。
func (c Config) Policy() policy.Config {
	p := policy.Default()
	if c.HeartbeatInterval > 0 {
		p.Heartbeat.Interval = c.HeartbeatInterval
	}
	if c.ReadTimeout > 0 {
		p.Heartbeat.ReadTimeout = c.ReadTimeout
	}
	if c.MaxFrameBytes > 0 {
		p.MaxFrameBytes = c.MaxFrameBytes
	}
	if c.WriteChanCap > 0 {
		p.WriteChanCap = c.WriteChanCap
	}
	if c.MaxConnsPerUser > 0 {
		p.MaxConnsPerUser = c.MaxConnsPerUser
	}
	if c.DrainTimeout > 0 {
		p.DrainTimeout = c.DrainTimeout
	}
	if c.UpgradeRatePerMin > 0 {
		p.UpgradeRatePerMin = c.UpgradeRatePerMin
	}
	if c.UpgradeBurst > 0 {
		p.UpgradeBurst = c.UpgradeBurst
	}
	if c.UpgradeFailureThreshold > 0 {
		p.UpgradeFailureThreshold = c.UpgradeFailureThreshold
	}
	if c.UpgradeFailureWindow > 0 {
		p.UpgradeFailureWindow = c.UpgradeFailureWindow
	}
	if c.UpgradeBanFor > 0 {
		p.UpgradeBanFor = c.UpgradeBanFor
	}
	for kind, r := range c.RateLimits {
		p.Limits.Rules[kind] = limitRule(r.Rate, r.Burst, r.Capacity, r.MaxPayload, r.Silent)
	}
	if !c.EnableRateLimit {
		// 测试/压测：放大到实际上无限（含 upgrade 限流——容量测试要的是
		// 带载极限，不是攻击面）。
		p.UpgradeRatePerMin = 1e9
		p.UpgradeBurst = 1e9
		p.UpgradeFailureThreshold = 1e9
		for k, r := range p.Limits.Rules {
			r.Rate = 1e9
			r.Burst = 1e9
			r.Capacity = 1e9
			p.Limits.Rules[k] = r
		}
		p.Limits.DefaultRule = limitRule(1e9, 1e9, 1e9, 0, false)
	}
	return p
}
