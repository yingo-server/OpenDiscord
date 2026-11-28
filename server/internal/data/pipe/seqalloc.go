package pipe

import (
	"context"

	"github.com/yingo-server/UFOT/server/internal/data/store"
)

// seqAlloc batches per-conversation seq allocation: messages entering the
// write pipeline for the SAME cid in one dispatch cycle share ONE
// AllocateSeq range reservation (one UPDATE...RETURNING per conversation per
// batch, not per message — docs §1.1 seqalloc: 按会话聚组批量分配).
type seqAlloc struct {
	w store.MsgWriter
}

// assign reserves seq ranges per distinct cid in msgs and writes each
// message's Seq field in place. Messages are grouped by cid; each group gets
// one contiguous range [start, start+len-1] assigned in arrival order.
// msg_id assignment (snowflake) happens before this stage in the domain
// handler; if a message arrives with MsgID==0, a placeholder local id is
// minted so the store's PK is never zero.
func (s *seqAlloc) assign(ctx context.Context, msgs []store.PendingMsg) error {
	if len(msgs) == 0 {
		return nil
	}
	// group by cid, preserving order
	groups := map[int64][]int{} // cid -> indices into msgs
	order := []int64{}
	for i, m := range msgs {
		if _, ok := groups[m.Cid]; !ok {
			order = append(order, m.Cid)
		}
		groups[m.Cid] = append(groups[m.Cid], i)
	}
	for _, cid := range order {
		idxs := groups[cid]
		start, err := s.w.AllocateSeq(ctx, cid, len(idxs))
		if err != nil {
			return &seqAllocError{cid: cid, err: err}
		}
		for j, i := range idxs {
			msgs[i].Seq = start + int64(j)
			if msgs[i].MsgID == 0 {
				msgs[i].MsgID = localMsgID(cid, msgs[i].Seq)
			}
		}
	}
	return nil
}

type seqAllocError struct {
	cid int64
	err error
}

func (e *seqAllocError) Error() string {
	return "seqalloc: cid=" + itoa(e.cid) + ": " + e.err.Error()
}
func (e *seqAllocError) Unwrap() error { return e.err }

// localMsgID mints a deterministic placeholder msg id when a caller did not
// supply a snowflake. Composite avoids PK collisions within the deployment:
// (cid << 32) | seq is unique because (cid, seq) is unique in the hot store.
// Real snowflakes come from the domain layer; this is the safety net.
func localMsgID(cid, seq int64) int64 {
	return (cid << 32) | (seq & 0xFFFFFFFF)
}
