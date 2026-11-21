// Package ack 是传输可靠性级（docs/bridge-design.md §2.2）：
// 连接级 seq in-flight 窗口、RTO 300ms 重发、3 次失败判死。
//
// 禁令：不管业务 request_id 配对。不变量断言失败 → panic
// （exit 70 语义由 Supervisor 转）。
package ack

import (
	"sync"
	"time"

	"github.com/yingo-server/UFOT/server/internal/proto"
)

// RTO 重发超时；MaxRetries 判死阈值。
const (
	RTO        = 300 * time.Millisecond
	MaxRetries = 3
)

// pending 一条 in-flight 帧。
type pending struct {
	frame    *proto.Frame
	deadline time.Time
	retries  int
}

// Tracker 单连接（单 peer）的 AckTracker。每 peer 独立实例。
type Tracker struct {
	mu       sync.Mutex
	inflight map[uint32]*pending

	// Resend 重发回调：把帧重新 Enqueue 到 egress（由装配方注入）。
	Resend func(seq uint32, f *proto.Frame)
	// OnDead 3 次失败判死回调（连接级）。
	OnDead func(seq uint32, f *proto.Frame)
	// clock 可注入（测试）。
	clock func() time.Time
}

// New 构造。
func New() *Tracker {
	return &Tracker{inflight: map[uint32]*pending{}, clock: time.Now}
}

// Track 登记一条 in-flight 帧。
func (t *Tracker) Track(seq uint32, f *proto.Frame) {
	if f == nil {
		panic("ack: invariant violated — Track(nil frame)")
	}
	t.mu.Lock()
	if _, dup := t.inflight[seq]; dup {
		t.mu.Unlock()
		panic("ack: invariant violated — duplicate in-flight seq " + u32(seq))
	}
	t.inflight[seq] = &pending{frame: f, deadline: t.clock().Add(RTO)}
	t.mu.Unlock()
}

// OnAck 确认一条 seq。未知 seq 是不变量破坏（已 ACK 重复/幽灵确认）。
func (t *Tracker) OnAck(seq uint32) {
	t.mu.Lock()
	p, ok := t.inflight[seq]
	if !ok {
		t.mu.Unlock()
		panic("ack: invariant violated — ack for unknown seq " + u32(seq))
	}
	delete(t.inflight, seq)
	t.mu.Unlock()
	_ = p
}

// Tick 扫描超时：到期未 ACK → 重发（Resend 回调）；
// 重试达 MaxRetries → OnDead 判死并移除。返回本 tick 处理的到期条数。
// 调用方以 <RTO 周期轮询（如 100ms）。
func (t *Tracker) Tick() int {
	now := t.clock()
	type action struct {
		seq uint32
		f   *proto.Frame
	}
	var resends, deads []action
	t.mu.Lock()
	for seq, p := range t.inflight {
		if now.After(p.deadline) {
			p.retries++
			if p.retries >= MaxRetries {
				deads = append(deads, action{seq, p.frame})
				delete(t.inflight, seq)
			} else {
				p.deadline = now.Add(RTO)
				resends = append(resends, action{seq, p.frame})
			}
		}
	}
	t.mu.Unlock()
	for _, a := range resends {
		if t.Resend != nil {
			t.Resend(a.seq, a.f)
		}
	}
	for _, a := range deads {
		if t.OnDead != nil {
			t.OnDead(a.seq, a.f)
		}
	}
	return len(resends) + len(deads)
}

// Fail 外部 NACK 判死一条 seq（Egress firm 水位溢出路径）。
// 未知 seq 视为已确认/已判死，静默返回。
func (t *Tracker) Fail(seq uint32) {
	t.mu.Lock()
	p, ok := t.inflight[seq]
	if ok {
		delete(t.inflight, seq)
	}
	t.mu.Unlock()
	if ok && t.OnDead != nil {
		t.OnDead(seq, p.frame)
	}
}

// InFlight 当前 in-flight 数（水位观测）。
func (t *Tracker) InFlight() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.inflight)
}

func u32(v uint32) string {
	if v == 0 {
		return "0"
	}
	var buf [10]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
