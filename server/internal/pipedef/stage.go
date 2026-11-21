// Package pipedef 定义流水线级间契约（docs/bridge-design.md §2.3）：
// 级间只有 chan *proto.Frame——任何一级可独立替换/旁路/内嵌探针。
package pipedef

import (
	"context"
	"io"

	"github.com/yingo-server/UFOT/server/internal/proto"
)

// FrameCh 全流水线级间唯一流通货币。
type FrameCh = chan *proto.Frame

// Stage 流水线中的一级：Run 消费输入、产出输出，ctx 取消即返回。
// Name 用于日志 component 注入（log.For(stage.Name())）。
type Stage interface {
	Run(ctx context.Context) error
	Name() string
}

// InOut Stage 的通用组合物：任何持有 in/out channel 的级可嵌入它。
type InOut struct {
	In  FrameCh
	Out FrameCh
}

// TeeStage 探针级：把流经的每一帧原样转发到下一级，同时复制一份
// 到 Sink（io.Writer 或回调），供测试/旁路观测。不改帧、不产生背压语义
// （写 Writer 失败仅丢弃副本，不影响主流水线）。
type TeeStage struct {
	InOut
	Sink interface{} // func(*proto.Frame) 或 io.Writer
}

func (s *TeeStage) Name() string { return "tee" }

func (s *TeeStage) Run(ctx context.Context) error {
	if s.In == nil || s.Out == nil {
		return nil
	}
	// A passthrough stage owns Out: downstream ranges until it is closed, so
	// every return path (In closed / ctx done) closes it. Without this the
	// pipeline would stall forever on shutdown.
	defer close(s.Out)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case f, ok := <-s.In:
			if !ok {
				return nil
			}
			s.emit(f)
			select {
			case s.Out <- f:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

func (s *TeeStage) emit(f *proto.Frame) {
	switch sink := s.Sink.(type) {
	case func(*proto.Frame):
		if sink != nil {
			sink(f)
		}
	case io.Writer:
		if sink != nil {
			s.writeFrame(sink, f)
		}
	}
}

func (s *TeeStage) writeFrame(w io.Writer, f *proto.Frame) {
	// 文本可观测：一行一帧的简要摘要（trace_id + kind + seq）。
	defer func() { _ = recover() }() // Writer 故障不影响主流水线
	line := "frame"
	if f != nil && f.Meta != nil && f.Meta.TraceID != "" {
		line = "frame trace_id=" + f.Meta.TraceID
	}
	_, _ = io.WriteString(w, line+"\n")
}

// EchoStage 测试用回投级：把输入帧原样送回指定的反馈 channel
// （不入主流水线出向），用于压测环回。
type EchoStage struct {
	InOut
	// Back 回投目标；为 nil 时行为等同直通。
	Back FrameCh
}

func (s *EchoStage) Name() string { return "echo" }

func (s *EchoStage) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case f, ok := <-s.In:
			if !ok {
				return nil
			}
			if s.Back != nil {
				select {
				case s.Back <- f:
				case <-ctx.Done():
					return ctx.Err()
				default: // 回投 channel 满即丢弃（测试探针不背压）
				}
			}
			if s.Out != nil {
				select {
				case s.Out <- f:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
	}
}

// Target 是路由/投递目标的级间契约：层 + bus topic。
// 定义在契约层（非兄弟组件包），router 产出、egress 消费，
// 二者不得互相 import（CI import-lint）。
type Target struct {
	Layer string
	Topic string
}
