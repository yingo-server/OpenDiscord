package bridge

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yingo-server/UFOT/server/internal/pkg/log"
	"github.com/yingo-server/UFOT/server/internal/proto"
	"github.com/yingo-server/UFOT/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/UFOT/server/internal/transport"
)

// ErrQueueFull 降级队列已满，帧被丢弃（内存上限保护）。
var ErrQueueFull = errors.New("bridge: retry queue full")

// UpstreamFrame 是 router 交给 forwarder 的一帧内部帧（已 Marshal）。
type UpstreamFrame struct {
	// Kind 冗余 kind（线路头）。
	Kind uint32
	// TraceID 前端生成的链路 id。
	TraceID string
	// ConnID 入口连接 id。
	ConnID uint64
	// Body 已 Marshal 的 chatpb.InternalFrame 字节。
	Body []byte
}

// Forwarder 是 usergw 到桥接的唯一一条 Peer 管道（docs §1.1 forwarder）。
//
//   - 上行 goroutine：Upstream → peer.Send（失败进降级重试队列，≤10k，满则丢帧计数）
//   - 下行 goroutine：peer.Frames → Downstream（Peer 重连后自动切到新 Frames 流）
//   - 断线重连成功 / 流关闭时向 Resync 发信号，router 据此向全部连接打 RESUME 补拉标记
//
// 明确不做：不解析 Envelope 业务字段（只透传字节）。
type Forwarder struct {
	peer   transport.Peer
	logger *log.Logger

	// Upstream router→forwarder。
	Upstream chan *UpstreamFrame
	// Downstream forwarder→router（桥接下行帧）。
	Downstream chan *proto.Frame
	// Resync 重连成功信号（容量 1，合并重复信号）。
	Resync chan struct{}

	maxRetry int

	mu      sync.Mutex
	retry   []*UpstreamFrame
	dropped atomic.Uint64
	sent    atomic.Uint64
	recv    atomic.Uint64
}

// NewForwarder 构造。maxRetry ≤0 时取 10000（docs §1.1 上限）。
func NewForwarder(peer transport.Peer, logger *log.Logger, maxRetry int) *Forwarder {
	if maxRetry <= 0 {
		maxRetry = 10000
	}
	return &Forwarder{
		peer:       peer,
		logger:     logger,
		Upstream:   make(chan *UpstreamFrame, 1024),
		Downstream: make(chan *proto.Frame, 1024),
		Resync:     make(chan struct{}, 1),
		maxRetry:   maxRetry,
	}
}

// Send 入队一帧（非阻塞；Upstream 满则丢弃并计数）。
func (f *Forwarder) Send(fr *UpstreamFrame) bool {
	select {
	case f.Upstream <- fr:
		return true
	default:
		f.dropped.Add(1)
		return false
	}
}

// Run 启动上下行两个 goroutine，ctx 取消后退出。
func (f *Forwarder) Run(ctx context.Context) {
	go f.runUpstream(ctx)
	go f.runDownstream(ctx)
}

// toProtoFrame 把内部帧字节包装成 transport 层 Frame。
func (f *Forwarder) toProtoFrame(fr *UpstreamFrame) *proto.Frame {
	return &proto.Frame{
		Kind: fr.Kind,
		Env:  &chatpb.Envelope{Kind: fr.Kind, Payload: fr.Body},
		Meta: &proto.FrameMeta{TraceID: fr.TraceID, SrcLayer: "user", ConnID: fr.ConnID},
	}
}

// runUpstream 上行：消费 Upstream，并周期性重试降级队列。
func (f *Forwarder) runUpstream(ctx context.Context) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case fr := <-f.Upstream:
			f.deliver(fr)
		case <-ticker.C:
			f.flushRetry()
		}
	}
}

// deliver 尝试发送；失败入降级队列。
func (f *Forwarder) deliver(fr *UpstreamFrame) {
	if fr == nil {
		return
	}
	if err := f.peer.Send(f.toProtoFrame(fr)); err != nil {
		if f.logger != nil {
			f.logger.Debug("bridge send failed, queued", "kind", fr.Kind, "err", err.Error())
		}
		f.enqueueRetry(fr)
		return
	}
	f.sent.Add(1)
}

// flushRetry 重试降级队列（连接恢复后按序补发；中途失败则保留剩余）。
func (f *Forwarder) flushRetry() {
	f.mu.Lock()
	batch := f.retry
	f.retry = nil
	f.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	var remain []*UpstreamFrame
	for i, fr := range batch {
		if err := f.peer.Send(f.toProtoFrame(fr)); err != nil {
			remain = append(remain, batch[i:]...)
			break
		}
		f.sent.Add(1)
	}
	if len(remain) > 0 {
		f.mu.Lock()
		f.retry = append(remain, f.retry...)
		f.mu.Unlock()
		return
	}
	f.signalResync() // 排空成功说明桥接已恢复
}

// enqueueRetry 入降级队列；超上限丢弃最旧帧（内存保护优先，保留新帧）。
func (f *Forwarder) enqueueRetry(fr *UpstreamFrame) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.retry) >= f.maxRetry {
		copy(f.retry, f.retry[1:])
		f.retry[len(f.retry)-1] = fr
		f.dropped.Add(1)
		return
	}
	f.retry = append(f.retry, fr)
}

// runDownstream 下行：peer.Frames() → Downstream；Peer 断线后重新取流。
func (f *Forwarder) runDownstream(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		ch := f.peer.Frames()
		if ch == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
				continue
			}
		}
		gotAny := false
		for fr := range ch {
			gotAny = true
			f.recv.Add(1)
			select {
			case f.Downstream <- fr:
			case <-ctx.Done():
				return
			}
		}
		if gotAny {
			// 流关闭（桥接断开）：通知 router 触发 RESUME 补拉标记。
			f.signalResync()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (f *Forwarder) signalResync() {
	select {
	case f.Resync <- struct{}{}:
	default:
	}
}

// Drain 等待降级队列与上行队列排空（≤ timeout），返回是否排空。
func (f *Forwarder) Drain(ctx context.Context, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if f.QueueDepth() == 0 && len(f.Upstream) == 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-tick.C:
			f.flushRetry()
		}
	}
}

// QueueDepth 降级队列当前长度。
func (f *Forwarder) QueueDepth() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.retry)
}

// Stats 观测快照。
type Stats struct {
	Sent    uint64
	Recv    uint64
	Dropped uint64
	Retry   int
}

// Stats 返回统计。
func (f *Forwarder) Stats() Stats {
	return Stats{Sent: f.sent.Load(), Recv: f.recv.Load(), Dropped: f.dropped.Load(), Retry: f.QueueDepth()}
}
