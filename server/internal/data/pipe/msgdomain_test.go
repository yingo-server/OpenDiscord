package pipe

// msgdomain_test.go: FE-1a message-domain coverage (plan §5 rows 9/10).
// All tests assemble the pipe with msg_domain_v2_enabled = true EXCEPT the
// first one, which pins the flag-off contract: 106/109/453 uplinks hit the
// legacy default branch (no reply, no write) — zero behaviour change.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yingo-server/OpenDiscord/server/internal/data/pipe/pipefakes"
	"github.com/yingo-server/OpenDiscord/server/internal/data/pipe/wire"
	"github.com/yingo-server/OpenDiscord/server/internal/data/store"
	"github.com/yingo-server/OpenDiscord/server/internal/pkg/featureflags"
	"github.com/yingo-server/OpenDiscord/server/internal/pkg/log"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/kinds"
)

// v2Pipe assembles a test pipe with the FE-1a flag ON (production default after
// 1.4 rollout) and (optionally) an audit journal. The off-baseline tests use
// the shared newOffFlags() from pullmode_test.go.
func v2Pipe(t *testing.T, fa *FakeAudit) (*Pipe, *pipefakes.FakeMsgStore, *pipefakes.FakeConvStore) {
	t.Helper()
	msgs := pipefakes.NewFakeMsgStore()
	convs := pipefakes.NewFakeConvStore()
	sys := pipefakes.NewFakeSysStore()
	hot := hotBundle{w: msgs, r: msgs, conv: convs, sys: sys}
	var cold ColdDeps
	if fa != nil {
		cold = ColdDeps{Audit: fa}
	}
	p := NewStores(nil, hot, cold, log.For("data-test"))
	p.fl.interval = 5 * time.Millisecond
	flags := newOffFlags()
	flags.Apply(map[string]any{featureflags.KeyMsgDomainV2: true})
	p.SetFeatureFlags(flags)
	return p, msgs, convs
}

// outEnv collects one frame from the pipe's reply lane with a timeout.
func outEnv(t *testing.T, p *Pipe) *env {
	t.Helper()
	select {
	case e := <-p.Outbound():
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting on reply lane")
		return nil
	}
}

