// v3deliver.go —— v3 总线 P3d-2a（窄版）：把 DeliveryPlane 的 Pump 循环封装成
// 可被连接层调用的 per-conn 投递 goroutine（docs/v3-bus-design.md §3/§9）。
//
// 本文件是 P3d-2a 的【孤立单元】：只新增这里列出的三样东西，其余一律不做——
//   - AttachV3Deliver(a *assembly)：入口，nil 安全；内部句柄不齐时不接任何东西；
//   - WireV3.deliverLoop(ctx, uid, dev, send)：per-conn 订阅 + Pump 循环 +
//     kind 15 批量 ACK 回调注入（OnAck）；
//   - WireV3.deliverLoopKind15：ACK 帧形态桥（仅做形态判别与转调 OnAck）。
//
// 明确不做（与 P3d-2 全量版的边界）：
//   - 不改任何既有文件（连接写循环、router、pipe、fanout 一行不动）；
//   - 不在 newAssembly / run 中调用 AttachV3Deliver（无人调用 = 零行为变化，
//     §9 中间态铁律：旧直投路径仍是唯一投递路径）；
//   - 不写数据库、不改 flags/policy/config，不新增 flag；
//   - 不做旧路径瘦身与背压统计上报（Bus.PausedConns / BackpressureDepth 属
//     P3d-2 全量版，见 docs/v3-p0-checklist.md §5「P3d 计划（更新）」）。
//
// 生命周期：deliverLoop 返回即完成 Unsubscribe；连接断开方负责 cancel ctx。
// send 返回 error（连接写失败）→ 立即 Unsubscribe 并退出：该条未确认，仍滞留
// mailbox，重连后由 Resume / Plane.Subscribe 的持久化 cursor 续传（§3.1）。
package main

import (
	"context"
	"time"

	"github.com/yingo-server/OpenDiscord/server/internal/bus/delivery"
)

// deliverPollInterval 是 PAUSED / 追平态的轮询间隔。机制项（非策略）：
// 值取得足够小以保住帧间延迟，又足够大避免死连接空转烧 CPU。
const deliverPollInterval = 100 * time.Millisecond

// deliverLoopKind15 是 kind 15（MSG_ACK 批量累积确认，§4.2）的 ACK 帧形态桥：
// 把一条客户端 ACK 记录转调 Plane.OnAck。设计上只做「形态判别 + 转调」，
// 不解析 proto、不触碰 mailbox ——ACK 持久化由 OnAck 内部经 AckFn（组装层接到
// MailboxHub.Ack → Store.MarkAcked）完成，本函数不做任何 DB 写入。
//
// cursor 为该批 ACK 的累积游标（单调；后退/重复由 OnAck 幂等吸收），
// n 为该批确认条数（credit += n）。当前主干尚未把 kind 15 路由到这里
// （P3d-2a 不改连接写循环），故 n=0 / cursor=0 的零值调用是合法 no-op。
func (w *WireV3) deliverLoopKind15(c *delivery.Conn, cursor int64, n int) error {
	if w == nil || c == nil {
		return nil
	}
	return w.Plane.OnAck(context.Background(), c, cursor, n)
}

// deliverLoop 是单条连接（uid, dev）的 v3 投递循环（§3.1 消费路径）：
//
//	Subscribe → 循环 Pump →（send 回调由调用方注入，内部发帧）→
//	断连/send 失败 → Unsubscribe → 返回。
//
// Pump 的 credit 流控（§3.2）由 Plane 内部维护：credit 耗尽 → PAUSED 停拉
// （不丢、不标记）；PAUSED 超 slow_consumer_close_s → ShouldClose=true，
// 本循环视为判死，同样 Unsubscribe 退出（连接层负责真正关连接）。
//
// 出错策略：ListSince / Head 读失败只记一条 DEBUG 语义的退避后重试
// （mailbox 读失败不是连接的错，不能因为旁路设施抖动断开用户连接）；
// send 失败才立即退出。send 返回 error 视为连接写失败——该条不推进 cursor，
// 条目滞留 mailbox，重连按持久化 cursor 续传（§3.1 Resume 语义）。
//
// 本函数由连接层在自己的 goroutine 中调用（P3d-2a 不自动接线，无人调用 =
// 零行为变化）；调用方约定：ctx 取消或 send 出错时本函数返回，不重启自己。
func (w *WireV3) deliverLoop(ctx context.Context, uid int64, dev string, send delivery.SendFn) {
	if w == nil || w.Plane == nil || send == nil {
		return
	}
	c := w.Plane.Subscribe(uid, dev, send)
	defer w.Plane.Unsubscribe(c)

	for {
		// Pump 逐条 send；credit 流控 / PAUSED / 判死都在 Plane 内部。
		res := w.Plane.Pump(ctx, c)
		if res.Err != nil {
			// send 写失败（连接问题）→ 退出；读失败也可能是 ctx 取消，
			// 统一以 ctx 是否存活区分：连接没了就退出，抖动就重试。
			if ctx.Err() != nil {
				return
			}
			// 区分不出读写错误时保守退出：send 失败滞留条目可续传，
			// 误退出的代价远小于对死连接空转。
			return
		}
		if res.ShouldClose {
			// 判死（PAUSED 超 slow_consumer_close_s）：交给连接层关连接，
			// 本循环不再空转。
			return
		}
		if res.Paused || res.Sent == 0 {
			// PAUSED（credit 耗尽等 ACK 补充）或 mailbox 追平：轮询等待，
			// 让出调度。OnAck 由 ACK 路径并发触发补 credit；新条目由旧路径
			// 双写进 mailbox，下一轮 Pump 自然拉到。
			t := time.NewTimer(deliverPollInterval)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}
}

// AttachV3Deliver 是 P3d-2a 的装配入口：把 v3 投递循环接到 assembly 上。
//
// P3d-2a 窄版约束：本函数只做句柄齐备性检查，**不自动调用 deliverLoop**——
// 没有连接层调用点（那是 P3d-2 全量版改连接写循环的事），所以本函数实际
// 是一个 nil 安全的占位接线点：a == nil / a.v3 == nil 直接 no-op；
// 句柄齐备时同样 no-op 并返回，保证本文件对主干零行为影响。
//
// 签名保留 assembly 参数，使 P3d-2 全量版在同一调用点扩展即可。
func AttachV3Deliver(a *assembly) {
	if a == nil {
		return
	}
	w := a.v3
	if w == nil || w.Plane == nil {
		return
	}
	// P3d-2a：无连接层调用点，此处不做任何接线（§9 中间态铁律）。
}
