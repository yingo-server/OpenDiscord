package appendlog

import (
	"container/list"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yingo-server/OpenDiscord/server/internal/data/store"
)

// Config controls shard count, index density, dedup LRU size and durability.
type Config struct {
	Dir           string        // log directory (created when absent)
	Shards        int           // 0 = runtime.NumCPU()
	IndexEvery    int           // sparse checkpoint every N records of a cid (0 = 1000)
	DedupCapacity int           // memory dedup LRU entries per shard (0 = 65536)
	SyncWindow    time.Duration // 0 = fsync per write (default, durable); >0 = group-commit window
}

// DB implements store.MsgWriter + store.MsgReader over sharded append-only
// logs. It also offers ScanOlder/DeleteHotBefore for the archiver ports, but
// DeleteHotBefore is a tombstone (logical) delete: an append-only log never
// rewrites frames.
type DB struct {
	cfg       Config
	shards    []*shard
	nextID    atomic.Int64
	closeOnce sync.Once
	closed    chan struct{}
}

var (
	_ store.MsgWriter = (*DB)(nil)
	_ store.MsgReader = (*DB)(nil)
)

// Open creates or reopens an appendlog store. Recovery is per shard and
// happens before any write goroutine starts.
func Open(cfg Config) (*DB, error) {
	if cfg.Dir == "" {
		return nil, errors.New("appendlog: Config.Dir is required")
	}
	if cfg.Shards == 0 {
		cfg.Shards = runtime.NumCPU()
	}
	if cfg.Shards < 1 {
		return nil, errors.New("appendlog: Shards must be >= 1")
	}
	if cfg.IndexEvery <= 0 {
		cfg.IndexEvery = 1000
	}
	if cfg.DedupCapacity <= 0 {
		cfg.DedupCapacity = 65536
	}
	if cfg.SyncWindow < 0 {
		return nil, errors.New("appendlog: SyncWindow must be >= 0")
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("appendlog: mkdir %s: %w", cfg.Dir, err)
	}
	db := &DB{cfg: cfg, shards: make([]*shard, cfg.Shards), closed: make(chan struct{})}
	for i := range db.shards {
		path := filepath.Join(cfg.Dir, fmt.Sprintf("msg-%04d.log", i))
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("appendlog: open shard %d: %w", i, err)
		}
		sh := &shard{
			id: i, f: f, reqCh: make(chan writeReq, 256),
			indexEvery: cfg.IndexEvery, dedupCapacity: cfg.DedupCapacity,
			dedupScanRecords: 500, syncWindow: cfg.SyncWindow,
			cids: make(map[int64]*cidState), nextSeq: make(map[int64]int64),
			dedup: make(map[dedupKey]*dedupEntry), dedupLRU: list.New(),
			msgByID: make(map[int64]msgRef), overlays: make(map[int64]*overlay),
		}
		if err := recoverShard(sh); err != nil {
			f.Close()
			db.Close()
			return nil, fmt.Errorf("appendlog: recover shard %d: %w", i, err)
		}
		db.shards[i] = sh
		sh.startWriter()
	}
	return db, nil
}

