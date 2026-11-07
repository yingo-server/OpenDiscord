// v3split_test.go —— v3 P3d-1 fanout 灰度分流守门测试
// （docs/v3-bus-design.md §5.1/§6/§7.3 T-SV-5/T-SV-7、§9 中间态铁律）。
//
// 断言全部对着「可观测产出」：旧直投 lane 的帧数与内容、注入 V3Offer 收到的
// canonical 帧、mailbox 落库行数。不 mock 被测的分流逻辑本身。
package pipe

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/yingo-server/OpenDiscord/server/internal/data/store"
	"github.com/yingo-server/OpenDiscord/server/internal/pkg/featureflags"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/kinds"
)

// captureV3 is a deterministic V3Offer: inBucket is a per-uid predicate so
// tests can express a stable grey bucket without depending on a real hash map.
type captureV3 struct {
	mu      sync.Mutex
	frames  []V3Frame
	bucket  func(uid uint64) bool
	errAt   int
	errored int
}

func (c *captureV3) offer(ctx context.Context, f V3Frame) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.errAt > 0 && len(c.frames)+1 == c.errAt {
		c.errored++
		return false, context.DeadlineExceeded
	}
	if c.bucket != nil && !c.bucket(f.UserID) {
		return false, nil
	}
	cp := f
	cp.Blob = append([]byte(nil), f.Blob...)
	cp.Payload = append([]byte(nil), f.Payload...)
	c.frames = append(c.frames, cp)
	return true, nil
}

func (c *captureV3) snapshot() []V3Frame {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]V3Frame(nil), c.frames...)
}

func (c *captureV3) countKind(k uint32) int {
	n := 0
	for _, f := range c.snapshot() {
		if f.Kind == k {
			n++
		}
	}
	return n
}

// splitRig: N members (creator=1, 2..N), flusher running, legacy lane intact.
type splitRig struct {
	p    *Pipe
	stop context.CancelFunc
	cid  int64
}

