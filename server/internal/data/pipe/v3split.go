// v3split.go —— v3 P3d-1：fanout 产出按灰度分流入 MailboxHub
// （docs/v3-bus-design.md §5.1/§6/§9；§10 低价值帧旁路）。
//
// 中间态铁律（§9 / v3-p0-checklist §5）：
//   - 旧直投路径（putBulk/fanoutToPut）完全保留，hub 是**旁路写入**（双写，
//     便于逐字节对照）；连接写循环一行不动——那是 P3d-2。
//   - sink 为 nil（未装配，例如全部单测夹具）时 offerTo 是零成本 no-op：
//     flag-off / 未装配时旧路径行为逐字节不变。
//   - 灰度判定（uid 哈希分桶 + 总开关）不在这里做：由装配层的 WireV3
//     adapter 读最新 flag 快照完成，本层只负责「产出 → 每 uid 一条 canonical
//     帧」的展开。因此同 uid 稳定落同一路径（T-SV-7）。
//
// 分层：本文件不 import internal/bus/*；hub 通过函数注入，bus 层仍只依赖
// database/sql，data 层不反向依赖 bus 实现（import-lint 铁律）。
package pipe

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"sync/atomic"

	"github.com/yingo-server/OpenDiscord/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/kinds"
)

// V3Frame 是投递展开后、写入 mailbox 的 canonical 下行帧（§2.1）。
// Blob 是已经 proto 编码的完整 Envelope（含 user_id 目标），P3d-2 的
// DeliveryPlane 可以直接把它交给连接写循环，不需要再编码一次。
type V3Frame struct {
	Kind      uint32
	ConvID    uint64
	UserID    uint64
	RequestID string
	Payload   []byte
	// MsgID / Seq 组成 §2.2 的 delivery_key = hash(uid, kind, msg_id|seq)。
	// 102 用 msg_id+seq，14 用 latest_seq，其余帧从 payload 提取或退化成
	// payload 的 FNV-1a（无序号帧无法从协议字段稳定去重）。
	MsgID int64
	Seq   int64
	Blob  []byte
}

// V3Offer 是 MailboxHub.Offer 的注入点。返回 (inBucket, err)：
// inBucket=false 表示该 uid 不在灰度桶（或总开关关闭），不计入 Offered。
// 实现必须每次读最新 flag 快照（SIGHUP/911 热改即刻生效）。
type V3Offer func(ctx context.Context, f V3Frame) (bool, error)

// v3Split 是 pipe / fanoutPlane / flusher 共享的旁路状态（指针共享，
// 三个产出点共用一组计数器）。零值（nil offer）= 纯 no-op。
type v3Split struct {
	offer V3Offer

	stOffered   atomic.Uint64 // 真正尝试写 hub 的帧数（含去重命中）
	stBypassed  atomic.Uint64 // 不在灰度桶 / 总开关关闭
	stFailed    atomic.Uint64 // Offer 返回 error（只观测，绝不影响旧路径）
	stBroadcast atomic.Uint64 // 展开出的 per-uid 帧数（观测双写比例）
}

// V3SplitStats 是 P3d-1 的可观测快照（§5.2 指标的接线点；P3d-2 再补
// Bus.PausedConns / Bus.BackpressureDepth）。
type V3SplitStats struct {
	Offered   uint64
	Bypassed  uint64
	Failed    uint64
	Broadcast uint64
}

func (v *v3Split) stats() V3SplitStats {
	if v == nil {
		return V3SplitStats{}
	}
	return V3SplitStats{
		Offered:   v.stOffered.Load(),
		Bypassed:  v.stBypassed.Load(),
		Failed:    v.stFailed.Load(),
		Broadcast: v.stBroadcast.Load(),
	}
}

// v3Eligible 排除 §10「明确不做」的低价值无序旁路帧：typing(230) 与
// presence(400/401/402) 不强排序进 mailbox，旧直投路径照常发送。
// 数据帧（102/104/105/14/108/202…）全部进总线。
func v3Eligible(kind uint32) bool {
	switch kind {
	case uint32(kinds.KindTyping),
		uint32(kinds.KindPresenceUpdate),
		uint32(kinds.KindPresenceOnline),
		uint32(kinds.KindPresenceOffline):
		return false
	}
	return true
}

// offerTo 把一条 fanout 产出按 uid 展开后交给 sink。它在旧直投之前调用，
// 但**任何失败都只记数、不 return**：旧路径是中间态唯一的投递路径。
// 每条 per-uid 帧独立编码（Envelope.target.user_id 不同），编码一次
// 共享 body 的优化留在 P3d-2 与 DeliveryPlane 合并时做。
func (v *v3Split) offerTo(ctx context.Context, kind uint32, cid uint64, uids []uint64,
	requestID string, payload []byte, msgID, seq int64) {
	if v == nil || v.offer == nil || !v3Eligible(kind) || len(uids) == 0 {
		return
	}
	if msgID == 0 && seq == 0 {
		msgID, seq = v3PayloadIdentity(kind, payload)
	}
	for _, uid := range uids {
		if uid == 0 {
			continue
		}
		v.stBroadcast.Add(1)
		f := V3Frame{
			Kind: kind, ConvID: cid, UserID: uid, RequestID: requestID,
			Payload: payload, MsgID: msgID, Seq: seq,
		}
		env := &chatpb.Envelope{Kind: kind, RequestId: requestID, Payload: payload}
		env.SetUserId(uid) // 与 installEmit 的逐成员寻址形态一致
		b, err := env.Marshal()
		if err != nil {
			v.stFailed.Add(1)
			continue
		}
		f.Blob = b
		in, err := v.offer(ctx, f)
		if err != nil {
			v.stFailed.Add(1)
			continue
		}
		if in {
			v.stOffered.Add(1)
		} else {
			v.stBypassed.Add(1)
		}
	}
}

// v3PayloadIdentity 从下行 payload 提取 msg_id / seq / latestSeq 作为去重键；
// 都没有时退化为 payload 的 FNV-1a（同一帧重投稳定命中去重 LRU）。
func v3PayloadIdentity(kind uint32, payload []byte) (int64, int64) {
	if kind == uint32(kinds.KindWatermark) {
		var w struct {
			LatestSeq uint64 `json:"latestSeq"`
		}
		if json.Unmarshal(payload, &w) == nil && w.LatestSeq > 0 {
			return 0, int64(w.LatestSeq)
		}
	}
	var p struct {
		MsgID uint64 `json:"msg_id"`
		Seq   uint64 `json:"seq"`
	}
	if json.Unmarshal(payload, &p) == nil && (p.MsgID > 0 || p.Seq > 0) {
		return int64(p.MsgID), int64(p.Seq)
	}
	h := fnv.New64a()
	_, _ = h.Write(payload)
	return 0, int64(h.Sum64())
}
