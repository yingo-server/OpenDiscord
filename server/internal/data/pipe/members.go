// members.go: the TYPED member-list / peer-set cache used by the fan-out
// plane (and by the flusher's MSG_NEW fan-out).
//
// Contract (same as data/cache): a cache hit is a HINT that saves a query,
// never an authority beyond its 60s TTL. Every membership mutation
// invalidates EXPLICITLY — see convDomain.addMember / removeMember /
// setRole, which call InvalidateConv.
//
// The cache has its OWN small LRU budget (memberListCap): a member LIST is
// an order of magnitude bigger than a single membership row, so the shared
// 100k-entry budget would be a memory hazard.

package pipe

import (
	"context"
	"sync"

	"github.com/yingo-server/UFOT/server/internal/data/cache"
	"github.com/yingo-server/UFOT/server/internal/data/store"
)

// memberListCap is the entry budget of the member-list cache. 512
// conversations × (up to 500 members) is the worst case; entries are evicted
// LRU-first, so a burst of one-shot conversations never grows the heap.
const memberListCap = 512

// memberCache caches two derived views of the conv store:
//
//   - the MEMBER LIST of a conversation (fan-out recipient set)
//   - the PRESENCE PEER SET of a user (other users sharing a conversation)
//
// Both are bounded by cache.MemberTTL (60s) and invalidated explicitly on
// membership mutations.
type memberCache struct {
	c     *cache.Cache
	convs store.ConvStore

	// fillMu serializes cache fills: a burst of concurrent misses for the
	// same key must not turn into N identical queries (thundering herd).
	// It is a per-cache (not per-key) lock on purpose — the queries are
	// indexed and fast; simplicity beats lock striping here.
	fillMu sync.Mutex
}

// newMemberCache builds the member/peer cache over a conv store.
func newMemberCache(convs store.ConvStore) *memberCache {
	return &memberCache{c: cache.New(memberListCap), convs: convs}
}

// Members returns the member list of cid, from the cache when fresh.
func (m *memberCache) Members(ctx context.Context, cid int64) ([]store.Member, error) {
	if v, ok := m.c.MemberList(cid); ok {
		if ms, is := v.([]store.Member); is {
			return ms, nil
		}
	}
	m.fillMu.Lock()
	defer m.fillMu.Unlock()
	// Re-check under the fill lock: a concurrent miss may have filled it.
	if v, ok := m.c.MemberList(cid); ok {
		if ms, is := v.([]store.Member); is {
			return ms, nil
		}
	}
	ms, err := m.convs.ListMembers(ctx, cid)
	if err != nil {
		return nil, err
	}
	m.c.PutMemberList(cid, ms)
	return ms, nil
}

// Peers returns the presence peer set of uid, from the cache when fresh.
func (m *memberCache) Peers(ctx context.Context, uid int64) ([]int64, error) {
	if v, ok := m.c.PeerList(uid); ok {
		if ps, is := v.([]int64); is {
			return ps, nil
		}
	}
	m.fillMu.Lock()
	defer m.fillMu.Unlock()
	if v, ok := m.c.PeerList(uid); ok {
		if ps, is := v.([]int64); is {
			return ps, nil
		}
	}
	ps, err := m.convs.MemberPeersOf(ctx, uid)
	if err != nil {
		return nil, err
	}
	m.c.PutPeerList(uid, ps)
	return ps, nil
}

// InvalidateConv drops the cached member list of cid AND the cached peer
// sets of its members (a membership change changes everybody's peers).
func (m *memberCache) InvalidateConv(cid int64) {
	// Capture the OLD list first: it tells us whose peer set changed.
	old, _ := m.c.MemberList(cid)
	if list, ok := old.([]store.Member); ok {
		for _, mem := range list {
			m.c.InvalidatePeerList(mem.UID)
		}
	}
	m.c.InvalidateMemberList(cid)
}

// InvalidateUser drops the cached peer set of uid.
func (m *memberCache) InvalidateUser(uid int64) {
	m.c.InvalidatePeerList(uid)
}
