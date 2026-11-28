package pipe

// ack construction: the flusher and dedup replay both emit MSG_ACK-shaped
// envelopes {msg_id, seq, client_msg_id, status}.

import (
	"github.com/yingo-server/UFOT/server/internal/data/pipe/wire"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
)

// buildAckEnv composes the ack env for a committed (or replayed) message.
// status 0 = OK; non-zero is the failure code (flusher nack path).
//
// senderID is carried as env.UserID: the ack is addressed to the SENDER, and
// the bridge's default route for 100-499 fans out by conv_id/user_id only.
// Without the uid the ack is unroutable on a real bridge (assembly contract).
func buildAckEnv(senderID, cid int64, clientMsgID string, msgID, seq int64, status uint32) *env {
	ack := wire.MsgAck{
		MsgID:       uint64(msgID),
		Seq:         uint64(seq),
		ClientMsgID: clientMsgID,
		Status:      status,
	}
	pl, _ := wire.Marshal(ack)
	return &env{Kind: uint32(kinds.KindMsgAck), ConvID: uint64(cid),
		UserID: uint64(senderID), Payload: pl}
}

// buildErrEnv composes an ERROR envelope reply.
func buildErrEnv(req Req, code uint32, reason string) *env {
	pl, _ := wire.Marshal(struct {
		Code   uint32 `json:"code,omitempty"`
		Reason string `json:"reason,omitempty"`
	}{code, reason})
	return &env{Kind: uint32(kinds.KindError), RequestID: req.RequestID,
		ConvID: req.ConvID, UserID: req.UserID, Payload: pl}
}

// buildResultEnv composes a generic result envelope.
func buildResultEnv(req Req, kind uint32, payload []byte) *env {
	return &env{Kind: kind, RequestID: req.RequestID,
		ConvID: req.ConvID, UserID: req.UserID, Payload: payload}
}
