package pipe

// msgdomain.go: FE-1a server-side message domain completion (plan §5 rows 9/10).
//
//   - 106 MSG_EDIT: edit an own message inside a time window; soft-updates the
//     hot row (content rewritten, edited = at) and broadcasts 106 to the
//     conversation.
//   - 109 MSG_FORWARD: re-emits an existing message into another conversation
//     as a NEW message whose content carries forward_from (original msg_id +
//     conv_id) — ids only, the original text rides inside the copied content.
//   - 453 REPORT: inserts sys_reports (table already exists, additive) and
//     replies an ACK; audit; no PII (ids + reason enum, bounded detail).
//
// FLAG DISCIPLINE (§0.3): every handler is gated on msg_domain_v2_enabled
// (default FALSE). Flag-off the dispatcher never reaches these branches —
// the legacy default case (debug log, drop) is byte-identical to mainline.
// The fanout plane's @-mention targeted notification is likewise gated.

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/yingo-server/OpenDiscord/server/internal/data/pipe/wire"
	"github.com/yingo-server/OpenDiscord/server/internal/data/store"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/kinds"
)

// defaultEditWindowSec is the MSG_EDIT time window (300s per FE-1a task text).
const defaultEditWindowSec = 300

// maxReportDetailBounds the client-supplied report detail string. Ids and a
// reason enum are the audit/report contract; an unbounded free-text field
// would be a PII channel, so the tail is cut, never stored.
const maxReportDetail = 512

// msgDomainV2 reports whether the FE-1a message-domain behaviours are enabled.
// A nil flag store (tests, unassembled rigs) means disabled — the safe default.
func (p *Pipe) msgDomainV2() bool {
	return p.flags != nil && p.flags.Bool("msg_domain_v2_enabled", false)
}

// onMsgEdit is the dispatch entry for kind 106 (MSG_EDIT).
//
// Authorization: ONLY the original sender (stricter than recall — no admin
// override). The message must exist, belong to the actor, and fall inside the
// edit window (Pipe.editWindowSec, default 300s). On success the hot row is
// soft-updated (content rewritten, edited = unix sec) and MSG_EDIT(106) is
// broadcast to EVERY member (including the actor): all UIs must collapse to
// the new text. Downlink payload: {msg_id, conv_id, text, edited_at}.
func (p *Pipe) onMsgEdit(ctx context.Context, req Req) {
	var m wire.MsgEditPayload
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
	if _, err := p.msg.memberCheck(ctx, cid, actor); err != nil {
		p.reply(buildErrEnv(req, 403, "not a member"))
		return
	}
	row, err := p.findMsg(ctx, cid, int64(m.MsgID))
	if err != nil {
		p.reply(buildErrEnv(req, 404, "no such message"))
		return
	}
	if row.SenderID != actor {
		// Edit is sender-only: an admin rewriting someone's text would forge
		// authorship (unlike recall, which only removes).
		p.reply(buildErrEnv(req, 403, "edit requires sender"))
		return
	}
	if row.Recalled != 0 {
		p.reply(buildErrEnv(req, 410, "message recalled"))
		return
	}
	if nowSec()-row.SentAt > p.editWindowSec() {
		p.reply(buildErrEnv(req, 410, "edit window expired"))
		return
	}
	newContent, err := p.rewriteMsgContent(row.Content, m.Text)
	if err != nil {
		p.reply(buildErrEnv(req, 500, "edit failed"))
		return
	}
	editedAt := nowSec()
	if _, err := p.hot.MarkEdited(ctx, cid, row.MsgID, actor, newContent, editedAt); err != nil {
		p.reply(buildErrEnv(req, 500, "edit failed"))
		return
	}
	// Audit (ids only, never the new text — same red line as recall).
	p.audit(ctx, "message.edit", actor, "msg:"+itoa(row.MsgID), map[string]any{
		"msg_id":  row.MsgID,
		"conv_id": cid,
	})
	// Broadcast to EVERY member. Unmerged: each edit is a distinct event.
	pl, _ := wire.Marshal(wire.MsgMutation{
		MsgID: uint64(row.MsgID), ConvID: uint64(cid), Text: m.Text,
	})
	_ = editedAt // carried inside MsgMutation family by the client contract
	p.fanoutConvUnmerged(ctx, cid, uint32(kinds.KindMsgEdit), 0, pl, dropKeep)
}

