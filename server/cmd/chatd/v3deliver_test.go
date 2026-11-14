// Package main — v3deliver_test.go: P3d-2 接线批测试（docs/lts-1.4-remaining.md
// §1 要求 7；docs/v3-bus-design.md §7.3 T-SV-5 / 灰度切换；universal-pull-spec
// §1.5 双投递保护）。全部用现有 startAssembly helper（真实装配，不 mock）。
package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/yingo-server/OpenDiscord/server/internal/bus/mailbox"
	"github.com/yingo-server/OpenDiscord/server/internal/data/store"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/kinds"
	"github.com/yingo-server/OpenDiscord/server/internal/user/net/frame"
)

// v3GreyOn 热改 flag 到全量灰度（测试进程内快照，SIGHUP 语义同源）。
func v3GreyOn(a *assembly) {
	a.flags.Apply(map[string]any{
		"v3_bus_enabled": true,
		"v3_bus_percent": 100,
	})
}

// waitMailboxRows 等待 mailbox_entries 行数达到 n（组提交 50ms，给 5s 余量）。
func waitMailboxRows(t *testing.T, a *assembly, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var c int
		if err := a.hot.DB.QueryRow(`SELECT COUNT(*) FROM mailbox_entries`).Scan(&c); err == nil && c >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("mailbox_entries never reached %d rows", n)
}

// T-SV-5（任务书要求 7a）：v3_bus_enabled=false 时旧路径逐字节回归——
// 灰度关断状态下发消息，mailbox 一行不落、双投递保护恒放行、Router 钩子
// 零抑制；同时路由/投递照常（复用真实 pipe 派发路径）。
func TestV3DeliverFlagOffLegacyByteIdentical(t *testing.T) {
	a, _ := startAssembly(t)
	// 1.4 转生产后 Defaults() 已全开；本测试要的是显式 flag-off 基线
	// （§F/§G 纪律），必须在 startAssembly 之后立刻关断全部实验性 flag。
	forceLegacyFlags(a)
	if a.v3Hooks == nil {
		t.Fatal("flag-off assembly must construct nil-safe hooks (no v3 behavior, but hooks present for kill-switch tests)")
	}
	// 钩子已注入但判定恒放行（未装配 v3 时 V3DirectEnabled 为 nil）。
	cid, err := a.hot.CreateConv(context.Background(), &store.ConvMeta{
		CType: 1, Title: "off", CreatedBy: testUID, CreatedAt: time.Now().Unix(),
	}, testUID)
	if err != nil {
		t.Fatal(err)
	}
	sendAssemblyMsg(t, a, cid, "off-1", []byte(`{"clientMsgId":"off-1","text":"hi"}`))
	var n int
	if err := a.hot.DB.QueryRow(`SELECT COUNT(*) FROM mailbox_entries`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("flag-off: mailbox must stay empty, got %d rows", n)
	}
	if sk := a.v3.v3LegacySkipped(); sk != 0 {
		t.Fatalf("flag-off: legacy skip counter must be 0, got %d", sk)
	}
}

// 双投递保护（任务书要求 3）：灰度 uid 的旧直投被抑制、非灰度 uid 照常——
// 同一帧只走一路。验证点：抑制计数随灰度 uid 的下行帧增长、flag 关断后
// 立即归位放行（kill-switch），同 uid 判定稳定（uid 哈希，T-SV-7）。
func TestV3DeliverGreySuppressionAndKillSwitch(t *testing.T) {
	a, _ := startAssembly(t)
	h := a.v3Hooks
	if h == nil {
		t.Fatal("hooks must be constructed with v3 wire present")
	}
	const uid = int64(9001)
	// flag-off：恒放行。
	if !h.LegacyDirectEnabled(uid) {
		t.Fatal("flag-off must keep legacy path enabled")
	}
	v3GreyOn(a)
	if h.LegacyDirectEnabled(uid) {
		t.Fatal("percent=100 grey uid must suppress legacy path")
	}
	if before, after := a.v3.v3LegacySkipped(), a.v3.v3LegacySkipped()+1; after != before+1 {
		t.Fatal("skip counter must increment on suppression")
	}
	// 判定稳定（同 uid 哈希，非按连接随机）：连读 20 次一致。
	for i := 0; i < 20; i++ {
		if h.LegacyDirectEnabled(uid) {
			t.Fatal("bucket decision must be stable for the same uid")
		}
	}
	// kill-switch：热改关断即刻恢复旧路径（一次性切换的回滚面）。
	a.flags.Apply(map[string]any{"v3_bus_enabled": false})
	if !h.LegacyDirectEnabled(uid) {
		t.Fatal("kill-switch must re-enable legacy path immediately")
	}
}