func seedMsg(t *testing.T, msgs *pipefakes.FakeMsgStore, cid, sender, msgID int64, content string) {
	t.Helper()
	if err := msgs.AppendMessages(context.Background(), []store.PendingMsg{{
		MsgID: msgID, Cid: cid, Seq: 1, SenderID: sender, ClientMsgID: "m1",
		Content: []byte(content), SentAt: nowSec(),
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// TestMsgDomainFlagOffIsNoop: flag-off, an MSG_EDIT uplink gets NO reply and
// NO store write — the legacy default branch is byte-identical to mainline.
func TestMsgDomainFlagOffIsNoop(t *testing.T) {
	msgs := pipefakes.NewFakeMsgStore()
	convs := pipefakes.NewFakeConvStore()
	p := NewStores(nil, hotBundle{w: msgs, r: msgs, conv: convs, sys: pipefakes.NewFakeSysStore()},
		ColdDeps{}, log.For("data-test"))
	p.fl.interval = 5 * time.Millisecond
	// Default flags: msg_domain_v2_enabled = false（§F.2：显式 NewFromMap(off)，
	// 不依赖 Defaults() —— 1.4 转生产后 Defaults 已默认 true）。
	p.SetFeatureFlags(featureflags.NewFromMap(map[string]any{featureflags.KeyMsgDomainV2: false}))
	ctx := context.Background()
	cid, _ := convs.CreateConv(ctx, &store.ConvMeta{CType: 2, CreatedBy: 7, CreatedAt: 1}, 7)
	seedMsg(t, msgs, cid, 7, 55, `{"text":"hi","media":[]}`)

	p.dispatchReq(ctx, mkReq(uint32(kinds.KindMsgEdit), cid, 7, `{"msg_id":55,"text":"new"}`))

	select {
	case e := <-p.Outbound():
		t.Fatalf("flag-off produced a reply frame: kind=%d", e.Kind)
	case <-time.After(150 * time.Millisecond):
	}
	if got := msgs.Rows[itoaKey(cid, 7, "m1")].Content; string(got) != `{"text":"hi","media":[]}` {
		t.Fatalf("flag-off mutated content: %s", got)
	}
}

// itoaKey formats the fake store's row key.
func itoaKey(cid, sender int64, cmsg string) string {
	return itoa(cid) + ":" + itoa(sender) + ":" + cmsg
}

// TestMsgEditHappyPath: the sender edits within the window → content is
// rewritten, edited stamped, MSG_EDIT broadcast to every member, audited.
func TestMsgEditHappyPath(t *testing.T) {
	fa := &FakeAudit{}
	p, msgs, convs := v2Pipe(t, fa)
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go p.fanout.start(ctx)
	cid, _ := convs.CreateConv(ctx, &store.ConvMeta{CType: 2, CreatedBy: 7, CreatedAt: 1}, 7)
	_ = convs.AddMember(ctx, &store.Member{Cid: cid, UID: 8})
	seedMsg(t, msgs, cid, 7, 55, `{"client_msg_id":"m1","text":"hi","media":[]}`)

	p.dispatchReq(ctx, mkReq(uint32(kinds.KindMsgEdit), cid, 7, `{"msg_id":55,"conv_id":`+itoa(cid)+`,"text":"edited"}`))

	row := msgs.Rows[itoaKey(cid, 7, "m1")]
	var c map[string]any
	if err := json.Unmarshal(row.Content, &c); err != nil {
		t.Fatalf("content not json: %v", row.Content)
	}
	if c["text"] != "edited" {
		t.Fatalf("text not rewritten: %v", c["text"])
	}
	if row.Edited == 0 {
		t.Fatal("edited stamp missing")
	}
	// Broadcast: kind 106 frames on the fan-out lane (one per member incl. actor).
	deadline := time.After(2 * time.Second)
	sawEdit := 0
loop:
	for {
		select {
		case e := <-p.OutboundFan():
			if e.Kind == uint32(kinds.KindMsgEdit) {
				sawEdit++
			}
		case <-deadline:
			break loop
		}
	}
	if sawEdit == 0 {
		t.Fatal("no MSG_EDIT broadcast frame")
	}
	ev := findAudit(fa.snapshot(), "message.edit")
	if ev == nil {
		t.Fatal("no message.edit audit event")
	}
	if strings.Contains(string(ev.Detail), "edited") {
		t.Fatalf("audit leaked edit text: %s", ev.Detail)
	}
}

// TestMsgEditAuthz: non-sender and out-of-window edits are rejected.
func TestMsgEditAuthz(t *testing.T) {
	p, msgs, convs := v2Pipe(t, nil)
	ctx := context.Background()
	cid, _ := convs.CreateConv(ctx, &store.ConvMeta{CType: 2, CreatedBy: 7, CreatedAt: 1}, 7)
	_ = convs.AddMember(ctx, &store.Member{Cid: cid, UID: 8})
	seedMsg(t, msgs, cid, 7, 55, `{"text":"hi"}`)

	// Not the sender.
	p.dispatchReq(ctx, mkReq(uint32(kinds.KindMsgEdit), cid, 8, `{"msg_id":55,"text":"x"}`))
	e := outEnv(t, p)
	if e.Kind != uint32(kinds.KindError) {
		t.Fatalf("want error, got kind %d", e.Kind)
	}
	// Sender but outside the window (simulate by shrinking the window to 0).
	p.editWindow = 1
	seeded := msgs.Rows[itoaKey(cid, 7, "m1")]
	seeded.SentAt = nowSec() - 10
	msgs.Rows[itoaKey(cid, 7, "m1")] = seeded
	p.dispatchReq(ctx, mkReq(uint32(kinds.KindMsgEdit), cid, 7, `{"msg_id":55,"text":"x"}`))
	e = outEnv(t, p)
	if e.Kind != uint32(kinds.KindError) {
		t.Fatalf("want window error, got kind %d", e.Kind)
	}
}

// TestMsgForwardMetadata: forwarding stamps forward_from inside the new row's
// content and produces a normal MSG_NEW pipeline message.
func TestMsgForwardMetadata(t *testing.T) {
	fa := &FakeAudit{}
	p, msgs, convs := v2Pipe(t, fa)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.fl.run(ctx)
	go p.fanout.start(ctx)
	cid, _ := convs.CreateConv(ctx, &store.ConvMeta{CType: 2, CreatedBy: 7, CreatedAt: 1}, 7)
	toCid, _ := convs.CreateConv(ctx, &store.ConvMeta{CType: 2, CreatedBy: 7, CreatedAt: 1}, 7)
	_ = convs.AddMember(ctx, &store.Member{Cid: toCid, UID: 8})
	seedMsg(t, msgs, cid, 7, 55, `{"client_msg_id":"m1","text":"orig","media":[]}`)

	body := `{"msg_id":55,"from_conv_id":` + itoa(cid) + `,"to_conv_id":` + itoa(toCid) + `}`
	p.dispatchReq(ctx, mkReq(uint32(kinds.KindMsgForward), toCid, 7, body))

	// The forwarded row lands through the normal pipeline: wait for the flush.
	deadline := time.After(2 * time.Second)
	found := false
	for !found {
		select {
		case <-p.Outbound(): // acks drain
		case <-p.OutboundFan(): // msg_new drains
		case <-deadline:
			t.Fatal("forwarded message never committed")
		}
		for _, m := range msgs.Snapshot() {
			var c map[string]json.RawMessage
			if err := json.Unmarshal(m.Content, &c); err == nil {
				if ff, ok := c["forward_from"]; ok {
					var meta wire.ForwardFrom
					if json.Unmarshal(ff, &meta) == nil && meta.MsgID == 55 && meta.ConvID == uint64(cid) {
						found = true
					}
				}
			}
		}
	}
	ev := findAudit(fa.snapshot(), "message.forward")
	if ev == nil {
		t.Fatal("no message.forward audit event")
	}
}

// TestReportInsertsRow: kind 453 inserts sys_reports (via the pipe's sink),
// replies a result ACK and audits ids-only.
type reportSink struct {
	called bool
}

func (r *reportSink) InsertReport(ctx context.Context, reporter, targetMsg, targetUser int64, reason int, detail string, at int64) error {
	r.called = true
	return nil
}

func TestReportInsertsRow(t *testing.T) {
	fa := &FakeAudit{}
	p, _, convs := v2Pipe(t, fa)
	sink := &reportSink{}
	p.hot = &reportHot{hotBundle: p.hot.(hotBundle), sink: sink}
	ctx := context.Background()
	cid, _ := convs.CreateConv(ctx, &store.ConvMeta{CType: 2, CreatedBy: 7, CreatedAt: 1}, 7)

	p.dispatchReq(ctx, mkReq(uint32(kinds.KindReport), cid, 7,
		`{"target_msg":55,"target_user":8,"reason":1,"detail":"spam"}`))

	if !sink.called {
		t.Fatal("InsertReport not called")
	}
	e := outEnv(t, p)
	if e.Kind != uint32(kinds.KindSecurityResult) {
		t.Fatalf("want 961 ack, got kind %d", e.Kind)
	}
	ev := findAudit(fa.snapshot(), "report.create")
	if ev == nil {
		t.Fatal("no report.create audit event")
	}
	m := detailMap(t, ev)
	if m["reason"].(float64) != 1 {
		t.Fatalf("reason not audited: %v", m)
	}
}

// reportHot overrides the bundle's report sink with a controllable fake.
type reportHot struct {
	hotBundle
	sink *reportSink
}

func (h *reportHot) InsertReport(ctx context.Context, reporter, targetMsg, targetUser int64, reason int, detail string, at int64) error {
	return h.sink.InsertReport(ctx, reporter, targetMsg, targetUser, reason, detail, at)
}

// TestMentionTargetedNotice: a MSG_SEND with mentions produces one targeted
// CONV_PAYLOAD(202) per mentioned member; non-members get nothing.
func TestMentionTargetedNotice(t *testing.T) {
	p, msgs, convs := v2Pipe(t, nil)
	ctx := context.Background()
	cid, _ := convs.CreateConv(ctx, &store.ConvMeta{CType: 2, CreatedBy: 7, CreatedAt: 1}, 7)
	_ = convs.AddMember(ctx, &store.Member{Cid: cid, UID: 8})

	content := `{"client_msg_id":"m2","text":"hi @8","mentions":[8,999]}`
	if err := msgs.AppendMessages(ctx, []store.PendingMsg{{
		MsgID: 77, Cid: cid, Seq: 2, SenderID: 7, ClientMsgID: "m2",
		Content: []byte(content), SentAt: nowSec(),
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p.fl.emitMentionNotices(store.PendingMsg{MsgID: 77, Cid: cid, Seq: 2, SenderID: 7,
		ClientMsgID: "m2", Content: []byte(content), SentAt: nowSec()}, nil)

	deadline := time.After(2 * time.Second)
	saw8, saw999 := false, false
loop:
	for {
		select {
		case e := <-p.OutboundFan():
			if e.Kind == uint32(kinds.KindConvPayload) {
				if e.UserID == 8 {
					saw8 = true
				}
				if e.UserID == 999 {
					saw999 = true
				}
			}
		case <-deadline:
			break loop
		}
	}
	if !saw8 {
		t.Fatal("mentioned member 8 got no 202 notice")
	}
	if saw999 {
		t.Fatal("non-member 999 received a notice")
	}
}
