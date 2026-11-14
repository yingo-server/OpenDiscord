package main

import (
	"github.com/yingo-server/OpenDiscord/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/OpenDiscord/server/internal/user/net/frame"
	"github.com/yingo-server/OpenDiscord/server/internal/user/net/ws"
)

// mailbox.go —— v3 总线 P3d-2 接线的第二半（任务书 §1 CORE-3）。
//
// P3d-1（v3split.go / AttachV3Split）已经把灰度 uid 的 canonical 下行帧双写
// 进 mailbox；本文件把 hub 的读侧接到连接层（v3hooks 的 deliverLoop 只做了
// Subscribe + Pump，帧形态/ACK 的桥接都在这里）：
//
//  1. v3EnvelopeBytes：把 mailbox 条目的 Blob（P3d-1 offerTo 编码的 Envelope
//     proto 字节）转成 user 层连接写循环要的完整 WS 帧字节（frame.Encode
//     形态）。SendFn 回调直接拿到与旧路径 byte-identical 的下行帧。
//
//  2. mailboxACKSink：kind 15 MAILBOX_ACK 上行（{cursor, ackedCount?}）在
//     Router 上行侧解析后按 (uid, dev) 定位 Plane 订阅，转调
//     WireV3.deliverLoopKind15 → Plane.OnAck → AckFn → MailboxHub.Ack →
//     Store.MarkAcked（§4.2 累积游标，幂等单调）。不新增 kind、不改
//     Envelope；v3 off / 无订阅时与未知 kind 同路径丢弃，零行为差异。
//
// 双投递保护（最高优先级，universal-pull-spec §1.5 / §5 开关 2）：
// v3ConnLifecycle.DeliveryEnabled（v3hooks.go）是按 uid 的唯一单一路径判定
//（flag 快照 Enabled() && InV3Bucket(uid)），连接订阅（Connected）与 Router
// 旧直投抑制两处共用；Router 侧只见函数指针（V3DirectEnabled），user 层不
// import bus 包，无 user→bus 反向依赖（import-lint 铁律）。
//
// v3_bus_enabled=false 时 DeliveryEnabled 恒 false（= 旧路径恒开）、
// mailboxACKSink 在无订阅时 no-op：Router 两个 hook 零成本，旧路径逐字节
// 不变（T-SV-5 / S-7 基线）。

// v3EnvelopeBytes 见上。
func v3EnvelopeBytes(b []byte) []byte {
	env := &chatpb.Envelope{}
	if err := env.Unmarshal(b); err != nil {
		return nil
	}
	out, err := frame.Encode(env)
	if err != nil {
		return nil
	}
	return out
}

// mailboxACKSinkFor 返回挂在 WireV3 上的 kind 15 处理回调
// （Router.V3AckSink 注入）；句柄不齐返回 nil（Router 对 nil sink 直接丢弃，
// 与改动前的未知 kind 同路径）。
//
// 定位语义：Router 解析 payload 后按连接（c）定位 Plane 订阅——OnAck 需要
// 属于该 (uid, dev) 的 Conn 才能推进 per-conn cursor/credit。找不到订阅
// （v3 off 窗口、刚断连、非灰度 uid）时静默丢弃：客户端下一批 ACK 自然
// 收敛，不产出任何副作用。
func mailboxACKSinkFor(w *WireV3) func(c *ws.Conn, cursor int64, ackedCount int) {
	if w == nil || w.Plane == nil {
		return nil
	}
	plane := w.Plane
	return func(c *ws.Conn, cursor int64, ackedCount int) {
		if c == nil {
			return
		}
		conn := plane.ConnOf(c.UserID(), c.DeviceID())
		if conn == nil {
			return
		}
		// deliverLoopKind15 只做形态判别与转调（OnAck 幂等吸收后退/重复）。
		_ = w.deliverLoopKind15(conn, cursor, ackedCount)
	}
}