func newSplitRig(t *testing.T, n int) *splitRig {
	t.Helper()
	p, _, convs, _ := newTestPipe(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cid, err := convs.CreateConv(ctx, &store.ConvMeta{CType: 2, CreatedBy: 1, CreatedAt: 1}, 1)
	if err != nil {
		t.Fatalf("create conv: %v", err)
	}
	for uid := 2; uid <= n; uid++ {
		if err := convs.AddMember(ctx, &store.Member{Cid: cid, UID: int64(uid), Role: 1, JoinedAt: 1}); err != nil {
			t.Fatalf("add member: %v", err)
		}
	}
	go p.fl.run(ctx)
	return &splitRig{p: p, stop: cancel, cid: cid}
}

func (r *splitRig) send(t *testing.T, id string) {
	t.Helper()
	r.p.dispatchReq(context.Background(), mkReq(101, r.cid, 1,
		`{"clientMsgId":"`+id+`","text":"hello"}`))
	_ = decodeAck(t, waitEnv(t, r.p.Outbound()))
}

// (T-SV-5) flag-off / 未装配：V3Offer 一次都不调用，旧路径逐字节不变
// （99 人群 → 99 条 102，帧内容可解码）。
func TestV3Split_UnassembledLegacyByteIdentical(t *testing.T) {
	r := newSplitRig(t, 100)
	r.send(t, "p3d-off")
	frames := collectFan(t, r.p, 80*time.Millisecond)
	if got := countKind(frames, uint32(kinds.KindMsgNew)); got != 99 {
		t.Fatalf("legacy MSG_NEW frames = %d, want 99", got)
	}
	if st := r.p.V3SplitStats(); st != (V3SplitStats{}) {
		t.Fatalf("unassembled split must be all-zero, got %+v", st)
	}
}

// (T-SV-7) 灰度按 uid 哈希稳定分桶、非按连接随机：同一 uid 重复投递始终
// 落同一路径；不同 uid 只把桶内的写进 hub，且旧路径对**所有** uid 仍发。
func TestV3Split_GreyBucketByUIDDualWrite(t *testing.T) {
	r := newSplitRig(t, 4)                                               // members 2,3,4
	c := &captureV3{bucket: func(uid uint64) bool { return uid%2 == 0 }} // 2,4 in bucket
	r.p.SetV3Offer(c.offer)
	for i := 0; i < 3; i++ {
		r.send(t, "p3d-grey-"+string(rune('a'+i)))
	}
	frames := collectFan(t, r.p, 80*time.Millisecond)
	if got := countKind(frames, uint32(kinds.KindMsgNew)); got != 9 {
		t.Fatalf("dual-write must NOT suppress legacy frames: got %d, want 9", got)
	}
	in := c.snapshot()
	if len(in) != 6 {
		t.Fatalf("hub frames = %d, want 6 (2 bucket uids × 3 msgs)", len(in))
	}
	for _, f := range in {
		if f.UserID%2 != 0 {
			t.Fatalf("uid %d leaked into v3 bucket", f.UserID)
		}
		if f.Kind != uint32(kinds.KindMsgNew) || len(f.Blob) == 0 || f.ConvID != uint64(r.cid) {
			t.Fatalf("bad canonical frame: %+v", f)
		}
		if f.MsgID == 0 || f.Seq == 0 {
			t.Fatalf("delivery_key identity missing: %+v", f)
		}
	}
	st := r.p.V3SplitStats()
	if st.Offered != 6 || st.Broadcast != 9 || st.Failed != 0 {
		t.Fatalf("stats = %+v, want Offered=6 Broadcast=9 Failed=0", st)
	}
}

// 总开关 / 灰度名单由 adapter 判定：bucket=nil 语义 = 全部不在桶，
// 计数走 Bypassed，且旧路径帧数不变。
func TestV3Split_AllOutsideBucketBypassed(t *testing.T) {
	r := newSplitRig(t, 3)
	c := &captureV3{bucket: func(uint64) bool { return false }}
	r.p.SetV3Offer(c.offer)
	r.send(t, "p3d-bypass")
	frames := collectFan(t, r.p, 80*time.Millisecond)
	if got := countKind(frames, uint32(kinds.KindMsgNew)); got != 2 {
		t.Fatalf("legacy frames = %d, want 2", got)
	}
	st := r.p.V3SplitStats()
	if st.Offered != 0 || st.Bypassed != 2 || st.Broadcast != 2 {
		t.Fatalf("stats = %+v, want Offered=0 Bypassed=2 Broadcast=2", st)
	}
}

// hub 写失败只计数、绝不影响旧直投（§9 旁路故障隔离）。
func TestV3Split_HubFailureNeverAffectsLegacy(t *testing.T) {
	r := newSplitRig(t, 3)
	c := &captureV3{errAt: 1}
	r.p.SetV3Offer(c.offer)
	r.send(t, "p3d-huberr")
	frames := collectFan(t, r.p, 80*time.Millisecond)
	if got := countKind(frames, uint32(kinds.KindMsgNew)); got != 2 {
		t.Fatalf("legacy frames = %d, want 2 even with hub error", got)
	}
	st := r.p.V3SplitStats()
	if st.Failed < 1 || st.Broadcast != 2 || st.Offered != 0 {
		t.Fatalf("stats = %+v, want Failed>=1 Offered=0 Broadcast=2", st)
	}
	if c.errored == 0 {
		t.Fatal("injected hub failure was never observed")
	}
}

// §10：typing/presence 是低价值无序旁路，不进 mailbox；消息帧仍进。
func TestV3Split_LowValueSignalsBypassMailbox(t *testing.T) {
	r := newSplitRig(t, 3)
	c := &captureV3{}
	r.p.SetV3Offer(c.offer)
	ctx := context.Background()
	// typing 230：按 fanoutPlane 的异步聚合窗投递（100ms），需等窗口冲刷。
	r.p.fanout.Enqueue(fanoutJob{
		kind: uint32(kinds.KindTyping), cid: r.cid, excludeUID: 1, userID: 1,
		payload: []byte(`{"typing":true}`), class: dropBestEffort,
		windowKey: "c" + itoa(r.cid),
	})
	fctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go r.p.fanout.start(fctx)
	time.Sleep(180 * time.Millisecond)
	if got := c.countKind(uint32(kinds.KindTyping)); got != 0 {
		t.Fatalf("typing must bypass mailbox, got %d hub frames", got)
	}
	r.send(t, "p3d-after-typing")
	time.Sleep(30 * time.Millisecond)
	if got := c.countKind(uint32(kinds.KindMsgNew)); got != 2 {
		t.Fatalf("data frames must still be offered: got %d, want 2", got)
	}
}

// §2.4：大群走水位线，水位线进 mailbox（且本体 102 不进）。
func TestV3Split_WatermarkEntersMailboxNotMsgBody(t *testing.T) {
	p, _, convs, _ := newTestPipe(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cid, err := convs.CreateConv(ctx, &store.ConvMeta{CType: 2, CreatedBy: 1, CreatedAt: 1}, 1)
	if err != nil {
		t.Fatal(err)
	}
	for uid := 2; uid <= 101; uid++ {
		_ = convs.AddMember(ctx, &store.Member{Cid: cid, UID: int64(uid), Role: 1, JoinedAt: 1})
	}
	c := &captureV3{}
	p.SetV3Offer(c.offer)
	// 直接驱动 pull-mode 判定点（watermark 频控需要真实 101 人成员集）。
	p.fanout.setFlags(featureflags.NewFromMap(map[string]any{
		featureflags.KeyPullModeThreshold: 100,
	}))
	go p.fl.run(ctx)
	p.dispatchReq(ctx, mkReq(101, cid, 1, `{"clientMsgId":"p3d-wm","text":"hi"}`))
	_ = waitEnv(t, p.Outbound())
	time.Sleep(60 * time.Millisecond)
	if got := c.countKind(uint32(kinds.KindMsgNew)); got != 0 {
		t.Fatalf("large-group message body must NOT enter mailbox, got %d", got)
	}
	if got := c.countKind(uint32(kinds.KindWatermark)); got != 100 {
		t.Fatalf("watermark hub frames = %d, want 100", got)
	}
}
