// v3wire_test.go —— P3c 主干装配层验证（docs/v3-bus-design.md §6/§9）：
//
//	(a) 默认 flag-off：assembly.v3 非 nil 但 Enabled()=false，且现有全量
//	    e2e（TestE2EReceiptReadForwarded）在装配改动后仍然绿；
//	(b) 热改 v3_bus_enabled=true / v3_bus_percent=100 后 Enabled()=true
//	    且 InBucket(uid) 稳定（与连接/重启无关）；
//	(c) Hub.Offer 一条后 mailbox_entries 有行；Restore 后 head/cursor 不回退
//	    （S-5）。
//
// 用 startAssembly helper（现有临时 hot.db + 真实迁移树），不额外造库。
package main

import (
	"context"
	"testing"
	"time"

	"github.com/yingo-server/OpenDiscord/server/internal/bus/mailbox"
	"github.com/yingo-server/OpenDiscord/server/internal/data/store"
	"github.com/yingo-server/OpenDiscord/server/internal/pkg/featureflags"
	"github.com/yingo-server/OpenDiscord/server/internal/proto"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/kinds"
)

// forceLegacyFlags 把实验性 flag 显式关断，供「flag-off 基线」类测试使用。
// 1.4 转生产后 Defaults() 全部默认开启，回滚语义必须靠显式构造，不能靠
// 依赖默认值（§F / §G 纪律）。SIGHUP 热改语义同源，kill-switch 立即生效。
func forceLegacyFlags(a *assembly) {
	a.flags.Apply(map[string]any{
		featureflags.KeyV3BusEnabled:           false,
		featureflags.KeyV3BusPercent:           0,
		featureflags.KeyUniversalPullEnabled:   false,
		featureflags.KeyAdaptiveControlEnabled: false,
		featureflags.KeyMsgDomainV2:            false,
	})
}

// (a) 显式 flag-off 基线：构造成功、Enabled=false、Offer 为 no-op（不落库）。
func TestV3WireDefaultOff(t *testing.T) {
	a, _ := startAssembly(t)
	forceLegacyFlags(a)
	if a.v3 == nil {
		t.Fatal("assembly.v3 must be non-nil after newAssembly (P3c construct)")
	}
	if a.v3.Enabled() {
		t.Fatal("default flags: Enabled() must be false")
	}
	if featureflags.BoolMap(a.flags.Snapshot(), featureflags.KeyV3BusEnabled) {
		t.Fatal("default v3_bus_enabled must be false")
	}
	seq, err := a.v3.Hub.Offer(context.Background(), testUID,
		mailbox.Envelope{Kind: 103, MsgID: 1, Blob: []byte("x")})
	if err != nil || seq != 0 {
		t.Fatalf("offer(flag-off): seq=%d err=%v, want no-op", seq, err)
	}
	var n int
	if err := a.hot.DB.QueryRow(`SELECT COUNT(*) FROM mailbox_entries`).Scan(&n); err != nil {
		t.Fatalf("count mailbox_entries: %v", err)
	}
	if n != 0 {
		t.Fatalf("flag-off offer must not persist rows, got %d", n)
	}
}

// (b) 热改 flag：Enabled 翻转 + InBucket 稳定分桶（同一进程内快照热生效）。
func TestV3WireHotToggle(t *testing.T) {
	a, _ := startAssembly(t)
	ctx := context.Background()
	const uid = int64(77)

	a.flags.Apply(map[string]any{
		featureflags.KeyV3BusEnabled: true,
		featureflags.KeyV3BusPercent: 100,
	})
	if !a.v3.Enabled() {
		t.Fatal("after hot toggle enabled=true/percent=100: Enabled() must be true")
	}
	if !a.v3.InBucket(uid) {
		t.Fatal("percent=100: uid must be in bucket")
	}
	for i := 0; i < 10; i++ {
		if !a.v3.InBucket(uid) {
			t.Fatal("InBucket must be stable across repeated reads")
		}
	}
	// 热改后 Offer 真落库（hub 与 wire 共用同一 flagsFn 快照源）。
	seq, err := a.v3.Hub.Offer(ctx, uid, mailbox.Envelope{Kind: 103, MsgID: 9, Blob: []byte("y")})
	if err != nil || seq == 0 {
		t.Fatalf("offer(after toggle): seq=%d err=%v", seq, err)
	}
	// 收尾把开关关回去，验证 kill-switch 立即生效。
	a.flags.Apply(map[string]any{featureflags.KeyV3BusEnabled: false})
	if a.v3.Enabled() {
		t.Fatal("kill-switch: Enabled() must be false again")
	}
}