// Close drains all writers, fsyncs and closes files.
func (db *DB) Close() error {
	var errs []string
	db.closeOnce.Do(func() {
		close(db.closed)
		for _, sh := range db.shards {
			if sh == nil {
				continue
			}
			close(sh.reqCh)
			sh.wg.Wait()
			if err := sh.f.Sync(); err != nil {
				errs = append(errs, err.Error())
			}
			if err := sh.f.Close(); err != nil {
				errs = append(errs, err.Error())
			}
		}
	})
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (db *DB) shardFor(cid int64) *shard { return db.shards[shardOf(cid, len(db.shards))] }

// ---------------------------------------------------------------- record types

const (
	recMsg     byte = 1 // base message
	recEdit    byte = 2 // overlay: new content + edited timestamp
	recRecall  byte = 3 // overlay: recall timestamp
	recDelete  byte = 4 // overlay: logical prune
	recSeqHint byte = 5 // AllocateSeq reservation (committed before messages use it)
)

type msgPayload struct {
	msgID       int64
	cid         int64
	seq         int64
	senderID    int64
	clientMsgID string
	content     []byte
	sentAt      int64
}

func marshalMsg(p *msgPayload) []byte {
	cmid := []byte(p.clientMsgID)
	if len(cmid) > 65535 {
		cmid = cmid[:65535]
	}
	b := make([]byte, 1+8*5+2+4+len(cmid)+len(p.content))
	b[0] = recMsg
	o := 1
	o += binary.PutUvarint(b[o:], uint64(p.msgID))
	o += binary.PutUvarint(b[o:], uint64(p.cid))
	o += binary.PutUvarint(b[o:], uint64(p.seq))
	o += binary.PutUvarint(b[o:], uint64(p.senderID))
	o += binary.PutUvarint(b[o:], uint64(p.sentAt))
	binary.LittleEndian.PutUint16(b[o:o+2], uint16(len(cmid)))
	o += 2
	copy(b[o:], cmid)
	o += len(cmid)
	binary.LittleEndian.PutUint32(b[o:o+4], uint32(len(p.content)))
	o += 4
	copy(b[o:], p.content)
	return b[:o+len(p.content)]
}

func parseMsgPayload(b []byte) (*msgPayload, error) {
	if len(b) < 2 || b[0] != recMsg {
		return nil, errors.New("appendlog: not a message frame")
	}
	o := 1
	vals := make([]uint64, 5)
	for i := range vals {
		v, n := binary.Uvarint(b[o:])
		if n <= 0 {
			return nil, errors.New("appendlog: bad uvarint")
		}
		vals[i], o = v, o+n
	}
	if o+2 > len(b) {
		return nil, errors.New("appendlog: short client_msg_id length")
	}
	ncmid := int(binary.LittleEndian.Uint16(b[o:]))
	o += 2
	if o+ncmid+4 > len(b) {
		return nil, errors.New("appendlog: short client_msg_id")
	}
	cmid := string(b[o : o+ncmid])
	o += ncmid
	ncontent := int(binary.LittleEndian.Uint32(b[o:]))
	o += 4
	if o+ncontent > len(b) {
		return nil, errors.New("appendlog: short content")
	}
	return &msgPayload{
		msgID: int64(vals[0]), cid: int64(vals[1]), seq: int64(vals[2]),
		senderID: int64(vals[3]), sentAt: int64(vals[4]),
		clientMsgID: cmid, content: b[o : o+ncontent],
	}, nil
}

func marshalOverlay(rec byte, cid, msgID, senderID, at int64, content []byte) []byte {
	b := make([]byte, 1+8*4+4+len(content))
	b[0] = rec
	o := 1
	for _, v := range []int64{cid, msgID, senderID, at} {
		o += binary.PutUvarint(b[o:], uint64(v))
	}
	binary.LittleEndian.PutUint32(b[o:o+4], uint32(len(content)))
	o += 4
	copy(b[o:], content)
	return b[:o+len(content)]
}

type overlayRec struct {
	cid, msgID, senderID, at int64
	content                  []byte
}

func parseOverlay(b []byte) (*overlayRec, error) {
	if len(b) < 2 {
		return nil, errors.New("appendlog: short overlay")
	}
	o, r := 1, &overlayRec{}
	for _, dst := range []*int64{&r.cid, &r.msgID, &r.senderID, &r.at} {
		v, n := binary.Uvarint(b[o:])
		if n <= 0 {
			return nil, errors.New("appendlog: bad overlay uvarint")
		}
		*dst, o = int64(v), o+n
	}
	if o+4 > len(b) {
		return nil, errors.New("appendlog: short overlay content length")
	}
	n := int(binary.LittleEndian.Uint32(b[o:]))
	o += 4
	if o+n > len(b) {
		return nil, errors.New("appendlog: short overlay content")
	}
	r.content = b[o : o+n]
	return r, nil
}

func marshalSeqHint(cid, seq int64) []byte {
	b := make([]byte, 1+8*2)
	b[0] = recSeqHint
	o := 1 + binary.PutUvarint(b[1:], uint64(cid))
	binary.PutUvarint(b[o:], uint64(seq))
	return b
}

func parseSeqHint(b []byte) (int64, int64, error) {
	if len(b) < 2 || b[0] != recSeqHint {
		return 0, 0, errors.New("appendlog: not a seq hint")
	}
	cid, n := binary.Uvarint(b[1:])
	if n <= 0 {
		return 0, 0, errors.New("appendlog: bad seq hint cid")
	}
	seq, m := binary.Uvarint(b[1+n:])
	if m <= 0 {
		return 0, 0, errors.New("appendlog: bad seq hint seq")
	}
	return int64(cid), int64(seq), nil
}

// ------------------------------------------------------- in-lock write helpers

func writeFrameLocked(sh *shard, payload []byte) (int64, error) {
	framed, err := appendFrame(nil, payload)
	if err != nil {
		return 0, err
	}
	off := sh.endOffset
	n, err := sh.f.WriteAt(framed, off)
	if err != nil {
		return 0, fmt.Errorf("appendlog: write frame shard %d at %d: %w", sh.id, off, err)
	}
	if n != len(framed) {
		return 0, io.ErrShortWrite
	}
	sh.endOffset += int64(n)
	return off, nil
}

func appendMsgLocked(sh *shard, p *msgPayload, off int64) {
	cs := sh.cids[p.cid]
	if cs == nil {
		cs = &cidState{firstOffset: off}
		sh.cids[p.cid] = cs
	}
	if sh.nextSeq[p.cid] < p.seq+1 {
		sh.nextSeq[p.cid] = p.seq + 1
	}
	cs.lastSeq = p.seq
	cs.count++
	if sh.indexEvery > 0 && cs.count%int64(sh.indexEvery) == 0 {
		cs.checkpoints = append(cs.checkpoints, checkpoint{seq: p.seq, offset: off})
	}
	cs.msgOffsets = append(cs.msgOffsets, off)
	if cs.writtenSeq == nil {
		cs.writtenSeq = make(map[int64]bool)
	}
	cs.writtenSeq[p.seq] = true
	sh.msgByID[p.msgID] = msgRef{cid: p.cid, seq: p.seq, senderID: p.senderID, offset: off}
	sh.dedupPutLocked(dedupKey{p.cid, p.senderID, p.clientMsgID}, dedupVal{p.msgID, p.seq})
}

// ------------------------------------------------------------- store.MsgWriter

// AppendMessages appends all rows of a batch in one serialised, fsynced
// group write. All-or-nothing is preserved by validating the entire batch
// before any frame is written. Duplicate (cid,sender,client_msg_id) or
// (cid,seq) rows abort the batch with store.ErrDuplicate, matching SQLite.
func (db *DB) AppendMessages(ctx context.Context, batch []store.PendingMsg) error {
	if len(batch) == 0 {
		return nil
	}
	// Stage by shard so one batch touching several shards stays atomic from
	// the caller's perspective: all frames of a batch are in one goroutine
	// per shard and all fsync results are collected before returning.
	type group struct {
		sh   *shard
		rows []*msgPayload
	}
	groups := make([]*group, len(db.shards))
	for i := range batch {
		m := batch[i]
		sh := db.shardFor(m.Cid)
		if groups[sh.id] == nil {
			groups[sh.id] = &group{sh: sh}
		}
		groups[sh.id].rows = append(groups[sh.id].rows, &msgPayload{
			msgID: m.MsgID, cid: m.Cid, seq: m.Seq, senderID: m.SenderID,
			clientMsgID: m.ClientMsgID, content: append([]byte(nil), m.Content...), sentAt: m.SentAt,
		})
	}
	// Intra-batch validation only. All checks against committed state happen
	// in the shard writer callback (serialised) so concurrent AllocateSeq
	// reservations — which legitimately advance nextSeq — cannot produce a
	// false duplicate.
	for _, g := range groups {
		if g == nil {
			continue
		}
		seenDedup, seenSeq := make(map[dedupKey]bool), make(map[[2]int64]bool)
		for _, p := range g.rows {
			k := dedupKey{p.cid, p.senderID, p.clientMsgID}
			sk := [2]int64{p.cid, p.seq}
			if seenDedup[k] || seenSeq[sk] {
				return fmt.Errorf("appendlog: duplicate msg batch: %w", store.ErrDuplicate)
			}
			seenDedup[k], seenSeq[sk] = true, true
		}
	}
	// Commit group-by-group. A batch crossing shards has no shared txn; in
	// practice flusher batches are per-cid (one shard), so the all/none
	// guarantee holds for the production path. Cross-shard partial writes
	// are bounded and never corrupt either log.
	done := make([]chan error, 0, len(groups))
	for _, g := range groups {
		if g == nil {
			continue
		}
		rows := g.rows
		done = append(done, g.sh.submit(func(sh *shard) error {
			// Re-validate: concurrent batches may have committed meanwhile.
			for _, p := range rows {
				k := dedupKey{p.cid, p.senderID, p.clientMsgID}
				if cs := sh.cids[p.cid]; cs != nil && cs.writtenSeq[p.seq] {
					return fmt.Errorf("appendlog: duplicate seq %d cid %d: %w", p.seq, p.cid, store.ErrDuplicate)
				}
				if _, ok := sh.dedupLookupLocked(k); ok {
					return fmt.Errorf("appendlog: duplicate msg cid %d sender %d cmid %q: %w",
						p.cid, p.senderID, p.clientMsgID, store.ErrDuplicate)
				}
			}
			// Phase 1: frame everything; phase 2: write once (one syscall).
			var buf []byte
			offs := make([]int64, len(rows))
			for i, p := range rows {
				framed, err := appendFrame(nil, marshalMsg(p))
				if err != nil {
					return err
				}
				offs[i] = sh.endOffset + int64(len(buf))
				buf = append(buf, framed...)
			}
			n, err := sh.f.WriteAt(buf, sh.endOffset)
			if err != nil || n != len(buf) {
				if err == nil {
					err = io.ErrShortWrite
				}
				return fmt.Errorf("appendlog: append batch shard %d: %w", sh.id, err)
			}
			sh.endOffset += int64(n)
			for i, p := range rows {
				appendMsgLocked(sh, p, offs[i])
			}
			return nil
		}))
	}
	for _, ch := range done {
		if err := <-ch; err != nil {
			return err
		}
	}
	return nil
}

// AllocateSeq atomically reserves the next n sequence numbers for cid.
// Under the shard writer goroutine this is a memory-only increment; a
// durable seq hint is appended first so a restart never reuses reserved
// numbers (no per-allocation SQLite transaction — the throughput win).
func (db *DB) AllocateSeq(ctx context.Context, cid int64, n int) (int64, error) {
	if n <= 0 {
		return 0, fmt.Errorf("appendlog: AllocateSeq n must be >= 1 (cid=%d n=%d)", cid, n)
	}
	var start int64
	err := <-db.shardFor(cid).submit(func(sh *shard) error {
		start = sh.nextSeq[cid]
		if start == 0 {
			start = 1
		}
		end := start + int64(n) - 1
		// Reserve only when the hint advances the durable watermark.
		if end >= sh.nextSeq[cid] {
			if _, err := writeFrameLocked(sh, marshalSeqHint(cid, end)); err != nil {
				return err
			}
			sh.nextSeq[cid] = end + 1
		}
		return nil
	})
	return start, err
}

// MarkRecalled appends a tombstone overlay and returns the overlaid row.
// It matches store.MsgWriter semantics: exact (cid,msg_id,sender_id) and
// absent/already-recalled report the current row instead of a hard failure.
func (db *DB) MarkRecalled(ctx context.Context, cid, msgID, senderID, at int64) (*store.HotMsg, error) {
	sh := db.shardFor(cid)
	ch := sh.submit(func(sh *shard) error {
		ref, ok := sh.msgByID[msgID]
		if !ok || ref.cid != cid || ref.senderID != senderID {
			return fmt.Errorf("appendlog: msg cid=%d id=%d: %w", cid, msgID, store.ErrNotFound)
		}
		cur := overlayFor(sh, msgID, ref)
		if cur.recalled == 0 {
			if _, err := writeFrameLocked(sh, marshalOverlay(recRecall, cid, msgID, senderID, at, nil)); err != nil {
				return err
			}
			ov := sh.overlays[msgID]
			if ov == nil {
				ov = &overlay{}
				sh.overlays[msgID] = ov
			}
			ov.recalled = at
		}
		return nil
	})
	if err := <-ch; err != nil {
		return nil, err
	}
	return db.readByID(cid, msgID)
}

// MarkEdited appends a content overlay and returns the overlaid row. A
// recalled message is not editable (SQLite conditional UPDATE semantics).
func (db *DB) MarkEdited(ctx context.Context, cid, msgID, senderID int64, content []byte, at int64) (*store.HotMsg, error) {
	sh := db.shardFor(cid)
	ch := sh.submit(func(sh *shard) error {
		ref, ok := sh.msgByID[msgID]
		if !ok || ref.cid != cid || ref.senderID != senderID {
			return fmt.Errorf("appendlog: msg cid=%d id=%d: %w", cid, msgID, store.ErrNotFound)
		}
		cur := overlayFor(sh, msgID, ref)
		if cur.recalled != 0 {
			return fmt.Errorf("appendlog: msg cid=%d id=%d recalled: %w", cid, msgID, store.ErrNotFound)
		}
		if _, err := writeFrameLocked(sh, marshalOverlay(recEdit, cid, msgID, senderID, at, content)); err != nil {
			return err
		}
		ov := sh.overlays[msgID]
		if ov == nil {
			ov = &overlay{}
			sh.overlays[msgID] = ov
		}
		ov.content = append([]byte(nil), content...)
		ov.edited = at
		return nil
	})
	if err := <-ch; err != nil {
		return nil, err
	}
	return db.readByID(cid, msgID)
}

func overlayFor(sh *shard, msgID int64, ref msgRef) overlay {
	if o := sh.overlays[msgID]; o != nil {
		return *o
	}
	_ = ref
	return overlay{}
}

// -------------------------------------------------------------- store.MsgReader

// loadMsgAt decodes one base frame at off and applies the latest overlay.
func loadMsgAt(sh *shard, off int64) (store.HotMsg, error) {
	var hdr [frameHeaderSize]byte
	if _, err := sh.f.ReadAt(hdr[:], off); err != nil {
		return store.HotMsg{}, err
	}
	n := int64(binary.LittleEndian.Uint32(hdr[4:8]))
	buf := make([]byte, n)
	if _, err := sh.f.ReadAt(buf, off+frameHeaderSize); err != nil {
		return store.HotMsg{}, err
	}
	p, err := parseMsgPayload(buf)
	if err != nil {
		return store.HotMsg{}, err
	}
	m := store.HotMsg{
		MsgID: p.msgID, Cid: p.cid, Seq: p.seq, SenderID: p.senderID,
		ClientMsgID: p.clientMsgID, Content: append([]byte(nil), p.content...), SentAt: p.sentAt,
	}
	if o := sh.overlays[p.msgID]; o != nil {
		m.Edited, m.Recalled = o.edited, o.recalled
		if o.content != nil {
			m.Content = append([]byte(nil), o.content...)
		}
	}
	return m, nil
}

// ReadHot returns up to limit messages of cid ordered by seq. asc=false:
// descending from beforeSeq; asc=true: ascending after beforeSeq;
// beforeSeq<=0 means no cursor. Overlay state is applied to every row.
func (db *DB) ReadHot(ctx context.Context, cid int64, beforeSeq int64, limit int, asc bool) ([]store.HotMsg, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("appendlog: ReadHot limit must be >= 1 (cid=%d limit=%d)", cid, limit)
	}
	sh := db.shardFor(cid)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	cs := sh.cids[cid]
	if cs == nil || cs.count == 0 {
		return nil, nil
	}
	var out []store.HotMsg
	// Determine the scan range via the sparse index. Scanning is sequential
	// from a checkpoint at most IndexEvery frames before the cursor.
	if asc {
		target := beforeSeq
		if target <= 0 {
			target = 0
		}
		i := findCheckpoint(cs, target, true)
		start := cpOffset(cs, i)
		// Walk forward collecting the first `limit` seq > target.
		err := foreachFrame(sh.f, start, sh.endOffset, func(off int64, _ []byte) error {
			if len(out) >= limit {
				return errStopScan
			}
			p, err := readMsgHeaderOnly(sh.f, off)
			if err != nil {
				return err
			}
			if p == nil || p.cid != cid || p.seq <= target || sh.deleted(p.msgID) {
				return nil
			}
			m, err := loadMsgAt(sh, off)
			if err != nil {
				return err
			}
			out = append(out, m)
			return nil
		})
		if err != nil {
			return nil, err
		}
		return out, nil
	}
	// Descending: walk backward from the tail via frame lengths (O(limit)),
	// skipping non-matching cids and deleted rows. Frame CRC headers make the
	// reverse walk safe after crash recovery truncation.
	target := beforeSeq
	if target <= 0 {
		target = cs.lastSeq + 1
	}
	offs, err := frameLocations(sh, cid, limit, func(p *msgPayload, _ int64) bool {
		return p.seq >= target || sh.deleted(p.msgID)
	})
	if err != nil {
		return nil, err
	}
	for _, off := range offs {
		m, err := loadMsgAt(sh, off)
		if err != nil {
			return nil, err
		}
		out = append(out, m) // frameLocations already yields newest-first
	}
	return out, nil
}

