// Package egress 是流水线末级背压/调度器（docs/bridge-design.md §2.2）：
// 每目标 layer 一个有界队列（容量 4096）+ 三档水位
// + kind 分级溢出策略。
//
// 禁令：不解析帧内容、不重试（重发是 AckTracker 的职责）。
package egress

import (
	"context"
	"sync"
	"time"

	"github.com/yingo-server/UFOT/server/internal/pipedef"
	"github.com/yingo-server/UFOT/server/internal/pkg/log"
	"github.com/yingo-server/UFOT/server/internal/proto"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
)

// 队列容量与默认水位（0 < soft < firm <= 1，可配）。
const (
	DefaultCap    = 4096
	DefaultSoft   = 0.6
	DefaultFirm   = 0.85
	framesChanCap = 256
)

// NACK 通过 Reject chan 上报（消息帧 firm 水位溢出 → AckTracker 判重发/判死）。
type Config struct {
	Cap  int
	Soft float64
	Firm float64
	// Rejects NACK 输出（可 nil，日志/观测用）。
	Rejects chan *proto.Reject
	// OnNack 消息帧 firm 水位 NACK 回调（装配方接 AckTracker）。
	OnNack func(f *proto.Frame)
}

// Egress 多目标调度器。
type Egress struct {
	cfg Config

	mu     sync.Mutex
	queues map[string]*queue

	out chan TaggedFrame // 统一出向流（目标 layer+topic 附带）
}

// TaggedFrame 出向流元素：帧 + 目标。
type TaggedFrame struct {
	T pipedef.Target
	F *proto.Frame
}

// ErrOverflow 目标队列满且帧不允许丢弃（消息帧）——NACK 语义。
type overflowErr struct{ layer string }

func (e overflowErr) Error() string { return "egress: firm watermark overflow on layer " + e.layer }

// New 构造。
func New(cfg Config) *Egress {
	if cfg.Cap <= 0 {
		cfg.Cap = DefaultCap
	}
	if cfg.Soft <= 0 {
		cfg.Soft = DefaultSoft
	}
	if cfg.Firm <= cfg.Soft {
		cfg.Firm = DefaultFirm
	}
	return &Egress{cfg: cfg, queues: map[string]*queue{}, out: make(chan TaggedFrame, framesChanCap)}
}

// Out 返回出向流：携带目标 layer/topic 的帧，由装配方接到各 Peer 写侧。
func (e *Egress) Out() <-chan TaggedFrame { return e.out }

// Enqueue 入队一帧到目标层队列。消息帧（100-199 号段）在 firm 水位
// 溢出时返回错误并投 NACK（调用方 AckTracker）；可丢帧直接丢弃。
// 心跳/ack/控制帧不可丢：满时仍须入队（若彻底满则返回错误，判连接死）。
func (e *Egress) Enqueue(t pipedef.Target, f *proto.Frame) error {
	e.mu.Lock()
	q := e.queues[t.Layer]
	if q == nil {
		q = newQueue(t.Layer, e.cfg)
		e.queues[t.Layer] = q
		go q.pump(context.Background(), e.out)
	}
	e.mu.Unlock()
	return q.enqueue(f)
}

// Stats 水位快照（SIGUSR1 dump 用）。
type Stats struct {
	Layer             string
	Len               int
	Cap               int
	DroppedBestEffort uint64
	NACKed            uint64
}

func (e *Egress) Stats() []Stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Stats, 0, len(e.queues))
	for l, q := range e.queues {
		q.mu.Lock()
		out = append(out, Stats{Layer: l, Len: len(q.pending), Cap: q.cfg.Cap,
			DroppedBestEffort: q.dropped, NACKed: q.nacked})
		q.mu.Unlock()
	}
	return out
}

// ---- 单目标队列 ----

type queue struct {
	layer string
	cfg   Config

	mu      sync.Mutex
	pending chan *proto.Frame // 主队列

	dropped uint64 // best-effort 帧被丢计数
	nacked  uint64 // 消息帧 NACK 计数
}

func newQueue(layer string, cfg Config) *queue {
	return &queue{layer: layer, cfg: cfg, pending: make(chan *proto.Frame, cfg.Cap)}
}

func (q *queue) soft() int { return int(float64(q.cfg.Cap) * q.cfg.Soft) }
func (q *queue) firm() int { return int(float64(q.cfg.Cap) * q.cfg.Firm) }

