// hub.go —— MailboxHub：per-uid 单写者 + 去重 LRU + head 跟踪（v3-bus-design §2）。
//
// 规则：
//   - 每 uid 恰好一个单写者（per-uid mutex），mailbox_seq 分配与追加同锁完成（§2.2）；
//   - 去重：delivery_key = hash(uid, kind, msg_id|seq) 的 LRU（4096/uid），
//     fanout 重试/bridge 重投不产生重复条目；
//   - flag v3_bus_enabled=false 时 Offer 为 no-op（§6 灰度中间态，旧路径唯一投递路径）；
//   - 内存只驻留 head 差量，SQLite 是权威态（§8）。
package mailbox

import (
	"context"
	"encoding/binary"
	"hash/fnv"
	"sync"

	"github.com/yingo-server/UFOT/server/internal/pkg/featureflags"
)

// Envelope 是 hub 收到的下行帧的抽象形态。hub 层不解释 proto——只需要
// 去重键的三要素（§2.2：hash(uid, kind, msg_id|seq)）与序列化字节。
type Envelope struct {
	Kind  int    // 102/103/104/105/14/20x/…（kinds.go 号段）
	MsgID int64  // 消息帧的 msg_id；非消息帧填 0
	Seq   int64  // 消息帧的 conv seq；非消息帧填 0（msg_id|seq 二选一有效）
	Blob  []byte // proto 编码后的原始帧
}

// DedupKey 计算 delivery_key（FNV-1a 64，与 featureflags 灰度分桶同族散列）。
func (e Envelope) DedupKey(uid int64) uint64 {
	h := fnv.New64a()
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(uid))
	_, _ = h.Write(buf[:])
	binary.LittleEndian.PutUint32(buf[:4], uint32(e.Kind))
	_, _ = h.Write(buf[:4])
	binary.LittleEndian.PutUint64(buf[:], uint64(e.MsgID))
	_, _ = h.Write(buf[:])
	binary.LittleEndian.PutUint64(buf[:], uint64(e.Seq))
	_, _ = h.Write(buf[:])
	return h.Sum64()
}

// dedupLRU 是定容去重集合（容量 4096/uid，§2.2）。O(1) 命中/淘汰，无锁——
// 每个实例由该 uid 的单写者独占访问。
type dedupLRU struct {
	cap   int
	order []uint64 // LRU 淘汰序（近似：满时整体淘汰最早一半，均摊 O(1)）
	live  map[uint64]struct{}
}

func newDedupLRU(capacity int) *dedupLRU {
	if capacity <= 0 {
		capacity = 4096
	}
	return &dedupLRU{cap: capacity, live: make(map[uint64]struct{}, capacity)}
}

// Seen 返回 key 是否已存在（不存在则登记）。
func (l *dedupLRU) Seen(key uint64) bool {
	if _, ok := l.live[key]; ok {
		return true
	}
	if len(l.live) >= l.cap {
		// 近似 LRU：批量淘汰前半段，均摊成本低于逐键双向链表，够用于去重语义
		//（重复投递窗口取淘汰批次尺寸，远小于容量）。
		half := l.cap / 2
		for _, k := range l.order[:half] {
			delete(l.live, k)
		}
		l.order = l.order[half:]
	}
	l.live[key] = struct{}{}
	l.order = append(l.order, key)
	return false
}

// userMailbox 是单写者状态机（同一 uid 的全部访问持 mu）。
type userMailbox struct {
	mu     sync.Mutex
	head   int64 // 本 uid 已分配的最大 mailbox_seq（内存视图，重启由 Store.Head 恢复）
	cursor int64 // 生效游标（max(105 累计, 15.cursor)，重启由 mailbox_cursors 恢复）
	dedup  *dedupLRU
}

// HubConfig 是 hub 的机制参数（数值属策略，由组装层从 flag/policy 注入）。
type HubConfig struct {
	DedupCapacityPerUID int // 去重 LRU 容量，规范值 4096（§2.2）
}

// MailboxHub 是 v3 总线的投递真相源入口（§1.1）。
type MailboxHub struct {
	store   *Store
	flagsFn func() map[string]any // flag 快照读取器（911 热重载语义由上游保证）
	cfg     HubConfig

	mu    sync.Mutex // 保护 users map 本身（注册/摘除），不参与写入路径
	users map[int64]*userMailbox
}

