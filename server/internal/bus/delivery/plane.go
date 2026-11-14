// Package delivery —— v3 消息总线 P3a：DeliveryPlane credit 流控
// （docs/v3-bus-design.md §3/§5/§7）。
//
// 规则（§3.2）：
//   - 每 conn(uid,dev) 独立 cursor/credit/inflight，多设备互不影响（§3.4）；
//   - credit 初始 512（flag: delivery_credit，经 Config 注入）；每发 1 条 credit--；
//     客户端批 ACK（kind 15 累积）→ credit += n；
//   - credit == 0 → conn PAUSED：停拉 mailbox 新条目（不丢、不标记）；
//     该 uid 全部 conn 均 PAUSED → BACKPRESSURED（Stats 暴露给上游聚合窗）；
//   - 保命帧（Ping 4/5、ERROR/KICKED 10）不占 credit 且 PAUSED 下仍可发（SendVital）；
//   - PAUSED 持续 > slow_consumer_close_s（默认 60s）→ ShouldClose=true（判死逃生）。
//
// 分层铁律：本包属 bus 层，只依赖 mailbox（同层 Entry 形态）与标准库；
// ACK 持久化不直接 import mailbox 写路径，经 AckFn 回调由组装层接到
// MailboxHub.Ack / Store.MarkAcked。mailbox.Store 天然满足 MailReader 接口，
// 但本包只依赖接口，便于 fake 测试（T-SV-2/3/7 不依赖真 SQLite）。
package delivery

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/yingo-server/OpenDiscord/server/internal/bus/mailbox"
)

// 默认值对应 §6 flag 表：delivery_credit=512、slow_consumer_close_s=60。
// 数值属策略，组装层应从 flag 注入 Config 覆盖。
const (
	DefaultInitialCredit     = 512
	DefaultSlowConsumerClose = 60 * time.Second
	defaultReadBatch         = 256
)

// MailReader 是 DeliveryPlane 对 mailbox 只读视图的最小依赖。
// *mailbox.Store 实现该接口；测试用 fake 实现同签名。
type MailReader interface {
	ListSince(ctx context.Context, uid, cursor int64, limit int) ([]mailbox.Entry, error)
	Cursor(ctx context.Context, uid int64) (int64, error)
	Head(ctx context.Context, uid int64) (int64, error)
}

// SendFn 是向单连接写一帧的回调（组装层接 user 层发送缓冲）。
// seq 为 mailbox_seq（conn 内严格有序，§3.3）；返回 error 视为连接写失败，
// Pump 立即停止且不推进 cursor（该条仍滞留 mailbox，重连 Resume 续传）。
type SendFn func(ctx context.Context, seq int64, envelope []byte) error

// AckFn 是累积游标推进回调（kind 15 语义，§4.2）。cursor 单调；
// 由组装层接到 MailboxHub.Ack（持久化 MarkAcked + 内存视图推进）。
type AckFn func(ctx context.Context, uid, cursor int64)

// Config 是 credit 流控机制参数（数值属策略，来自 §6 flag）。
type Config struct {
	InitialCredit     int           // delivery_credit，默认 512
	SlowConsumerClose time.Duration // slow_consumer_close_s，默认 60s
	ReadBatch         int           // 单次 ListSince 批量，默认 256（内部机制项）
}

// Conn 是一个 (uid, dev) 连接的消费状态机（§3.1）。
type Conn struct {
	uid  int64
	dev  string
	send SendFn

	mu        sync.Mutex
	cursor    int64     // 已确认游标（=max(kind105 累计, kind15.cursor)，§4.2）
	credit    int       // 剩余发送额度
	inflight  int       // 已发未 ACK 条数
	pausedAt  time.Time // credit==0 时刻；零值 = 未 PAUSED
	lastAcked int64     // 已上报 AckFn 的最大 cursor（幂等去重）
}

// UserID 返回该连接所属 uid。
func (c *Conn) UserID() int64 { return c.uid }

// Device 返回设备标识。
func (c *Conn) Device() string { return c.dev }

// Cursor 返回当前已确认游标快照。
func (c *Conn) Cursor() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cursor
}

