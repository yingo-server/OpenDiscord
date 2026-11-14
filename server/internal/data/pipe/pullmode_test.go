// pullmode_test.go: T-SV-1 … T-SV-6 of docs/pull-mode-spec-v1.2.md §4.3.
//
// 全部用例都对着"可观测的帧"断言：要么走完整链路（101 → seqalloc →
// group-commit flusher → fan-out 决策 → 出站 lane），要么直接驱动 fan-out
// 决策点（watermark 频控需要一个可控时钟）。没有 mock 掉被测逻辑。
package pipe

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/yingo-server/OpenDiscord/server/internal/data/pipe/pipefakes"
	"github.com/yingo-server/OpenDiscord/server/internal/data/pipe/wire"
	"github.com/yingo-server/OpenDiscord/server/internal/data/store"
	"github.com/yingo-server/OpenDiscord/server/internal/pkg/featureflags"
	"github.com/yingo-server/OpenDiscord/server/internal/pkg/log"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/kinds"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// pullRig is a pipe with N members (creator = 1, members 2..N), a feature flag
// store installed through the PUBLIC Pipe.SetFeatureFlags, and the flusher
// running so MSG_SEND goes through the real commit → fan-out path.
type pullRig struct {
	p     *Pipe
	msgs  *pipefakes.FakeMsgStore
	convs *pipefakes.FakeConvStore
	flags *featureflags.Flags
	cid   int64
	n     int
	stop  context.CancelFunc
}

