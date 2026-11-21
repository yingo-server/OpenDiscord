// Package appendlog implements the hot message store as an append-only,
// sharded write-ahead log. It satisfies store.MsgWriter + store.MsgReader
// without SQLite. Frames are length-prefixed (magic + len + crc32 + payload);
// one writer goroutine per shard serialises all mutations, readers use
// positional ReadAt so they never contend on a file offset.
package appendlog

import (
	"container/list"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"sync"
	"time"
)

const (
	frameHeaderSize = 12 // magic(4) | len(4) | crc32(4)
	maxPayloadSize  = 64 << 20
	maxSyncGroup    = 128 // upper bound of requests sharing one fsync window
)

var frameMagic = [4]byte{'O', 'D', 'L', '1'}

// errStopScan is an internal sentinel: the visitor found what it needed.
var errStopScan = errors.New("appendlog: stop scan")

// frameError describes a torn or corrupt frame. Recovery truncates the file
// at Off; a partial frame at EOF is the normal crash signature.
type frameError struct {
	off     int64
	partial bool
	reason  string
}

func (e *frameError) Error() string {
	kind := "corrupt"
	if e.partial {
		kind = "incomplete"
	}
	return fmt.Sprintf("appendlog: %s frame at offset %d: %s", kind, e.off, e.reason)
}

// checkpoint is one sparse-index entry: at Offset the cid's Seq was exactly
// Seq. Checkpoints are written every IndexEvery records of that cid.
type checkpoint struct {
	seq    int64
	offset int64
}

// cidState is the per-conversation view rebuilt from the log at startup.
// Seq is strictly increasing for a cid, so checkpoints are sortable by seq
// and a lookup is binary search + forward scan of at most IndexEvery frames.
type cidState struct {
	firstOffset int64
	checkpoints []checkpoint
	lastSeq     int64
	count       int64
	msgOffsets  []int64        // byte offsets of this cid's base frames, in seq order
	writtenSeq  map[int64]bool // seq values with a committed message frame
}

// msgRef locates a base message frame for edit/recall overlay lookups.
type msgRef struct {
	cid      int64
	seq      int64
	senderID int64
	offset   int64
}

// overlay carries the latest soft state (edit content / recall timestamp).
// A nil overlay means the base frame is authoritative.
type overlay struct {
	content  []byte
	edited   int64
	recalled int64
	deleted  bool // logical prune (DeleteHotBefore)
}

type dedupKey struct {
	cid    int64
	sender int64
	cmid   string
}

type dedupVal struct {
	msgID int64
	seq   int64
}

type dedupEntry struct {
	key dedupKey
	val dedupVal
	el  *list.Element
}

// writeReq is handled by the shard's single writer goroutine. fn runs while
// shard.mu is held and performs validation + positional write; fsync is
// performed once for the collected group by the goroutine itself.
type writeReq struct {
	fn   func(*shard) error
	done chan error
}

// shard owns one log file. All mutations go through reqCh; all in-memory
// indexes are guarded by mu. Reads take RLock and use ReadAt on f.
type shard struct {
	id    int
	f     *os.File
	wg    sync.WaitGroup
	reqCh chan writeReq

	indexEvery       int
	dedupCapacity    int
	dedupScanRecords int
	syncWindow       time.Duration

	mu        sync.RWMutex
	cids      map[int64]*cidState
	nextSeq   map[int64]int64
	dedup     map[dedupKey]*dedupEntry
	dedupLRU  *list.List
	msgByID   map[int64]msgRef
	overlays  map[int64]*overlay
	endOffset int64 // first byte after the last valid frame
}

// shardOf maps cid -> shard with a stable SplitMix64 finaliser (no rand, no
// string formatting on the hot path). All keys of a cid land in one shard,
// which makes per-shard serialisation sufficient for dedup/seq invariants.
func shardOf(cid int64, n int) int {
	if n <= 1 {
		return 0
	}
	x := uint64(cid) + 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	x ^= x >> 31
	return int(x % uint64(n))
}

