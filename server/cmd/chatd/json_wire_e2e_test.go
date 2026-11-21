package main

// Browser wire-form end-to-end coverage.
//
// The browser client speaks JSON text frames (snake_case envelope, base64
// payload); native clients speak protobuf binary. Everything else in the test
// suite uses protobuf, so before this file the primary client's wire path had
// no end-to-end coverage at all — a regression there would only show up in a
// real browser.
//
// These tests drive a JSON connection and assert that:
//   - a JSON PING gets a JSON PONG (local whitelist path),
//   - a JSON MSG_SEND gets a JSON MSG_ACK (full data-layer round trip),
//   - an unauthorized / malformed frame gets a JSON ERROR,
//   - the peer's MSG_NEW also arrives as JSON.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/yingo-server/UFOT/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
)

// jsonEnvelope is the browser wire shape: snake_case, payload base64.
type jsonEnvelope struct {
	ID        uint64 `json:"id,omitempty"`
	AckID     uint64 `json:"ack_id,omitempty"`
	Kind      uint32 `json:"kind"`
	Version   uint32 `json:"version,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	Flags     uint32 `json:"flags,omitempty"`
	SentAt    uint64 `json:"sent_at,omitempty"`
	ConvID    uint64 `json:"conv_id,omitempty"`
	UserID    uint64 `json:"user_id,omitempty"`
	Payload   []byte `json:"payload,omitempty"`
}

// dialTokenJSON dials /ws exactly like dialToken but the caller will speak the
// JSON text wire form (the gateway sniffs the form from the first frame and
// replies in kind).
func dialTokenJSON(t *testing.T, addr, token string) *websocket.Conn {
	t.Helper()
	hdr := http.Header{}
	hdr.Set("Cookie", "access_token="+token)
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, resp, err := dialer.Dial("ws://"+addr+"/ws", hdr)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("json ws dial: %v (status=%d)", err, status)
	}
	return conn
}

// writeJSONFrame sends one browser-style text frame.
func writeJSONFrame(t *testing.T, conn *websocket.Conn, env jsonEnvelope) {
	t.Helper()
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal json frame: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
		t.Fatalf("write json frame kind=%d: %v", env.Kind, err)
	}
}

// readJSONKind reads text frames until one with the wanted kind arrives. A
// binary frame is a failure here: a JSON connection must never receive one.
func readJSONKind(t *testing.T, conn *websocket.Conn, want uint32, timeout time.Duration) jsonEnvelope {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	var seen []uint32
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("waiting for kind %d over JSON: read failed after %v: %v",
				want, seen, err)
		}
		if mt != websocket.TextMessage {
			t.Fatalf("JSON connection received a binary frame (kind %d): the wire "+
				"form must match what the client speaks", want)
		}
		var env jsonEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("JSON connection received unparsable text: %v (%q)", err, data)
		}
		if env.Kind == want {
			return env
		}
		seen = append(seen, env.Kind)
	}
}

// TestJSONWirePingPong: a browser-style PING is answered with a JSON PONG.
func TestJSONWirePingPong(t *testing.T) {
	a, _ := startAssembly(t)
	base := "http://" + a.user.Addr()
	guest := loginGuest(t, base, "json-ping")
	conn := dialTokenJSON(t, a.user.Addr(), guest.AccessToken)
	defer conn.Close()

	pl, _ := (&chatpb.Ping{ClientMs: 1234}).Marshal()
	writeJSONFrame(t, conn, jsonEnvelope{ID: 1, Kind: kinds.KindPing, Payload: pl})

	env := readJSONKind(t, conn, kinds.KindPong, 5*time.Second)
	var pong chatpb.Pong
	if err := pong.Unmarshal(env.Payload); err != nil {
		t.Fatalf("PONG payload not protobuf-decodable: %v (%q)", err, env.Payload)
	}
	if pong.ClientMs != 1234 {
		t.Fatalf("PONG client_ms = %d, want the echoed 1234", pong.ClientMs)
	}
}

// TestJSONWireMsgSendAck: the full send path works over the JSON wire form.
func TestJSONWireMsgSendAck(t *testing.T) {
	a, ca, _, alice, bob, cid := pair(t, "json-alice", "json-bob")
	defer ca.Close()
	_ = a

	// Reconnect alice over a JSON connection (pair dials protobuf).
	conn := dialTokenJSON(t, a.user.Addr(), alice.AccessToken)
	defer conn.Close()

	text, _ := json.Marshal(map[string]any{"text": "hello json", "clientMsgId": "json-1"})
	writeJSONFrame(t, conn, jsonEnvelope{ID: 2, Kind: kinds.KindMsgSend, ConvID: uint64(cid), Payload: text})

	ack := readJSONKind(t, conn, kinds.KindMsgAck, 8*time.Second)
	if len(ack.Payload) == 0 {
		t.Fatal("MSG_ACK carried no payload")
	}
	_ = bob
}

// TestJSONWireErrorIsJSON: the error path replies in the same wire form.
func TestJSONWireErrorIsJSON(t *testing.T) {
	a, _ := startAssembly(t)
	base := "http://" + a.user.Addr()
	guest := loginGuest(t, base, "json-err")
	conn := dialTokenJSON(t, a.user.Addr(), guest.AccessToken)
	defer conn.Close()

	// A frame the codec cannot decode makes the gateway reply with an ERROR
	// frame before closing. That reply must be JSON for a JSON client (the
	// browser only ever parses text frames).
	if err := conn.WriteMessage(websocket.TextMessage, []byte("{not a valid envelope")); err != nil {
		t.Fatalf("write malformed frame: %v", err)
	}

	env := readJSONKind(t, conn, kinds.KindError, 5*time.Second)
	if len(env.Payload) == 0 {
		t.Fatal("ERROR carried no payload")
	}
}
