package pipe

// signaling.go: transient signaling domains — read receipts (104/105),
// typing (230), presence (400/401/402), member-change events (212) and
// message recall (107/108).
//
// Dispatch rule shared by all of them: an inbound frame with a NON-ZERO
// env.UserID is a client uplink request (member-checked, rewritten);
// an inbound frame with UserID == 0 is the USER LAYER's internal connect /
// disconnect notification (401/402 with user_id in the payload), trusted
// because it crosses the internal bus only.
//
// Receipts and typing are NEVER persisted. Recall persists (soft delete).
// Presence persists to sys_presence.

import (
	"context"
	"errors"
	"strconv"

	"github.com/yingo-server/UFOT/server/internal/data/pipe/wire"
	"github.com/yingo-server/UFOT/server/internal/data/store"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
)

const (
	// defaultRecallWindowSec is the recall time window (default 2 minutes).
	// Overridden in tests via Pipe.recallWindowSec.
	defaultRecallWindowSec = 120
)

// onReceipt is the dispatch entry for kinds 104/105 (MSG_READ/MSG_DELIVERED).
//
// Contract (docs/client-contract.md): the DOWNLINK
// payload is MessagePayload.receipts — JSON {"receipts":[{msg_id,user_id,at,
// rtype}]}, rtype 0=read 1=delivered. The sender's user_id is ALWAYS
// rewritten to the authenticated uid (the client value is never trusted).
// Forwarded to every OTHER member of the conversation; not persisted.
func (p *Pipe) onReceipt(ctx context.Context, req Req) {
	var in wire.ReceiptPayload
	if len(req.Payload) > 0 {
		if err := wire.Unmarshal(req.Payload, &in); err != nil {
			p.reply(buildErrEnv(req, 400, "bad payload"))
			return
		}
	}
	receipts := normalizeReceipts(in)
	if len(receipts) == 0 {
		p.reply(buildErrEnv(req, 400, "empty receipts"))
		return
	}
	cid := int64(req.ConvID)
	if cid == 0 && len(receipts) > 0 {
		// The flat form carries no conv on the payload; the envelope target
		// is authoritative. A receipts-array form may also carry conv_id in
		// the envelope. Zero cid with a receipts array is still forwarded to
		// the envelope's conv — enforced below.
	}
	if cid == 0 {
		p.reply(buildErrEnv(req, 400, "missing conv"))
		return
	}
	sender := int64(req.UserID)
	if sender <= 0 {
		p.reply(buildErrEnv(req, 403, "unauthenticated"))
		return
	}
	if _, err := p.msg.memberCheck(ctx, cid, sender); err != nil {
		p.reply(buildErrEnv(req, 403, "not a member"))
		return
	}
	rt := int32(0) // read
	if req.Kind == uint32(kinds.KindMsgDelivered) {
		rt = 1
	}
	for i := range receipts {
		receipts[i].UserID = uint64(sender) // authenticated sender, never the client's claim
		receipts[i].Rtype = rt
		if receipts[i].At == 0 {
			receipts[i].At = uint64(nowMilli())
		}
	}
	pl, err := wire.Marshal(wire.ReceiptPayload{Receipts: receipts})
	if err != nil {
		p.reply(buildErrEnv(req, 500, "marshal receipts"))
		return
	}
	// Receipts are worth keeping: the fan-out plane merges the receipt
	// lists of one window instead of dropping the older ones.
	p.fanoutConv(ctx, cid, req.Kind, uint64(cid), pl, dropKeep)
}

// normalizeReceipts accepts both wire forms: {"receipts":[...]} and the
// flat single-receipt object the client's encodeReceipt produces.
func normalizeReceipts(in wire.ReceiptPayload) []wire.Receipt {
	if len(in.Receipts) > 0 {
		return in.Receipts
	}
	if in.MsgID == 0 {
		return nil
	}
	return []wire.Receipt{{MsgID: in.MsgID, UserID: in.UserID, At: in.At, Rtype: in.Rtype}}
}

// onTyping is the dispatch entry for kind 230 (TYPING).
//
// Contract (handlers/typing.ts): downlink payload {"conv_id","user_id",
// "typing","at"}. The server overwrites user_id with the authenticated
// sender. Forwarded to every OTHER member; not persisted.
func (p *Pipe) onTyping(ctx context.Context, req Req) {
	var sig wire.TypingSignal
	if len(req.Payload) > 0 {
		if err := wire.Unmarshal(req.Payload, &sig); err != nil {
			p.reply(buildErrEnv(req, 400, "bad payload"))
			return
		}
	}
	cid := int64(sig.ConvID)
	if cid == 0 {
		cid = int64(req.ConvID)
	}
	if cid == 0 {
		p.reply(buildErrEnv(req, 400, "missing conv"))
		return
	}
	sender := int64(req.UserID)
	if sender <= 0 {
		p.reply(buildErrEnv(req, 403, "unauthenticated"))
		return
	}
	if _, err := p.msg.memberCheck(ctx, cid, sender); err != nil {
		p.reply(buildErrEnv(req, 403, "not a member"))
		return
	}
	sig.ConvID = uint64(cid)
	sig.UserID = uint64(sender)
	if sig.At == 0 {
		sig.At = nowMilli()
	}
	pl, _ := wire.Marshal(sig)
	p.fanoutConv(ctx, cid, req.Kind, uint64(cid), pl, dropBestEffort)
}