// appendFrame prefixes one length-prefixed, checksummed frame.
func appendFrame(dst, payload []byte) ([]byte, error) {
	if len(payload) == 0 || len(payload) > maxPayloadSize {
		return nil, fmt.Errorf("appendlog: payload size %d out of range", len(payload))
	}
	var h [frameHeaderSize]byte
	copy(h[:4], frameMagic[:])
	binary.LittleEndian.PutUint32(h[4:8], uint32(len(payload)))
	binary.LittleEndian.PutUint32(h[8:12], crc32.ChecksumIEEE(payload))
	out := append(dst, h[:]...)
	return append(out, payload...), nil
}

// frameLen is the on-disk size of a payload of n bytes.
func frameLen(n int) int64 { return int64(n) + frameHeaderSize }

// foreachFrame walks [start, end) frame by frame. payload is a reused buffer:
// visitors must copy bytes they retain. errStopScan ends the walk cleanly.
func foreachFrame(f *os.File, start, end int64, visit func(off int64, payload []byte) error) error {
	if start < 0 {
		start = 0
	}
	if start >= end {
		return nil
	}
	var hdr [frameHeaderSize]byte
	var payload []byte
	for start < end {
		if end-start < frameHeaderSize {
			return &frameError{off: start, partial: true, reason: "short frame header"}
		}
		if _, err := f.ReadAt(hdr[:], start); err != nil {
			return &frameError{off: start, partial: true, reason: err.Error()}
		}
		if string(hdr[:4]) != string(frameMagic[:]) {
			return &frameError{off: start, reason: "bad magic"}
		}
		n := int64(binary.LittleEndian.Uint32(hdr[4:8]))
		if n == 0 || n > maxPayloadSize {
			return &frameError{off: start, reason: "payload length out of range"}
		}
		if n > end-start-frameHeaderSize {
			return &frameError{off: start, partial: true, reason: "short payload"}
		}
		if int64(cap(payload)) < n {
			payload = make([]byte, n)
		} else {
			payload = payload[:n]
		}
		if _, err := f.ReadAt(payload, start+frameHeaderSize); err != nil {
			return &frameError{off: start, partial: true, reason: err.Error()}
		}
		if binary.LittleEndian.Uint32(hdr[8:12]) != crc32.ChecksumIEEE(payload) {
			return &frameError{off: start, reason: "crc32 mismatch"}
		}
		next := start + frameHeaderSize + n
		if visit != nil {
			if err := visit(start, payload); err != nil {
				if errors.Is(err, errStopScan) {
					return nil
				}
				return err
			}
		}
		start = next
	}
	return nil
}

// -- sparse index helpers (caller holds the shard lock or is in recovery) --

func cpOffset(cs *cidState, i int) int64 {
	if i < 0 {
		return cs.firstOffset
	}
	return cs.checkpoints[i].offset
}