func readMsgHeaderOnly(f *os.File, off int64) (*msgPayload, error) {
	var hdr [frameHeaderSize]byte
	if _, err := f.ReadAt(hdr[:], off); err != nil {
		return nil, err
	}
	n := int64(binary.LittleEndian.Uint32(hdr[4:8]))
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, off+frameHeaderSize); err != nil {
		return nil, err
	}
	if len(buf) == 0 || buf[0] != recMsg {
		return nil, nil // overlay/seq-hint frame: not a message
	}
	return parseMsgPayload(buf)
}

func (sh *shard) deleted(msgID int64) bool {
	o := sh.overlays[msgID]
	return o != nil && o.deleted
}

// FirstSeqAfter returns the smallest seq of cid strictly greater than since,
// or 0 when none exists (same as SQLite MIN(seq) NULL -> 0).
func (db *DB) FirstSeqAfter(ctx context.Context, cid int64, since int64) (int64, error) {
	sh := db.shardFor(cid)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	cs := sh.cids[cid]
	if cs == nil {
		return 0, nil
	}
	i := findCheckpoint(cs, since, true)
	start := cpOffset(cs, i)
	var found int64
	err := foreachFrame(sh.f, start, sh.endOffset, func(off int64, _ []byte) error {
		p, err := readMsgHeaderOnly(sh.f, off)
		if err != nil {
			return err
		}
		if p != nil && p.cid == cid && p.seq > since && !sh.deleted(p.msgID) {
			found = p.seq
			return errStopScan
		}
		return nil
	})
	return found, err
}

