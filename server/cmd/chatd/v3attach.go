// Package main — v3attach.go: P3d-2 接线（任务书 docs/lts-1.4-remaining.md §1 CORE-3）。
//
// 三件事，全部只在装配层（cmd/chatd）动刀，机制层文件零改动：
//
//  1. AttachV3Deliver 真接线（v3deliver.go 原为 no-op 占位）：在 newAssembly
//     末尾构造 v3ConnLifecycle（v3hooks.go 已有的 ConnLifecycle 适配）注入
//     app.Server.AddConnLifecycle。OnConnect → Subscribe + deliverLoop；
//     OnDisconnect → cleanup（cancel → Unsubscribe）。
//  2. 双投递保护（最高优先级，universal-pull-spec §1.5「一次性切换」）：
//     user 层 Router 每帧下行前调 v3ConnLifecycle.LegacyDirectEnabled(uid)，
//     灰度 uid（flag 快照 Enabled() && InBucket(uid)）关闭旧逐成员直投，
//     关闭计数进 Router.OnV3Skip 指标；判定函数是唯一路径真相源，连接
//     订阅（v3hooks.Connected）与直投抑制两处共用，杜绝双投递窗口。
//     v3_bus_enabled=false 时本函数恒 true → user 层行为逐字节不变（S-7）。
//  3. kind 15 MAILBOX_ACK 上行：Router 上行侧（handleLocal）新增本地 kind，
//     在 user 层解析 {cursor, ackedCount}（JSON wire，客户端 encodeMailboxAck
//     形态）后经注入的 ackSink 转调 Plane.OnAck → AckFn → MailboxHub.Ack →
//     Store.MarkAcked。不新增 kind、不改 Envelope；v3 off / 无 ackSink 时
//     kind 15 与改动前的未知 kind 一样丢弃，零行为差异。
package main

import (
	"github.com/yingo-server/OpenDiscord/server/internal/user/app"
	"github.com/yingo-server/OpenDiscord/server/internal/user/core/session"
	"github.com/yingo-server/OpenDiscord/server/internal/user/net/ws"
)

// AttachV3Deliver 把 v3 投递循环真正接进 assembly（P3d-2a 占位函数的真实现）：
//
//   - 构造 v3ConnLifecycle（连接建立时启动 deliverLoop、断连 cancel）注入
//     app.Server.AddConnLifecycle —— 复用 user 层已有 ConnLifecycle 机制，
//     不改其接口语义（v3hooks.go 的适配原样可用，本函数只是补上调用点）；
//   - 向 Router 注入两样：V3DirectEnabled（双投递保护判定 + 抑制计数）、
//     V3AckSink（kind 15 上行 ACK）。
//
// 句柄不齐（a==nil / a.v3==nil / a.user==nil / Plane==nil）直接 no-op：
// v3 是旁路设施，任何缺件都不得阻断启动（§9 中间态铁律）。
func AttachV3Deliver(a *assembly) {
	if a == nil || a.v3 == nil || a.user == nil {
		return
	}
	w := a.v3
	if w.Plane == nil {
		return
	}
	if h := newV3ConnLifecycle(a, a.runCtx); h != nil {
		a.user.AddConnLifecycle(h)
		a.v3Hooks = h.(*v3ConnLifecycle)
		if r := a.user.Router(); r != nil {
			r.V3DirectEnabled = legacyDirectEnabledFor(h)
			r.V3AckSink = sinkForSessionConn(mailboxACKSinkFor(w))
		}
	}
}

// legacyDirectEnabledFor 是 Router.V3DirectEnabled 的适配形态：Router 拿到
// 的是 uid（而非 ConnLifecycle 句柄），内部转调 v3ConnLifecycle 的统一判定。
// 抑制发生时同步累加 WireV3.v3Skip（dump 的 legacy_skipped 指标）。
func legacyDirectEnabledFor(h app.ConnLifecycle) func(int64) bool {
	lh, ok := h.(*v3ConnLifecycle)
	if !ok {
		return nil
	}
	return func(uid int64) bool {
		ok := lh.LegacyDirectEnabled(uid)
		if !ok && lh.w != nil {
			lh.w.v3Skip.Add(1) // 旧直投被抑制一次（v3 投递接管该 uid）
		}
		return ok
	}
}

// sinkForSessionConn 把 *ws.Conn 形态的 kind 15 回调适配成 session.Conn 形态
// （Router 上行只见 session.Conn；两个接口的 UserID/DeviceID 语义一致）。
func sinkForSessionConn(sink func(*ws.Conn, int64, int)) func(session.Conn, int64, int) {
	if sink == nil {
		return nil
	}
	return func(c session.Conn, cursor int64, n int) {
		wc, ok := c.(*ws.Conn)
		if !ok || wc == nil {
			return
		}
		sink(wc, cursor, n)
	}
}

// mailboxACKSink 形态与 kind 15 桥接实现（mailboxACKSinkFor）已在 mailbox.go
//（与 v3EnvelopeBytes 同处，保持单一主题）。
