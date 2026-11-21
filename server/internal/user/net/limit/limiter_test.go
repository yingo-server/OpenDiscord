package limit

import (
	"math"
	"testing"
	"time"

	"github.com/yingo-server/OpenDiscord/server/internal/proto/kinds"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestDefaultRules 逐行断言 security-spec §1.2.2 参数表。
func TestDefaultRules(t *testing.T) {
	cfg := DefaultConfig()
	cases := []struct {
		kind  kinds.Kind
		rate  float64
		burst float64
	}{
		{kinds.KindMsgSend, 20, 40},
		{KindPriority, 5, 10},
		{kinds.KindTyping, 1, 3},
		{kinds.KindPresenceUpdate, 0.1, 3},
		{kinds.KindMsgRead, 10, 30},
		{kinds.KindMsgDelivered, 10, 30},
		{kinds.KindSyncRequest, 2, 4},
		{kinds.KindMediaPresignRequest, 1.0 / 30.0, 2},
	}
	for _, c := range cases {
		r, ok := cfg.Rules[c.kind]
		if !ok {
			t.Fatalf("missing rule kind=%s", kinds.KindName(c.kind))
		}
		if !near(r.Rate, c.rate) || !near(r.Burst, c.burst) {
			t.Fatalf("kind=%s rule=%+v want rate=%v burst=%v", kinds.KindName(c.kind), r, c.rate, c.burst)
		}
	}
	if cfg.Rules[kinds.KindTyping].Silent != true || cfg.Rules[kinds.KindPresenceUpdate].Silent != true {
		t.Fatal("typing/presence must be silent-drop kinds")
	}
	if cfg.Rules[kinds.KindMsgSend].MaxPayload != 8<<10 {
		t.Fatalf("msg payload cap = %d, want 8192", cfg.Rules[kinds.KindMsgSend].MaxPayload)
	}
	if !cfg.ExemptKinds[kinds.KindPing] || !cfg.ExemptKinds[kinds.KindPong] {
		t.Fatal("ping/pong must be exempt")
	}
}

func newTestLimiter(rule Rule) *Limiter {
	cfg := DefaultConfig()
	cfg.Rules = map[uint32]Rule{101: rule}
	cfg.DefaultRule = rule
	cfg.KillMinFrames = 1 << 30 // 默认不触发 kill
	return New(cfg)
}

// TestConnBucket 连接级桶：burst 用尽 → Throttle，时间推进后恢复。
func TestConnBucket(t *testing.T) {
	l := newTestLimiter(Rule{Rate: 1, Burst: 2, Capacity: 2})
	l.AddConn(1, 100)
	now := time.Unix(1000, 0)
	if v := l.AllowAt(1, 100, 101, 10, now); v != OK {
		t.Fatalf("1st = %v, want OK", v)
	}
	if v := l.AllowAt(1, 100, 101, 10, now); v != OK {
		t.Fatalf("2nd = %v, want OK", v)
	}
	if v := l.AllowAt(1, 100, 101, 10, now); v != Throttle {
		t.Fatalf("3rd = %v, want Throttle", v)
	}
	// 一个令牌 1s 后恢复。
	if v := l.AllowAt(1, 100, 101, 10, now.Add(time.Second)); v != OK {
		t.Fatalf("after refill = %v, want OK", v)
	}
}

// TestSilentDrop typing/presence 超限静默丢弃。
func TestSilentDrop(t *testing.T) {
	l := newTestLimiter(Rule{Rate: 1, Burst: 1, Capacity: 1, Silent: true})
	l.AddConn(2, 200)
	now := time.Unix(2000, 0)
	if v := l.AllowAt(2, 200, 101, 0, now); v != OK {
		t.Fatalf("1st = %v", v)
	}
	if v := l.AllowAt(2, 200, 101, 0, now); v != Drop {
		t.Fatalf("2nd = %v, want Drop", v)
	}
}

// TestPayloadCap 单帧 payload 上限。
func TestPayloadCap(t *testing.T) {
	l := newTestLimiter(Rule{Rate: 100, Burst: 100, Capacity: 100, MaxPayload: 8})
	l.AddConn(3, 300)
	if v := l.AllowAt(3, 300, 101, 9, time.Now()); v != Drop {
		t.Fatalf("oversize = %v, want Drop", v)
	}
}

// TestKillRatio 60s 窗口超限占比 > 50% → Kill（L4）。
func TestKillRatio(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Rules = map[uint32]Rule{101: {Rate: 0.01, Burst: 1, Capacity: 1}}
	cfg.DefaultRule = cfg.Rules[101]
	cfg.KillMinFrames = 6
	cfg.KillRatio = 0.5
	l := New(cfg)
	l.AddConn(4, 400)
	now := time.Unix(3000, 0)
	// 第 1 帧放行，其后全部超限 → 5/6 超限。
	var last Verdict
	for i := 0; i < 6; i++ {
		last = l.AllowAt(4, 400, 101, 0, now)
	}
	if last != Kill {
		t.Fatalf("verdict = %v, want Kill", last)
	}
}

// TestUserAggregation 3 设备：用户级桶 = 单桶 ×3（security-spec §1.2.1）。
func TestUserAggregation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Rules = map[uint32]Rule{101: {Rate: 2, Burst: 4, Capacity: 4}}
	cfg.DefaultRule = cfg.Rules[101]
	cfg.KillMinFrames = 1 << 30
	l := New(cfg)
	for i := uint64(1); i <= 3; i++ {
		l.AddConn(i, 777)
	}
	if n := l.ConnCount(777); n != 3 {
		t.Fatalf("ConnCount = %d, want 3", n)
	}
	now := time.Unix(4000, 0)
	// 每连接 burst 4 → 12 帧全部 OK；用户桶 burst 12 刚好耗尽。
	for conn := uint64(1); conn <= 3; conn++ {
		for i := 0; i < 4; i++ {
			if v := l.AllowAt(conn, 777, 101, 0, now); v != OK {
				t.Fatalf("conn %d frame %d = %v, want OK", conn, i, v)
			}
		}
	}
	// 第 13 帧：连接桶与用户桶都空 → Throttle。
	if v := l.AllowAt(1, 777, 101, 0, now); v != Throttle {
		t.Fatalf("13th = %v, want Throttle", v)
	}
	// 单设备场景用户桶倍率 ×1：burst 100 规则下第 5 帧仍应放行。
	cfg2 := DefaultConfig()
	cfg2.Rules = map[uint32]Rule{101: {Rate: 100, Burst: 100, Capacity: 100}}
	cfg2.DefaultRule = cfg2.Rules[101]
	cfg2.KillMinFrames = 1 << 30
	l2 := New(cfg2)
	l2.AddConn(10, 888)
	for i := 0; i < 5; i++ {
		if v := l2.AllowAt(10, 888, 101, 0, now); v != OK {
			t.Fatalf("single-device frame %d = %v, want OK", i, v)
		}
	}
}

