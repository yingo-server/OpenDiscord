package adaptive

import (
	"errors"
	"testing"
	"time"
)

// ---------- 假时钟 / 假审计 / 假 dump ----------

type fakeClock struct{ t time.Time }

func (f *fakeClock) Now() time.Time { return f.t }
func (f *fakeClock) Advance(d time.Duration) {
	f.t = f.t.Add(d)
}

type recEvent struct {
	kind   EventKind
	cid    uint64
	old    Effective
	new    Effective
	reason string
}

type fakeAuditor struct{ events []Event }

func (f *fakeAuditor) AdaptiveEvent(ev Event) { f.events = append(f.events, ev) }

type fakeDumper struct{ dumps []Event }

func (f *fakeDumper) AdaptiveDump(ev Event) { f.dumps = append(f.dumps, ev) }

// testConfig：全部策略数字由测试注入（对应生产装配的 1/200/10/2000、25%、1s、98/95、50/2000）。
func testConfig() Config {
	return Config{
		BatchMin: 1, BatchMax: 200,
		IntervalMinMS: 10, IntervalMaxMS: 2000,
		DefaultBatch: 50, DefaultIntervalMS: 2000,
		SoftQueueDepth: 256, FirmQueueDepth: 1024,
		AckHealthyPct: 98, AckCongestedPct: 95,
		P95RegressFactor: 2,
		AlphaIntervalMS:  10, AlphaBatch: 1,
		BetaNum: 2, BetaDen: 1,
		RateLimitNum: 1, RateLimitDen: 4,
		Cooldown:               1 * time.Second,
		MaxConsecutiveFailures: 2,
		AdaptiveEnabled:        true,
		Version:                "test-1.4",
	}
}

func healthySignals() (Signals, Global) {
	return Signals{
		QueueDepth: 10, DroppedDelta: 0, AckRatePct: 99,
		HasQueue: true, HasAckSample: true,
		HasP95Base: true, P95MS: 50, P95SteadyMS: 100,
	}, Global{PausedConns: 0, Backpressured: 0}
}

func congestedSignals() (Signals, Global) {
	return Signals{
		QueueDepth: 2048, DroppedDelta: 3, AckRatePct: 80,
		HasQueue: true, HasAckSample: true,
	}, Global{PausedConns: 0, Backpressured: 0}
}

func newCtl(clock *fakeClock) *Controller {
	cfg := testConfig()
	cfg.AdaptiveEnabled = true
	return New(cfg, clock.Now)
}

// seedFromDefault：首个 Tick 让控制器从静态默认 50/2000 起控（不写变更事件）。
func seedFromDefault(c *Controller, prev State) (Effective, State) {
	sig, g := healthySignals()
	return c.Tick(nil, nil, 1, sig, g, prev)
}

// ---------- T-AD-1：无拥塞 → 单调下降，钳在 10ms / 1，不击穿硬界 ----------

func TestTAD1_NoCongestionMonotonicDecreaseClampsAtBounds(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	c := newCtl(clock)
	aud, dum := &fakeAuditor{}, &fakeDumper{}

	eff, st := seedFromDefault(c, State{})
	if eff.IntervalMS != 2000 || eff.Batch != 50 {
		t.Fatalf("seed from default: got %v", eff)
	}

	prevInterval, prevBatch := eff.IntervalMS, eff.Batch
	for i := 0; i < 500; i++ {
		clock.Advance(2 * time.Second) // 越过冷却
		sig, g := healthySignals()
		eff, st = c.Tick(aud, dum, 1, sig, g, st)
		if eff.IntervalMS > prevInterval || eff.Batch > prevBatch {
			t.Fatalf("not monotonic decreasing at step %d: %v (prev %d/%d)", i, eff, prevInterval, prevBatch)
		}
		if eff.IntervalMS < 10 || eff.Batch < 1 {
			t.Fatalf("hard lower bound breached: %v", eff)
		}
		prevInterval, prevBatch = eff.IntervalMS, eff.Batch
		if eff.IntervalMS == 10 && eff.Batch == 1 {
			break
		}
	}
	if eff.IntervalMS != 10 || eff.Batch != 1 {
		t.Fatalf("should clamp at 10ms/1, got %v", eff)
	}
}

// ---------- T-AD-2：拥塞 → 乘性上升钳在 2000/200；转健康可回落，不死锁 ----------

