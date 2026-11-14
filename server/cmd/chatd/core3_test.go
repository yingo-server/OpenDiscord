// core3_test.go —— CORE-3 测试补齐（docs/lts-1.4-acceptance.md §A）。
//
// 三个测试，全部用现有 helper（startAssembly / pair / sendMsg / writeJSONEnv /
// sendAssemblyMsg），不 mock、不改任何生产逻辑：
//
//	T1 TestCORE3AdaptiveHotEffectAndStaticFallback
//	    控制器热生效：adaptive_control_enabled false→true 后 effective[cid]
//	    经 AdaptiveWrite 生效（e2e 以水位线节奏可观测）；AdaptiveClear 后
//	    回退静态 50/2000；越界值拒载（spec §2.2）。
//	T2 TestCORE3GreySwitchNoDoubleDelivery
//	    灰度切换不双投递：同一 uid 的一条消息在 flag-on 时只经一条路径
//	    （mailbox 承载 + 旧直投被抑制），flag-off 只走 legacy（mailbox 零行）。
//	T3 TestCORE3OldClientKeepsInlineUnderUniversalPull
//	    旧客户端隔离：universal_pull_enabled=true 时无 kind14 能力的连接
//	    仍走 inline 102（不漏消息）；上报能力后改收 kind 14，两路互斥。
//
// 前提说明（§A 第 3 条纪律：不发明协议字段）：
//
//	「连接是否具备 kind 14 能力」由既有字段承载 —— proto Hello.pull_capable
//	（字段号 10）与 CLIENT_CAPS(18) 上报帧（kinds.go / kinds.ts 已登记），
//	user 层 session 能力表按 uid 聚合（任一连接未上报 ⇒ 整个 uid 按老客户端
//	处理 ⇒ inline）。若该能力协商在生产代码中缺失/未接线，本文件 T3 的前提
//	不成立，此时 universal_pull_enabled 不得开启 ——【未实现·阻断生产启用】。
package main

import (
	"context"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/yingo-server/OpenDiscord/server/internal/data/store"
	"github.com/yingo-server/OpenDiscord/server/internal/pkg/featureflags"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/kinds"
	"github.com/yingo-server/OpenDiscord/server/internal/user/net/frame"
)

// ---------------------------------------------------------------- 工具

// collectKinds 在时间窗内读取某连接上的所有帧，返回它们的 kind 列表。
// 读超时 / 连接关闭都视为「读完了」，不 Fatal —— 用于统计某类帧出现几次、
// 以及某类帧在窗口内没有出现。
func collectKinds(conn *websocket.Conn, window time.Duration) []uint32 {
	var out []uint32
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		env, derr := frame.Decode(data)
		if derr != nil {
			continue
		}
		out = append(out, env.Kind)
	}
	return out
}

// countKinds 统计 kind 列表中某个 kind 的出现次数。
func countKinds(list []uint32, k uint32) int {
	n := 0
	for _, v := range list {
		if v == k {
			n++
		}
	}
	return n
}

// sendPullCapable 让一条连接上报「具备 kind 14 拉取能力」：
// CLIENT_CAPS(18) + JSON {"pull_capable":true}（§B 既有协议字段，不新造）。
// 上报后等 user 层 Router → CapSink → session.SetPullCapable 落表。
func sendPullCapable(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	writeJSONEnv(t, conn, 900, uint32(kinds.KindClientCaps), 0,
		map[string]any{"pull_capable": true})
	time.Sleep(300 * time.Millisecond) // 能力登记余量
}

// quietFlags 把与本批无关的旁路关掉，隔离被测路径：
//   - v3 off：隔离 T2/T3 的灰度 mailbox 路径（T2 自己按阶段开）；
//   - adaptive off：停掉 run() 里默认启动的控制循环（默认 flag 已全开），
//     避免哨兵键 effective[1] 的周期写入干扰水位线节奏观测。
func quietFlags(a *assembly) {
	a.flags.Apply(map[string]any{
		featureflags.KeyV3BusEnabled:           false,
		featureflags.KeyV3BusPercent:           0,
		featureflags.KeyAdaptiveControlEnabled: false,
	})
}

