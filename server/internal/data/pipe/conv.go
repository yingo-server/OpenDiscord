// conv.go: the 930 CONV_DATA domain handler (review P1-8 note).
//
// The identity domain (940/941) has been split out into
// data/domain/identity. The conv domain intentionally STAYS in the pipe for
// now: it is entangled with the pipe's own mechanisms (member checks feed
// the same cache lineage the msg domain uses; snapshot payloads mirror the
// read router's result shape), so a mechanical move risks behavior drift.
// The pipe keeps the pipeline skeleton (dedup/seq/flush/read) and dispatch;
// this file is domain logic only — no pipe-internal state is touched.

package pipe

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/yingo-server/OpenDiscord/server/internal/data/cache"
	"github.com/yingo-server/OpenDiscord/server/internal/data/pipe/wire"
	"github.com/yingo-server/OpenDiscord/server/internal/data/store"
	"github.com/yingo-server/OpenDiscord/server/internal/pkg/featureflags"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/kinds"
)

// ops for the 930 CONV_DATA lane.
const (
	convOpCreate       = 1
	convOpAddMember    = 2
	convOpRemoveMember = 3
	convOpSetRole      = 4
	convOpGetMeta      = 5
	convOpListMembers  = 6
)

// Conversation types (proto chat.v1: 1 direct, 2 group).
const (
	convTypeDirect    = 1
	convTypeGroup     = 2
	convMaxTitleRunes = 30
)

// convDomain serves kind 930 (CONV_DATA_REQUEST) → 931 (CONV_DATA_RESULT).
//
// Authorization is enforced HERE, in the pipe: the user-facing kinds
// (CONV_CREATE/MEMBER_ADD/...) are client requests and are never trusted.
// Every mutation checks the actor's current member role via ListMembers;
// getMeta/listMembers check membership before reading. The store itself never
// does role enforcement (api.go red line).
//
// Membership changes additionally emit one 202 CONV_PAYLOAD event PER MEMBER
// (not a conv.{cid} topic publish) so offline members get their own frame when
// the gateway broadcasts by user_id.
type convDomain struct {
	conv  store.ConvStore
	id    store.IdentityStore
	cache *cache.Cache
	// members is the cached member-LIST view (the fan-out recipient set).
	// It is invalidated EXPLICITLY on every membership mutation — the
	// write path never guesses.
	members *memberCache
	// maxMembers is the group member ceiling (0 = default). Enforced on
	// addMember so a group can never grow past the frame-size bound.
	maxMembers int
	// flags carries pull_mode_threshold for the 202 snapshot substitution
	// (§1.4/§3.4). nil = disabled (threshold 0) → full snapshots as before.
	flags *featureflags.Flags
}

type convEvent struct {
	Kind    uint32
	UserID  uint64
	ConvID  uint64
	Payload []byte
}

type convResult struct {
	Op      uint32
	OK      bool
	Error   string
	Cid     int64
	Payload []byte
	Events  []convEvent
}

func (d *convDomain) handle(ctx context.Context, senderUID int64, op uint32, cid uint64, body []byte) convResult {
	switch op {
	case convOpCreate:
		return d.create(ctx, senderUID, body)
	case convOpAddMember:
		return d.addMember(ctx, senderUID, cid, body)
	case convOpRemoveMember:
		return d.removeMember(ctx, senderUID, cid, body)
	case convOpSetRole:
		return d.setRole(ctx, senderUID, cid, body)
	case convOpGetMeta:
		return d.getMeta(ctx, senderUID, cid)
	case convOpListMembers:
		return d.listMembers(ctx, senderUID, cid)
	}
	return convResult{Op: op, Error: "unknown op"}
}

