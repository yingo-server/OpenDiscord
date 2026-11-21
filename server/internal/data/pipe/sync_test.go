package pipe

import (
	"context"
	"fmt"
	"testing"

	"github.com/yingo-server/UFOT/server/internal/data/pipe/wire"
	"github.com/yingo-server/UFOT/server/internal/data/store"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
)

// SYNC (kind 8) gap recovery: rows seq 1,2,5 exist; a member requests
// SYNC{sinceSeq:2} and must receive ONLY seq 5, re-sent as a plain
// MSG_NEW(102) frame addressed to the requester (not a wrapped result).
func TestSyncRequestReplaysGap(t *testing.T) {
	p, msgs, convs, _ := newTestPipe(t)
	ctx := context.Background()
	cid, _ := convs.CreateConv(ctx, &store.ConvMeta{CType: 2, CreatedBy: 7, CreatedAt: 1}, 7)

	seed := func(seq int64, sender int64, text string) {
		id := fmt.Sprintf("m%d", seq)
		msgs.Rows[fmt.Sprintf("%d:%d:%s", cid, sender, id)] = store.HotMsg{
			MsgID: 1000 + seq, Cid: cid, Seq: seq, SenderID: sender,
			ClientMsgID: id, Content: []byte(fmt.Sprintf(`{"text":%q}`, text)), SentAt: seq,
		}
	}
	seed(1, 8, "one")
	seed(2, 8, "two")
	seed(5, 8, "five")

	// member (7) requests catch-up after seq 2.
	body := fmt.Sprintf(`{"convId":%d,"sinceSeq":2,"limit":100}`, cid)
	p.dispatchReq(ctx, mkReq(uint32(kinds.KindSyncRequest), cid, 7, body))

	e := waitEnv(t, p.Outbound())
	if e.Kind != uint32(kinds.KindMsgNew) {
		t.Fatalf("reply kind = %d, want MSG_NEW(%d)", e.Kind, kinds.KindMsgNew)
	}
	if e.UserID != 7 || e.ConvID != uint64(cid) {
		t.Fatalf("reply routed wrong: uid=%d cid=%d", e.UserID, e.ConvID)
	}
	var got struct {
		Seq   uint64 `json:"seq"`
		MsgID uint64 `json:"msg_id"`
	}
	if err := wire.Unmarshal(e.Payload, &got); err != nil {
		t.Fatalf("MSG_NEW payload: %v (%s)", err, e.Payload)
	}
	if got.Seq != 5 {
		t.Fatalf("replayed seq = %d, want 5 (only the gap; 1,2 precede sinceSeq)", got.Seq)
	}

	// No extra frames: seq 1 and 2 must NOT be replayed.
	select {
	case extra := <-p.Outbound():
		t.Fatalf("unexpected extra frame kind=%d payload=%s", extra.Kind, extra.Payload)
	default:
	}
}

// Non-member SYNC must be rejected with 403 and leak no message frames.
func TestSyncRequestNonMember403(t *testing.T) {
	p, msgs, convs, _ := newTestPipe(t)
	ctx := context.Background()
	cid, _ := convs.CreateConv(ctx, &store.ConvMeta{CType: 2, CreatedBy: 7, CreatedAt: 1}, 7)
	msgs.Rows[fmt.Sprintf("%d:%d:secret", cid, 7)] = store.HotMsg{
		MsgID: 5000, Cid: cid, Seq: 9, SenderID: 7, ClientMsgID: "secret",
		Content: []byte(`{"text":"secret"}`), SentAt: 9,
	}

	body := fmt.Sprintf(`{"convId":%d,"sinceSeq":0,"limit":100}`, cid)
	p.dispatchReq(ctx, mkReq(uint32(kinds.KindSyncRequest), cid, 99, body))

	e := waitEnv(t, p.Outbound())
	if e.Kind == uint32(kinds.KindMsgNew) {
		t.Fatalf("non-member received MSG_NEW: %s", e.Payload)
	}
	var er struct {
		Code uint32 `json:"code"`
	}
	if err := wire.Unmarshal(e.Payload, &er); err != nil || er.Code != 403 {
		t.Fatalf("want 403, got kind=%d payload=%s err=%v", e.Kind, e.Payload, err)
	}
}