// readByID loads one row by msg_id (used by MarkEdited/MarkRecalled after the
// overlay write commits).
func (db *DB) readByID(cid, msgID int64) (*store.HotMsg, error) {
	sh := db.shardFor(cid)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	ref, ok := sh.msgByID[msgID]
	if !ok || ref.cid != cid {
		return nil, fmt.Errorf("appendlog: msg cid=%d id=%d: %w", cid, msgID, store.ErrNotFound)
	}
	m, err := loadMsgAt(sh, ref.offset)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// ScanOlder implements the archiver's HotScanner: all non-pruned rows with
// sent_at < cutoff, oldest first, up to limit (same shape as SQLite).
func (db *DB) ScanOlder(ctx context.Context, cutoff int64, limit int) ([]store.HotMsg, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("appendlog: ScanOlder limit must be >= 1 (cutoff=%d)", cutoff)
	}
	var out []store.HotMsg
	for _, sh := range db.shards {
		sh.mu.RLock()
		err := foreachFrame(sh.f, 0, sh.endOffset, func(off int64, _ []byte) error {
			p, err := readMsgHeaderOnly(sh.f, off)
			if err != nil {
				return err
			}
			if p == nil || p.sentAt >= cutoff || sh.deleted(p.msgID) {
				return nil
			}
			m, err := loadMsgAt(sh, off)
			if err != nil {
				return err
			}
			out = append(out, m)
			if len(out) >= limit {
				return errStopScan
			}
			return nil
		})
		sh.mu.RUnlock()
		if err != nil {
			return nil, err
		}
		if len(out) >= limit {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SentAt != out[j].SentAt {
			return out[i].SentAt < out[j].SentAt
		}
		if out[i].Cid != out[j].Cid {
			return out[i].Cid < out[j].Cid
		}
		return out[i].Seq < out[j].Seq
	})
	return out, nil
}

