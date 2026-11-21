package pipe

import (
	"sync"

	"github.com/yingo-server/UFOT/server/internal/data/store"
)

// Dedup is the client_msg_id idempotency guard: an in-memory LRU of
// recent dedup results (capacity 100k entries) layered over the store's
// UNIQUE(cid, sender_id, client_msg_id) constraint as the durable fallback.
//
// Layers (docs §1.2, dedupguard):
//  1. memory hit → immediately return the old ack (no pipeline entry, no
//     seq burn — the message was already assigned a msg_id/seq);
//  2. memory miss → message flows on; if the flusher then hits
//     store.ErrDuplicate (durable layer), the OLD row is looked up and its
//     ack is replayed (TranslateConflict).
//
// The memory layer is best-effort (bounded LRU): correctness never depends on
// it because the DB constraint is the last line of defense.
type Dedup struct {
	mu    sync.Mutex
	cap   int
	order []string // front = most recent (LRU ring via slice)
	idx   map[string]dedupResult
}

// dedupResult is the remembered outcome for one (cid, sender, client_msg_id).
type dedupResult struct {
	MsgID  int64
	Seq    int64
	SentAt int64
	Status uint32 // ack status code
}

// NewDedup creates the guard with capacity entries (100_000 in production;
// smaller in tests). cap <= 0 falls back to 1.
func NewDedup(capacity int) *Dedup {
	if capacity <= 0 {
		capacity = 1
	}
	return &Dedup{cap: capacity, idx: map[string]dedupResult{}}
}

// Key composes the dedup cache key.
func Key(cid, sender int64, clientMsgID string) string {
	return itoa(cid) + ":" + itoa(sender) + ":" + clientMsgID
}

// Check returns the remembered result for the dedup key, if any.
func (d *Dedup) Check(cid, sender int64, clientMsgID string) (dedupResult, bool) {
	k := Key(cid, sender, clientMsgID)
	d.mu.Lock()
	defer d.mu.Unlock()
	if r, ok := d.idx[k]; ok {
		d.touch(k)
		return r, true
	}
	return dedupResult{}, false
}

// Remember records a successful outcome.
func (d *Dedup) Remember(cid, sender int64, clientMsgID string, r dedupResult) {
	k := Key(cid, sender, clientMsgID)
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.idx[k]; ok {
		d.idx[k] = r
		d.touch(k)
		return
	}
	d.idx[k] = r
	d.order = append(d.order, k)
	for len(d.order) > d.cap {
		victim := d.order[0]
		d.order = d.order[1:]
		delete(d.idx, victim)
	}
}

// touch moves k to the back (most-recent) of the LRU order.
func (d *Dedup) touch(k string) {
	for i, e := range d.order {
		if e == k {
			d.order = append(d.order[:i], d.order[i+1:]...) // O(n); acceptable at 100k? — see note
			break
		}
	}
	d.order = append(d.order, k)
}

// NOTE on touch's O(n): dedup hits are the common case (client retries within
// seconds, so the hit is near the slice tail — the loop exits early). The
// bounded capacity keeps the worst case finite. Replacing with an intrusive
// list is a mechanical change if profiling demands it.

// TranslateConflict maps a store-layer duplicate failure into the remembered
// (or freshly read) old result. oldRow may be nil (row unknown — the caller
// then answers a generic 409-conflict ack). Returns the replay-able result
// and whether a concrete row was found.
func (d *Dedup) TranslateConflict(cid, sender int64, clientMsgID string, oldRow *store.HotMsg) (dedupResult, bool) {
	k := Key(cid, sender, clientMsgID)
	d.mu.Lock()
	if r, ok := d.idx[k]; ok {
		d.mu.Unlock()
		return r, true
	}
	d.mu.Unlock()
	if oldRow == nil {
		return dedupResult{}, false
	}
	r := dedupResult{MsgID: oldRow.MsgID, Seq: oldRow.Seq, SentAt: oldRow.SentAt}
	d.Remember(cid, sender, clientMsgID, r)
	return r, true
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [21]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