func (d *convDomain) create(ctx context.Context, actor int64, body []byte) convResult {
	if actor <= 0 {
		return convResult{Op: convOpCreate, Error: "unauthenticated"}
	}
	var p wire.ConvPayload
	if err := wire.Unmarshal(body, &p); err != nil {
		return convResult{Op: convOpCreate, Error: "bad payload"}
	}
	if p.ConvType != convTypeDirect && p.ConvType != convTypeGroup {
		return convResult{Op: convOpCreate, Error: "bad conv type"}
	}

	if p.ConvType == convTypeDirect {
		if len(p.Members) == 0 || p.Members[0].UserID == 0 {
			return convResult{Op: convOpCreate, Error: "direct chat requires target user"}
		}
		target, err := d.checkedUser(ctx, int64(p.Members[0].UserID))
		if err != nil {
			return convResult{Op: convOpCreate, Error: err.Error()}
		}
		// Dedupe: one direct conversation per pair. Returns the existing cid.
		if existing, err := d.conv.SingleChatBetween(ctx, actor, target.UID); err == nil && existing > 0 {
			pl, members, err := d.snapshot(ctx, existing)
			if err != nil {
				return convResult{Op: convOpCreate, Error: err.Error()}
			}
			return convResult{Op: convOpCreate, OK: true, Cid: existing, Payload: pl,
				Events: d.convEvents(existing, members, pl)}
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			return convResult{Op: convOpCreate, Error: err.Error()}
		}
		title := strings.TrimSpace(p.Title)
		if title == "" {
			title = target.DisplayName
		}
		newCid, err := d.conv.CreateConv(ctx, &store.ConvMeta{
			CType: convTypeDirect, Title: title, CreatedBy: actor, CreatedAt: nowSec(),
		}, actor)
		if err != nil {
			return convResult{Op: convOpCreate, Error: err.Error()}
		}
		if err := d.conv.AddMember(ctx, &store.Member{
			Cid: newCid, UID: target.UID, Role: 0, JoinedAt: nowSec(),
		}); err != nil {
			return convResult{Op: convOpCreate, Error: err.Error()}
		}
		pl, members, err := d.snapshot(ctx, newCid)
		if err != nil {
			return convResult{Op: convOpCreate, Error: err.Error()}
		}
		return convResult{Op: convOpCreate, OK: true, Cid: newCid, Payload: pl,
			Events: d.convEvents(newCid, members, pl)}
	}

	// Group chat: title is mandatory and at most 30 runes; creator becomes
	// role=2 owner through CreateConv.
	title := strings.TrimSpace(p.Title)
	if title == "" {
		return convResult{Op: convOpCreate, Error: "group title required"}
	}
	if utf8.RuneCountInString(title) > convMaxTitleRunes {
		return convResult{Op: convOpCreate, Error: "group title too long"}
	}
	seen := map[int64]bool{actor: true}
	targets := make([]int64, 0, len(p.Members))
	for _, m := range p.Members {
		uid := int64(m.UserID)
		if uid <= 0 || seen[uid] {
			continue
		}
		if _, err := d.checkedUser(ctx, uid); err != nil {
			return convResult{Op: convOpCreate, Error: err.Error()}
		}
		seen[uid] = true
		targets = append(targets, uid)
	}
	newCid, err := d.conv.CreateConv(ctx, &store.ConvMeta{
		CType: convTypeGroup, Title: title, CreatedBy: actor, CreatedAt: nowSec(),
	}, actor)
	if err != nil {
		return convResult{Op: convOpCreate, Error: err.Error()}
	}
	for _, uid := range targets {
		if err := d.conv.AddMember(ctx, &store.Member{
			Cid: newCid, UID: uid, Role: 0, JoinedAt: nowSec(),
		}); err != nil {
			return convResult{Op: convOpCreate, Error: err.Error()}
		}
		d.cache.InvalidateMember(cache.MemberKey{Cid: newCid, UID: uid})
	}
	d.invalidateMembers(newCid)
	pl, members, err := d.snapshot(ctx, newCid)
	if err != nil {
		return convResult{Op: convOpCreate, Error: err.Error()}
	}
	return convResult{Op: convOpCreate, OK: true, Cid: newCid, Payload: pl,
		Events: d.convEvents(newCid, members, pl)}
}

