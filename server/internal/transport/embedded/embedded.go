// Package embedded 实现进程内 EmbeddedTransport（docs/bridge-design.md §4）：
// embedded 模式 = 把 FrameRW 换成内存队列（双向 unbuffered chan 对），
// 其余流水线照跑。本质是一对无缓冲 channel + 生命周期管理。
package embedded

import (
	"errors"
	"sync"
	"time"

	"github.com/yingo-server/UFOT/server/internal/proto"
	"github.com/yingo-server/UFOT/server/internal/transport"
)

var _ = transport.Peer(nil) // 接口契约锚点

// ErrClosed 对端已关闭。
var ErrClosed = errors.New("embedded transport: closed")

// link 一条层⇄总线的双向无缓冲管道。
type link struct {
	layer   string
	toBus   chan *proto.Frame // 层 → 总线
	toLayer chan *proto.Frame // 总线 → 层

	mu     sync.Mutex
	closed bool
	doneCh chan struct{}
}

func (lk *link) close() {
	lk.mu.Lock()
	defer lk.mu.Unlock()
	if !lk.closed {
		lk.closed = true
		close(lk.doneCh)
		close(lk.toLayer)
	}
}

func (lk *link) done() <-chan struct{} {
	lk.mu.Lock()
	defer lk.mu.Unlock()
	return lk.doneCh
}

// Bridge 是进程内总线的总线端句柄。
type Bridge struct {
	mu     sync.Mutex
	closed bool
	// Attach 记录最后接入的 Peer。
	lastPeerMu sync.Mutex
	lastPeer   transport.Peer
	links      []*link
	fanin      chan *proto.Frame // 所有层上行帧的汇聚流
}

// NewBridge 构造总线端。l 为初始接入层（可传 nil，之后用 Attach 追加）。
func NewBridge(l transport.Layer) *Bridge {
	b := &Bridge{fanin: make(chan *proto.Frame)}
	if l != nil {
		b.Attach(l)
	}
	return b
}

// Attach 把一个层接到总线：启动层端泵 goroutine（层 Outbound→toBus、
// toLayer→层 Deliver），并返回该层的 Peer 端。每个 link 一条泵 goroutine
// 把 toBus 搬运到 fanin，无需显式 Pump。
func (b *Bridge) Attach(l transport.Layer) transport.Peer {
	lk := &link{
		layer:   l.LayerName(),
		toBus:   make(chan *proto.Frame),
		toLayer: make(chan *proto.Frame),
		doneCh:  make(chan struct{}),
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		lk.close()
		return &layerPeer{lk: lk}
	}
	b.links = append(b.links, lk)
	b.mu.Unlock()
	b.lastPeerMu.Lock()
	b.lastPeer = &layerPeer{lk: lk}
	b.lastPeerMu.Unlock()

	// 泵 1：层上行 → 总线 fanin
	go func() {
		for {
			select {
			case f, ok := <-lk.toBus:
				if !ok {
					return
				}
				select {
				case b.fanin <- f:
				case <-lk.done():
					return
				}
			case <-lk.done():
				return
			}
		}
	}()
	// 泵 2：总线下行 → 层 Deliver
	go func() {
		for {
			select {
			case f, ok := <-lk.toLayer:
				if !ok {
					_ = l.Close()
					return
				}
				_ = l.Deliver(f)
			case <-lk.done():
				return
			}
		}
	}()
	// 泵 3：层 Outbound → toBus
	go func() {
		for {
			select {
			case f, ok := <-l.Outbound():
				if !ok {
					lk.close()
					return
				}
				select {
				case lk.toBus <- f:
				case <-lk.done():
					return
				}
			case <-lk.done():
				return
			}
		}
	}()

	return &layerPeer{lk: lk}
}

// lastAttach 返回最近一次 Attach 的层端 Peer（测试辅助）。
func (b *Bridge) lastAttach() transport.Peer {
	b.lastPeerMu.Lock()
	defer b.lastPeerMu.Unlock()
	return b.lastPeer
}

// Pump 兼容占位：新版 Attach 内置泵 goroutine，无需外部调用。
// 保留空方法以兼容旧装配代码。
func (b *Bridge) Pump() {}

// Peer 返回总线自身的 Peer 形态（供 pipeline 装配）：
// Send=向所有层扇出（非阻塞，层慢即丢弃——bus 不做背压决策），
// Frames=层上行帧汇聚流。
func (b *Bridge) Peer() transport.Peer { return &busPeer{b: b} }

// Close 关闭所有 link。幂等。
func (b *Bridge) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	for _, lk := range b.links {
		lk.close()
	}
	return nil
}

// ---- 总线端 Peer ----

// fanoutGrace bounds how long the in-process bus waits for one layer before
// treating it as slow and dropping the frame.
const fanoutGrace = 100 * time.Millisecond

type busPeer struct{ b *Bridge }

func (p *busPeer) Layer() string { return "bridge" }

func (p *busPeer) Send(f *proto.Frame) error {
	p.b.mu.Lock()
	links := append([]*link(nil), p.b.links...)
	closed := p.b.closed
	p.b.mu.Unlock()
	if closed {
		return ErrClosed
	}
	for _, lk := range links {
		select {
		case lk.toLayer <- f:
		case <-lk.done():
		case <-time.After(fanoutGrace): // 层消费慢（超过宽限）→ 丢弃（bus 不做背压决策）
		}
	}
	return nil
}

func (p *busPeer) Frames() <-chan *proto.Frame { return p.b.fanin }

func (p *busPeer) Close() error { return p.b.Close() }

// ---- 层端 Peer ----

type layerPeer struct{ lk *link }

func (p *layerPeer) Layer() string { return p.lk.layer }

func (p *layerPeer) Send(f *proto.Frame) error {
	select {
	case p.lk.toBus <- f:
		return nil
	case <-p.lk.done():
		return ErrClosed
	}
}

func (p *layerPeer) Frames() <-chan *proto.Frame { return p.lk.toLayer }

func (p *layerPeer) Close() error {
	p.lk.close()
	return nil
}

// SendTo 向指定层投递一帧（按层名选择性发送）。
// 用于桥接流水线 Egress 按 Target.Layer 精确下发——busPeer.Send 是
// 全层扇出，无法区分目标层。层不存在返回 ErrNoLayer。
var ErrNoLayer = errors.New("embedded transport: no such layer")

func (b *Bridge) SendTo(layer string, f *proto.Frame) error {
	b.mu.Lock()
	links := append([]*link(nil), b.links...)
	closed := b.closed
	b.mu.Unlock()
	if closed {
		return ErrClosed
	}
	for _, lk := range links {
		if lk.layer != layer {
			continue
		}
		select {
		case lk.toLayer <- f:
			return nil
		case <-lk.done():
			return ErrClosed
		case <-time.After(fanoutGrace):
			// toLayer 是无缓冲 channel：裸 default 会把「泵恰好还没回到接收点」
			// 的正常帧误判成满队列直接丢弃（装配实测：MSG_SEND 偶发丢失）。
			// 与 busPeer.Send 一样给一个宽限窗口，超过才判 BUSY。
			return ErrLayerBusy
		}
	}
	return ErrNoLayer
}

// ErrLayerBusy 目标层消费慢（队列满）。
var ErrLayerBusy = errors.New("embedded transport: layer queue full")