// NewHub 构造 hub。flagsFn 允许 nil（视为全关——安全中间态）。
func NewHub(store *Store, flagsFn func() map[string]any, cfg HubConfig) *MailboxHub {
	if cfg.DedupCapacityPerUID <= 0 {
		cfg.DedupCapacityPerUID = 4096
	}
	return &MailboxHub{
		store:   store,
		flagsFn: flagsFn,
		cfg:     cfg,
		users:   make(map[int64]*userMailbox),
	}
}

// enabled 读当前灰度快照（§6：v3_bus_enabled 总开关）。
func (h *MailboxHub) enabled() bool {
	if h.flagsFn == nil {
		return false
	}
	return featureflags.BoolMap(h.flagsFn(), featureflags.KeyV3BusEnabled)
}

func (h *MailboxHub) user(uid int64) *userMailbox {
	h.mu.Lock()
	defer h.mu.Unlock()
	if u, ok := h.users[uid]; ok {
		return u
	}
	u := &userMailbox{dedup: newDedupLRU(h.cfg.DedupCapacityPerUID)}
	h.users[uid] = u
	return u
}

// Offer 追加一条下行帧到 uid 的 mailbox（§2.2 写入路径）。
// 返回分配的 mailbox_seq；去重命中时返回 (0, nil)——不重复追加。
// flag 关闭时返回 (0, nil)（no-op，旧路径直投）。
func (h *MailboxHub) Offer(ctx context.Context, uid int64, env Envelope) (int64, error) {
	if !h.enabled() {
		return 0, nil
	}
	u := h.user(uid)
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.dedup.Seen(env.DedupKey(uid)) {
		return 0, nil
	}
	seq, err := h.store.Append(ctx, uid, dedupKeyString(env.DedupKey(uid)), env.Blob)
	if err != nil {
		return 0, err
	}
	u.head = seq // 序号分配与追加在同一把锁内（§2.2 铁律）
	return seq, nil
}

// Head 返回 uid 当前内存视图的 head（分配过即单调；进程内不回退）。
func (h *MailboxHub) Head(uid int64) int64 {
	u := h.user(uid)
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.head
}

// Cursor 返回 uid 的生效游标（内存视图）。
func (h *MailboxHub) Cursor(uid int64) int64 {
	u := h.user(uid)
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.cursor
}

// Ack 累积推进游标（kind 15，§4.2）：游标单调前进（后退的 ACK 幂等忽略），
// 持久化交给 Store.MarkAcked（105/15 双源取大，崩溃可恢复）。
func (h *MailboxHub) Ack(ctx context.Context, uid, cursor int64) error {
	u := h.user(uid)
	u.mu.Lock()
	if cursor <= u.cursor {
		u.mu.Unlock()
		return nil // 幂等：乱序/重复 ACK 无害
	}
	u.cursor = cursor
	u.mu.Unlock()
	return h.store.MarkAcked(ctx, uid, cursor)
}

// Restore 从持久层重建某 uid 的 head/cursor 下界（重启路径，S-5：cursor 不回退）。
// 已知活跃 uid 的内存视图若更大则以内存为准（进程内单调）。
func (h *MailboxHub) Restore(ctx context.Context, uid int64) error {
	head, err := h.store.Head(ctx, uid)
	if err != nil {
		return err
	}
	cur, err := h.store.Cursor(ctx, uid)
	if err != nil {
		return err
	}
	u := h.user(uid)
	u.mu.Lock()
	defer u.mu.Unlock()
	if head > u.head {
		u.head = head
	}
	if cur > u.cursor {
		u.cursor = cur
	}
	return nil
}

// Stats 导出运行时指标快照（§5.5 Bus.BackpressureDepth 的后续接线点；P2 先供观测）。
type Stats struct {
	Users   int   // 活跃单写者数（近似 = 活跃 uid 数）
	Heads   int64 // 全部 uid head 之和（观测用，非精确总量）
	Cursors int64 // 全部 uid cursor 之和
}

func (h *MailboxHub) Stats() Stats {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := Stats{Users: len(h.users)}
	for _, u := range h.users {
		u.mu.Lock()
		st.Heads += u.head
		st.Cursors += u.cursor
		u.mu.Unlock()
	}
	return st
}

// dedupKeyString 把 uint64 delivery_key 定长编码为落盘兜底键（TEXT 列）。
func dedupKeyString(k uint64) string {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], k)
	return string(buf[:])
}
