// v3hooks.go —— v3 总线 P3d-2b-1（窄版）：连接生命周期钩子适配
// （docs/v3-bus-design.md §3.1 连接订阅模型；§9 中间态铁律）。
//
// 本批只交付「机制」：把 user 层 Server 的 OnConnect/OnDisconnect 钩子
// （server/internal/user/app/server.go 的 app.ConnLifecycle）适配到已有的
// WireV3.deliverLoop，使后续批次可以在连接粒度启动/停止 v3 投递循环。
//
// 孤立说明（重要）：本批【不启用】。没有任何调用点把本文件构造的钩子注入
// app.Server（AddConnLifecycle 无人调用），AttachV3Deliver 仍是 no-op。旧
// 直投路径因此仍是唯一投递路径，不会出现双投递；待后续批次做路径切换时，
// 装配层在同一调用点接线即可。
//
// 明确不做：不改 assemble.go 的组件构造顺序、不做旧路径瘦身、不新增 flag。
// （P3d-2 接线本身落在 v3attach.go：AttachV3Deliver 已由 newAssembly 调用。）
package main

import (
	"context"
	"errors"

	"github.com/yingo-server/OpenDiscord/server/internal/bus/delivery"
	"github.com/yingo-server/OpenDiscord/server/internal/user/app"
	"github.com/yingo-server/OpenDiscord/server/internal/user/net/ws"
)

// errV3ConnWrite 是 send 适配器把 c.Write 的 false 转成 error 的哨兵值：
var errV3ConnWrite = errors.New("v3: conn write buffer full or closed")

// DeliveryEnabled 判定 uid 当前是否应走 v3 mailbox 投递（§6 灰度语义，
// universal-pull-spec §1.5「同一 uid 不允许两路径同时投递」）。
// 这是【唯一路径判定】：每次调用读最新 flag 快照（v3_bus_enabled &&
// v3_bus_percent>0 && InV3Bucket(uid)），连接订阅（Connected）与旧直投
// 抑制（Router）两处共用，中途热改对下一个判定点生效，不存在窗口期分裂。
func (h *v3ConnLifecycle) DeliveryEnabled(uid int64) bool {
	if h == nil || h.w == nil {
		return false
	}
	w := h.w
	if w.Plane == nil || w.Enabled == nil || w.InBucket == nil {
		return false
	}
	return w.Enabled() && w.InBucket(uid)
}

// LegacyDirectEnabled 是给 user 层 Router 的旧直投开关（双投递保护）：
// 灰度 uid 返回 false —— 该 uid 的帧不得再走旧逐成员直投，因为它的连接
// 已由 deliverLoop 从 mailbox 推送；flag-off / 非灰度 uid 恒返回 true，
// 旧路径逐字节不变（T-SV-5 / S-7 基线）。
func (h *v3ConnLifecycle) LegacyDirectEnabled(uid int64) bool {
	return !h.DeliveryEnabled(uid)
}

// v3ConnLifecycle 实现 app.ConnLifecycle：连接建立时（且仅当该 uid 落在
// v3 灰度桶）启动 per-conn deliverLoop，断连时由返回的 cancel 停掉它。
// 它同时是【双投递保护的判定与守卫方】：v3hooks.go 提供 LegacyDirectEnabled，
// Router 用它按帧抑制灰度 uid 的旧直投。
type v3ConnLifecycle struct {
	w      *WireV3
	runCtx context.Context
}

var _ app.ConnLifecycle = (*v3ConnLifecycle)(nil)

// newV3ConnLifecycle 从 assembly 构造钩子：runCtx 即 assembly.run 的根 ctx。
// 句柄不齐返回 nil 接口（app.Server.AddConnLifecycle 对 nil 直接忽略）。
// 本批由 v3attach.AttachV3Deliver 在 newAssembly 末尾调用（真接线点）。
func newV3ConnLifecycle(a *assembly, runCtx context.Context) app.ConnLifecycle {
	if a == nil || a.v3 == nil || runCtx == nil {
		return nil
	}
	return &v3ConnLifecycle{w: a.v3, runCtx: runCtx}
}

// Connected 在连接注册成功后由 Server.OnConnect 调用（§3.1：认证完成 →
// SUBSCRIBE mailbox://uid@dev）。灰度判定每次读最新 flag 快照：flag-off
// 或非灰度 uid 直接返回 nil —— 不启动 goroutine、不注册订阅、无清理动作。
//
// 灰度判定只允许走 DeliveryEnabled（v3attach 定义的唯一路径真相源），
// 与 Router 抑制旧直投共用同一判定（双投递保护铁律）。
func (h *v3ConnLifecycle) Connected(c *ws.Conn) func() {
	if h == nil || h.w == nil || c == nil {
		return nil
	}
	w := h.w
	if w.Plane == nil || w.Enabled == nil || w.InBucket == nil {
		return nil
	}
	uid := c.UserID()
	if !h.DeliveryEnabled(uid) {
		return nil
	}

	// 正常装配路径 runCtx 非 nil；零值结构体在这里兜底，避免 WithCancel
	// 收到 nil parent 直接 panic。子 ctx 的生命周期 = 本连接。
	parent := h.runCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)

	// send 把 DeliveryPlane 的 (ctx, mailbox_seq, envelope) 写回调适配到
	// ws.Conn 的非阻塞写缓冲：false（缓冲满 / 连接已关）→ error，Pump 立即
	// 停止且不推进 cursor，条目留给重连续传。seq/ctx 在 user 层不解释。
	var send delivery.SendFn = func(_ context.Context, _ int64, blob []byte) error {
		if !c.Write(blob) {
			return errV3ConnWrite
		}
		return nil
	}

	go w.deliverLoop(ctx, c.UserID(), c.DeviceID(), send)
	// 断连清理：cancel 子 ctx → deliverLoop 退出 → Plane.Unsubscribe。
	return cancel
}
