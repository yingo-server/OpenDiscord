// v3wire.go —— v3 总线 P3c 装配 + P3d-1 fanout 灰度分流 adapter
// （docs/v3-bus-design.md §5.1/§6/§9）。
//
// P3c：只构造 MailboxHub / DeliveryPlane 并挂到 assembly.v3。
// P3d-1：新增 AttachV3Split，把 hub.Offer 适配成 pipe.V3Offer 注入 data 层
//
//	fanout 产出点。**旧直投路径仍是唯一投递路径**：这里只做双写旁路，
//	不改连接写循环（P3d-2），不接 DeliveryPlane.Pump 到任何连接。
//	flag-off 时 adapter 直接返回 (false, nil)，hub.Offer 本身也是 no-op。
//
// 孤立说明：cmd/opendiscord/v3.go 是 P3b 的孤立构造/测试单元，不是主干入口；
// 主干唯一的 v3 构造点是本文件的 newV3Wire。
package main

import (
	"context"
	"database/sql"

	"github.com/yingo-server/OpenDiscord/server/internal/bus/delivery"
	"github.com/yingo-server/OpenDiscord/server/internal/bus/mailbox"
	"github.com/yingo-server/OpenDiscord/server/internal/data/pipe"
	"github.com/yingo-server/OpenDiscord/server/internal/pkg/featureflags"
)

// WireV3 是 v3 总线在 chatd 装配层的句柄集合。P3d-1 起 Hub 会收到 fanout
// 的旁路双写，但 Plane 仍未接连接（P3d-2）；旧直投路径不受影响。
type WireV3 struct {
	Hub   *mailbox.MailboxHub
	Plane *delivery.Plane

	// Enabled 反映当前灰度总开关（总开关 && percent>0）。
	// 每次调用读最新 flag 快照（SIGHUP/911 热重载语义由上游保证）。
	Enabled func() bool

	// InBucket 判定 uid 是否落在 v3 灰度桶（uid 哈希，与连接/重启无关）。
	InBucket func(uid int64) bool
}

// newV3Wire 构造 v3 投递栈（P3c 主干装配点；newAssembly 成功路径末尾调用）。
//
//	db      —— 热库句柄（与 data 层同一个连接池；mailbox 分域 DDL 已在
//	          P0 迁移树 store/migrations/hot/mailbox/0001）；
//	flagsFn —— flag 快照读取器，典型 a.flags.Snapshot（SIGHUP 热重载语义
//	          由上游保证），允许 nil（视为全关——安全中间态）；
//	planeCfg —— credit 流控参数（§6 flag：delivery_credit 等，零值取默认）。
//
// db/flagsFn 缺一即返回 nil（由调用方记 WARN 继续）：v3 总线在中间态是纯
// 旁路设施，任何故障都必须不影响旧路径投递（§9：旧路径是唯一投递路径）。
func newV3Wire(db *sql.DB, flagsFn func() map[string]any, planeCfg delivery.Config) *WireV3 {
	if db == nil || flagsFn == nil {
		return nil
	}
	store := mailbox.NewStore(db)
	hub := mailbox.NewHub(store, flagsFn, mailbox.HubConfig{})
	// AckFn 适配：plane 回调不带 error，持久化失败只留观测（cursor 仍单调）。
	ackFn := delivery.AckFn(func(ctx context.Context, uid, cursor int64) {
		_ = hub.Ack(ctx, uid, cursor)
	})
	plane := delivery.NewPlane(store, ackFn, planeCfg)

	w := &WireV3{Hub: hub, Plane: plane}
	w.Enabled = func() bool {
		flags := flagsFn()
		if !featureflags.BoolMap(flags, featureflags.KeyV3BusEnabled) {
			return false
		}
		percent, ok := featureflags.ToInt(flags[featureflags.KeyV3BusPercent])
		return ok && percent > 0
	}
	w.InBucket = func(uid int64) bool {
		return featureflags.InV3Bucket(flagsFn(), uint64(uid))
	}
	return w
}

// AttachV3Split 把 v3 灰度分流接进 data 层的 fanout 产出点（P3d-1）。
//
// adapter 每次调用都读最新 flag 快照：总开关 v3_bus_enabled=false 或
// percent=0 时不写 hub；否则按 InV3Bucket(uid)（uid 哈希，非连接随机）
// 决定该 uid 是否落 v3 桶，返回 inBucket 供观测计数。hub.Offer 内部还有
// 一层 flag 兜底 + delivery_key 去重 LRU（§2.2），双重保证 flag-off 零写入。
//
// 出错策略：hub 写失败只回传 error 给 pipe 计数，绝不返回到旧直投路径；
// mailbox 是旁路设施，任何故障都不影响既有投递（§9 中间态铁律）。
func (w *WireV3) AttachV3Split(p *pipe.Pipe) {
	if w == nil || p == nil {
		return
	}
	p.SetV3Offer(func(ctx context.Context, f pipe.V3Frame) (bool, error) {
		if !w.Enabled() || !w.InBucket(int64(f.UserID)) {
			return false, nil
		}
		_, err := w.Hub.Offer(ctx, int64(f.UserID), mailbox.Envelope{
			Kind:  int(f.Kind),
			MsgID: f.MsgID,
			Seq:   f.Seq,
			Blob:  f.Blob,
		})
		if err != nil {
			return false, err
		}
		return true, nil
	})
}