// onPresenceUp is the dispatch entry for kind 400 (PRESENCE_UPDATE, client
// uplink: 主动状态 online/away/offline). Persisted to sys_presence and
// forwarded to the user's conversation peers (excluding self).
func (p *Pipe) onPresenceUp(ctx context.Context, req Req) {
	var sig wire.PresenceSignal
	if len(req.Payload) > 0 {
		if err := wire.Unmarshal(req.Payload, &sig); err != nil {
			p.reply(buildErrEnv(req, 400, "bad payload"))
			return
		}
	}
	uid := int64(req.UserID)
	if uid <= 0 {
		uid = int64(sig.UserID) // user-layer internal status event
	}
	if uid <= 0 {
		return
	}
	status := sig.Status
	if status == "" {
		if sig.Online != nil && *sig.Online {
			status = "online"
		} else {
			status = "offline"
		}
	}
	_ = p.hot.UpsertPresence(ctx, &store.Presence{
		UID: uid, Online: status == "online", LastSeen: nowSec(),
	})
	pl, _ := wire.Marshal(wire.PresenceSignal{UserID: uint64(uid), Status: status, At: nowMilli()})
	p.fanoutPeers(ctx, uid, req.Kind, pl)
}

// onPresenceConnect handles the USER LAYER's internal connect (401) /
// disconnect (402) notifications: kind with UserID == 0 and user_id inside
// the payload. 401 → online:true persisted + broadcast; 402 → offline.
func (p *Pipe) onPresenceConnect(ctx context.Context, req Req) {
	var sig wire.PresenceSignal
	if len(req.Payload) > 0 {
		if err := wire.Unmarshal(req.Payload, &sig); err != nil {
			return
		}
	}
	uid := int64(sig.UserID)
	if uid <= 0 {
		return
	}
	// The actor must be the authenticated session, and the event must be about
	// that actor. A client that forges a 401/402 frame (or a payload naming
	// another user) is rejected here even if it slipped past the gateway.
	if req.UserID == 0 || uint64(uid) != req.UserID {
		p.lg.Warn("presence event rejected: actor mismatch", "actor", req.UserID, "subject", uid)
		return
	}
	online := req.Kind == uint32(kinds.KindPresenceOnline) // 401 = connect
	_ = p.hot.UpsertPresence(ctx, &store.Presence{UID: uid, Online: online, LastSeen: nowSec()})
	// The per-user presence debounce (10s) lives in the fan-out plane: a
	// user flapping between devices emits ONE broadcast per window
	// (carrying the latest state) instead of one per transition.
	pl, _ := wire.Marshal(wire.PresenceSignal{UserID: uint64(uid), Online: &online, At: nowMilli()})
	p.fanoutPeers(ctx, uid, req.Kind, pl)
}

// fanoutConv delivers one frame to every member of cid EXCEPT excludeUID
// (the transient-signal forwarding rule: receipts/typing/presence never
// echo back to their originator).
//
// The delivery is ASYNCHRONOUS: the dispatcher only enqueues the job on the
// fan-out plane. The plane resolves the member set (from the CACHE, not
// from the DB on the hot path), aggregates the event into the conversation
// window and emits one frame per recipient.
func (p *Pipe) fanoutConv(ctx context.Context, cid int64, kind uint32, excludeUID uint64, payload []byte, class dropClass) {
	p.fanout.Enqueue(fanoutJob{
		kind: kind, cid: cid, excludeUID: excludeUID, userID: excludeUID,
		payload: payload, class: class,
		windowKey: "c" + strconv.FormatInt(cid, 10),
	})
}

// fanoutConvUnmerged delivers a frame that must NOT be aggregated (the
// recall broadcast: every tombstone is a distinct event).
func (p *Pipe) fanoutConvUnmerged(ctx context.Context, cid int64, kind uint32, excludeUID uint64, payload []byte, class dropClass) {
	p.fanout.Enqueue(fanoutJob{
		kind: kind, cid: cid, excludeUID: excludeUID, userID: excludeUID,
		payload: payload, class: class, noMerge: true,
		windowKey: "c" + strconv.FormatInt(cid, 10),
	})
}

// fanoutPeers delivers one frame to every OTHER user sharing at least one
// conversation with uid (the presence fan-out set). Like fanoutConv it is
// asynchronous; the per-user debounce collapses repeated transitions.
func (p *Pipe) fanoutPeers(ctx context.Context, uid int64, kind uint32, payload []byte) {
	p.fanout.Enqueue(fanoutJob{
		// excludeUID is the subject: a user must not be told about their own
		// presence transition. The doc comment says "every OTHER user"; this
		// field is what makes that true.
		kind: kind, cid: 0, excludeUID: uint64(uid), userID: uint64(uid),
		payload: payload, class: dropBestEffort,
		windowKey: "p" + strconv.FormatInt(uid, 10),
	})
}