// enqueue 按帧类别施策。
func (q *queue) enqueue(f *proto.Frame) error {
	q.mu.Lock()
	qlen := len(q.pending)
	defer q.mu.Unlock()
	switch {
	case isNeverDrop(f.Kind):
		// 心跳/ACK/层间控制：不可丢。队列彻底满 → 判连接死。
		select {
		case q.pending <- f:
			return nil
		default:
			return overflowErr{layer: q.layer}
		}
	case isDropFirst(f.Kind):
		// typing/presence：soft 水位后先丢。
		if qlen >= q.soft() {
			q.dropped++
			return nil
		}
	default:
		// 消息帧：firm 水位 NACK。
		if qlen >= q.firm() {
			q.nacked++
			q.nack(f)
			return overflowErr{layer: q.layer}
		}
	}
	select {
	case q.pending <- f:
		return nil
	default:
		// 竞态窗口内满：按类别二次裁决。
		if isNeverDrop(f.Kind) {
			return overflowErr{layer: q.layer}
		}
		if !isDropFirst(f.Kind) {
			q.nacked++
			q.nack(f)
			return overflowErr{layer: q.layer}
		}
		q.dropped++
		return nil
	}
}

func (q *queue) nack(f *proto.Frame) {
	if q.cfg.OnNack != nil {
		q.cfg.OnNack(f)
	}
	if q.cfg.Rejects == nil {
		return
	}
	select {
	case q.cfg.Rejects <- &proto.Reject{Kind: f.Kind, Code: 3, Reason: "egress firm watermark on layer " + q.layer}:
	default:
	}
}

// DrainWithContext 在 ctx 内尽力排空所有队列并停止泵。
func (e *Egress) DrainWithContext(ctx context.Context) {
	e.mu.Lock()
	qs := make([]*queue, 0, len(e.queues))
	for _, q := range e.queues {
		qs = append(qs, q)
	}
	e.queues = map[string]*queue{}
	e.mu.Unlock()
	for _, q := range qs {
		q.drain(ctx, e.out)
	}
}

// Drain 排空所有队列（默认 5s 上限，SIGTERM 优雅退出用）。
func (e *Egress) Drain() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.DrainWithContext(ctx)
}

func (q *queue) drain(ctx context.Context, out chan TaggedFrame) {
	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-q.pending:
			if !ok {
				return
			}
			tf := TaggedFrame{T: pipedef.Target{Layer: q.layer, Topic: topicOf(f)}, F: f}
			select {
			case out <- tf:
			case <-ctx.Done():
				return
			}
		default:
			return
		}
	}
}

func (q *queue) pump(ctx context.Context, out chan TaggedFrame) {
	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-q.pending:
			if !ok {
				return
			}
			tf := TaggedFrame{T: pipedef.Target{Layer: q.layer, Topic: topicOf(f)}, F: f}
			select {
			case out <- tf:
			case <-ctx.Done():
				return
			}
		}
	}
}

// topicOf 从帧推导出向 topic（用于 bus 订阅侧对齐；不解析 payload）。
func topicOf(f *proto.Frame) string {
	if f == nil || f.Env == nil {
		return ""
	}
	if cid := f.Env.GetConvId(); cid != 0 {
		return "conv." + u64(cid)
	}
	if uid := f.Env.GetUserId(); uid != 0 {
		return "user." + u64(uid)
	}
	return "sys"
}

// isNeverDrop lists frames whose loss is a correctness or liveness bug rather
// than a degradation. MSG_ACK belongs here: the sending client is blocked on
// that round trip, so dropping it behind broadcast traffic turns a capacity
// problem into a silent "my message never sent" for the user. Measured before
// the fix: 128 connections in 50-member groups delivered only 67% of ACKs
// because the fan-out frames pushed the layer queue past its firm watermark.
func isNeverDrop(kind uint32) bool {
	return kind == kinds.KindInternalHeartbeat || kind == kinds.KindInternalFrameAck ||
		kind == kinds.KindMsgAck ||
		kind == kinds.KindPing || kind == kinds.KindPong || kind < 100 ||
		(kind >= 900 && kind < 910)
}

func isDropFirst(kind uint32) bool {
	return kind == kinds.KindTyping || (kind >= 400 && kind < 450)
}

func u64(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

var lg = log.For("egress")

var _ = lg.Warn