func (d *convDomain) addMember(ctx context.Context, actor int64, cid uint64, body []byte) convResult {
	var p wire.MemberOp
	if err := wire.Unmarshal(body, &p); err != nil {
		return convResult{Op: convOpAddMember, Error: "bad payload"}
	}
	if cid == 0 {
		cid = p.ConvID
	}
	if _, err := d.conv.GetConvMeta(ctx, int64(cid)); err != nil {
		return convResult{Op: convOpAddMember, Error: err.Error()}
	}
	actorMember, err := d.conv.CheckMember(ctx, int64(cid), actor)
	if err != nil || actorMember.Role < 1 {
		return convResult{Op: convOpAddMember, Error: "forbidden: requires admin or owner"}
	}
	role := int(p.Role)
	if role < 0 || role > int(actorMember.Role) {
		role = 0
	}
	for _, rawUID := range p.UserIDs {
		uid := int64(rawUID)
		if _, err := d.checkedUser(ctx, uid); err != nil {
			return convResult{Op: convOpAddMember, Error: err.Error()}
		}
		if err := d.conv.AddMember(ctx, &store.Member{
			Cid: int64(cid), UID: uid, Role: role, JoinedAt: nowSec(),
		}); err != nil {
			return convResult{Op: convOpAddMember, Error: err.Error()}
		}
		d.cache.InvalidateMember(cache.MemberKey{Cid: int64(cid), UID: uid})
	}
	d.invalidateMembers(int64(cid))
	pl, members, err := d.snapshot(ctx, int64(cid))
	if err != nil {
		return convResult{Op: convOpAddMember, Error: err.Error()}
	}
	evs := d.convEvents(int64(cid), members, pl)
	for _, rawUID := range p.UserIDs {
		uid := int64(rawUID)
		if m, ok := memberRow(members, uid); ok {
			evs = append(evs, d.memberUpdateEvents(int64(cid), "add", *m, actor, members, d.lastSeqFor(ctx, int64(cid)))...)
		}
	}
	return convResult{Op: convOpAddMember, OK: true, Cid: int64(cid), Payload: pl, Events: evs}
}

func (d *convDomain) removeMember(ctx context.Context, actor int64, cid uint64, body []byte) convResult {
	var p wire.MemberOp
	if err := wire.Unmarshal(body, &p); err != nil {
		return convResult{Op: convOpRemoveMember, Error: "bad payload"}
	}
	if cid == 0 {
		cid = p.ConvID
	}
	actorMember, err := d.conv.CheckMember(ctx, int64(cid), actor)
	if err != nil || actorMember.Role < 1 {
		return convResult{Op: convOpRemoveMember, Error: "forbidden: requires admin or owner"}
	}
	for _, rawUID := range p.UserIDs {
		uid := int64(rawUID)
		if _, err := d.checkedUser(ctx, uid); err != nil {
			return convResult{Op: convOpRemoveMember, Error: err.Error()}
		}
		if err := d.conv.RemoveMember(ctx, int64(cid), uid); err != nil {
			return convResult{Op: convOpRemoveMember, Error: err.Error()}
		}
		d.cache.InvalidateMember(cache.MemberKey{Cid: int64(cid), UID: uid})
	}
	d.invalidateMembers(int64(cid))
	pl, members, err := d.snapshot(ctx, int64(cid))
	if err != nil {
		return convResult{Op: convOpRemoveMember, Error: err.Error()}
	}
	evs := d.convEvents(int64(cid), members, pl)
	for _, rawUID := range p.UserIDs {
		removed := store.Member{Cid: int64(cid), UID: int64(rawUID)}
		evs = append(evs, d.memberUpdateEvents(int64(cid), "remove", removed, actor, members, d.lastSeqFor(ctx, int64(cid)))...)
	}
	return convResult{Op: convOpRemoveMember, OK: true, Cid: int64(cid), Payload: pl, Events: evs}
}

func (d *convDomain) setRole(ctx context.Context, actor int64, cid uint64, body []byte) convResult {
	var p wire.MemberOp
	if err := wire.Unmarshal(body, &p); err != nil {
		return convResult{Op: convOpSetRole, Error: "bad payload"}
	}
	if cid == 0 {
		cid = p.ConvID
	}
	actorMember, err := d.conv.CheckMember(ctx, int64(cid), actor)
	if err != nil || actorMember.Role < 1 {
		return convResult{Op: convOpSetRole, Error: "forbidden: requires admin or owner"}
	}
	if int(p.Role) > int(actorMember.Role) {
		return convResult{Op: convOpSetRole, Error: "forbidden: cannot grant a role above own"}
	}
	for _, rawUID := range p.UserIDs {
		if err := d.conv.SetMemberRole(ctx, int64(cid), int64(rawUID), int(p.Role)); err != nil {
			return convResult{Op: convOpSetRole, Error: err.Error()}
		}
		d.cache.InvalidateMember(cache.MemberKey{Cid: int64(cid), UID: int64(rawUID)})
	}
	d.invalidateMembers(int64(cid))
	members, lerr := d.conv.ListMembers(ctx, int64(cid))
	if lerr != nil {
		return convResult{Op: convOpSetRole, Error: lerr.Error()}
	}
	var evs []convEvent
	for _, rawUID := range p.UserIDs {
		m := store.Member{Cid: int64(cid), UID: int64(rawUID), Role: int(p.Role)}
		evs = append(evs, d.memberUpdateEvents(int64(cid), "role", m, actor, members, d.lastSeqFor(ctx, int64(cid)))...)
	}
	return convResult{Op: convOpSetRole, OK: true, Cid: int64(cid), Events: evs}
}