func TestTAD2_CongestionMultiplicativeRiseThenRecover(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	c := newCtl(clock)
	eff, st := seedFromDefault(c, State{})

	// 拥塞上升：最终钳在 2000/200
	for i := 0; i < 200; i++ {
		clock.Advance(2 * time.Second)
		sig, g := congestedSignals()
		eff, st = c.Tick(nil, nil, 1, sig, g, st)
		if eff.IntervalMS > 2000 || eff.Batch > 200 {
			t.Fatalf("hard upper bound breached: %v", eff)
		}
		if eff.IntervalMS == 2000 && eff.Batch == 200 {
			break
		}
	}
	if eff.IntervalMS != 2000 || eff.Batch != 200 {
		t.Fatalf("should clamp at 2000ms/200, got %v", eff)
	}

	// 转健康：可回落（interval 至少降低一次），不死锁
	fell := false
	for i := 0; i < 100; i++ {
		clock.Advance(2 * time.Second)
		sig, g := healthySignals()
		prevInterval := eff.IntervalMS
		eff, st = c.Tick(nil, nil, 1, sig, g, st)
		if eff.IntervalMS < prevInterval {
			fell = true
			break
		}
	}
	if !fell {
		t.Fatalf("stuck at congestion bound; should recover when healthy")
	}
}

// ---------- T-AD-3：单次变更 ≤25%；1s 冷却内只一次变更；边界另一维可不动 ----------

func TestTAD3_RateLimitCooldownAndIndependentBounds(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	cfg := testConfig()
	// 收紧速率限制断言用的乘性系数放大场景：从默认 2000 加性一步只减 25% 上限
	c := New(cfg, clock.Now)
	eff, st := seedFromDefault(c, State{})
	oldInterval, oldBatch := eff.IntervalMS, eff.Batch

	clock.Advance(2 * time.Second)
	sig, g := healthySignals()
	eff, st = c.Tick(nil, nil, 1, sig, g, st)
	if d := oldInterval - eff.IntervalMS; d > oldInterval/4 {
		t.Fatalf("interval change %d exceeds 25%% of %d", d, oldInterval)
	}
	if d := oldBatch - eff.Batch; d > oldBatch/4 {
		t.Fatalf("batch change %d exceeds 25%% of %d", d, oldBatch)
	}

	// 1s 内重复 tick：只有一次变更（之后保持）
	t2 := st.LastChange
	eff2, st2 := c.Tick(nil, nil, 1, sig, g, st)
	if eff2 != eff || st2.LastChange != t2 {
		t.Fatalf("cooldown violated: changed again within 1s: %v -> %v", eff, eff2)
	}

	// 边界处另一维可不动：interval 已在 10ms 下界时，健康信号下 batch 仍可下降；
	// 反复 tick 后两维分别独立钳在 10ms / 1（边界处允许每 tick 只动一维）
	st.Eff = Effective{Batch: 8, IntervalMS: 10}
	st.LastChange = clock.t
	for i := 0; i < 50; i++ {
		clock.Advance(2 * time.Second)
		eff, st = c.Tick(nil, nil, 1, sig, g, st)
		if eff.IntervalMS < 10 || eff.Batch < 1 {
			t.Fatalf("hard lower bound breached: %v", eff)
		}
		if eff.IntervalMS == 10 && eff.Batch == 1 {
			break
		}
	}
	if eff.IntervalMS != 10 || eff.Batch != 1 {
		t.Fatalf("should clamp at 10ms/1, got %v", eff)
	}
	// interval 在下界时该次 tick 只动 batch（另一维可不动）
	st.Eff = Effective{Batch: 8, IntervalMS: 10}
	st.LastChange = clock.t
	clock.Advance(2 * time.Second)
	eff, _ = c.Tick(nil, nil, 1, sig, g, st)
	if eff.IntervalMS != 10 || eff.Batch >= 8 {
		t.Fatalf("boundary: interval stays, batch decreases: %v", eff)
	}
}

// ---------- T-AD-4：越界值拒载，effective 保持旧值，各产生一条 adaptive.reject ----------

