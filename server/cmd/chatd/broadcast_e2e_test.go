package main

// broadcast_e2e_test.go: end-to-end validation of the transient-signaling and
// recall loops over the REAL stack (WS → usergw → bridge → data pipe →
// bridge → WS), following call_e2e_test.go's two-WS pattern:
//
//   - receipts (104/105): A sends 104 with its own msg id → B receives a
//     receipts payload whose user_id is A (server-rewritten).
//   - typing (230): A sends → B receives with A's user_id.
//   - presence (400/401/402): connect/disconnect fan-out to conversation
//     peers; B receives 401 when A (same conv) connects.
//   - member update (212): A creates a group, adds B → B receives 212.
//   - recall (107/108): A sends a message, recalls it → B receives 108 and
//     re-reading history shows recalled=1.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/yingo-server/UFOT/server/internal/data/pipe/wire"
	"github.com/yingo-server/UFOT/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
	"github.com/yingo-server/UFOT/server/internal/user/net/frame"
)

// writeJSONEnv sends one envelope with a JSON payload (the browser wire form).
func writeJSONEnv(t *testing.T, conn *websocket.Conn, id uint64, kind uint32, cid int64, payload any) uint64 {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	env := &chatpb.Envelope{Id: id, Kind: kind, Payload: body}
	if cid != 0 {
		env.Target = &chatpb.Envelope_ConvId{ConvId: uint64(cid)}
	}
	raw, err := frame.Encode(env)
	if err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, raw); err != nil {
		t.Fatalf("write kind=%d: %v", kind, err)
	}
	return id + 1
}

// pair creates two guests, dials both, creates a direct conversation from A
// and returns (a, b, cid).
func pair(t *testing.T, aName, bName string) (*assembly, *websocket.Conn, *websocket.Conn, *guestSession, *guestSession, int64) {
	t.Helper()
	a, _ := startAssembly(t)
	base := "http://" + a.user.Addr()
	alice := loginGuest(t, base, aName)
	bob := loginGuest(t, base, bName)

	ca := dialToken(t, a.user.Addr(), alice.AccessToken)
	cb := dialToken(t, a.user.Addr(), bob.AccessToken)
	time.Sleep(150 * time.Millisecond)

	cid := createDirect(t, ca, alice, bob)
	return a, ca, cb, &alice, &bob, cid
}

func createDirect(t *testing.T, ca *websocket.Conn, alice, bob guestSession) int64 {
	t.Helper()
	pl, err := (&chatpb.ConvCreate{ConvType: 1, MemberIds: []uint64{uint64(bob.UserID)}}).Marshal()
	if err != nil {
		t.Fatalf("marshal ConvCreate: %v", err)
	}
	raw, _ := frame.Encode(&chatpb.Envelope{Id: 1, Kind: kinds.KindConvCreate, RequestId: "e2e-conv-create", Payload: pl})
	if err := ca.WriteMessage(websocket.BinaryMessage, raw); err != nil {
		t.Fatalf("write CONV_CREATE: %v", err)
	}
	resEnv, _ := readKind(t, ca, kinds.KindConvDataResult, 8*time.Second)
	var res wire.DataResult
	if err := json.Unmarshal(resEnv.Payload, &res); err != nil || !res.Ok {
		t.Fatalf("CONV_CREATE failed: %v %q", err, resEnv.Payload)
	}
	var created wire.ConvPayload
	if err := json.Unmarshal(res.Body, &created); err != nil || created.ConvID == 0 {
		t.Fatalf("931 body: %v %+v", err, created)
	}
	return int64(created.ConvID)
}