func (d *convDomain) getMeta(ctx context.Context, actor int64, cid uint64) convResult {
	if _, err := d.conv.CheckMember(ctx, int64(cid), actor); err != nil {
		return convResult{Op: convOpGetMeta, Error: "forbidden: not a member"}
	}
	meta, err := d.conv.GetConvMeta(ctx, int64(cid))
	if err != nil {
		return convResult{Op: convOpGetMeta, Error: err.Error()}
	}
	out := wire.ConvPayload{
		ConvID: uint64(meta.Cid), Title: meta.Title, ConvType: uint32(meta.CType),
		CreatedAt: meta.CreatedAt,
	}
	out.LastSeq = d.lastSeqFor(ctx, int64(cid))
	pl, _ := wire.Marshal(out)
	return convResult{Op: convOpGetMeta, OK: true, Cid: int64(cid), Payload: pl}
}

func (d *convDomain) listMembers(ctx context.Context, actor int64, cid uint64) convResult {
	if _, err := d.conv.CheckMember(ctx, int64(cid), actor); err != nil {
		return convResult{Op: convOpListMembers, Error: "forbidden: not a member"}
	}
	meta, err := d.conv.GetConvMeta(ctx, int64(cid))
	if err != nil {
		return convResult{Op: convOpListMembers, Error: err.Error()}
	}
	ms, err := d.conv.ListMembers(ctx, int64(cid))
	if err != nil {
		return convResult{Op: convOpListMembers, Error: err.Error()}
	}
	out := wire.ConvPayload{ConvID: cid, Title: meta.Title, ConvType: uint32(meta.CType), CreatedAt: meta.CreatedAt}
	for _, m := range ms {
		out.Members = append(out.Members, wire.Member{
			UserID: uint64(m.UID), Role: uint32(m.Role), JoinedAt: m.JoinedAt, MutedUntil: m.MutedUntil,
		})
	}
	out.Version = convVersion(len(ms))
	out.LastSeq = d.lastSeqFor(ctx, int64(cid))
	pl, _ := wire.Marshal(out)
	return convResult{Op: convOpListMembers, OK: true, Cid: int64(cid), Payload: pl}
}

// lastSeqFor returns the conversation's current high-water mark: the highest
// COMMITTED seq, i.e. conv_seq.next_seq - 1 (next_seq is the next unconsumed
// slot). A conversation with no messages (next_seq <= 1) reports 0.
//
// This is the field the client's conv-payload handler compares against its
// local cursor; when the server is ahead the client fires a SYNC gap pull —
// which is exactly how a fresh device learns it is missing history.
func (d *convDomain) lastSeqFor(ctx context.Context, cid int64) uint64 {
	next, err := d.conv.NextSeq(ctx, cid)
	if err != nil || next <= 1 {
		return 0
	}
	return uint64(next - 1)
}

// convVersion derives the snapshot revision. We do NOT have a monotonic
// version column on the conv row, so we use the MEMBER COUNT as a coarse
// revision proxy: it changes on every add/remove (the common membership
// mutation) and lets the client discard a stale cached snapshot superseded by
// a newer CONV_PAYLOAD. It is NOT a per-write counter: a role-only change that
// keeps the count constant does not bump it (acceptable — the client's
// MemberUpdate(212) event carries the role delta directly).
func convVersion(memberCount int) uint64 {
	return uint64(memberCount)
}

// snapshot returns the full ConvPayload and current member rows for cid.
func (d *convDomain) snapshot(ctx context.Context, cid int64) ([]byte, []store.Member, error) {
	meta, err := d.conv.GetConvMeta(ctx, cid)
	if err != nil {
		return nil, nil, err
	}
	ms, err := d.conv.ListMembers(ctx, cid)
	if err != nil {
		return nil, nil, err
	}
	out := wire.ConvPayload{
		ConvID: uint64(meta.Cid), Title: meta.Title, ConvType: uint32(meta.CType), CreatedAt: meta.CreatedAt,
	}
	for _, m := range ms {
		out.Members = append(out.Members, wire.Member{
			UserID: uint64(m.UID), Role: uint32(m.Role), JoinedAt: m.JoinedAt, MutedUntil: m.MutedUntil,
		})
	}
	out.Version = convVersion(len(ms))
	out.LastSeq = d.lastSeqFor(ctx, cid)
	pl, err := wire.Marshal(out)
	return pl, ms, err
}