// Credit 返回剩余信用快照（观测用）。
func (c *Conn) Credit() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.credit
}

// Paused 返回是否处于 PAUSED（credit==0）。
func (c *Conn) Paused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.credit <= 0
}

// Plane 是投递平面：conn[uid×dev] 订阅 mailbox，credit 流控（§1.2/§3）。
type Plane struct {
	reader MailReader
	ack    AckFn
	cfg    Config

	mu    sync.Mutex
	conns map[int64][]*Conn // uid → 该 uid 全部活跃连接（含多设备）
}

// Stats 是运行时指标快照（§5.2 Bus.PausedConns / Bus.BackpressureDepth 接线点）。
type Stats struct {
	PausedConns   int // 处于 PAUSED 的连接数
	Backpressured int // 全 conn PAUSED 的 uid 数（BACKPRESSURED mailbox 数）
	Inflight      int // 全部连接已发未 ACK 条数之和
}

// PumpResult 是单次 Pump 的产出。
type PumpResult struct {
	Sent        int   // 本次实际发出的条数
	Paused      bool  // 结束时 conn 是否 PAUSED（credit 耗尽或本就为 0）
	ShouldClose bool  // PAUSED 超过 slow_consumer_close_s → 判死（§3.2）
	Err         error // sendFn 首个错误（如有）；出错时该条不推进 cursor
}

// NewPlane 构造 DeliveryPlane。reader 必须非 nil；ack 允许 nil（仅流控，不持久化）。
func NewPlane(reader MailReader, ack AckFn, cfg Config) *Plane {
	if cfg.InitialCredit <= 0 {
		cfg.InitialCredit = DefaultInitialCredit
	}
	if cfg.SlowConsumerClose <= 0 {
		cfg.SlowConsumerClose = DefaultSlowConsumerClose
	}
	if cfg.ReadBatch <= 0 {
		cfg.ReadBatch = defaultReadBatch
	}
	return &Plane{
		reader: reader,
		ack:    ack,
		cfg:    cfg,
		conns:  make(map[int64][]*Conn),
	}
}

