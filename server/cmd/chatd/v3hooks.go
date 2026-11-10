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
// 明确不做：不改 assemble.go / v3deliver.go / AttachV3Deliver、不做路径
// 切换、不做 kind 15 上行、不做指标。
package main

import (
	"context"
	"errors"

	"github.com/yingo-server/OpenDiscord/server/internal/bus/delivery"
	"github.com/yingo-server/OpenDiscord/server/internal/user/app"
	"github.com/yingo-server/OpenDiscord/server/internal/user/net/ws"
)

// errV3ConnWrite 是 send 适配器把 c.Write 的 false 转成 error 的哨兵值：
// Pump 见 error 立即停止且不推进 cursor，该条目滞留 mailbox，重连后由
// Resume / Subscribe 的持久化 cursor 续传（v3deliver.go / plane.go 语义）。
var errV3ConnWrite = errors.New("v3: conn write buffer full or closed")

// v3ConnLifecycle 实现 app.ConnLifecycle：连接建立时（且仅当该 uid 落在
// v3 灰度桶）启动 per-conn deliverLoop，断连时由返回的 cancel 停掉它。
//
// runCtx 是 assembly.run(ctx) 的根 ctx（main 传入，SIGTERM 时 cancel）；
// 每条连接的 ctx 由它派生，进程退出时全部投递循环一并收敛，断连只 cancel
// 该连接自己的子 ctx，不影响同 uid 的其他设备（§3.4）。
type v3ConnLifecycle struct {
	w      *WireV3
	runCtx context.Context
}

var _ app.ConnLifecycle = (*v3ConnLifecycle)(nil)

// newV3ConnLifecycle 从 assembly 构造钩子：runCtx 即 assembly.run 的根 ctx。
// 句柄不齐返回 nil 接口（app.Server.AddConnLifecycle 对 nil 直接忽略）。
// 本批无调用点，签名保留给后续批次在同一装配点接线。
func newV3ConnLifecycle(a *assembly, runCtx context.Context) app.ConnLifecycle {
	if a == nil || a.v3 == nil || runCtx == nil {
		return nil
	}
	return &v3ConnLifecycle{w: a.v3, runCtx: runCtx}
}

// Connected 在连接注册成功后由 Server.OnConnect 调用（§3.1：认证完成 →
// SUBSCRIBE mailbox://uid@dev）。灰度判定每次读最新 flag 快照：flag-off
// 或非灰度 uid 直接返回 nil —— 不启动 goroutine、不注册订阅、无清理动作。
func (h *v3ConnLifecycle) Connected(c *ws.Conn) func() {
	if h == nil || h.w == nil || c == nil {
		return nil
	}
	w := h.w
	if w.Plane == nil || w.Enabled == nil || w.InBucket == nil {
		return nil
	}
	uid := c.UserID()
	if !w.Enabled() || !w.InBucket(uid) {
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
