package main

// wire.go encodes/decodes the UFOT WS JSON wire form used by browser
// clients (see docs/client-contract.md). The stress tool speaks this format directly,
// so it exercises the exact production path instead of a private binary variant.
//
// Two important facts encoded here:
//   - The outer Envelope is JSON with snake_case keys; `payload` is base64 of
//     the raw payload bytes (encoding/json does that automatically for []byte).
//   - The data-plane payloads (MSG_SEND / MSG_ACK / MSG_NEW / TYPING / RECEIPT
//     / PRESENCE) are camelCase *or* snake_case JSON bodies — NOT protobuf. The
//     server's data pipe unmarshals them with encoding/json. Mixing this up is
//     exactly why the old tool's ACK correlation silently failed.

import (
	"encoding/json"
	"math/rand"
)

// randIntn wraps math/rand.Intn (kept tiny so wire.go has one rand dependency).
func randIntn(n int) int { return rand.Intn(n) }

// envelope mirrors server/internal/user/net/frame jsonEnvelope. We keep our own
// copy so the tool does not import deep server internals; the shape is a stable
// public wire contract. Field set must stay identical (server decodes with
// DisallowUnknownFields).
type envelope struct {
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

// msgSendPayload is the MSG_SEND(101) body (wire.MessagePayload subset).
type msgSendPayload struct {
	ClientMsgID string `json:"clientMsgId,omitempty"`
	Text        string `json:"text,omitempty"`
}

// msgAckPayload is the MSG_ACK(103) body (wire.MsgAck). status 0 = OK, non-zero
// = server NACK (e.g. rate limited at the data layer).
type msgAckPayload struct {
	MsgID       uint64 `json:"msgId,omitempty"`
	Seq         uint64 `json:"seq,omitempty"`
	ClientMsgID string `json:"clientMsgId,omitempty"`
	Status      uint32 `json:"status,omitempty"`
}

// msgNewPayload is the MSG_NEW(102) body. The server emits it with snake_case
// keys and `content` as an embedded JSON object (buildMsgNewPayloadWithMedia),
// whose own client id key is `client_msg_id`. We decode content to correlate
// peer reception against the originating client_msg_id (send→peer_recv).
type msgNewPayload struct {
	MsgID    uint64          `json:"msg_id,omitempty"`
	Seq      uint64          `json:"seq,omitempty"`
	SenderID uint64          `json:"sender_id,omitempty"`
	SentAt   int64           `json:"sent_at,omitempty"`
	Content  json.RawMessage `json:"content,omitempty"`
}

// msgNewContent is the embedded content object inside MSG_NEW.
type msgNewContent struct {
	ClientMsgID string `json:"client_msg_id,omitempty"`
}

// typingPayload is the TYPING(230) body (wire.TypingSignal uplink form).
type typingPayload struct {
	ConvID uint64 `json:"conv_id,omitempty"`
	Typing bool   `json:"typing"`
}

// receiptPayload is the RECEIPT(104/105) body. The server rejects an empty
// receipt ("empty receipts"), so we always carry a receipts array with a
// non-zero msg_id (the fanout path rewrites the actor + timestamp).
type receiptPayload struct {
	Receipts []receiptItem `json:"receipts"`
	MsgID    uint64        `json:"msg_id,omitempty"`
	Rtype    int32         `json:"rtype,omitempty"`
}

type receiptItem struct {
	MsgID uint64 `json:"msg_id"`
}

// presencePayload is the PRESENCE_UPDATE(400) body (wire.PresenceSignal).
type presencePayload struct {
	UserID uint64 `json:"user_id,omitempty"`
	Status string `json:"status,omitempty"`
	At     int64  `json:"at,omitempty"`
}

var hexChars = []byte("0123456789abcdef")

// randHex returns n random lowercase hex chars. math/rand's global source is
// goroutine-safe; token/device ids do not require crypto strength.
func randHex(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = hexChars[randIntn(len(hexChars))]
	}
	return string(b)
}

// encodeEnvelope marshals an envelope to the JSON wire form.
func encodeEnvelope(e *envelope) ([]byte, error) { return json.Marshal(e) }

// decodeEnvelope parses a JSON wire frame received from the server.
func decodeEnvelope(b []byte) (*envelope, error) {
	var e envelope
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// Kind constants used by the tool. scripts/ lives outside server/ so it cannot
// import server/internal/proto/kinds (Go internal-package rule); these values
// are copied verbatim from that file, the single source of truth.
const (
	kindSyncRequest = uint32(8)   // SYNC_REQUEST (uplink gap pull; reply is 102 frames)
	kindRateLimited = uint32(13)  // RATE_LIMITED
	kindWatermark   = uint32(14)  // WATERMARK (pull-mode conv high-water notice)
	kindError       = uint32(10)  // ERROR (server-side NACK)
	kindMsgSend     = uint32(101) // MSG_SEND
	kindMsgNew      = uint32(102) // MSG_NEW
	kindMsgAck      = uint32(103) // MSG_ACK
	kindMsgRead     = uint32(104) // MSG_READ (receipt)
	kindTyping      = uint32(230) // TYPING
	kindPresence    = uint32(400) // PRESENCE_UPDATE
)

// watermarkPayload is the WATERMARK(14) body (spec §1.1/§4.4: camelCase JSON,
// the same wire convention as the conv payloads). reason/memberCount are
// diagnostic-only: client logic (and this tool) must never branch on them.
type watermarkPayload struct {
	ConvID      uint64 `json:"convId,omitempty"`
	LatestSeq   uint64 `json:"latestSeq,omitempty"`
	Reason      uint32 `json:"reason,omitempty"`
	MemberCount uint32 `json:"memberCount,omitempty"`
}

// syncRequestPayload is the uplink SYNC(8) body. The server decodes it as
// wire.ReadHistory ({convId, sinceSeq, limit} — pipe.go onSyncRequest) and
// replays every missed message as plain MSG_NEW(102) frames, ascending seq.
type syncRequestPayload struct {
	ConvID   uint64 `json:"convId,omitempty"`
	SinceSeq uint64 `json:"sinceSeq,omitempty"`
	Limit    uint32 `json:"limit,omitempty"`
}

// encodeWatermark / decodeWatermark marshal the 14 body. Kept as named helpers
// so the round-trip test can pin the exact wire contract (camelCase keys).
func encodeWatermark(w *watermarkPayload) ([]byte, error) { return json.Marshal(w) }

func decodeWatermark(b []byte) (*watermarkPayload, error) {
	var w watermarkPayload
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, err
	}
	return &w, nil
}

// encodeSyncRequest marshals the 8 body (same shape as the 920 ReadHistory).
func encodeSyncRequest(s *syncRequestPayload) ([]byte, error) { return json.Marshal(s) }

func decodeSyncRequest(b []byte) (*syncRequestPayload, error) {
	var s syncRequestPayload
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// syncPullLimit is the per-SYNC page size the tool asks for. It mirrors the
// client's single-page limit (spec §2.2: 单次 8 limit=200) and the server's
// catch-up page bound (readrouter.catchup clamps limit to 200).
const syncPullLimit = uint32(200)
