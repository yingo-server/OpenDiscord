// Package transport 定义层间传输的统一 Peer 抽象
// （docs/bridge-design.md §2/§4、docs/user-frontend-design.md §1）。
// 两种实现：
//   - embedded/：进程内 chan *Frame 直连（embedded 模式把 FrameRW 换成内存队列）
//   - remote/：UDS/TCP + BUS_SOCKET 帧编解码 + HMAC 握手 + 重连
package transport

import (
	"github.com/yingo-server/UFOT/server/internal/proto"
)

// Peer 是任意一层（user/data/admin/…）接入桥接的统一原语。
// 发送方向：Send 把帧写入对端；接收方向：Frames 返回接收流。
// Send 在连接关闭/断开后返回错误，不 panic（crash-only 由上层决定何时退出）。
type Peer interface {
	// Send 发送一帧到对端。实现必须可并发安全调用。
	Send(*proto.Frame) error
	// Frames 返回接收帧流；Peer 关闭后该 channel 关闭。
	Frames() <-chan *proto.Frame
	// Close 释放底层资源。幂等：第二次调用返回 nil。
	Close() error
	// Layer 返回本端所属层标识（"user"/"data"/"admin"/"bridge"…），
	// 供 HELLO 握手与日志 component 使用。
	Layer() string
}

// Layer 是嵌入式桥接的接入方：层内模块通过它收发帧。
// NewBridge(l Layer) 会把两端 Peer 接到进程内 bus。
type Layer interface {
	// LayerName 层标识。
	LayerName() string
	// Deliver 投递一帧到层内（下行方向）。
	Deliver(*proto.Frame) error
	// Outbound 返回层内待上行帧的流（上行方向）。
	Outbound() <-chan *proto.Frame
	// Close 通知层停止。
	Close() error
}