// Subscribe 注册一条连接的订阅（§3.1：HELLO/AUTH 完成 → SUBSCRIBE mailbox://uid@dev）。
// 初始 cursor 取持久化生效游标（Resume 给点或 0），credit 取 Config.InitialCredit。
func (p *Plane) Subscribe(uid int64, dev string, send SendFn) *Conn {
	c := &Conn{uid: uid, dev: dev, send: send, credit: p.cfg.InitialCredit}
	if cur, err := p.reader.Cursor(context.Background(), uid); err == nil && cur > 0 {
		c.cursor = cur
		c.lastAcked = cur
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conns[uid] = append(p.conns[uid], c)
	return c
}

// Unsubscribe 摘除一条连接（断连时由组装层调用）。
func (p *Plane) Unsubscribe(c *Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cs := p.conns[c.uid]
	for i, x := range cs {
		if x == c {
			p.conns[c.uid] = append(cs[:i], cs[i+1:]...)
			break
		}
	}
	if len(p.conns[c.uid]) == 0 {
		delete(p.conns, c.uid)
	}
}

// ConnOf 返回该 (uid, dev) 连接的订阅（kind 15 上行 ACK 的定位入口，
// P3d-2 接线：Router 解析 MAILBOX_ACK 后按连接转调 OnAck）；无订阅返回 nil。
func (p *Plane) ConnOf(uid int64, dev string) *Conn {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns[uid] {
		if c.dev == dev {
			return c
		}
	}
	return nil
}

// Pump 按 mailbox_seq 升序把 cursor+1..head 的条目逐条 sendFn（§3.1）。
// 每发 1 条 credit--；credit 耗尽 → PAUSED 并停止拉取（不丢、不标记）。
// 同一 conn 内严格有序；PAUSED 只发生在窗口边界（§3.3）。
func (p *Plane) Pump(ctx context.Context, c *Conn) PumpResult {
	res := PumpResult{}

	// 判死检查先行：PAUSED 超 slow_consumer_close_s → 上层关闭连接（§3.2 逃生）。
	c.mu.Lock()
	if c.credit <= 0 && !c.pausedAt.IsZero() &&
		time.Since(c.pausedAt) > p.cfg.SlowConsumerClose {
		c.mu.Unlock()
		res.Paused = true
		res.ShouldClose = true
		return res
	}
	if c.credit <= 0 {
		if c.pausedAt.IsZero() {
			c.pausedAt = time.Now()
		}
		c.mu.Unlock()
		res.Paused = true
		return res
	}
	c.mu.Unlock()

	head, err := p.reader.Head(ctx, c.uid)
	if err != nil {
		res.Err = err
		return res
	}

	for {
		c.mu.Lock()
		if c.credit <= 0 || c.cursor >= head {
			if c.credit <= 0 && c.pausedAt.IsZero() {
				c.pausedAt = time.Now()
			}
			paused := c.credit <= 0
			c.mu.Unlock()
			res.Paused = paused
			return res
		}
		limit := p.cfg.ReadBatch
		if c.credit < limit {
			limit = c.credit
		}
		c.mu.Unlock()

		entries, err := p.reader.ListSince(ctx, c.uid, c.cursor, limit)
		if err != nil {
			res.Err = err
			return res
		}
		if len(entries) == 0 {
			c.mu.Lock()
			res.Paused = c.credit <= 0
			c.mu.Unlock()
			return res
		}

		for _, e := range entries {
			if err := c.send(ctx, e.MailboxSeq, e.Envelope); err != nil {
				res.Err = err
				// 该条未确认，仍滞留 mailbox（重连 Resume 按 cursor 续传）。
				c.mu.Lock()
				res.Paused = c.credit <= 0
				c.mu.Unlock()
				return res
			}
			c.mu.Lock()
			c.cursor = e.MailboxSeq
			c.credit--
			c.inflight++
			res.Sent++
			if c.credit == 0 && c.pausedAt.IsZero() {
				c.pausedAt = time.Now()
			}
			c.mu.Unlock()
			if c.Credit() <= 0 {
				break
			}
		}

		c.mu.Lock()
		paused := c.credit <= 0
		c.mu.Unlock()
		if paused {
			res.Paused = true
			return res
		}
	}
}

// OnAck 处理一批累积 ACK（kind 15，§3.2/§4.2）：cursor 单调推进（后退幂等忽略），
// credit += n 并解除 PAUSED（如有余量）；cursor 前进时调用 AckFn（持久化在组装层）。
func (p *Plane) OnAck(ctx context.Context, c *Conn, cursor int64, n int) error {
	c.mu.Lock()
	if cursor > c.cursor {
		c.cursor = cursor
	}
	if n > 0 {
		c.credit += n
	}
	if c.inflight >= n && n > 0 {
		c.inflight -= n
	} else if n > 0 {
		c.inflight = 0
	}
	if c.credit > 0 {
		c.pausedAt = time.Time{} // 解除 PAUSED
	}
	advanced := cursor > c.lastAcked
	if advanced {
		c.lastAcked = cursor
	}
	c.mu.Unlock()

	if advanced && p.ack != nil {
		p.ack(ctx, c.uid, cursor)
	}
	return nil
}

// SendVital 发送保命帧（Ping 4/5、ERROR/KICKED 10，§3.2）：不占 credit、
// 不受 PAUSED 影响、不进 mailbox 有序流（低价值/系统帧走独立旁路，§10）。
// 调用方负责帧已含正确 kind（本包不解释 proto）。
func (p *Plane) SendVital(ctx context.Context, c *Conn, seq int64, envelope []byte) error {
	if c == nil || c.send == nil {
		return errors.New("delivery: vital send on nil conn")
	}
	return c.send(ctx, seq, envelope)
}

// Stats 导出指标快照（§5.2）。
func (p *Plane) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	var st Stats
	for _, cs := range p.conns {
		if len(cs) == 0 {
			continue
		}
		allPaused := true
		for _, c := range cs {
			c.mu.Lock()
			if c.credit <= 0 {
				st.PausedConns++
			} else {
				allPaused = false
			}
			st.Inflight += c.inflight
			c.mu.Unlock()
		}
		if allPaused {
			st.Backpressured++
		}
	}
	return st
}
