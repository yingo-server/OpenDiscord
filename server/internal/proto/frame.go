// Package proto 定义桥接层流通货币 Frame 及其只读元数据。
//
// 这是流水线级间唯一契约（docs/bridge-design.md §2.3）：
// 任何一级只接受 chan *Frame，任何一级可独立替换/旁路/内嵌探针。
package proto

import (
	"time"

	chatpb "github.com/yingo-server/UFOT/server/internal/proto/gen/chatpb"
)

// Frame 是全流水线唯一流通货币（定义在本包，FrameRW 产出，Egress 消费）。
//
// 只读约定：Frame 自进入 FrameRW 起，其 Meta 与 Env 的路由字段
// （id/kind/request_id/trace 上下文）在全程流转中**只读**；
// 任何组件不得原地改写入队时间戳或入口连接标识——需要派生新帧时
// 必须复制（Clone）出新 Frame，原帧所有权属于产出它的组件。
type Frame struct {
	Seq  uint32           // 连接内传输 seq（AckTracker 确认/重发单位）
	Kind uint32           // 冗余缓存 Envelope.Kind，Router 查表免解 payload
	Env  *chatpb.Envelope // 业务帧本体（payload 按 kind 二次解码）
	Meta *FrameMeta       // FrameRW 打上，全程只读

	// PreEncoded is the optional, immutable, recipient-independent encoded
	// envelope body (kind + request_id + payload) produced ONCE by the data
	// layer fan-out (perf-spec v1.2 §2.1). When set, the delivery layer writes
	// it verbatim after appending the per-recipient target field, instead of
	// re-serialising Env per member. Always READ-ONLY; shared by reference
	// across many frames. nil on the classic single-recipient path.
	PreEncoded []byte
}

// FrameMeta 是帧的入界快照：trace_id、入口连接、入队时间戳。
// FrameRW 在解码完成后一次性打上；下游全部只读。
type FrameMeta struct {
	TraceID    string    // 前端生成，journalctl -g 直接 grep 的全链路标识
	SrcLayer   string    // 入口层：user / data / admin / bridge
	SrcNode    string    // 入口实例标识
	ConnID     uint64    // 入口连接 id（回程投递的透传上下文）
	EnqueuedAt time.Time // FrameRW 解码完成时刻（水位/滞留时延计算基准）
}

// Clone 复制出可安全改写的新 Frame（浅拷贝 Env/Meta 指针以外的字段），
// 需要修改 Env 或 Meta 时调用方必须先深拷贝对应对象。
func (f *Frame) Clone() *Frame {
	if f == nil {
		return nil
	}
	nf := *f
	if f.Meta != nil {
		m := *f.Meta
		nf.Meta = &m
	}
	return &nf
}

// Reject 表示一次转发被拒绝（Router 无路由 / Verifier 能力外 / 目标层不可达）。
// 连接级错误断连接，进程级错误崩进程；Reject 是"降级"路径的统一表达。
type Reject struct {
	Kind   uint32 // 被拒帧的 kind
	Code   uint32 // 拒绝码（NO_ROUTE / NO_CAPABILITY / TARGET_DOWN …）
	Reason string // 人类可读原因，进日志与 NACK 帧
}

// Error 实现 error 接口。
func (r Reject) Error() string {
	return "reject kind=" + itoa(r.Kind) + " code=" + itoa(r.Code) + ": " + r.Reason
}

func itoa(v uint32) string {
	if v == 0 {
		return "0"
	}
	var buf [10]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