// findCheckpoint returns the index of the rightmost checkpoint with
// seq <= target (strict=false) or seq < target (strict=true), or -1.
func findCheckpoint(cs *cidState, target int64, strict bool) int {
	lo, hi := 0, len(cs.checkpoints)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		less := cs.checkpoints[mid].seq < target
		if !strict {
			less = cs.checkpoints[mid].seq <= target
		}
		if less {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo - 1
}

// -- dedup LRU (must be called with shard.mu held) --

func (sh *shard) dedupPutLocked(k dedupKey, v dedupVal) {
	if e, ok := sh.dedup[k]; ok {
		e.val = v
		sh.dedupLRU.MoveToFront(e.el)
		return
	}
	e := &dedupEntry{key: k, val: v}
	e.el = sh.dedupLRU.PushFront(e)
	sh.dedup[k] = e
	for sh.dedupLRU.Len() > sh.dedupCapacity {
		back := sh.dedupLRU.Back()
		if back == nil {
			break
		}
		sh.dedupLRU.Remove(back)
		delete(sh.dedup, back.Value.(*dedupEntry).key)
	}
}

func (sh *shard) dedupLookupLocked(k dedupKey) (dedupVal, bool) {
	if e, ok := sh.dedup[k]; ok {
		sh.dedupLRU.MoveToFront(e.el)
		return e.val, true
	}
	// LRU miss: fall back to a bounded tail scan of this cid's log window.
	// This is the durable dedup layer that mirrors pipe.resolveConflict's
	// "look up the old row in a recent ReadHot window" behaviour.
	cs := sh.cids[k.cid]
	if cs == nil {
		return dedupVal{}, false
	}
	startIdx := len(cs.checkpoints) - 1
	for i := startIdx; i >= 0; i-- {
		if sh.endOffset-cs.checkpoints[i].offset <= maxDedupTailBytes {
			startIdx = i
		} else {
			break
		}
	}
	if len(cs.checkpoints) == 0 || sh.endOffset <= maxDedupTailBytes {
		startIdx = -1
	}
	window := int64(sh.dedupScanRecords) * frameLen(avgMsgPayload)
	if window < maxDedupTailBytes/8 {
		window = maxDedupTailBytes / 8
	}
	start := cpOffset(cs, startIdx)
	if sh.endOffset-start > window && len(cs.checkpoints) > 0 {
		// Advance to the first checkpoint inside the byte window so frame
		// alignment is preserved.
		for i := 0; i < len(cs.checkpoints); i++ {
			if cs.checkpoints[i].offset >= sh.endOffset-window {
				startIdx, start = i, cs.checkpoints[i].offset
				break
			}
		}
	}
	var found dedupVal
	err := foreachFrame(sh.f, start, sh.endOffset, func(_ int64, p []byte) error {
		if len(p) == 0 || p[0] != recMsg {
			return nil
		}
		m, err := parseMsgPayload(p)
		if err != nil {
			return err
		}
		if m.cid == k.cid && m.senderID == k.sender && m.clientMsgID == k.cmid {
			found = dedupVal{msgID: m.msgID, seq: m.seq}
			return errStopScan
		}
		return nil
	})
	if err == nil && found.msgID != 0 {
		sh.dedupPutLocked(k, found)
		return found, true
	}
	return dedupVal{}, false
}

// maxDedupTailBytes bounds the durable dedup scan even for very large cids.
const maxDedupTailBytes = 32 << 20

// avgMsgPayload is only a rough byte-per-record estimate for window sizing.
const avgMsgPayload = 256

// -- writer goroutine --

func (sh *shard) startWriter() {
	sh.wg.Add(1)
	go func() {
		defer sh.wg.Done()
		for req := range sh.reqCh {
			reqs := []writeReq{req}
			// SyncWindow == 0 -> one request, one fsync (default, durable).
			// SyncWindow > 0 -> coalesce arrivals within the window into a
			// single write+fsync (group commit, weaker per-call durability).
			if sh.syncWindow > 0 {
				timer := time.NewTimer(sh.syncWindow)
			collect:
				for len(reqs) < maxSyncGroup {
					select {
					case more := <-sh.reqCh:
						reqs = append(reqs, more)
					case <-timer.C:
						break collect
					}
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			}
			results := make([]error, len(reqs))
			sh.mu.Lock()
			wrote := false
			for i, r := range reqs {
				if err := r.fn(sh); err != nil {
					results[i] = err
					continue
				}
				wrote = true
			}
			if wrote {
				if err := sh.f.Sync(); err != nil {
					syncErr := fmt.Errorf("appendlog: fsync shard %d: %w", sh.id, err)
					for i := range results {
						if results[i] == nil {
							results[i] = syncErr
						}
					}
				}
			}
			sh.mu.Unlock()
			for i, r := range reqs {
				r.done <- results[i]
			}
		}
	}()
}

func (sh *shard) submit(fn func(*shard) error) chan error {
	done := make(chan error, 1)
	sh.reqCh <- writeReq{fn: fn, done: done}
	return done
}
