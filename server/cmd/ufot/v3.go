// v3.go —— v3 总线 P3b 中间态装配（docs/v3-bus-design.md §5/§6/§9）。
//
// 本文件只做「构造 + 灰度判定准备」，不做任何旧路径切换/接线：
//   - mailbox.NewStore(a.hot.DB) → mailbox.NewHub(store, flagsFn, cfg)
//     → delivery.NewPlane(store, hub.Ack 适配为 AckFn, cfg)；
//   - Enabled() = v3_bus_enabled && v3_bus_percent>0（热改快照后即刻生效）；
//   - InBucket(uid) = featureflags.InV3Bucket(flagsFn(), uid)（uid 哈希稳定分桶）。
//
// §9 中间态铁律：v3_bus_enabled 默认 false，此时 hub.Offer 为 no-op、
// plane 无人订阅——旧路径行为逐字节不变。fanout 瘦身与背压传播留待 P3c。
package ufot

import (
	"context"
	"database/sql"

	"github.com/yingo-server/UFOT/server/internal/bus/delivery"
	"github.com/yingo-server/UFOT/server/internal/bus/mailbox"
	"github.com/yingo-server/UFOT/server/internal/pkg/featureflags"
)

// WireV3 是 v3 总线在装配层的句柄集合。P3b 只构造不接线：
// 调用方在 flag 打开前不得把 Hub/Plane 挂进任何下行 fanout 路径。
type WireV3 struct {
	Hub   *mailbox.MailboxHub
	Plane *delivery.Plane

	// Enabled 反映当前灰度总开关（总开关 && percent>0）。
	// 每次调用读最新 flag 快照（911/SIGHUP 热重载语义由上游保证）。
	Enabled func() bool

	// InBucket 判定 uid 是否落在 v3 灰度桶（uid 哈希，与连接/重启无关）。
	InBucket func(uid int64) bool
}

// NewWireV3 构造 v3 投递栈（P3b 中间态：只准备对象，不接线 fanout）。
//
//	db       —— 热库句柄（与 data 层 openDriver 打开的同一个 *sql.DB）；
//	flagsFn  —— flag 快照读取器（典型：a.flags.Snapshot），允许 nil（视为全关）；
//	planeCfg —— credit 流控参数（§6 flag：delivery_credit 等，零值取默认）。
func NewWireV3(db *sql.DB, flagsFn func() map[string]any, planeCfg delivery.Config) *WireV3 {
	store := mailbox.NewStore(db)
	hub := mailbox.NewHub(store, flagsFn, mailbox.HubConfig{})
	// AckFn 适配：plane 回调不带 error，持久化失败只留观测（cursor 仍单调）。
	ackFn := delivery.AckFn(func(ctx context.Context, uid, cursor int64) {
		_ = hub.Ack(ctx, uid, cursor)
	})
	plane := delivery.NewPlane(store, ackFn, planeCfg)

	w := &WireV3{Hub: hub, Plane: plane}
	w.Enabled = func() bool {
		if flagsFn == nil {
			return false
		}
		flags := flagsFn()
		if !featureflags.BoolMap(flags, featureflags.KeyV3BusEnabled) {
			return false
		}
		percent, ok := featureflags.ToInt(flags[featureflags.KeyV3BusPercent])
		return ok && percent > 0
	}
	w.InBucket = func(uid int64) bool {
		if flagsFn == nil {
			return false
		}
		return featureflags.InV3Bucket(flagsFn(), uint64(uid))
	}
	return w
}
