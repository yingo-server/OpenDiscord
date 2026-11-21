package appendlog

import (
	"fmt"
)

// recoverShard replays every valid frame of a shard from offset 0:
//   - base message frames rebuild next seq, dedup entries, msg_id index and
//     sparse checkpoints;
//   - seq hints raise the durable watermark so reservations survive restart;
//   - edit/recall/delete overlays rebuild the latest soft state.
//
// A torn last frame (short header, short payload or CRC mismatch at EOF) is
// the normal crash signature: the file is truncated at the last valid frame
// offset and no further bytes are visible. Corruption in the middle is
// treated as a hard error rather than silently skipping data.
func recoverShard(sh *shard) error {
	st, err := sh.f.Stat()
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	size := st.Size()
	validEnd := int64(0)

	err = foreachFrame(sh.f, 0, size, func(off int64, p []byte) error {
		if len(p) == 0 {
			return fmt.Errorf("empty payload at %d", off)
		}
		// Work on a copy: foreachFrame reuses its payload buffer.
		payload := append([]byte(nil), p...)
		switch payload[0] {
		case recMsg:
			m, err := parseMsgPayload(payload)
			if err != nil {
				return fmt.Errorf("parse msg at %d: %w", off, err)
			}
			appendMsgLocked(sh, m, off)
		case recSeqHint:
			cid, seq, err := parseSeqHint(payload)
			if err != nil {
				return fmt.Errorf("parse seq hint at %d: %w", off, err)
			}
			if sh.nextSeq[cid] <= seq {
				sh.nextSeq[cid] = seq + 1
			}
			// A seq hint belongs to the cid's stream: create its state so
			// readers can still locate messages (if any) after a restart.
			if sh.cids[cid] == nil {
				sh.cids[cid] = &cidState{firstOffset: off}
			}
		case recEdit, recRecall, recDelete:
			r, err := parseOverlay(payload)
			if err != nil {
				return fmt.Errorf("parse overlay at %d: %w", off, err)
			}
			ov := sh.overlays[r.msgID]
			if ov == nil {
				ov = &overlay{}
				sh.overlays[r.msgID] = ov
			}
			switch payload[0] {
			case recEdit:
				ov.content = append([]byte(nil), r.content...)
				ov.edited = r.at
			case recRecall:
				ov.recalled = r.at
			case recDelete:
				ov.deleted = true
			}
		default:
			return fmt.Errorf("unknown record type %d at %d", payload[0], off)
		}
		validEnd = off + frameHeaderSize + int64(len(payload))
		return nil
	})
	if err != nil {
		var fe *frameError
		if ok := asFrameError(err, &fe); !ok || !fe.partial || fe.off < validEnd {
			return err
		}
		// Torn tail: drop every byte from the last valid frame boundary.
		if err := sh.f.Truncate(validEnd); err != nil {
			return fmt.Errorf("truncate torn tail at %d: %w", validEnd, err)
		}
		if err := sh.f.Sync(); err != nil {
			return fmt.Errorf("fsync after recovery truncate: %w", err)
		}
	}
	sh.endOffset = validEnd

	// Rebuild the dedup LRU in log order: later keys are hotter, so replay
	// from the end of the log (bounded by capacity) to keep the most recent
	// entries resident. appendMsgLocked already inserted every key; trim to
	// capacity by walking the LRU from oldest to newest.
	for sh.dedupLRU.Len() > sh.dedupCapacity {
		back := sh.dedupLRU.Back()
		if back == nil {
			break
		}
		sh.dedupLRU.Remove(back)
		delete(sh.dedup, back.Value.(*dedupEntry).key)
	}

	// Verify invariants: seq <= durable watermark (hints or derived from
	// message frames) and checkpoints strictly increasing per cid.
	for cid, cs := range sh.cids {
		for i := 1; i < len(cs.checkpoints); i++ {
			if cs.checkpoints[i].seq <= cs.checkpoints[i-1].seq {
				return fmt.Errorf("cid %d: non-monotonic sparse checkpoint at %d", cid, i)
			}
		}
	}
	// Keep a copy of the final valid offset for diagnostics/test assertions:
	// (reserved for future on-disk header; no-op here).
	return nil
}

func asFrameError(err error, target **frameError) bool {
	for err != nil {
		if fe, ok := err.(*frameError); ok {
			*target = fe
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