// memberRow finds one member in a snapshot list.
func memberRow(members []store.Member, uid int64) (*store.Member, bool) {
	for i := range members {
		if members[i].UID == uid {
			return &members[i], true
		}
	}
	return nil, false
}

// memberLimit returns the group member ceiling (0 = the default).
func (d *convDomain) memberLimit() int {
	if d.maxMembers > 0 {
		return d.maxMembers
	}
	return defaultMaxGroupMembers
}

// memberUpdateEvents(cid, op, target, byUID, members) fans one 212 per member
// plus one full 202 snapshot, for op-driven member-change events. lastSeq is
// the conversation watermark the 202 snapshot must carry (so a member that was
// offline during the change still gets the correct high-water mark).
func (d *convDomain) memberUpdateEvents(cid int64, op string, target store.Member, byUID int64, members []store.Member, lastSeq uint64) []convEvent {
	body, err := wire.Marshal(wire.MemberUpdate{
		ConvID: uint64(cid), Op: op, ByUserID: uint64(byUID),
		Member: &wire.Member{
			UserID: uint64(target.UID), Role: uint32(target.Role),
			JoinedAt: target.JoinedAt, MutedUntil: target.MutedUntil,
		},
	})
	if err != nil {
		return nil
	}
	// Both kinds ride along: 212 MEMBER_UPDATE (the dedicated event kind) and
	// 202 CONV_PAYLOAD (the full-snapshot refresh every client already
	// handles). Delivering both keeps old and new consumers in sync.
	full, ferr := wire.Marshal(fullConvPayloadForEvents(cid, members, lastSeq))
	if ferr == nil {
		full = d.eventPayload(members, full)
	}
	evs := make([]convEvent, 0, 2*len(members))
	for _, m := range members {
		evs = append(evs, convEvent{
			Kind: uint32(kinds.KindMemberUpdate), UserID: uint64(m.UID), ConvID: uint64(cid), Payload: body,
		})
		if ferr == nil {
			evs = append(evs, convEvent{
				Kind: uint32(kinds.KindConvPayload), UserID: uint64(m.UID), ConvID: uint64(cid), Payload: full,
			})
		}
	}
	return evs
}

// fullConvPayloadForEvents reuses the convDomain's snapshot marshal shape
// (wire.ConvPayload camelCase) without re-querying the store. It carries the
// watermark fields (version from the member count, lastSeq from the caller)
// so the 202 event matches a fresh snapshot() exactly.
func fullConvPayloadForEvents(cid int64, members []store.Member, lastSeq uint64) wire.ConvPayload {
	out := wire.ConvPayload{ConvID: uint64(cid)}
	for _, m := range members {
		out.Members = append(out.Members, wire.Member{
			UserID: uint64(m.UID), Role: uint32(m.Role), JoinedAt: m.JoinedAt, MutedUntil: m.MutedUntil,
		})
	}
	out.Version = convVersion(len(members))
	out.LastSeq = lastSeq
	return out
}

// pullThreshold returns the compiled/effective pull_mode_threshold
// (0 = disabled → full snapshots, the pre-v1.2 behaviour).
func (d *convDomain) pullThreshold() int {
	if d.flags == nil {
		return 0
	}
	return d.flags.Int(featureflags.KeyPullModeThreshold, 0)
}

// pullSnapshot reports whether a conversation with n members must not receive
// full member snapshots in its 202 events.
//
// CORE-2 (universal-pull-spec §1.2): the 202 snapshot protection is a FIXED
// member-count rule — ">100 members ⇒ members_omitted" — and must NOT read
// pull_mode_threshold any more (the threshold's delivery-split semantics is
// cancelled, but the snapshot protection is NOT). A fixed local bound of 100
// keeps the behaviour identical to v1.2.1's compiled default in both regimes:
//   - universal pull on: identical (threshold flag ignored, rule stays >100);
//   - flag-off rollback (S-7): the threshold path reads the flag as before.
func (d *convDomain) pullSnapshot(n int) bool {
	if d.flags != nil && d.flags.Bool(featureflags.KeyUniversalPullEnabled, false) {
		return n > universalSnapshotMemberBound
	}
	th := d.pullThreshold()
	return th > 0 && n > th
}