func TestTAD4_OutOfBoundsRejected(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	c := newCtl(clock)
	aud, dum := &fakeAuditor{}, &fakeDumper{}
	prev := State{Eff: Effective{Batch: 50, IntervalMS: 2000}, HasEff: true, LastChange: clock.t}

	cases := []struct {
		name            string
		batch, interval any
	}{
		{"batch_zero", 0, 1000},
		{"batch_over", 201, 1000},
		{"interval_zero", 50, 0},
		{"interval_over", 50, 2001},
		{"string_batch", "101", 1000}, // 非整数字符串：ToInt 拒绝
	}
	for _, tc := range cases {
		clock.Advance(1 * time.Second)
		before := len(aud.events)
		eff, st := c.StaticOverride(aud, dum, 7, prev, tc.batch, tc.interval)
		if eff != prev.Eff {
			t.Fatalf("%s: effective must keep old value, got %v", tc.name, eff)
		}
		if st.Eff != prev.Eff {
			t.Fatalf("%s: state must keep old value", tc.name)
		}
		if len(aud.events) != before+1 {
			t.Fatalf("%s: expected one reject audit, got %d", tc.name, len(aud.events)-before)
		}
		ev := aud.events[len(aud.events)-1]
		if ev.Kind != EventReject || ev.Reason != "out_of_bounds" {
			t.Fatalf("%s: wrong event %+v", tc.name, ev)
		}
	}
	// dump 同步出现 reject
	if len(dum.dumps) != len(cases) {
		t.Fatalf("expected %d dumps, got %d", len(cases), len(dum.dumps))
	}
}

// ---------- T-AD-5：panic / 静态覆盖 / adaptive_control_enabled=false → 回退 50/2000 ----------

func TestTAD5_FallbackPathsReturnStaticDefaults(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	aud, dum := &fakeAuditor{}, &fakeDumper{}

	// (a) 开关关闭
	cfg := testConfig()
	cfg.AdaptiveEnabled = false
	c := New(cfg, clock.Now)
	eff, _ := c.Tick(aud, dum, 1, Signals{}, Global{}, State{Eff: Effective{Batch: 1, IntervalMS: 10}, HasEff: true})
	if eff.Batch != 50 || eff.IntervalMS != 2000 {
		t.Fatalf("disabled should fall back to 50/2000, got %v", eff)
	}

	// (b) 静态覆盖（合法值）优先于控制器输出且不受冷却限制；覆盖 50/2000 等价默认行为
	clock.Advance(1 * time.Second)
	c2 := newCtl(clock)
	eff, _ = c2.StaticOverride(aud, dum, 1, State{Eff: Effective{Batch: 1, IntervalMS: 10}, HasEff: true}, 50, 2000)
	if eff.Batch != 50 || eff.IntervalMS != 2000 {
		t.Fatalf("static override not applied: %v", eff)
	}

	// (c) panic：SafeTick 内 panic 注入 → 保持最后合法值；连续达到阈值回退 50/2000
	clock.Advance(1 * time.Second)
	c3 := newCtl(clock)
	last := State{Eff: Effective{Batch: 30, IntervalMS: 500}, HasEff: true}
	cfgP := testConfig()
	cp := New(cfgP, clock.Now)
	cp.PanicHook = func() { panic(errors.New("injected")) } // 测试故障注入
	sig, g := congestedSignals()
	eff, st := cp.SafeTick(aud, dum, 1, sig, g, last)
	if eff.Batch != 30 || eff.IntervalMS != 500 {
		t.Fatalf("single panic must hold last effective, got %v", eff)
	}
	if st.Failures != 1 {
		t.Fatalf("failure count = %d, want 1", st.Failures)
	}
	// 第二次连续 panic → 达 MaxConsecutiveFailures，回退静态默认
	clock.Advance(2 * time.Second)
	eff, st = cp.SafeTick(aud, dum, 1, sig, g, st)
	if eff.Batch != 50 || eff.IntervalMS != 2000 {
		t.Fatalf("consecutive panic must fall back to 50/2000, got %v", eff)
	}
	// fallback 均有审计 + dump
	found := 0
	for _, ev := range aud.events {
		if ev.Kind == EventFallback {
			found++
		}
	}
	if found < 2 {
		t.Fatalf("expected fallback audits, got %d", found)
	}
	if len(dum.dumps) == 0 {
		t.Fatalf("fallbacks must be dumped")
	}

	// (d) Fallback 显式回退
	eff = c3.Fallback(aud, dum, 1, "no_signals")
	if eff.Batch != 50 || eff.IntervalMS != 2000 {
		t.Fatalf("explicit fallback should be 50/2000, got %v", eff)
	}
}

// ---------- T-AD-6：每次变更 dump 与审计各一次，字段可回放；审计无消息正文 ----------