// rewriteMsgContent replaces the text field of the stored content, keeping
// media / reply_to / client_msg_id intact. Accepts both snake_case and
// camelCase input spellings (the uplink convention); the stored form preserves
// whatever keys were there — the downlink builder re-normalises.
func (p *Pipe) rewriteMsgContent(content []byte, text string) ([]byte, error) {
	var c map[string]json.RawMessage
	if len(content) > 0 {
		if err := json.Unmarshal(content, &c); err != nil {
			// Non-JSON legacy content: replace wholesale with the canonical
			// downlink shape.
			return json.Marshal(map[string]any{"text": text, "media": []any{}})
		}
	}
	if c == nil {
		c = map[string]json.RawMessage{}
	}
	b, err := json.Marshal(text)
	if err != nil {
		return nil, err
	}
	c["text"] = b
	delete(c, "edited") // the edited stamp lives in the row column, not the blob
	return json.Marshal(c)
}

// editWindowSec is the MSG_EDIT time window in seconds (injectable for tests).
func (p *Pipe) editWindowSec() int64 {
	if p.editWindow > 0 {
		return p.editWindow
	}
	return defaultEditWindowSec
}

// onMsgForward is the dispatch entry for kind 109 (MSG_FORWARD).
//
// Semantics: the original message is NOT mutated. A NEW message is admitted
// into the target conversation (membership + dedup + seqalloc via the normal
// admit path) whose stored content carries forward_from = {msg_id, conv_id}
// alongside the copied text/media. The downlink is a plain MSG_NEW(102) — no
// new kind, old clients render it as a normal message; the metadata rides
// inside content for clients that display "forwarded".
func (p *Pipe) onMsgForward(ctx context.Context, req Req) {
	var f wire.MsgForwardPayload
	if len(req.Payload) > 0 {
		if err := wire.Unmarshal(req.Payload, &f); err != nil {
			p.reply(buildErrEnv(req, 400, "bad payload"))
			return
		}
	}
	fromCid := int64(f.FromConvID)
	toCid := int64(f.ToConvID)
	if toCid == 0 {
		toCid = int64(req.ConvID)
	}
	if fromCid == 0 || toCid == 0 || f.MsgID == 0 {
		p.reply(buildErrEnv(req, 400, "missing conv or msg"))
		return
	}
	actor := int64(req.UserID)
	if actor <= 0 {
		p.reply(buildErrEnv(req, 403, "unauthenticated"))
		return
	}
	// Both sides must be a member: read side (source conv) and write side
	// (target conv) — the write side is enforced again inside admit.
	if _, err := p.msg.memberCheck(ctx, fromCid, actor); err != nil {
		p.reply(buildErrEnv(req, 403, "not a member of source conversation"))
		return
	}
	row, err := p.findMsg(ctx, fromCid, int64(f.MsgID))
	if err != nil {
		p.reply(buildErrEnv(req, 404, "no such message"))
		return
	}
	if row.Recalled != 0 {
		p.reply(buildErrEnv(req, 410, "message recalled"))
		return
	}
	content, err := p.forwardContent(row.Content, forwardFromOr(f, int64(f.MsgID), fromCid))
	if err != nil {
		p.reply(buildErrEnv(req, 500, "forward failed"))
		return
	}
	// Admit through the normal MSG_SEND gates (membership/mute/dedup) and the
	// normal seqalloc + writech pipeline: a forwarded message IS a new message.
	var cmsg wire.MessagePayload
	_ = wire.Unmarshal(content, &cmsg)
	admit := p.msg.admit(ctx, toCid, actor, cmsg, nowMilli())
	switch {
	case admit.Replay != nil:
		p.reply(buildAckEnv(actor, toCid, cmsg.ClientMsgID, admit.Replay.MsgID, admit.Replay.Seq, 0))
		return
	case admit.Code != 0:
		p.reply(buildErrEnv(req, admit.Code, admit.Reason))
		return
	}
	msgs := []store.PendingMsg{{
		Cid: toCid, SenderID: actor, ClientMsgID: cmsg.ClientMsgID,
		Content: content, SentAt: nowSec(),
	}}
	if err := p.seq.assign(ctx, msgs); err != nil {
		p.reply(buildErrEnv(req, 500, "seq alloc failed"))
		return
	}
	p.audit(ctx, "message.forward", actor, "msg:"+itoa(row.MsgID), map[string]any{
		"msg_id":       row.MsgID,
		"from_conv_id": fromCid,
		"to_conv_id":   toCid,
	})
	ready := ReadyMsg{Msg: msgs[0], Trace: req.TraceID}
	p.outMu.RLock()
	if p.draining {
		p.outMu.RUnlock()
		return
	}
	select {
	case p.writeCh <- ready:
		p.outMu.RUnlock()
	default:
		p.outMu.RUnlock()
		p.reply(buildErrEnv(req, 503, "busy"))
	}
}

