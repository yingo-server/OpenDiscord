// Package framerw 是流水线第 1 级：连接读写循环 + FrameMeta 打点
// （docs/bridge-design.md §2.2）。唯一接触传输的一级。
//
// 禁令：不看 kind 语义、不鉴权、不限流、不路由、不解析 payload。
package framerw

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/yingo-server/UFOT/server/internal/pkg/log"
	"github.com/yingo-server/UFOT/server/internal/proto"
	"github.com/yingo-server/UFOT/server/internal/transport"
)

var lg = log.For("framerw")

// FrameRW 持有一个 Peer 的读写循环。
// 读侧：Frames() → 打 FrameMeta → Out；写侧：Write → Peer.Send。
type FrameRW struct {
	Peer transport.Peer

	Out chan *proto.Frame // 打完 Meta 的入站帧流（下游 Verifier）

	// ConnID 本连接在 SessionManager 的编号（装配方注入，0=未注册）。
	ConnID uint64

	closeOnce chan struct{}
}

// New 构造。
func New(p transport.Peer, out chan *proto.Frame) *FrameRW {
	return &FrameRW{Peer: p, Out: out, closeOnce: make(chan struct{})}
}

// Run 读循环：Peer.Frames() → 打 Meta → 投 Out。Peer 关闭即返回 nil。
func (r *FrameRW) Run(ctx context.Context) error {
	src := r.Peer
	in := src.Frames()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.closeOnce:
			return nil
		case f, ok := <-in:
			if !ok {
				return nil
			}
			stampMeta(f, src.Layer(), r.ConnID)
			select {
			case r.Out <- f:
			case <-ctx.Done():
				return ctx.Err()
			case <-r.closeOnce:
				return nil
			}
		}
	}
}

// Write 写侧：序列化（Peer.Send 内部完成编码）发送一帧。
func (r *FrameRW) Write(f *proto.Frame) error {
	return r.Peer.Send(f)
}

// Close 停止读写循环（Peer 本身由装配方管理生命周期）。
func (r *FrameRW) Close() {
	select {
	case <-r.closeOnce:
	default:
		close(r.closeOnce)
	}
	_ = r.Peer.Close()
}

// stampMeta 打入界快照。TraceID 从 Envelope.request_id 提取，
// 缺失则生成 16 字节随机 hex。
func stampMeta(f *proto.Frame, srcLayer string, connID uint64) {
	if f.Meta == nil {
		f.Meta = &proto.FrameMeta{}
	}
	if f.Meta.TraceID == "" {
		if f.Env != nil && f.Env.GetRequestId() != "" {
			f.Meta.TraceID = f.Env.GetRequestId()
		} else {
			f.Meta.TraceID = newTraceID()
		}
	}
	if f.Meta.SrcLayer == "" {
		f.Meta.SrcLayer = srcLayer
	}
	if f.Meta.ConnID == 0 {
		f.Meta.ConnID = connID
	}
	f.Meta.EnqueuedAt = time.Now()
}

func newTraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().Format("20060102T150405.000000000")
	}
	return hex.EncodeToString(b[:])
}

var _ = lg.Info // 保留组件 logger 锚点