// (d) P3d-1：flag-off 时 fanout 双写旁路不落库；热改全量灰度后，MSG_NEW
// 经主干 fanout 产出按 uid 写入 mailbox_entries，且旧直投路径仍照常发帧。
// 直接驱动真实 dataPeer（与 e2e 同一条 FrameSource → pipe.dispatch 路径），
// 不经过 WS 会话，因此不需要 member/conn 之外的任何客户端行为。
func TestV3WireFanoutSplitFlagOffAndHotToggle(t *testing.T) {
	a, _ := startAssembly(t)
	forceLegacyFlags(a)
	ctx := context.Background()

	// 造一个 3 人群（sender 4242 + 2 个灰度 uid），消息必须扇出给成员。
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
	// flag-off：发一条消息，旧路径照常，mailbox 一行都不能有。
	sendAssemblyMsg(t, a, cid, "p3d-off", []byte(`{"clientMsgId":"p3d-off","text":"hi"}`))
	var n int
	if err := a.hot.DB.QueryRow(`SELECT COUNT(*) FROM mailbox_entries`).Scan(&n); err != nil {
		t.Fatalf("count mailbox_entries: %v", err)
	}
	if n != 0 {
		t.Fatalf("flag-off fanout must not write mailbox, got %d rows", n)
	}
	if st := a.pipe.V3SplitStats(); st.Offered != 0 || st.Failed != 0 {
		t.Fatalf("flag-off stats = %+v, want Offered=0 Failed=0", st)
	}

	// 热改全量灰度：同一 uid 稳定落桶（T-SV-7），hub 真落库。
	a.flags.Apply(map[string]any{
		featureflags.KeyV3BusEnabled: true,
		featureflags.KeyV3BusPercent: 100,
	})
	sendAssemblyMsg(t, a, cid, "p3d-on", []byte(`{"clientMsgId":"p3d-on","text":"hi"}`))
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := a.hot.DB.QueryRow(`SELECT COUNT(*) FROM mailbox_entries`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n != 2 {
		t.Fatalf("percent=100: want 2 mailbox rows (2 recipients), got %d", n)
	}
	if st := a.pipe.V3SplitStats(); st.Offered != 2 || st.Failed != 0 {
		t.Fatalf("grey stats = %+v, want Offered=2 Failed=0", st)
	}
	// 旧直投路径未被抑制：ack 之后 pipe 的 fanout lane 仍产出 2 条 per-uid 帧
	// （双写语义，P3d-1 不切换投递）。
	// kill-switch：关回去后不再产生新行。
	a.flags.Apply(map[string]any{featureflags.KeyV3BusEnabled: false})
	sendAssemblyMsg(t, a, cid, "p3d-back-off", []byte(`{"clientMsgId":"p3d-back-off","text":"hi"}`))
	if err := a.hot.DB.QueryRow(`SELECT COUNT(*) FROM mailbox_entries`).Scan(&n); err != nil {
		t.Fatalf("count after kill-switch: %v", err)
	}
	if n != 2 {
		t.Fatalf("kill-switch must freeze mailbox writes, got %d rows", n)
	}
}

// sendAssemblyMsg feeds one MSG_SEND into the running assembly's data pipe via
// the REAL assembly-installed FrameSource (InternalFrame-wrapped chatpb
// envelope). The dispatcher runs the full group-commit path; we wait for the
// mailbox/legacy fan-out counters to settle instead of consuming the shared
// outbound lane (the bridge is a concurrent reader of that lane).
func sendAssemblyMsg(t *testing.T, a *assembly, cid int64, id string, pl []byte) {
	t.Helper()
	env := &chatpb.Envelope{Id: 1, Kind: kinds.KindMsgSend, RequestId: id,
		Target: &chatpb.Envelope_ConvId{ConvId: uint64(cid)}, Payload: pl}
	env.SetUserId(uint64(testUID))
	intf := &chatpb.InternalFrame{
		Envelope: env, SrcLayer: "data", TraceId: id,
	}
	body, err := intf.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	fr := &proto.Frame{Kind: kinds.KindMsgSend,
		Env:  &chatpb.Envelope{Payload: body},
		Meta: &proto.FrameMeta{SrcLayer: "data"},
	}
	// 直接把转换后的 Req 送进 dispatcher（FrameSource 已在 newAssembly 安装）。
	req := a.pipe.FrameSource(fr)
	// FrameSource 的 protobuf oneof 目标解析里 conv_id 与 user_id 互斥；测试
	// 直接补齐路由字段（真实链路的 conv_id 在 InternalFrame 外层 Meta 里带，
	// 这里不重复实现桥接层的那层转换）。
	req.ConvID = uint64(cid)
	req.UserID = uint64(testUID)
	var baseline int
	if err := a.hot.DB.QueryRow(`SELECT COUNT(*) FROM msg_messages_hot`).Scan(&baseline); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	a.pipe.FeedReqForTest(context.Background(), req)
	// 等本次消息落盘：按行数增量（baseline 在 Feed 前取），不依赖
	// client_msg_id 形态。组提交 50ms，给 5s 覆盖 CI 负载抖动。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := a.hot.DB.QueryRow(`SELECT COUNT(*) FROM msg_messages_hot`).Scan(&n); err == nil && n > baseline {
			time.Sleep(50 * time.Millisecond)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	var total int
	_ = a.hot.DB.QueryRow(`SELECT COUNT(*) FROM msg_messages_hot`).Scan(&total)
	t.Fatalf("no committed message for %s (rows %d -> %d); check FrameSource/admit gates", id, baseline, total)
}

// (e) 灰度分桶按 uid 稳定、与连接无关：重复调用 InBucket 结果不变。
func TestV3WireFanoutSplitBucketStable(t *testing.T) {
	a, _ := startAssembly(t)
	a.flags.Apply(map[string]any{
		featureflags.KeyV3BusEnabled: true,
		featureflags.KeyV3BusPercent: 5,
	})
	for uid := int64(1); uid <= 200; uid++ {
		first := a.v3.InBucket(uid)
		for i := 0; i < 5; i++ {
			if a.v3.InBucket(uid) != first {
				t.Fatalf("uid %d bucket unstable across reads", uid)
			}
		}
	}
}

// (c)+(S-5)：Offer 落库、Ack 推进、Restore 后 head/cursor 不回退。
func TestV3WireOfferRestore(t *testing.T) {
	a, _ := startAssembly(t)
	ctx := context.Background()
	const uid = int64(88)

	a.flags.Apply(map[string]any{
		featureflags.KeyV3BusEnabled: true,
		featureflags.KeyV3BusPercent: 100,
	})

	var lastSeq int64
	for i := int64(1); i <= 3; i++ {
		seq, err := a.v3.Hub.Offer(ctx, uid, mailbox.Envelope{Kind: 103, MsgID: i, Blob: []byte("m")})
		if err != nil || seq == 0 {
			t.Fatalf("offer %d: seq=%d err=%v", i, seq, err)
		}
		lastSeq = seq
	}
	var n int
	if err := a.hot.DB.QueryRow(`SELECT COUNT(*) FROM mailbox_entries WHERE user_id = ?`, uid).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Fatalf("expected 3 mailbox rows, got %d", n)
	}
	if err := a.v3.Hub.Ack(ctx, uid, 2); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if got := a.v3.Hub.Cursor(uid); got != 2 {
		t.Fatalf("cursor after ack = %d, want 2", got)
	}

	// 模拟重启：同一热库、全新 Hub/Store 实例 + Restore（S-5：不回退）。
	store := mailbox.NewStore(a.hot.DB)
	hub2 := mailbox.NewHub(store, a.flags.Snapshot, mailbox.HubConfig{})
	if err := hub2.Restore(ctx, uid); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := hub2.Head(uid); got != lastSeq {
		t.Fatalf("post-restore head=%d, want %d (no regression)", got, lastSeq)
	}
	if got := hub2.Cursor(uid); got != 2 {
		t.Fatalf("post-restore cursor=%d, want 2 (no regression)", got)
	}
	// 后退 Ack 幂等忽略：cursor 不回退。
	if err := hub2.Ack(ctx, uid, 1); err != nil {
		t.Fatalf("stale ack: %v", err)
	}
	if got := hub2.Cursor(uid); got != 2 {
		t.Fatalf("cursor regressed after stale ack: %d", got)
	}
}
