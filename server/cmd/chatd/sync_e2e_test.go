package main

// sync_e2e_test.go: end-to-end validation of the SYNC (kind 8) gap-recovery
// path over the real stack (WS → user gw → bridge → data pipe → SQLite).
//
// Scenario: A and B in a direct conversation. A sends three messages (seq
// 1,2,3). B disconnects, reconnects, then sends SYNC{sinceSeq:1}. The server
// must replay the missed messages (seq 2 then 3) as MSG_NEW frames read from
// the store — proving the real link (not a socket buffer) is what fills the gap.

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/yingo-server/UFOT/server/internal/data/pipe/wire"
	"github.com/yingo-server/UFOT/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
	"github.com/yingo-server/UFOT/server/internal/user/net/frame"
)

func TestE2ESyncAfterReconnect(t *testing.T) {
	a, _ := startAssembly(t)
	base := "http://" + a.user.Addr()

	alice := loginGuest(t, base, "alice-sync")
	bob := loginGuest(t, base, "bob-sync")

	ca := dialToken(t, a.user.Addr(), alice.AccessToken)
	defer ca.Close()
	cb := dialToken(t, a.user.Addr(), bob.AccessToken)
	time.Sleep(100 * time.Millisecond)

	// --- A: create direct conversation with B ---
	createPayload, err := (&chatpb.ConvCreate{ConvType: 1, MemberIds: []uint64{uint64(bob.UserID)}}).Marshal()
	if err != nil {
		t.Fatalf("marshal ConvCreate: %v", err)
	}
	createRaw, _ := frame.Encode(&chatpb.Envelope{Id: 1, Kind: kinds.KindConvCreate, RequestId: "sync-conv-create", Payload: createPayload})
	if err := ca.WriteMessage(websocket.BinaryMessage, createRaw); err != nil {
		t.Fatalf("write CONV_CREATE: %v", err)
	}
	resEnv, seen := readKind(t, ca, kinds.KindConvDataResult, 8*time.Second)
	var res wire.DataResult
	if err := json.Unmarshal(resEnv.Payload, &res); err != nil || !res.Ok {
		t.Fatalf("CONV_CREATE failed: %v %q (seen=%v)", err, resEnv.Payload, seen)
	}
	var created wire.ConvPayload
	if err := json.Unmarshal(res.Body, &created); err != nil || created.ConvID == 0 {
		t.Fatalf("931 body: %v %+v", err, created)
	}
	cid := int64(created.ConvID)

	// --- A: send three messages; each ack must carry seq 1,2,3 ---
	for i := 1; i <= 3; i++ {
		mp, _ := (&chatpb.MessagePayload{ClientMsgId: fmt.Sprintf("sync-msg-%d", i), Text: fmt.Sprintf("m%d", i)}).Marshal()
		raw, _ := frame.Encode(&chatpb.Envelope{
			Id: uint64(10 + i), Kind: kinds.KindMsgSend, RequestId: fmt.Sprintf("sync-send-%d", i),
			Target: &chatpb.Envelope_ConvId{ConvId: uint64(cid)}, Payload: mp,
		})
		if err := ca.WriteMessage(websocket.BinaryMessage, raw); err != nil {
			t.Fatalf("write MSG_SEND %d: %v", i, err)
		}
		ackEnv, _ := readKind(t, ca, kinds.KindMsgAck, 8*time.Second)
		var ack wire.MsgAck
		if err := json.Unmarshal(ackEnv.Payload, &ack); err != nil {
			t.Fatalf("MSG_ACK %d: %v", i, err)
		}
		if ack.Seq != uint64(i) {
			t.Fatalf("ack %d seq = %d, want %d", i, ack.Seq, i)
		}
	}

	// --- B: drop and reconnect (fresh connection, empty socket buffer) ---
	_ = cb.Close()
	cb2 := dialToken(t, a.user.Addr(), bob.AccessToken)
	defer cb2.Close()
	time.Sleep(100 * time.Millisecond)

	// --- B: SYNC after seq 1 → server must replay seq 2 and 3 as MSG_NEW ---
	syncP, _ := (&chatpb.SyncRequest{ConvId: uint64(cid), SinceSeq: 1, Limit: 100}).Marshal()
	syncRaw, _ := frame.Encode(&chatpb.Envelope{
		Id: 99, Kind: kinds.KindSyncRequest, RequestId: "sync-req",
		Target: &chatpb.Envelope_ConvId{ConvId: uint64(cid)}, Payload: syncP,
	})
	if err := cb2.WriteMessage(websocket.BinaryMessage, syncRaw); err != nil {
		t.Fatalf("write SYNC_REQUEST: %v", err)
	}

	var got []uint64
	deadline := time.Now().Add(8 * time.Second)
	for len(got) < 2 && time.Now().Before(deadline) {
		env, _ := readKind(t, cb2, kinds.KindMsgNew, 8*time.Second)
		var mn struct {
			Seq    uint64 `json:"seq"`
			ConvID uint64 `json:"conv_id"`
		}
		if err := json.Unmarshal(env.Payload, &mn); err != nil {
			t.Fatalf("MSG_NEW payload: %v (%q)", err, env.Payload)
		}
		if mn.ConvID == uint64(cid) && mn.Seq > 1 {
			got = append(got, mn.Seq)
		}
	}
	if len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("SYNC replay seqs = %v, want [2 3] (cid=%d)", got, cid)
	}
	t.Logf("E2E SYNC: B reconnected and received replay seqs %v after SYNC{sinceSeq:1} (cid=%d)", got, cid)
}