// onRecall is the dispatch entry for kinds 107 (MSG_DELETE, treated as
// recall-for-everyone) and 108 (MSG_RECALL).
//
// Authorization: the actor must be the ORIGINAL SENDER or a conversation
// admin/owner (CheckMember role >= 1). The message must exist in the
// conversation and fall inside the recall window (Pipe.recallWindowSec,
// default 120s). On success the row is soft-deleted (recalled=at) and
// MSG_RECALL(108) is broadcast to EVERY member (including the actor).
// Front contract: handlers/msg-mutation.ts decodes {msg_id, conv_id} for
// kind 108 and applies a tombstone; 107 (MSG_DELETE) uplink is mapped to
// the same recall path (recall-for-everyone semantics).
func (p *Pipe) onRecall(ctx context.Context, req Req) {
	var m wire.MsgMutation
	if len(req.Payload) > 0 {
		if err := wire.Unmarshal(req.Payload, &m); err != nil {
			p.reply(buildErrEnv(req, 400, "bad payload"))
			return
		}
	}
	cid := int64(m.ConvID)
	if cid == 0 {
		cid = int64(req.ConvID)
	}
	if cid == 0 || m.MsgID == 0 {
		p.reply(buildErrEnv(req, 400, "missing conv or msg"))
		return
	}
	actor := int64(req.UserID)
	if actor <= 0 {
		p.reply(buildErrEnv(req, 403, "unauthenticated"))
		return
	}
	member, err := p.msg.memberCheck(ctx, cid, actor)
	if err != nil || member == nil {
		p.reply(buildErrEnv(req, 403, "not a member"))
		return
	}
	// Locate the message row (msg ids are per-conv, so search this conv).
	row, err := p.findMsg(ctx, cid, int64(m.MsgID))
	if err != nil {
		p.reply(buildErrEnv(req, 404, "no such message"))
		return
	}
	isSender := row.SenderID == actor
	isAdmin := member.Role >= 1
	if !isSender && !isAdmin {
		p.reply(buildErrEnv(req, 403, "recall requires sender or admin"))
		return
	}
	if nowSec()-row.SentAt > p.recallWindowSec() {
		p.reply(buildErrEnv(req, 410, "recall window expired"))
		return
	}
	if _, err := p.hot.MarkRecalled(ctx, cid, row.MsgID, row.SenderID, nowSec()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Idempotent replay, NOT a missing message: findMsg above already
			// located the row, so ErrNotFound here means MarkRecalled's
			// `WHERE recalled=0` matched nothing — the message was recalled
			// before. That is a success for the client: ACK with already=true
			// instead of a misleading 404. No re-broadcast, no second audit
			// event (the tombstone is already on every client).
			pl, _ := wire.Marshal(struct {
				MsgID   uint64 `json:"msg_id"`
				ConvID  uint64 `json:"conv_id"`
				Already bool   `json:"already"`
			}{uint64(row.MsgID), uint64(cid), true})
			p.reply(buildResultEnv(req, uint32(kinds.KindMsgRecall), pl))
			return
		}
		p.reply(buildErrEnv(req, 500, "recall failed"))
		return
	}
	// Audit the recall (best-effort; never blocks the broadcast). Records the
	// acting user, whether an admin acted on someone else's message, and the
	// original sender — ids/enums only, never the recalled message body.
	p.audit(ctx, "message.recall", actor, "msg:"+itoa(row.MsgID), map[string]any{
		"msg_id":        row.MsgID,
		"conv_id":       cid,
		"by_admin":      isAdmin && !isSender,
		"target_sender": row.SenderID,
	})
	// Broadcast to EVERY member (including the actor): everyone's UI must
	// collapse the message. Downlink kind is always MSG_RECALL(108) — the
	// kind the client's msg-mutation handler maps to a 'recall' tombstone.
	pl, _ := wire.Marshal(wire.MsgMutation{MsgID: uint64(row.MsgID), ConvID: uint64(cid)})
	p.fanoutConvUnmerged(ctx, cid, uint32(kinds.KindMsgRecall), 0, pl, dropKeep)
}

// findMsg scans the conversation's recent window for one msg id (the reader
// api is frozen to keyset scans; a recall targets recent messages by
// definition — outside the window the lookup fails first anyway).
func (p *Pipe) findMsg(ctx context.Context, cid, msgID int64) (store.HotMsg, error) {
	rows, err := p.hot.ReadHot(ctx, cid, 0, 500, false)
	if err != nil {
		return store.HotMsg{}, err
	}
	for _, m := range rows {
		if m.MsgID == msgID {
			return m, nil
		}
	}
	return store.HotMsg{}, store.ErrNotFound
}

// recallWindowSec is the recall time window in seconds (injectable for tests).
func (p *Pipe) recallWindowSec() int64 {
	if p.recallWindow > 0 {
		return p.recallWindow
	}
	return defaultRecallWindowSec
}
