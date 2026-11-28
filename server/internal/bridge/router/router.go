// Package router 是流水线第 3 级：纯函数查表 (kind, envelope) → []Target
// （docs/bridge-design.md §2.2）。
//
// 禁令：不发帧、不记状态、不知道 Bus 存在。
package router

import (
	"sync"

	"github.com/yingo-server/UFOT/server/internal/pipedef"
	chatpb "github.com/yingo-server/UFOT/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
)

// Target 一个投递目标：层 + bus topic。
type Target = pipedef.Target

// Router 无状态查表器。表启动时注入（静态默认 + 可覆盖）。
type Router struct {
	mu sync.RWMutex
	// table: kind 段 → 目标。查询顺序：精确 kind → 段规则。
	exact map[uint32][]Target
	// fromData: 当帧来源层是 data 时的覆盖表。用于「同 kind 双向转发」的信令
	// （如通话 350-354）：客户端上行 350 → data；data 回填后下行 350 → user。
	// 静态按 kind 查表无法区分方向，故在此按来源层分流（仍是纯表查询）。
	fromData map[uint32][]Target
	ranges   []rangeRule
}

type rangeRule struct {
	lo, hi  uint32
	targets []Target
}

// New 用规则表构造；nil 用 DefaultTable。
func New(table map[uint32][]Target) *Router {
	if table == nil {
		table = DefaultTable()
	}
	r := &Router{exact: map[uint32][]Target{}, fromData: map[uint32][]Target{}}
	for k, ts := range table {
		cp := append([]Target(nil), ts...)
		r.exact[k] = cp
	}
	return r
}

// Route 纯函数：查表返回目标列表（可能为空 = NO_ROUTE）。
// 100-499：有 conv_id → conv.{cid}、有 user_id → user.{uid}，投 user 层；
// 数据面 kind(910-989) → data 层固定 topic；500-599 → admin+user 广播。
func (r *Router) Route(kind uint32, env *chatpb.Envelope) []Target {
	if ts, ok := r.exact[kind]; ok {
		return append([]Target(nil), ts...)
	}
	switch {
	case kind >= 100 && kind < 500:
		var ts []Target
		if env != nil {
			if cid := env.GetConvId(); cid != 0 {
				ts = append(ts, Target{Layer: "user", Topic: topicConv(cid)})
			}
			if uid := env.GetUserId(); uid != 0 {
				ts = append(ts, Target{Layer: "user", Topic: topicUser(uid)})
			}
		}
		return ts
	case kind >= 910 && kind < 990:
		return []Target{{Layer: "data", Topic: "data." + u64(uint64(kind))}}
	case kind >= 500 && kind < 600:
		return []Target{
			{Layer: "admin", Topic: "sys.admin"},
			{Layer: "user", Topic: "sys.broadcast"},
		}
	}
	return nil
}

// RouteFrom 在 Route 基础上增加「来源层」维度：data 来源的帧先查 fromData
// 覆盖表，其余落回普通 Route。用于同一 kind 上下行分流的信令转发。
func (r *Router) RouteFrom(kind uint32, env *chatpb.Envelope, srcLayer string) []Target {
	if srcLayer == "data" {
		r.mu.RLock()
		ts, ok := r.fromData[kind]
		r.mu.RUnlock()
		if ok {
			return append([]Target(nil), ts...)
		}
	}
	return r.Route(kind, env)
}

// SetFromData 设置一条「来源层=data 时」的精确路由覆盖。
func (r *Router) SetFromData(kind uint32, ts []Target) {
	r.mu.Lock()
	r.fromData[kind] = append([]Target(nil), ts...)
	r.mu.Unlock()
}

// Set 覆盖/追加一条精确规则（SIGHUP 热重载白名单：路由表）。
func (r *Router) Set(kind uint32, ts []Target) {
	r.mu.Lock()
	r.exact[kind] = append([]Target(nil), ts...)
	r.mu.Unlock()
}

// Table 返回当前精确规则副本（快照/测试用）。
func (r *Router) Table() map[uint32][]Target {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[uint32][]Target, len(r.exact))
	for k, ts := range r.exact {
		out[k] = append([]Target(nil), ts...)
	}
	return out
}

// DefaultTable 静态默认精确规则（可被配置覆盖）。
// 控制面 0-99 与层间控制 900-909 不进 Router/Bus（旁路），不在表内。
func DefaultTable() map[uint32][]Target {
	return map[uint32][]Target{
		kinds.KindShutdownNotice: {{Layer: "user", Topic: "sys.broadcast"}, {Layer: "data", Topic: "data.910"}, {Layer: "admin", Topic: "sys.admin"}},
		kinds.KindFlagSync:       {{Layer: "user", Topic: "sys.broadcast"}, {Layer: "data", Topic: "data.911"}},
		kinds.KindConfigUpdate:   {{Layer: "user", Topic: "sys.broadcast"}, {Layer: "data", Topic: "data.912"}},
		kinds.KindTopology:       {{Layer: "admin", Topic: "sys.admin"}},
	}
}

func topicConv(cid uint64) string { return "conv." + u64(cid) }
func topicUser(uid uint64) string { return "user." + u64(uid) }

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
