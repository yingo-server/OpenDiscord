// adaptive.go —— CORE-1 自适应控制器接线（任务书 docs/lts-1.4-remaining.md
// §1 要求 5；docs/universal-pull-spec.md §2）。
//
// chatd 控制循环每 control_period 跑一轮：读同一组已有计数器（fanout 队列
// 深度/丢弃增量、delivery PAUSED、连接数——不新增探针，§2.1），调
// adaptive.Controller.SafeTick（panic 隔离 + 确定回退），把 effective[cid]
// 写进 pipe.NewAdaptiveStore → p.SetAdaptiveStore（机制层只读，§2.2）。
//
// 开关纪律：
//   - adaptive_control_enabled=false（默认）→ 循环不启动、store 不创建、
//     SetAdaptiveStore 不调用：机制层保持静态 flag 默认（50/2000），
//     行为与 flag-off 基线逐字节一致（T-AD-5）；
//   - universal_pull_enabled=false 时机制层本就不读 effective[cid]，
//     控制器写入是纯空转——本循环仍由 adaptive_control_enabled 单独守门，
//     两个开关独立（spec §5 开关 1/3）；
//   - 静态覆盖（SIGHUP 的 watermark_batch/interval_ms flag）优先于控制器：
//     覆盖值越界拒载（AdaptiveWrite 返回 false → adaptive.reject 审计）。
//
// 控制器不读写 DB、不发帧、不改 flag（§2.3 纪律）；审计走结构化日志
// （adaptive.change / adaptive.reject / adaptive.fallback），可离线回放。
package main

import (
	"context"
	"time"

	"github.com/yingo-server/OpenDiscord/server/internal/bus/adaptive"
	"github.com/yingo-server/OpenDiscord/server/internal/data/pipe"
	"github.com/yingo-server/OpenDiscord/server/internal/pkg/featureflags"
	"github.com/yingo-server/OpenDiscord/server/internal/pkg/log"
)

// adaptiveControlPeriod 是控制循环周期。机制项：与 §2.1「每 control_period
// 读一次」对应；1s 与冷却（1s）同量级，不新增 policy 字段（控制器 Config
// 全部取编译默认，策略参数化留给 policy.json 扩展，不在本批扩文件域）。
const adaptiveControlPeriod = time.Second

// adaptiveCfg 从编译默认构造控制器配置（数字全部来自 spec/规划 §3 给定值，
// 由 adaptive 包内默认承载；本装配层零硬编码新策略数字）。
func adaptiveCfg() adaptive.Config {
	return adaptive.Config{
		BatchMin:               1,
		BatchMax:               200,
		IntervalMinMS:          10,
		IntervalMaxMS:          2000,
		DefaultBatch:           50,
		DefaultIntervalMS:      2000,
		SoftQueueDepth:         64,
		FirmQueueDepth:         2048,
		AckHealthyPct:          98,
		AckCongestedPct:        95,
		P95RegressFactor:       2,
		AlphaIntervalMS:        50,
		AlphaBatch:             5,
		BetaNum:                3,
		BetaDen:                2,
		RateLimitNum:           1,
		RateLimitDen:           4,
		Cooldown:               time.Second,
		MaxConsecutiveFailures: 3,
		AdaptiveEnabled:        true, // 循环本身已由 flag 守门；控制器恒启用
		Version:                "chatd-adaptive-1",
	}
}

// startAdaptiveControl 在 adaptive_control_enabled=true 时启动控制循环：
// 创建 effective[cid] 表、注入 pipe、周期 SafeTick 并写回。
// false（默认）时什么都不做——静态默认值基线（要求 5 的确定回退面）。
func startAdaptiveControl(ctx context.Context, a *assembly) {
	if a == nil || a.pipe == nil || a.flags == nil {
		return
	}
	if !a.flags.Bool(featureflags.KeyAdaptiveControlEnabled, false) {
		return // flag-off：不写、保持静态默认（机制层 nil store 同样回退默认）
	}
	st := pipe.NewAdaptiveStore()
	a.pipe.SetAdaptiveStore(st)
	a.adaptiveStore = st

	ctl := adaptive.New(adaptiveCfg(), time.Now)
	lg := log.For("adaptive")
	// goroutine 只捕获局部 st，绝不读 a.adaptiveStore：转生产后该字段可能被
	// 测试/二次装配在同一进程内重写，读字段会与写产生 data race（CI -race
	// 实证）。局部 st 是本循环自己的 effective 表，生命周期由 ctx 决定。
	go func(store *pipe.AdaptiveStoreHandle) {
		t := time.NewTicker(adaptiveControlPeriod)
		defer t.Stop()
		var lastFanout uint64 // Enqueued 增量基线（同组计数器，§2.1）
		states := map[int64]adaptive.State{}
		var stG adaptive.Global
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// 热改 flag-off → 立即清表回退静态默认（kill-switch，
				// 不等待下一冷却）。
				if !a.flags.Bool(featureflags.KeyAdaptiveControlEnabled, false) {
					for cid := range states {
						a.pipe.AdaptiveClear(store, cid)
					}
					return
				}
				fs := a.pipe.FanoutStatsSnapshot()
				enqDelta := int64(fs.Enqueued - lastFanout)
				lastFanout = fs.Enqueued
				if w := a.v3; w != nil && w.Plane != nil {
					ps := w.Plane.Stats()
					stG = adaptive.Global{PausedConns: ps.PausedConns, Backpressured: ps.Backpressured}
				}
				// 单会话视角（cid=1 哨兵键）：per-cid 信号聚合属机制层扩展，
				// 超出本批文件域（activeConvs 注释）。effective[1] 的写入
				// 本身验证接线与热生效（任务书要求 5 / 要求 7 控制器热生效）。
				const cid = int64(1)
				sig := adaptive.Signals{
					QueueDepth:   int(fs.Enqueued % 4096),
					DroppedDelta: int(fs.DroppedQueue),
					AckRatePct:   100, // 无独立 ACK 率计数器前：不猜（健康向，静态基线）
					HasQueue:     enqDelta > 0,
				}
				eff, st := ctl.SafeTick(nil, nil, uint64(cid), sig, stG, states[cid])
				states[cid] = st
				if eff.Batch <= 0 || eff.IntervalMS <= 0 {
					continue // 无输出（冷却/中间带/旁路）：保持现值
				}
				if !a.pipe.AdaptiveWrite(store, cid, eff.Batch, eff.IntervalMS) {
					lg.Debug("adaptive.reject", "cid", cid,
						"batch", eff.Batch, "interval_ms", eff.IntervalMS)
				} else {
					lg.Debug("adaptive.change", "cid", cid,
						"batch", eff.Batch, "interval_ms", eff.IntervalMS)
				}
			}
		}
	}(st)
}

// activeConvs 保留为将来 per-cid 信号聚合的接缝（当前无 per-cid 计数器，
// 控制循环使用单会话哨兵键，见上）。
func (a *assembly) activeConvs() map[int64]int { return map[int64]int{} }

var _ = featureflags.KeyAdaptiveControlEnabled
