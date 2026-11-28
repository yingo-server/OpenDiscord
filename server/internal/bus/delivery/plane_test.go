// plane_test.go —— DeliveryPlane credit 流控服务端单测（v3-bus-design §7.3）。
//
// 覆盖：T-SV-2（credit 流控 + ACK 批量推进）、T-SV-3（BACKPRESSURED 传播与收敛）、
// T-SV-7（同 uid 灰度路径稳定，经 featureflags.InV3Bucket 判定）、
// 保命帧不占 credit、多设备独立（§3.4）。
// 全部使用 fake MailReader + fake sendFn，不依赖真 SQLite。
package delivery

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yingo-server/UFOT/server/internal/bus/mailbox"
	"github.com/yingo-server/UFOT/server/internal/pkg/featureflags"
)

// ---- fake MailReader ----

type fakeReader struct {
	mu      sync.Mutex
	entries map[int64][]mailbox.Entry // uid → 升序条目（Seq 即 mailbox_seq）
	cursors map[int64]int64
	nextSeq map[int64]int64
}

func newFakeReader() *fakeReader {
	return &fakeReader{
		entries: make(map[int64][]mailbox.Entry),
		cursors: make(map[int64]int64),
		nextSeq: make(map[int64]int64),
	}
}

// Offer 追加 n 条 envelope，返回首个分配的 mailbox_seq（单写者语义：单调）。
func (f *fakeReader) Offer(uid int64, n int) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	first := f.nextSeq[uid] + 1
	for i := 0; i < n; i++ {
		seq := f.nextSeq[uid] + 1
		f.nextSeq[uid] = seq
		f.entries[uid] = append(f.entries[uid], mailbox.Entry{
			MailboxSeq: seq, UserID: uid,
			Envelope: []byte{byte(seq % 256)}, // 可辨识的最小 envelope
			State:    mailbox.StatePending,
		})
	}
	return first
}