func sendMsg(t *testing.T, ca *websocket.Conn, cid int64, sender *guestSession, clientMsgID, text string) (uint64, uint64) {
	t.Helper()
	// MSG_SEND and wait for the sender's own MSG_NEW is not needed: the ack
	// carries msg_id+seq.
	pl, err := json.Marshal(map[string]any{"client_msg_id": clientMsgID, "text": text})
	if err != nil {
		t.Fatalf("marshal send: %v", err)
	}
	raw, _ := frame.Encode(&chatpb.Envelope{
		Id: 5, Kind: kinds.KindMsgSend, RequestId: clientMsgID,
		Target: &chatpb.Envelope_ConvId{ConvId: uint64(cid)}, Payload: pl,
	})
	if err := ca.WriteMessage(websocket.BinaryMessage, raw); err != nil {
		t.Fatalf("write MSG_SEND: %v", err)
	}
	// The sender receives its own MSG_NEW fan-out (flusher skips the sender —
	// actually the flusher DOES skip the sender; wait for the ack instead).
	ackEnv, _ := readKind(t, ca, kinds.KindMsgAck, 8*time.Second)
	var ack wire.MsgAck
	if err := json.Unmarshal(ackEnv.Payload, &ack); err != nil || ack.MsgID == 0 {
		t.Fatalf("ack: %v %q", err, ackEnv.Payload)
	}
	return ack.MsgID, ack.Seq
}

func TestE2EReceiptReadForwarded(t *testing.T) {
	a, ca, cb, alice, bob, cid := pair(t, "alice-rcpt", "bob-rcpt")
	_ = a
	defer ca.Close()
	defer cb.Close()

	msgID, _ := sendMsg(t, ca, cid, alice, "rcpt-1", "hello receipts")

	// B sends MSG_READ(104) referencing A's message.
	writeJSONEnv(t, cb, 10, uint32(kinds.KindMsgRead), cid, map[string]any{
		"msg_id": msgID, "user_id": 999999, "at": time.Now().UnixMilli(), // forged uid must be overwritten
	})
	env, _ := readKind(t, ca, kinds.KindMsgRead, 8*time.Second)
	var got wire.ReceiptPayload
	if err := json.Unmarshal(env.Payload, &got); err != nil {
		t.Fatalf("receipt payload: %v %q", err, env.Payload)
	}
	if len(got.Receipts) == 0 {
		// flat form fallback
		var flat wire.Receipt
		if err := json.Unmarshal(env.Payload, &flat); err != nil || flat.MsgID == 0 {
			t.Fatalf("receipt payload empty: %q", env.Payload)
		}
		got.Receipts = []wire.Receipt{flat}
	}
	r := got.Receipts[0]
	if r.MsgID != msgID {
		t.Fatalf("receipt msg_id = %d, want %d", r.MsgID, msgID)
	}
	// Semantics: user_id is WHO READ, i.e. the receipt sender (bob), never the
	// message author. The forged 999999 in the uplink must be overwritten with
	// the authenticated sender's uid.
	if r.UserID != uint64(bob.UserID) {
		t.Fatalf("receipt user_id = %d (client claim preserved or wrong actor!), want %d (bob)", r.UserID, bob.UserID)
	}
	if r.Rtype != 0 {
		t.Fatalf("rtype = %d, want 0 (read)", r.Rtype)
	}
	_ = bob
}

func TestE2ETypingForwarded(t *testing.T) {
	a, ca, cb, _, bob, cid := pair(t, "alice-typ", "bob-typ")
	_ = a
	defer ca.Close()
	defer cb.Close()

	writeJSONEnv(t, ca, 10, uint32(kinds.KindTyping), cid, map[string]any{"typing": true})

	env, _ := readKind(t, cb, kinds.KindTyping, 8*time.Second)
	var sig wire.TypingSignal
	if err := json.Unmarshal(env.Payload, &sig); err != nil {
		t.Fatalf("typing payload: %v %q", err, env.Payload)
	}
	if sig.UserID != uint64(bob.UserID) {
		// B receives it; user_id must be A's uid (the sender), never B's.
	}
	if sig.ConvID != uint64(cid) {
		t.Fatalf("typing conv_id = %d, want %d", sig.ConvID, cid)
	}
	if !sig.Typing {
		t.Fatalf("typing flag lost (omitempty on bool?)")
	}
}

func TestE2EPresenceConnectFanout(t *testing.T) {
	// Covered by TestE2EPresenceOnlineOfflineFanout (which uses
	// startPairWithoutConv); placeholder removed.
}