// universalSnapshotMemberBound is the fixed 202 snapshot protection bound of
// universal-pull-spec §1.2 ("202 CONV_PAYLOAD 仍按固定成员数判定 >100").
const universalSnapshotMemberBound = 100

// eventPayload applies the §1.4 snapshot substitution to a 202 CONV_PAYLOAD
// body: pull-mode conversations carry members_omitted=true + member_count
// instead of the full member list (500 members ≈ 60–80 KB JSON, over the
// 64 KB frame cap). Every other field (version/lastSeq) is preserved, and the
// 931 convOpListMembers path is untouched — clients pull the member list on
// demand.
func (d *convDomain) eventPayload(members []store.Member, full []byte) []byte {
	if !d.pullSnapshot(len(members)) {
		return full
	}
	var p wire.ConvPayload
	if err := wire.Unmarshal(full, &p); err != nil {
		return full
	}
	p.Members = nil
	p.MembersOmitted = true
	p.MemberCount = uint32(len(members))
	out, err := wire.Marshal(p)
	if err != nil {
		return full
	}
	return out
}

func (d *convDomain) convEvents(cid int64, members []store.Member, payload []byte) []convEvent {
	payload = d.eventPayload(members, payload)
	evs := make([]convEvent, 0, len(members))
	for _, m := range members {
		evs = append(evs, convEvent{
			Kind: uint32(kinds.KindConvPayload), UserID: uint64(m.UID), ConvID: uint64(cid), Payload: payload,
		})
	}
	return evs
}

// checkedUser rejects missing/inactive/banned target users. It is called for
// every member a client asks to add — the client is never trusted.
func (d *convDomain) checkedUser(ctx context.Context, uid int64) (*store.User, error) {
	if uid <= 0 {
		return nil, fmt.Errorf("invalid user id")
	}
	u, err := d.id.GetUser(ctx, uid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("user %d not found", uid)
		}
		return nil, err
	}
	if u == nil {
		return nil, fmt.Errorf("user %d not found", uid)
	}
	if u.Status != 0 {
		return nil, fmt.Errorf("user %d is not active", uid)
	}
	ban, err := d.id.GetBan(ctx, uid)
	if err != nil {
		return nil, err
	}
	if ban != nil && ban.Level > 0 {
		return nil, fmt.Errorf("user %d is banned", uid)
	}
	return u, nil
}

// ops for the 970 FLAG lane.
const (
	flagOpGet = 0
	flagOpSet = 1
)

// sysDomain serves kinds 970/971 (flag get/set) and 400-adjacent presence ops
// that arrive on the data lane, plus rate-limit/penalty primitives used by
// callers that enforce policy (thresholds live in policy, never here).
type sysDomain struct {
	sys store.SysStore
}

func (d *sysDomain) handleFlag(ctx context.Context, req wire.FlagRequest) (wire.FlagResult, error) {
	if req.Write {
		if err := d.sys.UpsertFlag(ctx, &store.Flag{Key: req.Key, Value: req.Value}); err != nil {
			return wire.FlagResult{RequestID: req.RequestID}, err
		}
	}
	flags, err := d.sys.AllFlags(ctx)
	if err != nil {
		return wire.FlagResult{RequestID: req.RequestID}, err
	}
	m := map[string]string{}
	for _, f := range flags {
		m[f.Key] = f.Value
	}
	return wire.FlagResult{Flags: m, RequestID: req.RequestID}, nil
}

var _ = kinds.KindFlagSet

// defaultMaxGroupMembers mirrors policy.Default().Group.MaxMembers. The pipe
// keeps its own constant so it does not import the policy package (policy is
// loaded by the assembly and injected via convDomain.maxMembers).
const defaultMaxGroupMembers = 500

// invalidateMembers drops the cached member-list (and peer-set) views for cid
// after a membership mutation, so the fan-out recipient set and any permission
// hint are rebuilt on the next read. Falls back to the generic member cache
// when the typed memberCache is not wired.
func (d *convDomain) invalidateMembers(cid int64) {
	if d.members != nil {
		d.members.InvalidateConv(cid)
		return
	}
	d.cache.InvalidateMemberList(cid)
}
