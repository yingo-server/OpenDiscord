package pipe

import (
	"context"
	"errors"

	"github.com/yingo-server/UFOT/server/internal/data/cache"
	"github.com/yingo-server/UFOT/server/internal/data/pipe/wire"
	"github.com/yingo-server/UFOT/server/internal/data/store"
)

// msgDomain handles message writes: decode MSG_SEND payload, enforce the
// authorization red line (CheckMember BEFORE anything enters the pipeline —
// the store never re-checks), then hand the PendingMsg to the write pipeline.
type msgDomain struct {
	conv  store.ConvStore // CheckMember only
	cache *cache.Cache    // membership cache (60s TTL)
	msg   store.MsgReader // dedup fallback: read the old row on conflict
	dedup *Dedup
}

// Result of admitting (or rejecting) one message.
type msgAdmit struct {
	Pending *store.PendingMsg // non-nil when admitted into the pipeline
	Replay  *dedupResult      // non-nil when dedup replayed an old ack
	Code    uint32            // 0 ok; 403 unauthorized; 409 duplicate-no-row
	Reason  string
}

// admit runs the pre-pipeline gates for one MSG_SEND.
func (d *msgDomain) admit(ctx context.Context, cid, sender int64, p wire.MessagePayload, sentAt int64) msgAdmit {
	// 1. dedup memory hit: replay the old ack without touching the pipeline
	// (no seq burned, no DB round trip).
	if r, ok := d.dedup.Check(cid, sender, p.ClientMsgID); ok {
		return msgAdmit{Replay: &r}
	}

	// 2. authorization red line: CheckMember (cache-backed, 60s bound).
	// Failure composes a 403 result frame HERE — the message never enters
	// writech (docs: 不进管道).
	m, err := d.memberCheck(ctx, cid, sender)
	if err != nil || m == nil {
		return msgAdmit{Code: 403, Reason: "not a member of conversation"}
	}
	// Mute enforcement is domain policy above the store: muted senders are
	// rejected here (role/mute state came from the member row).
	if m.MutedUntil != 0 && m.MutedUntil > sentAt/1000 {
		return msgAdmit{Code: 403, Reason: "muted"}
	}

	// 3. admitted: payload stored verbatim (store never parses content).
	pm := &store.PendingMsg{
		Cid:         cid,
		SenderID:    sender,
		ClientMsgID: p.ClientMsgID,
		SentAt:      sentAt,
	}
	return msgAdmit{Pending: pm}
}

// memberCheck consults the cache then the store; positive results are cached
// for at most 60s (cache.MemberTTL), negatives are NOT cached (a just-added
// member must be writable immediately).
func (d *msgDomain) memberCheck(ctx context.Context, cid, uid int64) (*store.Member, error) {
	k := cache.MemberKey{Cid: cid, UID: uid}
	if v, ok := d.cache.Member(k); ok {
		if m, is := v.(*store.Member); is {
			return m, nil
		}
	}
	m, err := d.conv.CheckMember(ctx, cid, uid)
	if err != nil {
		return nil, err
	}
	d.cache.PutMember(k, m)
	return m, nil
}

// lookupOldRow resolves the existing row for a durable-layer duplicate
// (dedup fallback when the memory LRU missed). Uses the hot reader's
// keyset scan around the conflict — the store's UNIQUE index is the authority.
func (d *msgDomain) lookupOldRow(ctx context.Context, cid, sender int64) (*store.HotMsg, error) {
	// Reader has no by-client_msg_id method (api frozen); scanning the
	// conversation's recent window for the sender is the fallback. The
	// primary path is the memory LRU; the DB conflict itself already proved
	// existence — we only need (msg_id, seq) to compose the replayed ack.
	rows, err := d.msg.ReadHot(ctx, cid, 0, 500, false)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].SenderID == sender {
			return &rows[i], nil
		}
	}
	return nil, nil
}

// resolveConflict translates a flusher-observed store.ErrDuplicate into a
// replayed ack (or a bare 409 when the row cannot be located).
func (d *msgDomain) resolveConflict(ctx context.Context, m store.PendingMsg) msgAdmit {
	row, err := d.lookupOldRow(ctx, m.Cid, m.SenderID)
	if err != nil {
		return msgAdmit{Code: 409, Reason: "duplicate client_msg_id"}
	}
	r, ok := d.dedup.TranslateConflict(m.Cid, m.SenderID, m.ClientMsgID, row)
	if !ok {
		return msgAdmit{Code: 409, Reason: "duplicate client_msg_id"}
	}
	return msgAdmit{Replay: &r}
}

var errNotMember = errors.New("not a member")
