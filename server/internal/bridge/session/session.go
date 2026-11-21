// Package session 管理连接生命周期（docs/bridge-design.md §2.2）：
// Register / 心跳判死（60s）/ 订阅 TTL 30s 续租 / Unregister。
// 控制面旁路：HELLO/SUBSCRIBE/ACK/HEARTBEAT 直接消费，不进流水线。
//
// 禁令：不转发任何数据面帧。
package session

import (
	"sync"
	"time"

	"github.com/yingo-server/UFOT/server/internal/proto"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
	"github.com/yingo-server/UFOT/server/internal/transport"
)

// 心跳判死与订阅租约（可配）。
const (
	DefaultHeartbeatTTL = 60 * time.Second
	DefaultSubTTL       = 30 * time.Second
)

// Subscription 一条 topic 租约。
type Subscription struct {
	Topic    string
	Deadline time.Time
}

// Session 一条已注册连接的会话态。
type Session struct {
	Peer   transport.Peer
	Layer  string
	Caps   []kinds.Kind
	Subs   map[string]*Subscription
	LastHB time.Time
}

// Manager 连接表（fd 表语义）。
type Manager struct {
	mu       sync.Mutex
	sessions map[uint64]*Session // key: ConnID（来自 FrameMeta）
	nextID   uint64

	hbTTL  time.Duration
	subTTL time.Duration
	now    func() time.Time

	// OnDead 心跳判死回调（断连由装配方执行）。
	OnDead func(connID uint64)
	// OnSubscribe 订阅建立回调（由装配方接到 bus.Sub）。
	OnSubscribe func(connID uint64, topic string)
	// OnUnsubscribe 退订回调。
	OnUnsubscribe func(connID uint64, topic string)
}

// New 构造。
func New() *Manager {
	return &Manager{
		sessions: map[uint64]*Session{},
		hbTTL:    DefaultHeartbeatTTL,
		subTTL:   DefaultSubTTL,
		now:      time.Now,
	}
}

// SetClock 注入时钟（测试）。
func (m *Manager) SetClock(f func() time.Time) { m.now = f }

// SetTTLs 覆盖心跳/订阅 TTL。
func (m *Manager) SetTTLs(hb, sub time.Duration) {
	m.mu.Lock()
	m.hbTTL, m.subTTL = hb, sub
	m.mu.Unlock()
}

// Register 登记连接并分配 ConnID。
func (m *Manager) Register(peer transport.Peer, layer string, caps []kinds.Kind) uint64 {
	m.mu.Lock()
	m.nextID++
	id := m.nextID
	m.sessions[id] = &Session{
		Peer:   peer,
		Layer:  layer,
		Caps:   append([]kinds.Kind(nil), caps...),
		Subs:   map[string]*Subscription{},
		LastHB: m.now(),
	}
	m.mu.Unlock()
	return id
}

// Unregister 注销连接并触发全部退订回调。
func (m *Manager) Unregister(connID uint64) {
	m.mu.Lock()
	s, ok := m.sessions[connID]
	delete(m.sessions, connID)
	m.mu.Unlock()
	if ok && m.OnUnsubscribe != nil {
		for topic := range s.Subs {
			m.OnUnsubscribe(connID, topic)
		}
	}
}

// ConsumeControl 消费一条控制面帧（旁路）。返回 true 表示已消费
// （调用方不得再送入数据面流水线）。数据面帧返回 false。
func (m *Manager) ConsumeControl(connID uint64, f *proto.Frame) bool {
	switch f.Kind {
	case kinds.KindInternalHello, kinds.KindHello:
		m.touch(connID)
		return true
	case kinds.KindInternalHeartbeat, kinds.KindPing, kinds.KindPong:
		m.touch(connID)
		return true
	case kinds.KindInternalFrameAck:
		// 904 是唯一的传输层帧确认。MSG_ACK(103) 是业务消息，必须走
		// Router/Egress 回给用户层——曾在此被旁路吞掉，导致数据层 ack 永远
		// 到不了客户端（装配验收用例：MSG_SEND → MSG_ACK）。
		m.touch(connID)
		return true
	case kinds.KindInternalSubscribe:
		m.touch(connID)
		m.subscribe(connID, f)
		return true
	default:
		return false
	}
}

// subscribe 处理 SUBSCRIBE：topic 来自帧 Meta 之外的约定——
// 用 Envelope request_id 字段承载 topic 字符串（控制面旁路约定）。
func (m *Manager) subscribe(connID uint64, f *proto.Frame) {
	topic := ""
	if f.Env != nil {
		topic = f.Env.GetRequestId()
	}
	if topic == "" {
		return
	}
	m.mu.Lock()
	s, ok := m.sessions[connID]
	if ok {
		s.Subs[topic] = &Subscription{Topic: topic, Deadline: m.now().Add(m.subTTL)}
	}
	m.mu.Unlock()
	if ok && m.OnSubscribe != nil {
		m.OnSubscribe(connID, topic)
	}
}

// Renew 续租某连接的全部订阅（收到该连接任意活动可调用）。
func (m *Manager) Renew(connID uint64) {
	m.mu.Lock()
	if s, ok := m.sessions[connID]; ok {
		for _, sub := range s.Subs {
			sub.Deadline = m.now().Add(m.subTTL)
		}
	}
	m.mu.Unlock()
}

// touch 心跳续命。
func (m *Manager) touch(connID uint64) {
	m.mu.Lock()
	if s, ok := m.sessions[connID]; ok {
		s.LastHB = m.now()
	}
	m.mu.Unlock()
}

// Sweep 周期维护：心跳超时判死 + 订阅租约过期退订。返回死亡连接数。
func (m *Manager) Sweep() int {
	now := m.now()
	var dead []uint64
	type unsub struct {
		id    uint64
		topic string
	}
	var unsubs []unsub
	m.mu.Lock()
	for id, s := range m.sessions {
		if now.Sub(s.LastHB) > m.hbTTL {
			dead = append(dead, id)
			continue
		}
		for topic, sub := range s.Subs {
			if now.After(sub.Deadline) {
				delete(s.Subs, topic)
				unsubs = append(unsubs, unsub{id, topic})
			}
		}
	}
	for _, id := range dead {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	for _, u := range unsubs {
		if m.OnUnsubscribe != nil {
			m.OnUnsubscribe(u.id, u.topic)
		}
	}
	for _, id := range dead {
		if m.OnDead != nil {
			m.OnDead(id)
		}
	}
	return len(dead)
}

// Count 存活连接数。
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// Get 会话快照查询。
func (m *Manager) Get(connID uint64) (Session, bool) {
	m.mu.Lock()
	s, ok := m.sessions[connID]
	m.mu.Unlock()
	if !ok {
		return Session{}, false
	}
	return *s, true
}
