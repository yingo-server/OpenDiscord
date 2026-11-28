package pipe

import (
	"context"
	"fmt"

	"github.com/yingo-server/UFOT/server/internal/data/pipe/wire"
	"github.com/yingo-server/UFOT/server/internal/data/store"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
)

// ColdReader is the OPTIONAL dependency for cold-merge reads. When nil (no
// cold DB wired — the default in this batch), a read that would need the cold
// store returns the hot page plus the cold_merge marker and the caller sees
// an empty cold extension. The assembly layer injects the real implementation
// (pg.ColdDB satisfies it via Search/ReadCold adapter).
type ColdReader interface {
	ReadCold(ctx context.Context, cid, beforeSeq int64, limit int) ([]store.HotMsg, error)
}

// readRouter serves kind 920 (READ_HISTORY) from the hot store, and — when
// the requested window extends past what the hot store still holds — marks
// the reply with cold_merge semantics (docs §1.1: hot/cold/merge 透明路由).
type readRouter struct {
	r    store.MsgReader
	cold ColdReader // nil = cold not wired; replies carry from_cold=false + flag
}

// ReadHistoryRequest mirrors wire.ReadHistory.
func (rr *readRouter) handle(ctx context.Context, req wire.ReadHistory, senderUID int64) (*wire.ReadHistoryResult, error) {
	limit := int(req.Limit)
	if limit <= 0 || limit > 500 {
		limit = 50 // paging default; bound is mechanism, page size choice is caller's
	}
	// Authorization is enforced by the domain handler BEFORE this call
	// (CheckMember red line); readRouter contains no business semantics.

	// "Older" direction (opt-in via beforeSeq): a keyset cursor walking
	// backwards into history. This is the direction the client uses when the
	// user scrolls up past what the local cache already holds. It is a
	// distinct code path so the legacy sinceSeq behaviour below stays byte
	// for byte identical (kind 8 SYNC catch-up / old clients).
	if req.BeforeSeq > 0 {
		return rr.handleBefore(ctx, req, limit)
	}

	// cursor semantics: since_seq is the keyset cursor. Descending page
	// (typical history scroll) reads rows with seq < cursor.
	rows, err := rr.r.ReadHot(ctx, int64(req.ConvID), int64(req.SinceSeq), limit, false)
	if err != nil {
		return nil, fmt.Errorf("readrouter: hot read cid=%d: %w", req.ConvID, err)
	}

	res := &wire.ReadHistoryResult{ConvID: req.ConvID}
	for _, m := range rows {
		res.Messages = append(res.Messages, hotToMsgNew(m))
	}
	res.HasMore = len(rows) == limit

	// cold_merge determination: the hot page came back short of the requested
	// page size AND a cursor exists (we're deep in history). The cold store
	// is only consulted when it is wired; without ColdReader we return what
	// we have plus the merge flag so the caller knows the window may extend
	// into the archive (and the reply is NOT falsely "complete").
	if len(rows) < limit && req.SinceSeq > 0 {
		if rr.cold != nil {
			coldRows, cerr := rr.cold.ReadCold(ctx, int64(req.ConvID), int64(req.SinceSeq), limit-len(rows))
			if cerr == nil && len(coldRows) > 0 {
				for _, m := range coldRows {
					res.Messages = append(res.Messages, hotToMsgNew(m))
				}
				res.FromCold = true
			}
			// cold errors are NOT fatal: hot rows are already a valid answer;
			// the merge flag below tells the caller the archive was consulted.
		}
		// Mark merge-needed. When cold is not wired (nil), this batch simply
		// returns hot rows + the marker; the assembly layer's injected
		// ColdReader is where real merge happens (see ColdReader doc).
		res.HasMore = res.HasMore || len(rows) < limit
	}
	return res, nil
}

// handleBefore serves the "older" direction of kind 920: it reads the page
// immediately BEFORE beforeSeq (seq < beforeSeq) and returns it in ASCENDING
// seq order so the caller can prepend it verbatim. The hot store returns the
// page descending (newest-of-the-page first); we reverse here.
//
// hasMore is true iff a full page came back (there may be yet older rows).
// We deliberately do NOT consult the cold store here: this is the minimal,
// additive cursor the client drives one page at a time (limit=50), and the
// client stops on its own once hasMore=false.
func (rr *readRouter) handleBefore(ctx context.Context, req wire.ReadHistory, limit int) (*wire.ReadHistoryResult, error) {
	rows, err := rr.r.ReadHot(ctx, int64(req.ConvID), int64(req.BeforeSeq), limit, false)
	if err != nil {
		return nil, fmt.Errorf("readrouter: hot read (before) cid=%d before=%d: %w", req.ConvID, req.BeforeSeq, err)
	}
	res := &wire.ReadHistoryResult{ConvID: req.ConvID}
	// rows are seq DESC; emit ASC so the client prepends without re-sorting.
	for i := len(rows) - 1; i >= 0; i-- {
		res.Messages = append(res.Messages, hotToMsgNew(rows[i]))
	}
	res.HasMore = len(rows) == limit
	return res, nil
}

// catchup serves SYNC (kind 8) gap recovery: it returns the conversation's
// messages with seq strictly greater than sinceSeq, in ASCENDING seq order,
// so a reconnecting client can replay everything it missed after its last
// seen seq. Authorization is the caller's concern (memberCheck red line);
// this method carries no business semantics. Cold is not consulted: catch-up
// targets the recent tail, which always lives in the hot store.
func (rr *readRouter) catchup(ctx context.Context, cid, sinceSeq int64, limit int) ([]store.HotMsg, error) {
	if limit <= 0 || limit > 500 {
		limit = 200 // catch-up page size bound; the client pages by re-sending SYNC
	}
	rows, err := rr.r.ReadHot(ctx, cid, sinceSeq, limit, true)
	if err != nil {
		return nil, fmt.Errorf("readrouter: catchup cid=%d since=%d: %w", cid, sinceSeq, err)
	}
	return rows, nil
}

// hotToMsgNew maps a hot row to the wire MsgNew view.
//
// Recalled rides along: the hot row's soft-delete column (0 = live, else the
// unix-second recall timestamp). Without it a device that pulls history
// AFTER the recall broadcast (new phone, reinstall, or a plain 920 fetch)
// would receive the original text of a message that was already retracted.
func hotToMsgNew(m store.HotMsg) wire.MsgNew {
	return wire.MsgNew{
		MsgID:    uint64(m.MsgID),
		Seq:      uint64(m.Seq),
		SenderID: uint64(m.SenderID),
		SentAt:   m.SentAt,
		Recalled: m.Recalled,
		Content:  m.Content,
	}
}

// readHistoryResultPayload marshals a result for a 921 reply frame.
func readHistoryResultPayload(res *wire.ReadHistoryResult) []byte {
	b, err := wire.Marshal(res)
	if err != nil {
		// A marshalling failure must never surface as an empty payload: the
		// caller would decode "" and report a protocol error instead of a
		// usable result. Fall back to an explicit error result.
		b, _ = wire.Marshal(wire.DataResult{Ok: false, Error: "marshal result: " + err.Error()})
	}
	return b
}

var _ = kinds.KindReadHistoryResult // doc link: reply kind 921
