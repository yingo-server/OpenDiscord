package main

// call_e2e_test.go: end-to-end validation of the WebRTC call-signaling relay
// over the REAL stack (WS → user gw → bridge → data pipe → bridge → WS).
//
// Two real guest users, two real WS connections, one direct conversation.
// A sends CALL_OFFER → B must receive it with fromUserId == A (the server
// backfills the real sender). B replies CALL_ANSWER → A must receive it.
// A sends CALL_HANGUP → B must receive it. No media ever crosses the server.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/yingo-server/UFOT/server/internal/data/pipe/wire"
	"github.com/yingo-server/UFOT/server/internal/data/store"
	"github.com/yingo-server/UFOT/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
	"github.com/yingo-server/UFOT/server/internal/user/net/frame"
)

func writeCallFrame(t *testing.T, conn *websocket.Conn, id uint64, kind uint32, cid int64, sig wire.CallSignal) {
	t.Helper()
	payload, err := json.Marshal(sig)
	if err != nil {
		t.Fatalf("marshal call signal: %v", err)
	}
	raw, err := frame.Encode(&chatpb.Envelope{
		Id: id, Kind: kind, Target: &chatpb.Envelope_ConvId{ConvId: uint64(cid)}, Payload: payload,
	})
	if err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, raw); err != nil {
		t.Fatalf("write call frame kind=%d: %v", kind, err)
	}
}

func decodeCallEnv(t *testing.T, env *chatpb.Envelope) wire.CallSignal {
	t.Helper()
	var s wire.CallSignal
	if err := json.Unmarshal(env.Payload, &s); err != nil {
		t.Fatalf("decode call payload: %v (%s)", err, env.Payload)
	}
	return s
}

func TestE2ECallSignalRelay(t *testing.T) {
	a, _ := startAssembly(t)

	// Enable calling for this process (default is off). The pipe reads the
	// flag from sys_feature_flags on every signal, so this takes effect live.
	if err := a.hot.UpsertFlag(context.Background(), &store.Flag{Key: "call_enabled", Value: "true"}); err != nil {
		t.Fatalf("enable call_enabled: %v", err)
	}

	base := "http://" + a.user.Addr()
	alice := loginGuest(t, base, "alice-call")
	bob := loginGuest(t, base, "bob-call")

	ca := dialToken(t, a.user.Addr(), alice.AccessToken)
	defer ca.Close()
	cb := dialToken(t, a.user.Addr(), bob.AccessToken)
	defer cb.Close()
	time.Sleep(100 * time.Millisecond)

	// --- A: create a direct conversation with B ---
	createPayload, err := (&chatpb.ConvCreate{ConvType: 1, MemberIds: []uint64{uint64(bob.UserID)}}).Marshal()
	if err != nil {
		t.Fatalf("marshal ConvCreate: %v", err)
	}
	createRaw, _ := frame.Encode(&chatpb.Envelope{Id: 1, Kind: kinds.KindConvCreate, RequestId: "call-conv-create", Payload: createPayload})
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

	const callID = "e2e-call-1"

	// --- A: CALL_OFFER → B ---
	writeCallFrame(t, ca, 10, uint32(kinds.KindCallOffer), cid, wire.CallSignal{
		CallID: callID, ConvID: uint64(cid), ToUserID: uint64(bob.UserID),
		SDP: "v=0\r\no=- offer", Media: "audio", FromUserID: 999999, // forged, must be overwritten
	})
	offerEnv, _ := readKind(t, cb, kinds.KindCallOffer, 8*time.Second)
	offer := decodeCallEnv(t, offerEnv)
	if offer.CallID != callID {
		t.Fatalf("offer callId = %q, want %q", offer.CallID, callID)
	}
	if offer.FromUserID != uint64(alice.UserID) {
		t.Fatalf("offer fromUserId = %d, want %d (real sender)", offer.FromUserID, alice.UserID)
	}

	// --- B: CALL_ANSWER → A ---
	writeCallFrame(t, cb, 11, uint32(kinds.KindCallAnswer), cid, wire.CallSignal{
		CallID: callID, SDP: "v=0\r\no=- answer", FromUserID: uint64(bob.UserID),
	})
	ansEnv, _ := readKind(t, ca, kinds.KindCallAnswer, 8*time.Second)
	ans := decodeCallEnv(t, ansEnv)
	if ans.CallID != callID || ans.FromUserID != uint64(bob.UserID) {
		t.Fatalf("answer = %+v, want callId %q from %d", ans, callID, bob.UserID)
	}

	// --- A: CALL_HANGUP → B ---
	writeCallFrame(t, ca, 12, uint32(kinds.KindCallHangup), cid, wire.CallSignal{
		CallID: callID, Reason: "hangup", FromUserID: uint64(alice.UserID),
	})
	hangEnv, _ := readKind(t, cb, kinds.KindCallHangup, 8*time.Second)
	hang := decodeCallEnv(t, hangEnv)
	if hang.CallID != callID || hang.Reason != "hangup" {
		t.Fatalf("hangup = %+v, want callId %q reason hangup", hang, callID)
	}
	if hang.FromUserID != uint64(alice.UserID) {
		t.Fatalf("hangup fromUserId = %d, want %d", hang.FromUserID, alice.UserID)
	}

	t.Logf("E2E call relay OK: A(%d)→B(%d) offer/answer/hangup (cid=%d)", alice.UserID, bob.UserID, cid)
}