// ---------------------------------------------------------------- T1

// T1：控制器热生效 + AdaptiveClear 回退静态 50/2000。
//
// 观测口径（e2e，真装配）：静态口径 batch=50 / interval=60s（把时间兜底
// 拉长避免 2s 默认触发干扰计数）下，3 条消息只出 1 条水位线（首条 reason=1，
// §1.3 推进触发）；effective[cid]={batch:1, interval:10} 经 AdaptiveWrite
// 生效后每条消息都推进 → 3 条水位线；AdaptiveClear 后回到静态口径 → 0 条。
// 越界值（batch>200 / interval>2000）由 AdaptiveWrite 拒载（返回 false）。
func TestCORE3AdaptiveHotEffectAndStaticFallback(t *testing.T) {
	a, _ := startAssembly(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// 阶段 0：停掉默认控制循环（1.4 转产后 adaptive 默认 true，run() 已启动
	// 它）；等 ≥1 个控制周期让循环自行 AdaptiveClear 并退出。
	quietFlags(a)
	a.flags.Apply(map[string]any{
		featureflags.KeyUniversalPullEnabled: true,
		featureflags.KeyWatermarkBatch:       50,
		featureflags.KeyWatermarkIntervalMS:  60000,
	})
	time.Sleep(1600 * time.Millisecond)

	// 阶段 1：热开 false→true —— 装配层入口 startAdaptiveControl 创建
	// effective[cid] 表并注入 pipe（热生效的接线面）。
	a.flags.Apply(map[string]any{featureflags.KeyAdaptiveControlEnabled: true})
	startAdaptiveControl(ctx, a)
	if a.adaptiveStore == nil {
		t.Fatal("adaptive_control_enabled=true: controller must create + inject the effective[cid] table")
	}

	// 阶段 2：AdaptiveWrite 拒载越界值、接受合法值（spec §2.2 / T-AD-4）。
	if a.pipe.AdaptiveWrite(a.adaptiveStore, 1, 500, 2000) {
		t.Fatal("out-of-bounds batch (500>200) must be rejected")
	}
	if a.pipe.AdaptiveWrite(a.adaptiveStore, 1, 50, 99999) {
		t.Fatal("out-of-bounds interval (99999>2000) must be rejected")
	}
	if !a.pipe.AdaptiveWrite(a.adaptiveStore, 1, 50, 2000) {
		t.Fatal("in-bounds effective[cid] write must be accepted")
	}

	// 阶段 3：e2e 观测。两人直连会话；收水位线的连接必须具备 kind 14 能力
	// （否则按老客户端走 inline 102，见 T3）。
	a2, ca, cb, alice, _, cid := pair(t, "alice-adapt", "bob-adapt")
	_ = a2
	sendPullCapable(t, cb)

	// 3a 静态口径：无 effective[cid] 条目 → flags 50/60000 → 3 条消息 1 条水位线。
	sendMsg(t, ca, cid, alice, "adapt-static-1", "m1")
	sendMsg(t, ca, cid, alice, "adapt-static-2", "m2")
	sendMsg(t, ca, cid, alice, "adapt-static-3", "m3")
	got := collectKinds(cb, 2*time.Second)
	if n := countKinds(got, uint32(kinds.KindWatermark)); n != 1 {
		t.Fatalf("static defaults (batch=50): watermark frames = %d, want 1 (first, reason=1) (kinds=%v)", n, got)
	}

	// 3b 热生效：AdaptiveWrite(cid, batch=1, interval=10) → 每条消息推进 → 3 条。
	if !a.pipe.AdaptiveWrite(a.adaptiveStore, cid, 1, 10) {
		t.Fatal("AdaptiveWrite(batch=1, interval=10) must be accepted")
	}
	sendMsg(t, ca, cid, alice, "adapt-eff-1", "m4")
	sendMsg(t, ca, cid, alice, "adapt-eff-2", "m5")
	sendMsg(t, ca, cid, alice, "adapt-eff-3", "m6")
	got = collectKinds(cb, 2*time.Second)
	if n := countKinds(got, uint32(kinds.KindWatermark)); n != 3 {
		t.Fatalf("effective[cid] (batch=1): watermark frames = %d, want 3 (kinds=%v)", n, got)
	}

	// 3c 关断回退：AdaptiveClear（控制循环 kill-switch 对每个 state 条目做的
	// 就是这个调用）→ 回退静态 50/2000 → 0 条新水位线。
	a.pipe.AdaptiveClear(a.adaptiveStore, cid)
	sendMsg(t, ca, cid, alice, "adapt-clear-1", "m7")
	sendMsg(t, ca, cid, alice, "adapt-clear-2", "m8")
	sendMsg(t, ca, cid, alice, "adapt-clear-3", "m9")
	got = collectKinds(cb, 2*time.Second)
	if n := countKinds(got, uint32(kinds.KindWatermark)); n != 0 {
		t.Fatalf("after AdaptiveClear: watermark frames = %d, want 0 (static 50/2000 fallback) (kinds=%v)", n, got)
	}
}

// ---------------------------------------------------------------- T2

// T2：灰度切换不双投递（v3 灰度桶 = 唯一路径真相源，spec §1.5）。
//
// flag-off：mailbox 零行、v3 offer 计数 0、旧直投抑制计数 0 → 只走 legacy。
// flag-on（percent=100）：mailbox 每收件人 1 行（mailbox 路径承载），
// 同时旧直投抑制计数增长 → 同一 uid 只经一条路径到达。
// kill-switch 关断：mailbox 冻结，旧直投恢复 → 回 legacy。
func TestCORE3GreySwitchNoDoubleDelivery(t *testing.T) {
	a, _ := startAssembly(t)
	ctx := context.Background()
	quietFlags(a)
	a.flags.Apply(map[string]any{featureflags.KeyUniversalPullEnabled: false}) // 纯 legacy 102 扇出

	// 3 人群：发送者 testUID + 两个收件人（无 WS 连接，帧在 Router 层判定）。
	cid, err := a.hot.CreateConv(ctx, &store.ConvMeta{
		CType: 2, CreatedBy: testUID, CreatedAt: time.Now().Unix(),
	}, testUID)
	if err != nil {
		t.Fatalf("CreateConv: %v", err)
	}
	for _, uid := range []int64{5001, 5002} {
		if err := a.hot.AddMember(ctx, &store.Member{Cid: cid, UID: uid, Role: 1, JoinedAt: 1}); err != nil {
			t.Fatalf("AddMember: %v", err)
		}
	}

	rows := func() int {
		var n int
		if err := a.hot.DB.QueryRow(`SELECT COUNT(*) FROM mailbox_entries`).Scan(&n); err != nil {
			t.Fatalf("count mailbox_entries: %v", err)
		}
		return n
	}

	// --- flag-off：只走 legacy ---
	sendAssemblyMsg(t, a, cid, "core3-off", []byte(`{"clientMsgId":"core3-off","text":"hi"}`))
	if n := rows(); n != 0 {
		t.Fatalf("flag-off: mailbox must stay empty, got %d rows", n)
	}
	if st := a.pipe.V3SplitStats(); st.Offered != 0 || st.Failed != 0 {
		t.Fatalf("flag-off: v3 split stats = %+v, want Offered=0 Failed=0", st)
	}
	if skippedBefore := a.v3.v3LegacySkipped(); skippedBefore != 0 {
		t.Fatalf("flag-off: legacy direct must not be suppressed, skip counter = %d", skippedBefore)
	}

	// --- flag-on：mailbox 承载，旧直投被抑制（无双投递） ---
	a.flags.Apply(map[string]any{
		featureflags.KeyV3BusEnabled: true,
		featureflags.KeyV3BusPercent: 100,
	})
	sendAssemblyMsg(t, a, cid, "core3-on", []byte(`{"clientMsgId":"core3-on","text":"hi"}`))
	deadline := time.Now().Add(5 * time.Second)
	for rows() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("flag-on: mailbox rows = %d, want 2 (one per recipient)", rows())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st := a.pipe.V3SplitStats(); st.Offered != 2 || st.Failed != 0 {
		t.Fatalf("flag-on: v3 split stats = %+v, want Offered=2 Failed=0", st)
	}
	// 旧直投抑制计数必须增长：灰度 uid 的帧没有走第二条路。
	deadline = time.Now().Add(5 * time.Second)
	for a.v3.v3LegacySkipped() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("flag-on: legacy direct must be suppressed for grey uids (skip counter stayed %d)",
				a.v3.v3LegacySkipped())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// --- kill-switch：mailbox 冻结，回 legacy ---
	a.flags.Apply(map[string]any{featureflags.KeyV3BusEnabled: false})
	sendAssemblyMsg(t, a, cid, "core3-back", []byte(`{"clientMsgId":"core3-back","text":"hi"}`))
	if n := rows(); n != 2 {
		t.Fatalf("kill-switch: mailbox must stay frozen at 2 rows, got %d", n)
	}
}

// ---------------------------------------------------------------- T3

// T3：旧客户端隔离（universal_pull_enabled=true 时的按收件人能力分流）。
//
// 前提（§A 第 3 条纪律）：能力协商字段已存在 —— proto Hello.pull_capable
// （字段号 10）/ CLIENT_CAPS(18) 上报帧，user 层按 uid 聚合。
// 断言：
//   - 未上报能力的连接（老客户端）仍收到 inline 102 —— 不漏消息；
//   - 上报能力后改收 kind 14 水位线，且不再收 inline 102（互斥，无双投递）。
func TestCORE3OldClientKeepsInlineUnderUniversalPull(t *testing.T) {
	a, _ := startAssembly(t)
	quietFlags(a)
	a.flags.Apply(map[string]any{
		featureflags.KeyUniversalPullEnabled: true,
		featureflags.KeyWatermarkBatch:       50,
		featureflags.KeyWatermarkIntervalMS:  60000,
	})

	_, ca, cb, alice, _, cid := pair(t, "alice-cap", "bob-cap")
	// bob 不上报能力 ⇒ 老客户端路径。

	// 消息 1：老客户端必须收到 inline 102（不漏消息），且绝不收 kind 14。
	sendMsg(t, ca, cid, alice, "core3-old-1", "old client")
	got := collectKinds(cb, 3*time.Second)
	if n := countKinds(got, uint32(kinds.KindMsgNew)); n != 1 {
		t.Fatalf("old client (no pull_capable): MSG_NEW frames = %d, want 1 (no message loss) (kinds=%v)", n, got)
	}
	if n := countKinds(got, uint32(kinds.KindWatermark)); n != 0 {
		t.Fatalf("old client (no pull_capable): must NOT receive kind 14 watermark (kinds=%v)", got)
	}

	// bob 上报能力 ⇒ 改走水位线（互斥：不再收 inline 102）。
	sendPullCapable(t, cb)
	sendMsg(t, ca, cid, alice, "core3-new-1", "new client")
	got = collectKinds(cb, 3*time.Second)
	if n := countKinds(got, uint32(kinds.KindMsgNew)); n != 0 {
		t.Fatalf("pull-capable client: MSG_NEW frames = %d, want 0 (must switch to watermark) (kinds=%v)", n, got)
	}
	if n := countKinds(got, uint32(kinds.KindWatermark)); n == 0 {
		t.Fatalf("pull-capable client: must receive kind 14 watermark (kinds=%v)", got)
	}
}