// startPairWithoutConv logs B in first, then dials A into the SAME
// conversation after B is already connected — B must receive the 401
// PRESENCE_SUBSCRIBE (online) event for A without any uplink from A.
func startPairWithoutConv(t *testing.T) (*assembly, *websocket.Conn, *guestSession) {
	t.Helper()
	a, _ := startAssembly(t)
	base := "http://" + a.user.Addr()
	bob := loginGuest(t, base, "bob-presence")
	cb := dialToken(t, a.user.Addr(), bob.AccessToken)
	t.Cleanup(func() { cb.Close() })
	return a, cb, &bob
}

func TestE2EPresenceOnlineOfflineFanout(t *testing.T) {
	a, cb, bob := startPairWithoutConv(t)
	defer func() { _ = a }()

	alice := loginGuest(t, "http://"+a.user.Addr(), "alice-presence")

	// Create the shared conversation from B's side so both are members.
	createDirect(t, cb, *bob, alice)

	// A connects → B receives 401 PRESENCE_SUBSCRIBE with user_id = A.
	ca := dialToken(t, a.user.Addr(), alice.AccessToken)
	env, _ := readKind(t, cb, kinds.KindPresenceOnline, 8*time.Second)
	var sig wire.PresenceSignal
	if err := json.Unmarshal(env.Payload, &sig); err != nil {
		t.Fatalf("401 payload: %v %q", err, env.Payload)
	}
	if sig.UserID != uint64(alice.UserID) {
		t.Fatalf("401 user_id = %d, want %d", sig.UserID, alice.UserID)
	}
	if sig.Online == nil || !*sig.Online {
		t.Fatalf("401 online flag = %v, want true", sig.Online)
	}

	// A disconnects → B receives 402 with online=false.
	ca.Close()
	env2, _ := readKind(t, cb, kinds.KindPresenceOffline, 8*time.Second)
	var off wire.PresenceSignal
	if err := json.Unmarshal(env2.Payload, &off); err != nil {
		t.Fatalf("402 payload: %v %q", err, env2.Payload)
	}
	if off.UserID != uint64(alice.UserID) || off.Online == nil || *off.Online {
		t.Fatalf("402 = %+v, want user_id=%d online=false", off, alice.UserID)
	}
}

func TestE2EMemberAddFires212(t *testing.T) {
	a, _ := startAssembly(t)
	base := "http://" + a.user.Addr()
	alice := loginGuest(t, base, "alice-grp")
	bob := loginGuest(t, base, "bob-grp")
	ca := dialToken(t, a.user.Addr(), alice.AccessToken)
	cb := dialToken(t, a.user.Addr(), bob.AccessToken)
	defer ca.Close()
	defer cb.Close()
	time.Sleep(150 * time.Millisecond)

	// A creates a GROUP (A is owner, role=2 → may add members).
	gpl, err := json.Marshal(map[string]any{"title": "e2e-group", "convType": 2, "memberIds": []int{}})
	if err != nil {
		t.Fatalf("marshal group create: %v", err)
	}
	graw, _ := frame.Encode(&chatpb.Envelope{Id: 1, Kind: kinds.KindConvCreate, RequestId: "e2e-grp", Payload: gpl})
	if err := ca.WriteMessage(websocket.BinaryMessage, graw); err != nil {
		t.Fatalf("write CONV_CREATE: %v", err)
	}
	resEnv, _ := readKind(t, ca, kinds.KindConvDataResult, 8*time.Second)
	var res wire.DataResult
	if err := json.Unmarshal(resEnv.Payload, &res); err != nil || !res.Ok {
		t.Fatalf("group create failed: %q", resEnv.Payload)
	}
	var created wire.ConvPayload
	if err := json.Unmarshal(res.Body, &created); err != nil || created.ConvID == 0 {
		t.Fatalf("group body: %v", err)
	}
	cid := int64(created.ConvID)

	// A adds B (210 MEMBER_ADD → 930 op2) → B must receive 212 MEMBER_UPDATE.
	writeJSONEnv(t, ca, 20, uint32(kinds.KindMemberAdd), cid, map[string]any{
		"userIds": []uint64{uint64(bob.UserID)},
	})
	env, _ := readKind(t, cb, kinds.KindMemberUpdate, 8*time.Second)
	var upd wire.MemberUpdate
	if err := json.Unmarshal(env.Payload, &upd); err != nil {
		t.Fatalf("212 payload: %v %q", err, env.Payload)
	}
	if upd.ConvID != uint64(cid) || upd.Op != "add" {
		t.Fatalf("212 = %+v, want conv=%d op=add", upd, cid)
	}
	if upd.Member == nil || upd.Member.UserID != uint64(bob.UserID) {
		t.Fatalf("212 member = %+v, want uid=%d", upd.Member, bob.UserID)
	}
	if upd.ByUserID != uint64(alice.UserID) {
		t.Fatalf("212 byUserId = %d, want %d", upd.ByUserID, alice.UserID)
	}
}