func (f *fakeReader) ListSince(_ context.Context, uid, cursor int64, limit int) ([]mailbox.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []mailbox.Entry
	for _, e := range f.entries[uid] {
		if e.MailboxSeq > cursor {
			out = append(out, e)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeReader) Cursor(_ context.Context, uid int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cursors[uid], nil
}

func (f *fakeReader) Head(_ context.Context, uid int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := len(f.entries[uid]); n > 0 {
		return f.entries[uid][n-1].MailboxSeq, nil
	}
	return 0, nil
}

// MarkAcked 模拟 Store.MarkAcked（AckFn 目标）：累积游标推进持久化。
func (f *fakeReader) MarkAcked(uid, cursor int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cursor > f.cursors[uid] {
		f.cursors[uid] = cursor
		for i := range f.entries[uid] {
			if f.entries[uid][i].MailboxSeq <= cursor {
				f.entries[uid][i].State = mailbox.StateAcked
			}
		}
	}
}

// ---- fake sendFn（可编排慢消费者/丢帧行为） ----

type fakeSender struct {
	mu      sync.Mutex
	sent    []int64 // 已收到的 mailbox_seq（有序）
	dropped int     // Pump 之外的静默丢弃（本包语义下必须恒为 0）
	failAt  int     // 第 failAt 次调用起返回 error（<=0 不失败）
	calls   int
}

func (s *fakeSender) send(_ context.Context, seq int64, _ []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.failAt > 0 && s.calls >= s.failAt {
		return errors.New("fake: conn write failed")
	}
	s.sent = append(s.sent, seq)
	return nil
}

func (s *fakeSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

func (s *fakeSender) seqs() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int64, len(s.sent))
	copy(out, s.sent)
	return out
}

func newTestPlane(rd MailReader, cfg Config) *Plane {
	return NewPlane(rd, func(_ context.Context, uid, cursor int64) {}, cfg)
}

// ---- T-SV-2：credit 流控：credit=512 发 1000 条 → 488 条滞留 mailbox、0 丢弃；
// ACK 批量推进后继续发完。 ----

func TestTSV2CreditFlowControl(t *testing.T) {
	ctx := context.Background()
	rd := newFakeReader()
	sender := &fakeSender{}
	p := newTestPlane(rd, Config{InitialCredit: 512})

	// 先订阅，后投 1000 条（模拟在线时 fanout 写入）。
	c := p.Subscribe(1, "dev-a", sender.send)
	rd.Offer(1, 1000)
	head, _ := rd.Head(ctx, 1)
	if head != 1000 {
		t.Fatalf("head = %d, want 1000", head)
	}

	res := p.Pump(ctx, c)
	if res.Err != nil {
		t.Fatalf("Pump err: %v", res.Err)
	}
	if got := sender.count(); got != 512 {
		t.Fatalf("sent = %d, want 512 (initial credit)", got)
	}
	if res.Sent != 512 {
		t.Fatalf("res.Sent = %d, want 512", res.Sent)
	}
	if !res.Paused {
		t.Fatal("conn should be PAUSED after credit exhausted")
	}
	// 488 条滞留 mailbox：Head(1000) - Cursor(512) = 488，0 条丢弃。
	stranded := head - c.Cursor()
	if stranded != 488 {
		t.Fatalf("stranded = %d, want 488", stranded)
	}
	if sender.dropped != 0 {
		t.Fatalf("dropped = %d, want 0", sender.dropped)
	}

	// ACK 批量推进（mailbox_ack_window=128 语义，§6）：对已发出的 cursor 确认并
	// 归还 credit（+128），Pump 从真实 cursor 续发 —— 不得预支未发送的游标，
	// 否则 OnAck 会把 cursor 跳过未发条目造成丢失。
	for c.Cursor() < head {
		cur := c.Cursor()
		if err := p.OnAck(ctx, c, cur, 128); err != nil {
			t.Fatalf("OnAck: %v", err)
		}
		if res := p.Pump(ctx, c); res.Err != nil {
			t.Fatalf("Pump: %v", res.Err)
		}
	}
	if got := sender.count(); got != 1000 {
		t.Fatalf("total sent = %d, want 1000 (no frame lost)", got)
	}
	seqs := sender.seqs()
	for i, s := range seqs { // conn 内严格有序（§3.3）
		if s != int64(i+1) {
			t.Fatalf("seq[%d] = %d, want %d (order violated)", i, s, i+1)
		}
	}
}

// ---- T-SV-3：全 conn PAUSED → BACKPRESSURED；恢复 credit → 全量收敛。 ----

func TestTSV3BackpressurePropagation(t *testing.T) {
	ctx := context.Background()
	rd := newFakeReader()
	p := newTestPlane(rd, Config{InitialCredit: 8})

	s1, s2 := &fakeSender{}, &fakeSender{}
	c1 := p.Subscribe(7, "dev-a", s1.send)
	c2 := p.Subscribe(7, "dev-b", s2.send)
	rd.Offer(7, 20)

	if r := p.Pump(ctx, c1); r.Err != nil || r.Sent != 8 {
		t.Fatalf("c1 pump: sent=%d err=%v", r.Sent, r.Err)
	}
	if r := p.Pump(ctx, c2); r.Err != nil || r.Sent != 8 {
		t.Fatalf("c2 pump: sent=%d err=%v", r.Sent, r.Err)
	}
	// 两 conn 均 PAUSED（每 conn credit=8 各自耗尽）→ uid BACKPRESSURED。
	st := p.Stats()
	if st.PausedConns != 2 {
		t.Fatalf("PausedConns = %d, want 2", st.PausedConns)
	}
	if st.Backpressured != 1 {
		t.Fatalf("Backpressured = %d, want 1 (all conns of uid 7 paused)", st.Backpressured)
	}
	// 聚合窗上游依据：两 conn cursor 合计 16，滞留 20-16=4 条，0 丢弃。
	if s1.dropped != 0 || s2.dropped != 0 {
		t.Fatal("no frames may be dropped under backpressure")
	}
	total := c1.Cursor() + c2.Cursor()
	if total != 16 {
		t.Fatalf("total cursor = %d, want 16; stranded = %d", total, 20-total)
	}

	// 恢复：对已发 cursor 确认并归还 credit（+4）→ 全量收敛（20/20 发出）。
	// 不预支游标（cursor+n 会让 Pump 跳过未发条目）。
	for c1.Cursor() < 20 || c2.Cursor() < 20 {
		if c1.Cursor() < 20 {
			_ = p.OnAck(ctx, c1, c1.Cursor(), 4)
			if r := p.Pump(ctx, c1); r.Err != nil {
				t.Fatalf("c1 pump: %v", r.Err)
			}
		}
		if c2.Cursor() < 20 {
			_ = p.OnAck(ctx, c2, c2.Cursor(), 4)
			if r := p.Pump(ctx, c2); r.Err != nil {
				t.Fatalf("c2 pump: %v", r.Err)
			}
		}
	}
	if s1.count()+s2.count() != 40 {
		t.Fatalf("total sent = %d, want 40 (each of 2 devices receives full stream, §3.4)", s1.count()+s2.count())
	}
	// 全部 cursor 追上 head 后，两 conn 不再 PAUSED → uid 不再 BACKPRESSURED。
	if c1.Credit() == 0 || c2.Credit() == 0 {
		_ = p.OnAck(ctx, c1, 20, 4)
		_ = p.OnAck(ctx, c2, 20, 4)
	}
	st = p.Stats()
	if st.Backpressured != 0 {
		t.Fatalf("Backpressured after recovery = %d, want 0", st.Backpressured)
	}
}

// ---- T-SV-7：同 uid 灰度路径稳定（按 uid 哈希，非按连接随机）。 ----

func TestTSV7GreyPathStablePerUID(t *testing.T) {
	flags5 := map[string]any{"v3_bus_enabled": true, "v3_bus_percent": 5}
	flags100 := map[string]any{"v3_bus_enabled": true, "v3_bus_percent": 100}
	flagsOff := map[string]any{"v3_bus_enabled": false, "v3_bus_percent": 100}

	for uid := uint64(1); uid <= 500; uid++ {
		first := featureflags.InV3Bucket(flags5, uid)
		for i := 0; i < 3; i++ { // 多次连接（连接粒度切换，同 uid 判定稳定）
			if got := featureflags.InV3Bucket(flags5, uid); got != first {
				t.Fatalf("uid %d grey path unstable: first=%v now=%v", uid, first, got)
			}
		}
		// 边界：总开关关闭 → 一律旧路径（回滚主路径，§6）。
		if featureflags.InV3Bucket(flagsOff, uid) {
			t.Fatalf("uid %d: enabled=false must be legacy path", uid)
		}
		// 全量桶：enabled=true 且 percent=100 → 一律 v3。
		if !featureflags.InV3Bucket(flags100, uid) {
			t.Fatalf("uid %d: percent=100 must be v3", uid)
		}
	}
	// 5% 桶非空且远小于全集（sanity，非分布精确断言）。
	n := 0
	for uid := uint64(1); uid <= 500; uid++ {
		if featureflags.InV3Bucket(flags5, uid) {
			n++
		}
	}
	if n == 0 || n > 100 {
		t.Fatalf("5%% bucket over 500 uids = %d, want (0,100]", n)
	}
}

// ---- 保命帧不占 credit 且 PAUSED 下仍可发（§3.2）。 ----

func TestVitalFramesBypassCredit(t *testing.T) {
	ctx := context.Background()
	rd := newFakeReader()
	sender := &fakeSender{}
	p := newTestPlane(rd, Config{InitialCredit: 2})

	c := p.Subscribe(3, "dev-a", sender.send)
	rd.Offer(3, 10)
	if r := p.Pump(ctx, c); r.Err != nil || !r.Paused {
		t.Fatalf("pump: sent=%d paused=%v err=%v", r.Sent, r.Paused, r.Err)
	}
	if c.Credit() != 0 {
		t.Fatalf("credit = %d, want 0 (PAUSED)", c.Credit())
	}

	// PAUSED 下保命帧 kind 4/5/10（Ping/Pong/ERROR）仍可发，且不占 credit。
	for i := 0; i < 3; i++ {
		if err := p.SendVital(ctx, c, 0, []byte{byte(4 + i)}); err != nil {
			t.Fatalf("SendVital: %v", err)
		}
	}
	if c.Credit() != 0 || c.Paused() != true {
		t.Fatalf("vital frames must not consume credit: credit=%d paused=%v", c.Credit(), c.Paused())
	}
	if sender.count() != 5 { // 2 数据 + 3 保命
		t.Fatalf("sent total = %d, want 5", sender.count())
	}

	// 数据帧在 PAUSED 下不发（Pump 直接返回）。
	if r := p.Pump(ctx, c); r.Sent != 0 {
		t.Fatalf("PAUSED pump sent = %d, want 0", r.Sent)
	}
}

// ---- 多设备独立（§3.4）：弱设备 PAUSED 不阻塞其他设备。 ----

func TestMultiDeviceIndependence(t *testing.T) {
	ctx := context.Background()
	rd := newFakeReader()
	p := newTestPlane(rd, Config{InitialCredit: 4})

	slow, fast := &fakeSender{}, &fakeSender{}
	cs := p.Subscribe(9, "tab-paused", slow.send)
	cf := p.Subscribe(9, "phone", fast.send)
	rd.Offer(9, 16)

	// 弱设备耗尽 credit → PAUSED。
	if r := p.Pump(ctx, cs); r.Err != nil || !r.Paused {
		t.Fatalf("slow pump: paused=%v err=%v", r.Paused, r.Err)
	}
	// 强设备仍有 credit，继续消费同一 mailbox，不受阻塞。
	if r := p.Pump(ctx, cf); r.Err != nil || r.Sent != 4 {
		t.Fatalf("fast pump: sent=%d err=%v", r.Sent, r.Err)
	}
	if cs.Cursor() != 4 || cf.Cursor() != 4 {
		t.Fatalf("cursors: slow=%d fast=%d, want 4/4 (independent)", cs.Cursor(), cf.Cursor())
	}
	// ACK 只推进一个 conn 的 credit，互不串扰：cf 确认 4 条归还 credit。
	_ = p.OnAck(ctx, cf, 4, 4)
	st := p.Stats()
	if st.PausedConns != 1 || st.Backpressured != 0 {
		t.Fatalf("stats: paused=%d backpressured=%d, want 1/0 (one paused conn, uid not fully blocked)",
			st.PausedConns, st.Backpressured)
	}
	if cs.Credit() != 0 {
		t.Fatalf("slow conn credit = %d, want 0 (independent)", cs.Credit())
	}
	// cf 用归还的 credit 续发（每设备收全量流，§3.4）。
	if r := p.Pump(ctx, cf); r.Err != nil || r.Sent != 4 {
		t.Fatalf("fast repump: sent=%d err=%v", r.Sent, r.Err)
	}
}

// ---- PAUSED 超时判死（slow_consumer_close_s，§3.2 逃生）。 ----

func TestSlowConsumerShouldClose(t *testing.T) {
	ctx := context.Background()
	rd := newFakeReader()
	sender := &fakeSender{}
	p := newTestPlane(rd, Config{InitialCredit: 1, SlowConsumerClose: 20 * time.Millisecond})

	c := p.Subscribe(11, "dev-a", sender.send)
	rd.Offer(11, 5)
	if r := p.Pump(ctx, c); r.Err != nil || !r.Paused {
		t.Fatalf("pump: paused=%v err=%v", r.Paused, r.Err)
	}
	if r := p.Pump(ctx, c); r.ShouldClose {
		t.Fatal("ShouldClose before slow_consumer_close_s elapsed")
	}
	time.Sleep(40 * time.Millisecond)
	r := p.Pump(ctx, c)
	if !r.ShouldClose {
		t.Fatal("ShouldClose after slow_consumer_close_s, want true")
	}
	// ACK 恢复后不再判死。
	_ = p.OnAck(ctx, c, 1, 3)
	if r := p.Pump(ctx, c); r.ShouldClose {
		t.Fatal("ShouldClose after credit restored, want false")
	}
}

// ---- sendFn 失败：该条不推进 cursor，滞留 mailbox（重连续传语义）。 ----

func TestSendFailureKeepsEntryStranded(t *testing.T) {
	ctx := context.Background()
	rd := newFakeReader()
	sender := &fakeSender{failAt: 3}
	p := newTestPlane(rd, Config{InitialCredit: 16})

	c := p.Subscribe(13, "dev-a", sender.send)
	rd.Offer(13, 10)
	r := p.Pump(ctx, c)
	if r.Err == nil {
		t.Fatal("expected send error")
	}
	if c.Cursor() != 2 {
		t.Fatalf("cursor = %d, want 2 (failed entry not confirmed)", c.Cursor())
	}
	if r.Sent != 2 {
		t.Fatalf("sent = %d, want 2", r.Sent)
	}
	// 失败条目仍可由后续 ACK+Pump 重发（conn 存活场景）。
	sender.mu.Lock()
	sender.failAt = 0
	sender.mu.Unlock()
	_ = p.OnAck(ctx, c, 2, 2)
	if r := p.Pump(ctx, c); r.Err != nil {
		t.Fatalf("repump: %v", r.Err)
	}
	if c.Cursor() != 10 || sender.count() != 10 {
		t.Fatalf("after recovery: cursor=%d sent=%d, want 10/10", c.Cursor(), sender.count())
	}
}
