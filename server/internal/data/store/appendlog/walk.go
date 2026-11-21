package appendlog

// frameLocations returns byte offsets of a cid's base frames in reverse
// (newest-first) order. Offsets come from cidState.msgOffsets, rebuilt by the
// recovery scan and maintained on every append, so no reverse frame-length
// decoding (which is ambiguous with variable-length overlay frames) is used.
func frameLocations(sh *shard, cid int64, limit int, skip func(*msgPayload, int64) bool) ([]int64, error) {
	cs := sh.cids[cid]
	if cs == nil || limit <= 0 {
		return nil, nil
	}
	var offs []int64
	for i := len(cs.msgOffsets) - 1; i >= 0 && len(offs) < limit; i-- {
		off := cs.msgOffsets[i]
		if skip != nil {
			p, err := readMsgHeaderOnly(sh.f, off)
			if err != nil {
				return nil, err
			}
			if p == nil {
				continue // overlay/seq-hint frame
			}
			if skip(p, off) {
				continue
			}
		}
		offs = append(offs, off)
	}
	return offs, nil
}