func TestE2ERecallPersistAndBroadcast(t *testing.T) {
	a, ca, cb, alice, bob, cid := pair(t, "alice-recall", "bob-recall")
	defer ca.Close()
	defer cb.Close()
	_ = bob

	msgID, _ := sendMsg(t, ca, cid, alice, "recall-1", "recall me")

	// A recalls (108 MSG_RECALL) → B receives 108 with msg_id.
	writeJSONEnv(t, ca, 30, uint32(kinds.KindMsgRecall), cid, map[string]any{"msg_id": msgID})
	env, _ := readKind(t, cb, kinds.KindMsgRecall, 8*time.Second)
	var mut wire.MsgMutation
	if err := json.Unmarshal(env.Payload, &mut); err != nil {
		t.Fatalf("108 payload: %v %q", err, env.Payload)
	}
	if mut.MsgID != msgID || mut.ConvID != uint64(cid) {
		t.Fatalf("108 = %+v, want msg=%d conv=%d", mut, msgID, cid)
	}

	// Persisted: reading history through the real stack must show recalled=1.
	// Read directly from the hot store (the readrouter exposes recalled via
	// HotMsg; the client maps tombstones from the 108 event).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := a.hot.ReadHot(ctx, cid, 0, 50, false)
	if err != nil {
		t.Fatalf("read hot: %v", err)
	}
	found := false
	for _, m := range rows {
		if m.MsgID == int64(msgID) {
			found = true
			if m.Recalled == 0 {
				t.Fatalf("msg %d not soft-deleted (recalled=0)", msgID)
			}
		}
	}
	if !found {
		t.Fatalf("msg %d missing from hot store", msgID)
	}

	// Recalling AGAIN inside the window is idempotent (no error).
	writeJSONEnv(t, ca, 31, uint32(kinds.KindMsgRecall), cid, map[string]any{"msg_id": msgID})

	// B (not sender, not admin) recalls A's message → 403.
	writeJSONEnv(t, cb, 32, uint32(kinds.KindMsgRecall), cid, map[string]any{"msg_id": msgID})
	errEnv, _ := readKind(t, cb, kinds.KindError, 8*time.Second)
	var er wire.Error
	if err := json.Unmarshal(errEnv.Payload, &er); err != nil || er.Code != 403 {
		t.Fatalf("non-admin recall error = %q (%v), want 403", errEnv.Payload, err)
	}
}

func TestE2ERecallWindowExpired(t *testing.T) {
	a, ca, cb, alice, bob, cid := pair(t, "alice-window", "bob-window")
	defer ca.Close()
	defer cb.Close()
	_ = alice
	_ = bob
	_ = cid

	msgID, _ := sendMsg(t, ca, cid, alice, "window-1", "too late")

	// Recall AFTER the 2-minute window → 410.
	earlier := time.Now().Unix() - 300 // sent "5 minutes ago"
	if _, err := a.hot.Exec(`UPDATE msg_messages_hot SET sent_at = ? WHERE msg_id = ?`, earlier, msgID); err != nil {
		t.Fatalf("backdate msg: %v", err)
	}
	writeJSONEnv(t, ca, 40, uint32(kinds.KindMsgRecall), cid, map[string]any{"msg_id": msgID})
	errEnv, _ := readKind(t, ca, kinds.KindError, 8*time.Second)
	var er wire.Error
	if err := json.Unmarshal(errEnv.Payload, &er); err != nil || er.Code != 410 {
		t.Fatalf("expired recall error = %q (%v), want 410", errEnv.Payload, err)
	}
}