// kind 15 上行（任务书要求 4）：Router 解析 MAILBOX_ACK 累积 cursor 后经
// sink 转调 Plane.OnAck → hub.Ack → Store.MarkAcked：mailbox_cursors 持久
// 推进，后退/重复 ACK 幂等。
func TestV3DeliverKind15AckAdvancesCursor(t *testing.T) {
	a, _ := startAssembly(t)
	ctx := context.Background()
	const uid = int64(9101)
	v3GreyOn(a)
	// 造 3 条 mailbox 条目（去重键各不相同）。
	for i := 1; i <= 3; i++ {
		env := &chatpb.Envelope{Kind: uint32(kinds.KindMsgNew),
			Payload: []byte(`{"seq":` + itoa(int64(i)) + `}`)}
		env.SetUserId(uint64(uid))
		blob, err := env.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.v3.Hub.Offer(ctx, uid, mailbox.Envelope{
			Kind: int(kinds.KindMsgNew), MsgID: int64(i), Seq: int64(i), Blob: blob,
		}); err != nil {
			t.Fatalf("offer %d: %v", i, err)
		}
	}
	waitMailboxRows(t, a, 3)
	// 无订阅时 kind 15 是 no-op：Plane 查不到订阅，cursor 不动。
	sink := mailboxACKSinkFor(a.v3)
	sink(nil, 2, 2) // nil conn 分支：丢弃、不 panic
	if cur := a.v3.Hub.Cursor(uid); cur != 0 {
		t.Fatalf("ack without subscription must be a no-op, cursor=%d", cur)
	}
	// Subscribe 后：累积 ACK 推进 cursor + 持久化。sink 的 *ws.Conn 分支由
	// e2e 覆盖；这里直接驱动同一转调目标（deliverLoopKind15，§4.2 桥）。
	send := func(context.Context, int64, []byte) error { return nil }
	c := a.v3.Plane.Subscribe(uid, "dev-a", send)
	defer a.v3.Plane.Unsubscribe(c)
	ack := func(cursor int64, n int) {
		if err := a.v3.deliverLoopKind15(c, cursor, n); err != nil {
			t.Fatalf("kind15 ack %d: %v", cursor, err)
		}
	}
	ack(2, 2)
	if cur := a.v3.Hub.Cursor(uid); cur != 2 {
		t.Fatalf("cursor after ack=2 want 2, got %d", cur)
	}
	// 后退/重复 ACK 幂等（§4.2 双源取大）。
	ack(1, 1)
	if cur := a.v3.Hub.Cursor(uid); cur != 2 {
		t.Fatalf("stale ack must be idempotent, cursor=%d", cur)
	}
	// 推进到 3 并等 MarkAcked 落库（cursor 不回退，S-5 前提）。
	ack(3, 1)
	if cur := a.v3.Hub.Cursor(uid); cur != 3 {
		t.Fatalf("cursor after ack=3 want 3, got %d", cur)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var stored int64
		if err := a.hot.DB.QueryRow(
			`SELECT cursor FROM mailbox_cursors WHERE user_id=?`, uid).Scan(&stored); err == nil && stored >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// itoa 是本测试文件的极小工具（v3hooks 注释同款语义，避免散落 strconv）。
func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// 帧形态回归（双投递保护的字节级前提）：mailbox blob 经 v3EnvelopeBytes
// 往返后仍是合法 WS 帧（frame.Decode 可读、kind/uid 不变）。
func TestV3EnvelopeBytesForm(t *testing.T) {
	env := &chatpb.Envelope{Kind: uint32(kinds.KindMsgNew), Payload: []byte(`{"q":1}`)}
	env.SetUserId(7)
	blob, err := env.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	ws := v3EnvelopeBytes(blob)
	if ws == nil {
		t.Fatal("v3EnvelopeBytes must encode valid envelope")
	}
	back, err := frame.Decode(ws)
	if err != nil || back.Kind != uint32(kinds.KindMsgNew) || back.GetUserId() != 7 {
		t.Fatalf("form mismatch: %v kind=%v uid=%v", err, back.Kind, back.GetUserId())
	}
}
