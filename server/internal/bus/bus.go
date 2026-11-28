// Package bus 实现 topic 订阅 + 扇出（docs/bridge-design.md §2.2）。
//
// 禁令：不落盘、不去重、不背压决策——慢消费者由 Egress 负责；
// bus 侧 chan 容量固定 1024，满了 drop + 计数。
//
// 语义：发布只支持精确 topic（如 conv.123）；订阅支持 `>` 尾通配
// （如 sys.>、conv.>）。无锁分片：16 片按 topic hash。
package bus

import (
	"hash/fnv"
	"sync"
	"sync/atomic"

	"github.com/yingo-server/UFOT/server/internal/proto"
)

// ChanCap 每订阅者 channel 容量；满即 drop（Egress 负责真正的背压）。
const ChanCap = 1024

const shardCount = 16

// Stats 快照。
type Stats struct {
	Subs        int    // 精确 topic 订阅数
	WildSubs    int    // 尾通配订阅数
	Published   uint64 // Pub 调用次数
	Delivered   uint64 // 成功投递次数
	DroppedSlow uint64 // 慢消费者 drop 计数
}

// Bus topic 订阅表。零值不可用，用 New。
type Bus struct {
	shards [shardCount]shard
	wild   wildRegistry // `>` 尾通配单独广播列表

	// 计数器以独立字段 + atomic 访问。
	statsPublished uint64
	statsDelivered uint64
	statsDropped   uint64
}

type shard struct {
	mu   sync.Mutex
	subs map[string][]*subscriber
}

type wildRegistry struct {
	mu   sync.Mutex
	subs []*wildSubscriber
}

type subscriber struct {
	ch    chan *proto.Frame
	layer string
}

type wildSubscriber struct {
	prefix string // pattern 去掉尾 ">"，保留尾部 "."，如 "sys."
	sub    *subscriber
}

// New 构造。
func New() *Bus {
	b := &Bus{}
	for i := range b.shards {
		b.shards[i].subs = map[string][]*subscriber{}
	}
	return b
}

func (b *Bus) shardFor(topic string) *shard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(topic))
	return &b.shards[h.Sum32()%shardCount]
}

// Sub 订阅 pattern（精确 topic 或以 ".>" 结尾的尾通配）。
// layer 仅用于归属标识。返回订阅流与退订函数；退订后 channel 不关闭
// （bus 不保证关闭语义，调用方以 ctx/unsub 结束消费即可）。
func (b *Bus) Sub(pattern, layer string) (<-chan *proto.Frame, func()) {
	ch := make(chan *proto.Frame, ChanCap)
	sub := &subscriber{ch: ch, layer: layer}
	if prefix, ok := trimWildcard(pattern); ok {
		b.wild.mu.Lock()
		ws := &wildSubscriber{prefix: prefix, sub: sub}
		b.wild.subs = append(b.wild.subs, ws)
		b.wild.mu.Unlock()
		return ch, func() {
			b.wild.mu.Lock()
			for i, s := range b.wild.subs {
				if s == ws {
					b.wild.subs = append(b.wild.subs[:i], b.wild.subs[i+1:]...)
					break
				}
			}
			b.wild.mu.Unlock()
		}
	}
	sh := b.shardFor(pattern)
	sh.mu.Lock()
	sh.subs[pattern] = append(sh.subs[pattern], sub)
	sh.mu.Unlock()
	return ch, func() {
		sh.mu.Lock()
		list := sh.subs[pattern]
		for i, s := range list {
			if s == sub {
				sh.subs[pattern] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(sh.subs[pattern]) == 0 {
			delete(sh.subs, pattern)
		}
		sh.mu.Unlock()
	}
}

// trimWildcard 若 pattern 以 ".>" 结尾返回前缀（含尾部 "."）。
func trimWildcard(pattern string) (string, bool) {
	if len(pattern) > 2 && pattern[len(pattern)-1] == '>' && pattern[len(pattern)-2] == '.' {
		return pattern[:len(pattern)-1], true
	}
	return "", false
}

// Pub 向所有匹配订阅者非阻塞投递一帧到精确 topic。
// 精确订阅者查本 topic 分片；尾通配订阅者扫全局广播列表。
func (b *Bus) Pub(topic string, f *proto.Frame) {
	if f == nil {
		return
	}
	atomic.AddUint64(&b.statsPublished, 1)

	sh := b.shardFor(topic)
	sh.mu.Lock()
	subs := append([]*subscriber(nil), sh.subs[topic]...)
	sh.mu.Unlock()
	for _, s := range subs {
		b.offer(s, f)
	}

	b.wild.mu.Lock()
	wilds := append([]*wildSubscriber(nil), b.wild.subs...)
	b.wild.mu.Unlock()
	for _, w := range wilds {
		if len(topic) > len(w.prefix) && topic[:len(w.prefix)] == w.prefix {
			b.offer(w.sub, f)
		}
	}
}

// offer 非阻塞投递；满即 drop + 计数。
func (b *Bus) offer(s *subscriber, f *proto.Frame) {
	select {
	case s.ch <- f:
		atomic.AddUint64(&b.statsDelivered, 1)
	default:
		atomic.AddUint64(&b.statsDropped, 1)
	}
}

// Stats 返回快照。
func (b *Bus) Stats() Stats {
	st := Stats{
		Published:   atomic.LoadUint64(&b.statsPublished),
		Delivered:   atomic.LoadUint64(&b.statsDelivered),
		DroppedSlow: atomic.LoadUint64(&b.statsDropped),
	}
	for i := range b.shards {
		b.shards[i].mu.Lock()
		st.Subs += len(b.shards[i].subs)
		b.shards[i].mu.Unlock()
	}
	b.wild.mu.Lock()
	st.WildSubs = len(b.wild.subs)
	b.wild.mu.Unlock()
	return st
}
