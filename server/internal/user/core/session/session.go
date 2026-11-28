// Package session 是 usergw 的连接表（docs/user-frontend-design.md §1.1 session）：
// user_id → []conn 映射、conn_id 索引、presence 副本、tombstone(30s)。
//
// 明确不做：不发帧（只把字节交给 Conn）、不解析协议、不判投递目标。
// 16 分片锁降低高并发注册/广播的争用。
package session

import (
	"errors"
	"hash/fnv"
	"sync"
	"time"
)

// Conn 是连接表持有的最小连接视图（net/ws.Conn 实现本接口）。
type Conn interface {
	ID() uint64
	UserID() int64
	DeviceID() string
	// Write 非阻塞投递一帧到连接写缓冲；缓冲满返回 false（调用方计数丢弃）。
	Write([]byte) bool
	// Close 关闭连接（幂等）。
	Close() error
}

const shardCount = 16

// ErrTombstoned 连接 id 处于 tombstone 窗口内，拒绝复用（防止 conn_id 回绕重放）。
var ErrTombstoned = errors.New("session: conn id tombstoned")

// userShard 按 uid 分片：uid → connID → conn。
type userShard struct {
	mu     sync.RWMutex
	byUser map[int64]map[uint64]Conn
}

// connShard 按 connID 分片：connID → conn + tombstone。
type connShard struct {
	mu    sync.RWMutex
	byID  map[uint64]Conn
	tombs map[uint64]time.Time
}

// presenceShard presence 副本分片。
type presenceShard struct {
	mu sync.RWMutex
	m  map[int64]presenceEntry
}

type presenceEntry struct {
	status uint32
	at     time.Time
}

// Session 连接表。零值不可用，用 New。
type Session struct {
	users    [shardCount]userShard
	conns    [shardCount]connShard
	presence [shardCount]presenceShard
	// caps 是连接能力表（connID → pull_capable，旧客户端能力协商，
	// lts-1.4-acceptance §B）。只记录，不参与投递判定。
	caps [shardCount]capsShard
	ttl  time.Duration
	now  func() time.Time // 测试可注入
}

// New 构造连接表；tombstoneTTL 默认 30s（docs §1.1）。
func New(tombstoneTTL time.Duration) *Session {
	if tombstoneTTL <= 0 {
		tombstoneTTL = 30 * time.Second
	}
	s := &Session{ttl: tombstoneTTL, now: time.Now}
	for i := 0; i < shardCount; i++ {
		s.users[i].byUser = make(map[int64]map[uint64]Conn)
		s.conns[i].byID = make(map[uint64]Conn)
		s.conns[i].tombs = make(map[uint64]time.Time)
		s.presence[i].m = make(map[int64]presenceEntry)
	}
	return s
}

func shardIndex(v uint64) int {
	h := fnv.New32a()
	var b [8]byte
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (8 * i))
	}
	_, _ = h.Write(b[:])
	return int(h.Sum32() % shardCount)
}

// Register 注册连接。同一 uid 已有连接时追加（多设备）。
// tombstone 窗口内的 connID 拒绝复用。
//
// 锁顺序固定：conn 分片 → user 分片（避免交叉死锁）。
func (s *Session) Register(c Conn) error {
	if c == nil {
		return errors.New("session: nil conn")
	}
	cs := &s.conns[shardIndex(c.ID())]
	cs.mu.Lock()
	if exp, ok := cs.tombs[c.ID()]; ok {
		if s.now().Before(exp) {
			cs.mu.Unlock()
			return ErrTombstoned
		}
		delete(cs.tombs, c.ID())
	}
	cs.byID[c.ID()] = c
	cs.mu.Unlock()

	us := &s.users[shardIndex(uint64(c.UserID()))]
	us.mu.Lock()
	conns := us.byUser[c.UserID()]
	if conns == nil {
		conns = make(map[uint64]Conn, 4)
		us.byUser[c.UserID()] = conns
	}
	conns[c.ID()] = c
	us.mu.Unlock()
	return nil
}