// TestPriorityDegrade 用户级超限时高优帧降级为 Throttle（L3）。
func TestPriorityDegrade(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Rules = map[uint32]Rule{KindPriority: {Rate: 1, Burst: 1, Capacity: 1}}
	cfg.DefaultRule = cfg.Rules[KindPriority]
	cfg.KillMinFrames = 1 << 30
	l := New(cfg)
	l.AddConn(5, 500)
	now := time.Unix(5000, 0)
	if v := l.AllowAt(5, 500, KindPriority, 0, now); v != OK {
		t.Fatalf("1st = %v", v)
	}
	if v := l.AllowAt(5, 500, KindPriority, 0, now); v != Throttle {
		t.Fatalf("2nd = %v, want Throttle", v)
	}
}

// TestRemoveConn 注销后用户聚合连接数回落。
func TestRemoveConn(t *testing.T) {
	l := newTestLimiter(Rule{Rate: 1, Burst: 1, Capacity: 1})
	l.AddConn(6, 600)
	l.AddConn(7, 600)
	if n := l.ConnCount(600); n != 2 {
		t.Fatalf("conns = %d", n)
	}
	l.RemoveConn(6)
	if n := l.ConnCount(600); n != 1 {
		t.Fatalf("conns after remove = %d", n)
	}
}

// TestAdmissionQPSGlobalBucket：V2-0 准入容量——全局 QPS 桶只对 MSG_SEND 生效，
// 默认 0 时逐字节等价旧行为。
func TestAdmissionQPSGlobalBucket(t *testing.T) {
	now := time.Now()
	// 默认（GlobalQPS=0）：不受全局桶影响，按 per-conn 规则（放宽到不限）。
	cfg := DefaultConfig()
	cfg.Rules[uint32(kinds.KindMsgSend)] = Rule{Rate: 100, Burst: 100, Capacity: 100}
	l0 := New(cfg)
	for i := 0; i < 5; i++ {
		if v := l0.AllowAt(1, 100, uint32(kinds.KindMsgSend), 0, now); v != OK {
			t.Fatalf("admission-off: got %v at i=%d", v, i)
		}
	}
	// 开启：GlobalQPS=2，Burst/Cap=2 → 前 2 帧 OK，第 3 帧 Throttle（全局桶）。
	cfg.GlobalQPS = 2
	l := New(cfg)
	if v := l.AllowAt(2, 200, uint32(kinds.KindMsgSend), 0, now); v != OK {
		t.Fatalf("admission first = %v", v)
	}
	if v := l.AllowAt(2, 200, uint32(kinds.KindMsgSend), 0, now); v != OK {
		t.Fatalf("admission second = %v", v)
	}
	if v := l.AllowAt(2, 200, uint32(kinds.KindMsgSend), 0, now); v != Throttle {
		t.Fatalf("admission third = %v, want Throttle", v)
	}
	// 非 MSG_SEND（typing）不消耗全局桶：仍按自身规则（不限）放行。
	if v := l.AllowAt(2, 200, uint32(kinds.KindTyping), 0, now); v != OK {
		t.Fatalf("typing must not consume global bucket: %v", v)
	}
	// 时间推进后令牌恢复。
	if v := l.AllowAt(2, 200, uint32(kinds.KindMsgSend), 0, now.Add(time.Second)); v != OK {
		t.Fatalf("admission after 1s = %v", v)
	}
}
