// members.go: the cached VIEW of a conversation's member list and of a
// user's presence peer set.
//
// Why this exists: the transient fan-out (typing / receipts / presence)
// needs the member set of a conversation on EVERY event. Without a cache
// each event costs one full-table query on the hot SQLite store — and the
// dispatcher used to pay that price on the message hot path.
//
// Freshness bound: the SAME 60s TTL as the per-member permission entry
// (cache.MemberTTL) — the authorization red line ("权限不缓存超 60 秒")
// applies to the list exactly as it applies to the row. Membership
// mutations invalidate EXPLICITLY (no guessing, no background refresh).
//
// Capacity: the list entries are big (a 500-member group is ~20KB), so the
// list cache gets its OWN small LRU budget — see pipe.newMemberCache.
// The generic Cache below is shared (sharded LRU, per-entry TTL).

package cache

import "strconv"

// Key prefixes. "ml:" = member list, "pl:" = presence peer list. They are
// deliberately distinct from the "m:<cid>:<uid>" permission entries and from
// the "u:<uid>" identity entries.
const (
	memberListPrefix = "ml:"
	peerListPrefix   = "pl:"
)

// MemberListKey addresses a conversation's full member list.
func MemberListKey(cid int64) string {
	return memberListPrefix + strconv.FormatInt(cid, 10)
}

// PeerListKey addresses a user's presence peer set (other users sharing at
// least one conversation with uid).
func PeerListKey(uid int64) string {
	return peerListPrefix + strconv.FormatInt(uid, 10)
}

// PutMemberList caches the member list of cid with the 60s membership TTL.
func (c *Cache) PutMemberList(cid int64, v any) {
	c.Put(MemberListKey(cid), v, MemberTTL)
}

// MemberList returns the cached member list of cid, if present and fresh.
// The returned value is the caller's to read — it MUST NOT be mutated.
func (c *Cache) MemberList(cid int64) (any, bool) {
	return c.Get(MemberListKey(cid))
}

// InvalidateMemberList drops the cached member list of cid. Called on every
// membership mutation (add / remove / setRole / create).
func (c *Cache) InvalidateMemberList(cid int64) {
	c.Invalidate(MemberListKey(cid))
}

// PutPeerList caches a user's presence peer set with the 60s TTL.
func (c *Cache) PutPeerList(uid int64, v any) {
	c.Put(PeerListKey(uid), v, MemberTTL)
}

// PeerList returns the cached peer set of uid, if present and fresh.
func (c *Cache) PeerList(uid int64) (any, bool) {
	return c.Get(PeerListKey(uid))
}

// InvalidatePeerList drops the cached peer set of uid. Called when uid's
// membership set changed (its peers changed too).
func (c *Cache) InvalidatePeerList(uid int64) {
	c.Invalidate(PeerListKey(uid))
}