func newPullRig(t *testing.T, nMembers int) *pullRig {
	t.Helper()
	p, msgs, convs, _ := newTestPipe(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cid, err := convs.CreateConv(ctx, &store.ConvMeta{CType: 2, CreatedBy: 1, CreatedAt: 1}, 1)
	if err != nil {
		t.Fatalf("create conv: %v", err)
	}
	for uid := 2; uid <= nMembers; uid++ {
		if err := convs.AddMember(ctx, &store.Member{Cid: cid, UID: int64(uid), Role: 1, JoinedAt: 1}); err != nil {
			t.Fatalf("add member %d: %v", uid, err)
		}
	}
	go p.fl.run(ctx)
	return &pullRig{p: p, msgs: msgs, convs: convs, flags: newOffFlags(), cid: cid, n: nMembers, stop: cancel}
}

// newOffFlags 显式构造 off 语义快照（§F.2）：1.4 转生产后 Defaults() 已把
// universal_pull / adaptive_control / v3_bus / msg_domain_v2 全部默认开启，
// 需要 off 基线的测试不得再依赖 Defaults()，一律用 NewFromMap 显式构造。
func newOffFlags() *featureflags.Flags {
	return featureflags.NewFromMap(map[string]any{
		featureflags.KeyPullModeThreshold:      0,
		featureflags.KeyUniversalPullEnabled:   false,
		featureflags.KeyAdaptiveControlEnabled: false,
		featureflags.KeyV3BusEnabled:           false,
		featureflags.KeyV3BusPercent:           0,
		featureflags.KeyMsgDomainV2:            false,
	})
}

// send commits one message (waits for its ACK, i.e. the group commit is done)
// and returns the ACK so callers know the assigned seq.
func (r *pullRig) send(t *testing.T, clientMsgID string) wire.MsgAck {
	t.Helper()
	r.p.dispatchReq(context.Background(), mkReq(101, r.cid, 1,
		`{"clientMsgId":"`+clientMsgID+`","text":"hello"}`))
	return decodeAck(t, waitEnv(t, r.p.Outbound()))
}

// collectFan drains the bulk fan-out lane until it has been quiet for quiet.
func collectFan(t *testing.T, p *Pipe, quiet time.Duration) []*env {
	t.Helper()
	out := make([]*env, 0, 128)
	timer := time.NewTimer(quiet)
	defer timer.Stop()
	for {
		select {
		case e := <-p.OutboundFan():
			out = append(out, e)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(quiet)
		case <-timer.C:
			return out
		}
	}
}

func countKind(envs []*env, kind uint32) int {
	n := 0
	for _, e := range envs {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func firstOfKind(envs []*env, kind uint32) *env {
	for _, e := range envs {
		if e.Kind == kind {
			return e
		}
	}
	return nil
}

// newDeciderPlane builds a fan-out plane with an injected clock; the put
// function accepts everything so the decision logic (not the lane) is measured.
func newDeciderPlane(t *testing.T) (*fanoutPlane, *featureflags.Flags, *time.Time) {
	t.Helper()
	f := newFanoutPlane(nil, func(*env) bool { return true }, log.For("fanout-test"))
	fl := newOffFlags()
	f.setFlags(fl)
	clk := time.Unix(1700000000, 0)
	f.now = func() time.Time { return clk }
	return f, fl, &clk
}

func membersN(n int) []store.Member {
	ms := make([]store.Member, 0, n)
	for uid := 1; uid <= n; uid++ {
		ms = append(ms, store.Member{Cid: 1, UID: int64(uid), Role: 1, JoinedAt: 1})
	}
	return ms
}

// ---------------------------------------------------------------------------
// T-SV-1: threshold=100 → 101 人群 0 条逐成员 102 + ≥1 条水位线；
//         99 人群与现状逐字节一致。
// ---------------------------------------------------------------------------

func TestTSV1_PullModeSplitAtThreshold(t *testing.T) {
	// --- 101 人群（> 100）：不推 102，改推水位线 ---------------------------
	big := newPullRig(t, 101)
	big.flags.Apply(map[string]any{featureflags.KeyPullModeThreshold: 100})
	big.p.SetFeatureFlags(big.flags)
	ack := big.send(t, "big-1")
	frames := collectFan(t, big.p, 120*time.Millisecond)

	if got := countKind(frames, uint32(kinds.KindMsgNew)); got != 0 {
		t.Fatalf("pull mode emitted %d per-member MSG_NEW(102) frames, want 0", got)
	}
	wmEnv := firstOfKind(frames, uint32(kinds.KindWatermark))
	if wmEnv == nil {
		t.Fatalf("no WATERMARK(14) frame emitted; frames=%d", len(frames))
	}
	var wm wire.Watermark
	if err := wire.Unmarshal(wmEnv.Payload, &wm); err != nil {
		t.Fatalf("watermark decode: %v (%s)", err, wmEnv.Payload)
	}
	if wm.ConvID != uint64(big.cid) || wm.LatestSeq != ack.Seq || wm.MemberCount != 101 {
		t.Fatalf("watermark = %+v, want convId=%d latestSeq=%d memberCount=101", wm, big.cid, ack.Seq)
	}
	// 只发在线成员：帧地址是逐 uid 的（网关 Broadcast(uid) 只投递在线连接），
	// 发送者本人不回收自己的水位线（与 102 一致），其余 100 名成员各一条。
	if got := countKind(frames, uint32(kinds.KindWatermark)); got != 100 {
		t.Fatalf("watermark fan-out = %d frames, want 100 (one per non-sender member)", got)
	}
	for _, e := range frames {
		if e.Kind != uint32(kinds.KindWatermark) {
			t.Fatalf("unexpected kind %d in pull-mode fan-out", e.Kind)
		}
		if e.UserID == 1 {
			t.Fatalf("watermark echoed to sender uid 1")
		}
	}

	// --- 99 人群（<= 100）：与改动前逐字节一致 ----------------------------
	small := newPullRig(t, 99)
	small.flags.Apply(map[string]any{featureflags.KeyPullModeThreshold: 100})
	small.p.SetFeatureFlags(small.flags)
	sack := small.send(t, "small-1")
	smallFrames := collectFan(t, small.p, 120*time.Millisecond)

	if got := countKind(smallFrames, uint32(kinds.KindMsgNew)); got != 98 {
		t.Fatalf("inline fan-out = %d MSG_NEW frames, want 98 (99 members minus sender)", got)
	}
	if got := countKind(smallFrames, uint32(kinds.KindWatermark)); got != 0 {
		t.Fatalf("inline mode emitted %d watermarks, want 0", got)
	}
	// 逐字节：把收到的 body 与"改动前 flusher 的同一句 buildFanoutEnvs 调用"
	// 的结果比较，base（kind+requestId+payload 的编码结果）必须完全相等。
	uids := make([]uint64, 0, 98)
	uidWant := map[uint64]bool{}
	for uid := 2; uid <= 99; uid++ {
		uids = append(uids, uint64(uid))
		uidWant[uint64(uid)] = true
	}
	payload := smallFrames[0].Payload
	want, _ := buildFanoutEnvs(defaultFanoutEncode, uint32(kinds.KindMsgNew), "", payload, uint64(small.cid), uids)
	if len(want) != len(smallFrames) {
		t.Fatalf("frame count %d != expected %d", len(smallFrames), len(want))
	}
	gotUids := map[uint64]int{}
	for i := range smallFrames {
		gotUids[smallFrames[i].UserID]++
		delete(uidWant, smallFrames[i].UserID)
	}
	if len(uidWant) != 0 || len(gotUids) != len(uids) {
		t.Fatalf("frame uid set mismatch: got %v, want exactly uids %v (order-free broadcast)",
			gotUids, uids)
	}
	for i := range want {
		if smallFrames[i].UserID != want[i].UserID {
			// order-free: only membership matters; the body of frame i must match
			// the env built for THAT uid, which is identical for all uids anyway.
			_ = i
		}
		if string(smallFrames[i].base) != string(want[i].base) {
			t.Fatalf("frame %d body differs from the pre-v1.2 encoder:\n got %x\nwant %x",
				i, smallFrames[i].base, want[i].base)
		}
	}
	// 帧内容本身也必须是 MSG_NEW 语义：seq 与 ACK 一致。
	var mn struct {
		Seq uint64 `json:"seq"`
	}
	if err := json.Unmarshal(payload, &mn); err != nil || mn.Seq != sack.Seq {
		t.Fatalf("MSG_NEW payload seq = %d (err %v), want %d", mn.Seq, err, sack.Seq)
	}

	// --- 默认 flag（threshold 未设置 = 0）时 99 人群同样是 inline -----------
	def := newPullRig(t, 99) // 不调用 SetFeatureFlags：完全走编译默认值
	def.send(t, "default-1")
	defFrames := collectFan(t, def.p, 120*time.Millisecond)
	if got := countKind(defFrames, uint32(kinds.KindMsgNew)); got != 98 {
		t.Fatalf("default flags: %d MSG_NEW frames, want 98", got)
	}
	if got := countKind(defFrames, uint32(kinds.KindWatermark)); got != 0 {
		t.Fatalf("default flags: %d watermarks, want 0 (threshold must default to 0)", got)
	}
}

// ---------------------------------------------------------------------------
// T-SV-2: 频控 —— 1s 内 100 条 → ≤ ⌈100/50⌉+⌈1000/2000⌉（默认
// watermark_batch=50 / watermark_interval_ms=2000）；1 条/1s 时必须由时间兜底
// 保证"至多 interval + 1 条消息"的感知延迟（不饿死）。
// ---------------------------------------------------------------------------

func TestTSV2_WatermarkThrottle(t *testing.T) {
	f, fl, clk := newDeciderPlane(t)
	fl.Apply(map[string]any{featureflags.KeyPullModeThreshold: 100})

	// 1s 内 100 条消息（每 10ms 一条），seq 1..100。
	const total = 100
	emits := 0
	for i := 1; i <= total; i++ {
		*clk = clk.Add(10 * time.Millisecond)
		if ok, _ := f.shouldEmitWatermark(7, uint64(i)); ok {
			emits++
		}
	}
	bound := 2 + 1 // ⌈100/50⌉ + ⌈1000/2000⌉
	if emits > bound {
		t.Fatalf("watermarks in 1s = %d, want <= %d", emits, bound)
	}
	if emits != 2 {
		t.Fatalf("watermarks in 1s = %d, want 2 (one per watermark_batch of 50)", emits)
	}

	// 1 条消息/1s（低于 batch，仅靠时间兜底）：防饿死 —— 两次水位线的间隔
	// 最多 interval + 1 条消息，超过 5 条消息仍必须有水位线发出。
	f2, fl2, clk2 := newDeciderPlane(t)
	fl2.Apply(map[string]any{featureflags.KeyPullModeThreshold: 100})
	seqs := make([]uint64, 0, 5)
	for i := 1; i <= 5; i++ {
		*clk2 = clk2.Add(time.Second)
		if ok, _ := f2.shouldEmitWatermark(9, uint64(i)); ok {
			seqs = append(seqs, uint64(i))
		}
	}
	// 预期：seq1（首次）、seq3（2000ms 兜底）、seq5（再过 2000ms）；seq2/seq4 不发。
	if len(seqs) != 3 || seqs[0] != 1 || seqs[1] != 3 || seqs[2] != 5 {
		t.Fatalf("1 msg/s watermark seqs = %v, want [1 3 5] (interval bound, no starvation)", seqs)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i]-seqs[i-1] > 2 { // interval(2000ms) / 1 msg/s = 2 条消息
			t.Fatalf("watermark gap %d..%d exceeds the interval bound", seqs[i-1], seqs[i])
		}
	}

	// 时间兜底的下界：interval 内且不足 batch 的推进不发。
	f3, fl3, clk3 := newDeciderPlane(t)
	fl3.Apply(map[string]any{featureflags.KeyPullModeThreshold: 100})
	f3.shouldEmitWatermark(11, 1) // 首次必发
	*clk3 = clk3.Add(100 * time.Millisecond)
	if ok, _ := f3.shouldEmitWatermark(11, 2); ok {
		t.Fatalf("emitted before watermark_batch / watermark_interval (100ms, 1 msg)")
	}
	*clk3 = clk3.Add(2000 * time.Millisecond)
	if ok, _ := f3.shouldEmitWatermark(11, 3); !ok {
		t.Fatalf("no watermark after watermark_interval_ms (2000ms) elapsed")
	}
}

// ---------------------------------------------------------------------------
// T-SV-3: seq 未推进的窗 → 0 条水位线（重复/乱序同样不发）。
// ---------------------------------------------------------------------------

func TestTSV3_NoWatermarkWithoutSeqProgress(t *testing.T) {
	f, fl, clk := newDeciderPlane(t)
	fl.Apply(map[string]any{featureflags.KeyPullModeThreshold: 100})

	if ok, _ := f.shouldEmitWatermark(5, 42); !ok {
		t.Fatal("first watermark for a conversation should be emitted")
	}
	for i := 0; i < 20; i++ {
		*clk = clk.Add(5 * time.Second) // 时间远超 interval，但 seq 没动
		if ok, reason := f.shouldEmitWatermark(5, 42); ok {
			t.Fatalf("watermark emitted without seq progress (reason %d)", reason)
		}
		// 乱序/迟到的更小 seq 同样不得触发。
		if ok, _ := f.shouldEmitWatermark(5, 41); ok {
			t.Fatal("watermark emitted for a stale (lower) seq")
		}
	}
	// seq=0（未分配）连"首次"都不算。
	if ok, _ := f.shouldEmitWatermark(6, 0); ok {
		t.Fatal("watermark emitted for seq 0")
	}
}

// ---------------------------------------------------------------------------
// T-SV-4: flag 热改 threshold（200 → 100 → 1000）→ 行为随之切换（回滚路径实测）。
// ---------------------------------------------------------------------------

func TestTSV4_ThresholdHotChange(t *testing.T) {
	rig := newPullRig(t, 150)
	rig.p.SetFeatureFlags(rig.flags)

	// threshold=200：150 人 <= 200 → inline。
	rig.flags.Apply(map[string]any{featureflags.KeyPullModeThreshold: 200})
	rig.send(t, "hot-1")
	inline := collectFan(t, rig.p, 100*time.Millisecond)
	if got := countKind(inline, uint32(kinds.KindMsgNew)); got != 149 {
		t.Fatalf("threshold=200: %d MSG_NEW frames, want 149", got)
	}
	if got := countKind(inline, uint32(kinds.KindWatermark)); got != 0 {
		t.Fatalf("threshold=200: %d watermarks, want 0", got)
	}

	// threshold 热改 200 → 100：150 人 > 100 → 拉取模式，逐成员 102 归零。
	rig.flags.Apply(map[string]any{featureflags.KeyPullModeThreshold: 100})
	rig.send(t, "hot-2")
	pull := collectFan(t, rig.p, 120*time.Millisecond)
	if got := countKind(pull, uint32(kinds.KindMsgNew)); got != 0 {
		t.Fatalf("threshold=100: %d MSG_NEW frames, want 0", got)
	}
	if got := countKind(pull, uint32(kinds.KindWatermark)); got == 0 {
		t.Fatal("threshold=100: no watermark after hot change")
	}

	// 回滚：threshold 热改回 1000 → 立即回到 inline（无需重启）。
	rig.flags.Apply(map[string]any{featureflags.KeyPullModeThreshold: 1000})
	rig.send(t, "hot-3")
	back := collectFan(t, rig.p, 100*time.Millisecond)
	if got := countKind(back, uint32(kinds.KindMsgNew)); got != 149 {
		t.Fatalf("rollback to inline: %d MSG_NEW frames, want 149", got)
	}
	if got := countKind(back, uint32(kinds.KindWatermark)); got != 0 {
		t.Fatalf("rollback to inline: %d watermarks, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// T-SV-5: Watermark.latest_seq 引用的消息可被 920 since_seq 读到（时序约束）。
// ---------------------------------------------------------------------------

func TestTSV5_WatermarkSeqIsReadable(t *testing.T) {
	rig := newPullRig(t, 101)
	rig.flags.Apply(map[string]any{featureflags.KeyPullModeThreshold: 100})
	rig.p.SetFeatureFlags(rig.flags)

	// 发 60 条：默认 watermark_batch=50 → 首次(seq=1) 一条，之后推进满 50 条
	// 在 seq=51 再一条；52..60 不足一个批次，也远未到 2000ms 时间兜底。
	var last wire.MsgAck
	for i := 1; i <= 60; i++ {
		last = rig.send(t, "wm-"+itoa(int64(i)))
	}
	if last.Seq != 60 {
		t.Fatalf("last ack seq = %d, want 60", last.Seq)
	}
	frames := collectFan(t, rig.p, 150*time.Millisecond)
	if got := len(frames); got == 0 {
		t.Fatal("no watermark")
	}
	// 最后一条水位线：latestSeq 必须是已经落库的 seq（seq=51）。
	lastEnv := frames[len(frames)-1]
	var wm wire.Watermark
	if err := wire.Unmarshal(lastEnv.Payload, &wm); err != nil {
		t.Fatalf("watermark decode: %v", err)
	}
	if wm.LatestSeq != 51 || wm.Reason != 0 {
		t.Fatalf("latestSeq = %d reason = %d, want 51/0 (first at seq=1 + one batch of 50)", wm.LatestSeq, wm.Reason)
	}

	// 客户端按 [lastSeq+1, latestSeq] = [51,51] 拉取：必须非空。
	// 前向补拉走 8 SYNC（catchup：seq 严格大于 sinceSeq、升序），这正是
	// §2.1 客户端的路径。
	rig.p.dispatchReq(context.Background(), Req{Kind: 8, ConvID: uint64(rig.cid), UserID: 1,
		Payload: []byte(`{"convId":` + itoa(rig.cid) + `,"sinceSeq":` + itoa(int64(wm.LatestSeq-1)) + `,"limit":200}`)})
	syncEnv := waitEnv(t, rig.p.Outbound())
	if syncEnv.Kind != uint32(kinds.KindMsgNew) {
		t.Fatalf("SYNC reply kind = %d, want MSG_NEW(102)", syncEnv.Kind)
	}
	var got struct {
		Seq uint64 `json:"seq"`
	}
	if err := json.Unmarshal(syncEnv.Payload, &got); err != nil || got.Seq != wm.LatestSeq {
		t.Fatalf("SYNC sinceSeq=%d returned seq=%d (err %v), want exactly the watermark seq",
			wm.LatestSeq-1, got.Seq, err)
	}

	// 读到那条消息的另一种方式：920 的 beforeSeq 游标（seq < beforeSeq）。
	// 顺带证明 latest_seq 指向的行确实已经在库里（§1.1 生成时序约束）。
	rig.p.dispatchReq(context.Background(), Req{Kind: 920, ConvID: uint64(rig.cid), UserID: 1,
		Payload: []byte(`{"convId":` + itoa(rig.cid) + `,"beforeSeq":` + itoa(int64(wm.LatestSeq+1)) + `,"limit":1}`)})
	// SYNC catch-up replayed 10 MSG_NEW frames on the reply lane; only one was
	// consumed above. Drain the rest until the 921 result frame arrives —
	// otherwise the next waitEnv would read a stale 102 and decode it as an
	// empty ReadHistoryResult.
	var res wire.ReadHistoryResult
	for {
		e := waitEnv(t, rig.p.Outbound())
		if e.Kind != uint32(kinds.KindReadHistoryResult) {
			continue
		}
		if err := wire.Unmarshal(e.Payload, &res); err != nil {
			t.Fatalf("920 decode: %v", err)
		}
		break
	}
	if len(res.Messages) != 1 || res.Messages[0].Seq != wm.LatestSeq {
		t.Fatalf("920 beforeSeq=%d read %d messages (seqs %v), want the watermark seq %d",
			wm.LatestSeq+1, len(res.Messages), seqsOf(res.Messages), wm.LatestSeq)
	}
}

func seqsOf(msgs []wire.MsgNew) []uint64 {
	out := make([]uint64, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Seq)
	}
	return out
}

// ---------------------------------------------------------------------------
// T-SV-6: 202 members_omitted —— 旧客户端（忽略未知字段）解析不报错。
// ---------------------------------------------------------------------------

// legacyConvPayloadJSON is the pre-v1.2 client view of ConvPayload: fields 1-6
// only, no members_omitted / member_count.
type legacyConvPayloadJSON struct {
	ConvID    uint64        `json:"convId"`
	Title     string        `json:"title"`
	ConvType  uint32        `json:"convType"`
	CreatedAt int64         `json:"createdAt"`
	Members   []wire.Member `json:"members"`
	Version   uint64        `json:"version"`
}

func TestTSV6_MembersOmittedBackwardCompatible(t *testing.T) {
	// 101 人群：202 事件必须 members_omitted=true、不带全量成员。
	members := membersN(101)
	d := &convDomain{flags: newOffFlags()}
	d.flags.Apply(map[string]any{featureflags.KeyPullModeThreshold: 100})

	full, err := wire.Marshal(fullConvPayloadForEvents(3, members, 77))
	if err != nil {
		t.Fatalf("marshal full: %v", err)
	}
	evs := d.convEvents(3, members, full)
	if len(evs) != 101 {
		t.Fatalf("202 events = %d, want 101 (one per member)", len(evs))
	}
	var got wire.ConvPayload
	if err := wire.Unmarshal(evs[0].Payload, &got); err != nil {
		t.Fatalf("202 decode: %v", err)
	}
	if !got.MembersOmitted || got.MemberCount != 101 || len(got.Members) != 0 {
		t.Fatalf("202 = %+v, want membersOmitted=true memberCount=101 members=[]", got)
	}
	if got.Version == 0 || got.LastSeq != 77 {
		t.Fatalf("202 lost the watermark fields: %+v", got)
	}
	// 帧大小必须远低于 64KB 上限（这是 §0.3 的替代目的）。
	if len(evs[0].Payload) > 512 {
		t.Fatalf("members_omitted 202 payload = %d bytes, want < 512", len(evs[0].Payload))
	}

	// 旧客户端（只认字段 1-6）解析新 JSON：不报错，members 为空。
	var legacy legacyConvPayloadJSON
	if err := json.Unmarshal(evs[0].Payload, &legacy); err != nil {
		t.Fatalf("old client rejected the new 202 JSON: %v", err)
	}
	if legacy.ConvID != 3 || legacy.Version == 0 || len(legacy.Members) != 0 {
		t.Fatalf("old client view = %+v", legacy)
	}

	// 99 人群（<= 阈值）：仍然是全量快照，未启用时逐字节不变。
	small := membersN(99)
	fullSmall, _ := wire.Marshal(fullConvPayloadForEvents(4, small, 5))
	smallEvs := d.convEvents(4, small, fullSmall)
	if len(smallEvs) != 99 || string(smallEvs[0].Payload) != string(fullSmall) {
		t.Fatalf("99-member 202 changed: %d events, payload equal=%v",
			len(smallEvs), string(smallEvs[0].Payload) == string(fullSmall))
	}

	// 同一份 202 的 protobuf 形态：旧解码器（跳过未知字段）同样不报错。
	// 字段 7/8 由 chatpb 生成器覆盖，这里断言 wire 级字段号与新客户端读法一致。
	pbNew := &chatpb.ConvPayload{ConvId: 3, Version: 9, MembersOmitted: true, MemberCount: 101}
	raw, err := pbNew.Marshal()
	if err != nil {
		t.Fatalf("pb marshal: %v", err)
	}
	var pbBack chatpb.ConvPayload
	if err := pbBack.Unmarshal(raw); err != nil {
		t.Fatalf("pb unmarshal: %v", err)
	}
	if !pbBack.MembersOmitted || pbBack.MemberCount != 101 || pbBack.ConvId != 3 {
		t.Fatalf("pb round-trip = %+v", pbBack)
	}
}