// Unregister 注销连接并写入 tombstone（30s 内不可复用 connID）。
func (s *Session) Unregister(connID uint64) {
	cs := &s.conns[shardIndex(connID)]
	cs.mu.Lock()
	c, ok := cs.byID[connID]
	if !ok {
		cs.mu.Unlock()
		return
	}
	delete(cs.byID, connID)
	cs.tombs[connID] = s.now().Add(s.ttl)
	s.purgeTombsLocked(cs)
	cs.mu.Unlock()

	us := &s.users[shardIndex(uint64(c.UserID()))]
	us.mu.Lock()
	if conns := us.byUser[c.UserID()]; conns != nil {
		delete(conns, connID)
		if len(conns) == 0 {
			delete(us.byUser, c.UserID())
		}
	}
	us.mu.Unlock()

	// 能力条目随连接一起消失（connID 回绕复用时不得继承旧能力）。
	s.ClearPullCapable(connID)
}

// Lookup 按 connID 定位连接。
func (s *Session) Lookup(connID uint64) Conn {
	cs := &s.conns[shardIndex(connID)]
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.byID[connID]
}

// GetByUser 返回该 uid 的全部连接快照。
func (s *Session) GetByUser(uid int64) []Conn {
	us := &s.users[shardIndex(uint64(uid))]
	us.mu.RLock()
	defer us.mu.RUnlock()
	conns := us.byUser[uid]
	out := make([]Conn, 0, len(conns))
	for _, c := range conns {
		out = append(out, c)
	}
	return out
}

// Broadcast 向该 uid 的全部连接写同一帧，返回成功写入的连接数。
// 慢连接由 Conn.Write 非阻塞丢弃（调用方可通过计数观测）。
func (s *Session) Broadcast(uid int64, b []byte) int {
	us := &s.users[shardIndex(uint64(uid))]
	us.mu.RLock()
	conns := make([]Conn, 0, len(us.byUser[uid]))
	for _, c := range us.byUser[uid] {
		conns = append(conns, c)
	}
	us.mu.RUnlock()
	n := 0
	for _, c := range conns {
		if c.Write(b) {
			n++
		}
	}
	return n
}

// BroadcastAll 向全部连接广播（排空/维护通知用）。
func (s *Session) BroadcastAll(b []byte) int {
	var all []Conn
	for i := 0; i < shardCount; i++ {
		cs := &s.conns[i]
		cs.mu.RLock()
		for _, c := range cs.byID {
			all = append(all, c)
		}
		cs.mu.RUnlock()
	}
	n := 0
	for _, c := range all {
		if c.Write(b) {
			n++
		}
	}
	return n
}

// All 返回全部连接快照。
func (s *Session) All() []Conn {
	var all []Conn
	for i := 0; i < shardCount; i++ {
		cs := &s.conns[i]
		cs.mu.RLock()
		for _, c := range cs.byID {
			all = append(all, c)
		}
		cs.mu.RUnlock()
	}
	return all
}

// Count 返回连接总数。
func (s *Session) Count() int {
	n := 0
	for i := 0; i < shardCount; i++ {
		cs := &s.conns[i]
		cs.mu.RLock()
		n += len(cs.byID)
		cs.mu.RUnlock()
	}
	return n
}

// UserCount 返回在线用户数。
func (s *Session) UserCount() int {
	n := 0
	for i := 0; i < shardCount; i++ {
		us := &s.users[i]
		us.mu.RLock()
		n += len(us.byUser)
		us.mu.RUnlock()
	}
	return n
}

// Tombstoned 报告 connID 是否仍在 tombstone 窗口内。
func (s *Session) Tombstoned(connID uint64) bool {
	cs := &s.conns[shardIndex(connID)]
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	exp, ok := cs.tombs[connID]
	return ok && s.now().Before(exp)
}

// purgeTombsLocked 惰性清理过期 tombstone（写路径加锁时顺带执行）。
func (s *Session) purgeTombsLocked(cs *connShard) {
	now := s.now()
	for id, exp := range cs.tombs {
		if now.After(exp) {
			delete(cs.tombs, id)
		}
	}
}

// ---------- presence 副本 ----------

// SetPresence 更新 presence 副本（PRESENCE_UPDATE 下行 / 数据层广播时调用）。
func (s *Session) SetPresence(uid int64, status uint32, at time.Time) {
	ps := &s.presence[shardIndex(uint64(uid))]
	ps.mu.Lock()
	ps.m[uid] = presenceEntry{status: status, at: at}
	ps.mu.Unlock()
}

// Presence 读取 presence 副本。
func (s *Session) Presence(uid int64) (uint32, bool) {
	ps := &s.presence[shardIndex(uint64(uid))]
	ps.mu.RLock()
	e, ok := ps.m[uid]
	ps.mu.RUnlock()
	return e.status, ok
}