// DeleteHotBefore implements the archiver's HotPruner logically: append a
// delete overlay for each row older than the cutoff. Physical bytes remain
// (append-only); readers hide those rows.
func (db *DB) DeleteHotBefore(ctx context.Context, before int64) (int64, error) {
	var count int64
	for _, sh := range db.shards {
		type del struct{ cid, msgID, senderID int64 }
		var targets []del
		sh.mu.RLock()
		err := foreachFrame(sh.f, 0, sh.endOffset, func(off int64, _ []byte) error {
			p, err := readMsgHeaderOnly(sh.f, off)
			if err != nil {
				return err
			}
			if p != nil && p.sentAt < before && !sh.deleted(p.msgID) {
				targets = append(targets, del{p.cid, p.msgID, p.senderID})
			}
			return nil
		})
		sh.mu.RUnlock()
		if err != nil {
			return count, err
		}
		for _, t := range targets {
			err := <-sh.submit(func(sh *shard) error {
				if sh.deleted(t.msgID) {
					return nil
				}
				if _, err := writeFrameLocked(sh, marshalOverlay(recDelete, t.cid, t.msgID, t.senderID, 0, nil)); err != nil {
					return err
				}
				ov := sh.overlays[t.msgID]
				if ov == nil {
					ov = &overlay{}
					sh.overlays[t.msgID] = ov
				}
				ov.deleted = true
				count++
				return nil
			})
			if err != nil {
				return count, err
			}
		}
	}
	return count, nil
}