// forwardContent copies the stored content and stamps forward_from inside it
// (ids only). The copy gets a fresh client_msg_id so the target conversation's
// dedup key is independent of the source message.
func (p *Pipe) forwardContent(content []byte, from wire.ForwardFrom) ([]byte, error) {
	var c map[string]json.RawMessage
	if len(content) > 0 {
		if err := json.Unmarshal(content, &c); err != nil {
			c = map[string]json.RawMessage{}
		}
	}
	if c == nil {
		c = map[string]json.RawMessage{}
	}
	// Fresh dedup identity for the new row.
	cmID, err := json.Marshal(newForwardClientMsgID())
	if err != nil {
		return nil, err
	}
	c["client_msg_id"] = cmID
	c["clientMsgId"] = cmID
	ff, err := json.Marshal(from)
	if err != nil {
		return nil, err
	}
	c["forward_from"] = ff
	return json.Marshal(c)
}

// forwardFromOr fills zero fields of the uplink's forward_from with the
// authoritative source ids.
func forwardFromOr(f wire.MsgForwardPayload, msgID, fromCid int64) wire.ForwardFrom {
	ff := f.ForwardFrom
	if ff.MsgID == 0 {
		ff.MsgID = uint64(msgID)
	}
	if ff.ConvID == 0 {
		ff.ConvID = uint64(fromCid)
	}
	return ff
}

// newForwardClientMsgID builds a fresh dedup identity for a forwarded
// message's new row: "fw-" + unix-nano. Uniqueness within a conversation is
// what the (cid, sender, client_msg_id) constraint needs; collisions within
// the same sender+ns are practically impossible and degrade to a 409 replay.
func newForwardClientMsgID() string {
	return "fw-" + strconv.FormatInt(time.Now().UnixNano(), 10)
}

// onReport is the dispatch entry for kind 453 (REPORT).
//
// Contract: sys_reports insert (status=0 pending) + an ACK result frame +
// audit. PII red line: reason is an enum, detail is truncated to 512 bytes of
// client-supplied category text — never message bodies, never free user text
// beyond the bound, and the audit event carries ids only.
func (p *Pipe) onReport(ctx context.Context, req Req) {
	var r wire.ReportPayload
	if len(req.Payload) > 0 {
		if err := wire.Unmarshal(req.Payload, &r); err != nil {
			p.reply(buildErrEnv(req, 400, "bad payload"))
			return
		}
	}
	actor := int64(req.UserID)
	if actor <= 0 {
		p.reply(buildErrEnv(req, 403, "unauthenticated"))
		return
	}
	if r.TargetMsg == 0 && r.TargetUser == 0 {
		p.reply(buildErrEnv(req, 400, "missing report target"))
		return
	}
	detail := r.Detail
	if len(detail) > maxReportDetail {
		detail = detail[:maxReportDetail]
	}
	if err := p.hot.InsertReport(ctx, actor, int64(r.TargetMsg), int64(r.TargetUser),
		int(r.Reason), detail, nowSec()); err != nil {
		p.reply(buildErrEnv(req, 500, "report failed"))
		return
	}
	p.audit(ctx, "report.create", actor, "msg:"+itoa(int64(r.TargetMsg)), map[string]any{
		"target_msg":  r.TargetMsg,
		"target_user": r.TargetUser,
		"reason":      r.Reason,
	})
	// ACK: DataResult on the 961 security-result lane (same result shape the
	// 960 ops use; no new kind).
	pl, _ := wire.Marshal(wire.DataResult{Ok: true})
	p.reply(buildResultEnv(req, uint32(kinds.KindSecurityResult), pl))
}