func TestTAD6_ChangeAuditedAndDumpedReplayable(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	c := newCtl(clock)
	aud, dum := &fakeAuditor{}, &fakeDumper{}

	_, st := seedFromDefault(c, State{})
	clock.Advance(2 * time.Second)
	sig, g := healthySignals()
	next, st2 := c.Tick(aud, dum, 42, sig, g, st)

	if len(aud.events) != 1 || len(dum.dumps) != 1 {
		t.Fatalf("expected exactly 1 audit + 1 dump, got %d/%d", len(aud.events), len(dum.dumps))
	}
	ev, dv := aud.events[0], dum.dumps[0]
	if ev.Kind != EventChange || dv.Kind != EventChange {
		t.Fatalf("kind mismatch: %s / %s", ev.Kind, dv.Kind)
	}
	if ev.CID != 42 || dv.CID != 42 {
		t.Fatalf("cid mismatch")
	}
	if ev.Old != st.Eff || ev.New != next {
		t.Fatalf("old/new mismatch: %+v -> %+v vs %+v -> %+v", ev.Old, ev.New, st.Eff, next)
	}
	if ev.Signals != sig {
		t.Fatalf("signals snapshot not recorded")
	}
	if ev.At != clock.t || dv.At != ev.At {
		t.Fatalf("timestamp not replayable: %v vs %v", ev.At, dv.At)
	}
	if ev.Version != "test-1.4" || ev.Override {
		t.Fatalf("version/override fields wrong: %+v", ev)
	}
	// st2.LastChange = ev.At → 可复算冷却行为
	if st2.LastChange != ev.At {
		t.Fatalf("cooldown timestamp not consistent")
	}
}

// ---------- T-AD-7：同一计数器口径的前提/收益（本层可测部分：信号快照同源可回放） ----------

func TestTAD7_SameCounterSnapshotBothSides(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	c := newCtl(clock)
	aud := &fakeAuditor{}

	// 同一组 Signals 结构同时作为「前提（inline 侧拥塞证据）」与「收益（拉取侧健康）」输入，
	// 控制器对两侧使用完全相同的字段与窗口——这里断言同值信号在健康/拥塞判定上确定性一致。
	sigCong, gCong := congestedSignals()
	sigHealthy, gHealthy := healthySignals()

	// 从中位起点起控（默认 2000 已在上界，乘性上升会被钳住）
	st := State{Eff: Effective{Batch: 50, IntervalMS: 1000}, HasEff: true}
	clock.Advance(2 * time.Second)
	// 拥塞信号 → 上升；同一计数器在健康场景 → 下降（方向可由快照复算）
	effC, _ := c.Tick(aud, nil, 1, sigCong, gCong, st)
	clock.Advance(2 * time.Second)
	effH, _ := c.Tick(aud, nil, 1, sigHealthy, gHealthy, State{Eff: effC, LastChange: clock.t.Add(-2 * time.Second), HasEff: true})

	if effC.IntervalMS <= st.Eff.IntervalMS || effC.Batch <= st.Eff.Batch {
		t.Fatalf("congested counter must raise both dims (same counter premise): %v -> %v", st.Eff, effC)
	}
	if effH.IntervalMS >= effC.IntervalMS || effH.Batch >= effC.Batch {
		t.Fatalf("healthy counter must lower both dims (same counter benefit): %v -> %v", effC, effH)
	}
	// 审计事件中的快照足以离线复算判定（congested / healthy 路径标注在 Reason）
	last := aud.events[len(aud.events)-1]
	if last.Reason != "aimd_additive_decrease" || last.Signals != sigHealthy {
		t.Fatalf("snapshot not replayable: %+v", last)
	}
}

// ---------- 附加：信号不足保持现值；StaticOverride 合法值生效 + 审计 ----------

func TestExtra_InsufficientSignalsHold(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	c := newCtl(clock)
	prev := State{Eff: Effective{Batch: 50, IntervalMS: 2000}, HasEff: true, LastChange: clock.t}
	clock.Advance(2 * time.Second)
	eff, _ := c.Tick(nil, nil, 1, Signals{QueueDepth: 10, AckRatePct: 99}, Global{}, prev) // 无样本标记
	if eff != prev.Eff {
		t.Fatalf("insufficient signals must hold, got %v", eff)
	}
}

func TestExtra_StaticOverrideAuditsChange(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	c := newCtl(clock)
	aud, dum := &fakeAuditor{}, &fakeDumper{}
	prev := State{Eff: Effective{Batch: 50, IntervalMS: 2000}, HasEff: true}
	eff, _ := c.StaticOverride(aud, dum, 9, prev, 100, 500)
	if eff.Batch != 100 || eff.IntervalMS != 500 || !eff.Overridden {
		t.Fatalf("override not applied: %v", eff)
	}
	if len(aud.events) != 1 || len(dum.dumps) != 1 {
		t.Fatalf("override must emit audit+dump")
	}
	if aud.events[0].Reason != "static_override" {
		t.Fatalf("reason wrong: %s", aud.events[0].Reason)
	}
}

// panic 注入辅助编译期守护：确保 errors 包用途（保留以示 panic reason 可携带 error）。
