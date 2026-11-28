// capability.go —— 连接能力注册表（旧客户端能力协商，lts-1.4-acceptance §B）。
//
// 背景：普遍拉取模式（universal_pull_enabled=true）把消息通知从「逐成员
// inline 102」换成「kind 14 水位线 + 客户端批量拉取」。但老客户端没有
// kind 14 handler：给它发水位线 = 消息永远到不了它手里（v1.2 事故的根因）。
// 因此客户端必须先上报自己的能力，服务端在连接注册表里记住它，投递时才
// 决定「这个 uid 能不能走水位线」。
//
// 三条不变量：
//  1. 缺省 = 无能力。客户端没上报（或上报 false）时一律按老客户端对待，
//     保持 inline 逐成员推 —— 宁可多发，不漏消息。
//  2. flag-off 零行为影响。本表只是「记录」，不参与任何投递判定；只有
//     universal_pull_enabled=true 时数据层才会来问它（S-7 回滚基线）。
//  3. uid 粒度聚合。同一 uid 的多条连接只要有一条是无能力连接，该 uid 就
//     整体按 inline 服务（保守：宁可多发一条 102，不让老设备漏消息，也
//     不会造成同一 uid 双投递）。
//
// 锁顺序：conns → users → caps（本文件只在 users 之后取 caps）。
package session

import "sync"

// capsShard 是能力表的一个分片：connID → pull_capable。
type capsShard struct {
	mu sync.RWMutex
	m  map[uint64]bool
}

// SetPullCapable 记录/更新一条连接的能力（HELLO 或 CLIENT_CAPS 上报）。
// 幂等：同一连接重复上报只保留最后一次结果。
func (s *Session) SetPullCapable(connID uint64, pullCapable bool) {
	if s == nil || connID == 0 {
		return
	}
	cs := &s.caps[shardIndex(connID)]
	cs.mu.Lock()
	if cs.m == nil {
		cs.m = make(map[uint64]bool, 4)
	}
	cs.m[connID] = pullCapable
	cs.mu.Unlock()
}

// ClearPullCapable 在连接注销时清掉能力条目（防 connID 回绕复用后继承旧能力）。
func (s *Session) ClearPullCapable(connID uint64) {
	if s == nil {
		return
	}
	cs := &s.caps[shardIndex(connID)]
	cs.mu.Lock()
	delete(cs.m, connID)
	cs.mu.Unlock()
}

// PullCapableConn 报告该连接是否上报过 pull_capable（缺省 false）。
func (s *Session) PullCapableConn(connID uint64) bool {
	if s == nil {
		return false
	}
	cs := &s.caps[shardIndex(connID)]
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.m[connID]
}

// PullCapableUID 报告该 uid 是否「整体具备拉取能力」：
//   - 该 uid 当前没有在线连接 → false（没有收件人，无所谓）；
//   - 该 uid 有在线连接，且每一条都上报了 pull_capable → true；
//   - 只要有一条连接没上报（老客户端 / 还没上报）→ false（保守走 inline）。
func (s *Session) PullCapableUID(uid int64) bool {
	if s == nil {
		return false
	}
	us := &s.users[shardIndex(uint64(uid))]
	us.mu.RLock()
	conns := make([]uint64, 0, len(us.byUser[uid]))
	for id := range us.byUser[uid] {
		conns = append(conns, id)
	}
	us.mu.RUnlock()
	if len(conns) == 0 {
		return false
	}
	for _, id := range conns {
		if !s.PullCapableConn(id) {
			return false
		}
	}
	return true
}
