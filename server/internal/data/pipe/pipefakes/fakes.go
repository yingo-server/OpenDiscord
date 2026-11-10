// Package pipefakes provides hand-written in-memory fakes of the store api.go
// interfaces for pipe/job tests. They are TEST infrastructure (imported only
// from _test files), not a second implementation: their value is deterministic
// control (injected errors, call counts, BUSY windows).
package pipefakes

import (
	"context"
	"fmt"
	"sync"

	"github.com/yingo-server/OpenDiscord/server/internal/data/store"
)

// FakeMsgStore implements store.MsgWriter + store.MsgReader with counters.
type FakeMsgStore struct {
	mu        sync.Mutex
	NextSeqV  int64 // next seq to hand out (per all convs for simplicity)
	Appended  []store.PendingMsg
	Batches   int                     // number of AppendMessages calls
	Rows      map[string]store.HotMsg // clientMsgKey -> row
	AppendErr error                   // injected failure
}

func NewFakeMsgStore() *FakeMsgStore {
	return &FakeMsgStore{NextSeqV: 1, Rows: map[string]store.HotMsg{}}
}

func (f *FakeMsgStore) AppendMessages(ctx context.Context, batch []store.PendingMsg) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Batches++
	if f.AppendErr != nil {
		return f.AppendErr
	}
	for _, m := range batch {
		key := fmt.Sprintf("%d:%d:%s", m.Cid, m.SenderID, m.ClientMsgID)
		if _, exists := f.Rows[key]; exists {
			return fmt.Errorf("fake: dup %q: %w", key, store.ErrDuplicate)
		}
		f.Rows[key] = store.HotMsg{MsgID: m.MsgID, Cid: m.Cid, Seq: m.Seq, SenderID: m.SenderID,
			ClientMsgID: m.ClientMsgID, Content: m.Content, SentAt: m.SentAt}
		f.Appended = append(f.Appended, m)
	}
	return nil
}

func (f *FakeMsgStore) AllocateSeq(ctx context.Context, cid int64, n int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.NextSeqV
	f.NextSeqV += int64(n)
	return s, nil
}

func (f *FakeMsgStore) ReadHot(ctx context.Context, cid int64, beforeSeq int64, limit int, asc bool) ([]store.HotMsg, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.HotMsg
	for _, m := range f.Rows {
		if m.Cid != cid {
			continue
		}
		if beforeSeq > 0 {
			// cursor semantics: asc (newer direction) keeps seq > beforeSeq;
			// desc (older direction) keeps seq < beforeSeq. The cursor row
			// itself is excluded in both directions.
			if asc && m.Seq <= beforeSeq {
				continue
			}
			if !asc && m.Seq >= beforeSeq {
				continue
			}
		}
		out = append(out, m)
	}
	// sort by seq asc then maybe reverse
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Seq < out[i].Seq {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if !asc {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *FakeMsgStore) FirstSeqAfter(ctx context.Context, cid int64, since int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	min := int64(0)
	for _, m := range f.Rows {
		if m.Cid == cid && m.Seq > since && (min == 0 || m.Seq < min) {
			min = m.Seq
		}
	}
	return min, nil
}

func (f *FakeMsgStore) Snapshot() []store.PendingMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.PendingMsg(nil), f.Appended...)
}

// FakeConvStore implements store.ConvStore with a member map.
type FakeConvStore struct {
	mu       sync.Mutex
	Convs    map[int64]*store.ConvMeta
	Members  map[[2]int64]*store.Member
	NextSeqs map[int64]int64
}

func NewFakeConvStore() *FakeConvStore {
	return &FakeConvStore{Convs: map[int64]*store.ConvMeta{}, Members: map[[2]int64]*store.Member{}, NextSeqs: map[int64]int64{}}
}

func (f *FakeConvStore) CreateConv(ctx context.Context, m *store.ConvMeta, creatorUID int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cid := int64(len(f.Convs) + 1)
	f.Convs[cid] = m
	f.Members[[2]int64{cid, creatorUID}] = &store.Member{Cid: cid, UID: creatorUID, Role: 2, JoinedAt: m.CreatedAt}
	f.NextSeqs[cid] = 1
	return cid, nil
}

func (f *FakeConvStore) AddMember(ctx context.Context, m *store.Member) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Members[[2]int64{m.Cid, m.UID}] = m
	return nil
}

func (f *FakeConvStore) RemoveMember(ctx context.Context, cid, uid int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.Members, [2]int64{cid, uid})
	return nil
}

func (f *FakeConvStore) CheckMember(ctx context.Context, cid, uid int64) (*store.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.Members[[2]int64{cid, uid}]
	if !ok {
		return nil, fmt.Errorf("fake: not member: %w", store.ErrNotFound)
	}
	return m, nil
}

func (f *FakeConvStore) ListMembers(ctx context.Context, cid int64) ([]store.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Member
	for _, m := range f.Members {
		if m.Cid == cid {
			out = append(out, *m)
		}
	}
	return out, nil
}

func (f *FakeConvStore) SetMemberRole(ctx context.Context, cid, uid int64, role int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, ok := f.Members[[2]int64{cid, uid}]; ok {
		m.Role = role
	}
	return nil
}

func (f *FakeConvStore) GetConvMeta(ctx context.Context, cid int64) (*store.ConvMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.Convs[cid]
	if !ok {
		return nil, fmt.Errorf("fake: no conv: %w", store.ErrNotFound)
	}
	return m, nil
}

func (f *FakeConvStore) SingleChatBetween(ctx context.Context, uid1, uid2 int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if uid1 == uid2 {
		return 0, fmt.Errorf("fake: no single chat: %w", store.ErrNotFound)
	}
	for cid, meta := range f.Convs {
		if meta.CType != 1 {
			continue
		}
		var members []int64
		for key := range f.Members {
			if key[0] == cid {
				members = append(members, key[1])
			}
		}
		if len(members) == 2 && ((members[0] == uid1 && members[1] == uid2) || (members[0] == uid2 && members[1] == uid1)) {
			return cid, nil
		}
	}
	return 0, fmt.Errorf("fake: no single chat: %w", store.ErrNotFound)
}

func (f *FakeConvStore) NextSeq(ctx context.Context, cid int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.NextSeqs[cid], nil
}

// MemberPeersOf implements store.ConvStore.MemberPeersOf over the fake member map.
func (f *FakeConvStore) MemberPeersOf(ctx context.Context, uid int64) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cids := map[int64]bool{}
	for key := range f.Members {
		if key[1] == uid {
			cids[key[0]] = true
		}
	}
	seen := map[int64]bool{}
	var out []int64
	for key, m := range f.Members {
		if cids[key[0]] && m.UID != uid && !seen[m.UID] {
			seen[m.UID] = true
			out = append(out, m.UID)
		}
	}
	return out, nil
}

// FakeSysStore implements store.SysStore minimally.
type FakeSysStore struct {
	mu        sync.Mutex
	Flags     map[string]store.Flag
	Presence  map[int64]*store.Presence
	Counters  map[string]int64
	Penalties map[string]*store.Penalty
	Calls     int // RateLimitIncr invocations
}

func NewFakeSysStore() *FakeSysStore {
	return &FakeSysStore{Flags: map[string]store.Flag{}, Presence: map[int64]*store.Presence{},
		Counters: map[string]int64{}, Penalties: map[string]*store.Penalty{}}
}

func (f *FakeSysStore) UpsertFlag(ctx context.Context, fl *store.Flag) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	old, ok := f.Flags[fl.Key]
	fl2 := *fl
	if ok {
		fl2.Version = old.Version + 1
	}
	f.Flags[fl.Key] = fl2
	return nil
}

func (f *FakeSysStore) AllFlags(ctx context.Context) ([]store.Flag, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Flag
	for _, fl := range f.Flags {
		out = append(out, fl)
	}
	return out, nil
}

func (f *FakeSysStore) UpsertPresence(ctx context.Context, p *store.Presence) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Presence[p.UID] = p
	return nil
}

func (f *FakeSysStore) GetPresence(ctx context.Context, uid int64) (*store.Presence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.Presence[uid]; ok {
		return p, nil
	}
	return &store.Presence{UID: uid}, nil
}

func (f *FakeSysStore) RateLimitIncr(ctx context.Context, subject, action string, windowStart int64) (*store.RateWindow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	key := fmt.Sprintf("%s|%s|%d", subject, action, windowStart)
	f.Counters[key]++
	return &store.RateWindow{Subject: subject, Action: action, WindowStart: windowStart,
		Count: f.Counters[key]}, nil
}

func (f *FakeSysStore) GetPenalty(ctx context.Context, subject, penalty string) (*store.Penalty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Penalties[subject+"|"+penalty], nil
}

func (f *FakeSysStore) SetPenalty(ctx context.Context, p *store.Penalty) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Penalties[p.Subject+"|"+p.Penalty] = p
	return nil
}

// MarkRecalled soft-deletes one fake row (exact cid+msg+sender match,
// not-already-recalled guard), mirroring the sqlite semantics.
func (f *FakeMsgStore) MarkRecalled(ctx context.Context, cid, msgID, senderID, at int64) (*store.HotMsg, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, m := range f.Rows {
		if m.Cid == cid && m.MsgID == msgID && m.SenderID == senderID {
			if m.Recalled != 0 {
				row := m
				return &row, nil // already recalled: report the row
			}
			m.Recalled = at
			f.Rows[k] = m
			row := m
			return &row, nil
		}
	}
	return nil, fmt.Errorf("fake: msg cid=%d id=%d: %w", cid, msgID, store.ErrNotFound)
}

// MarkEdited implements store.MsgWriter.MarkEdited: soft content update on
// the exact (cid, msg_id, sender_id) row; ErrNotFound when absent or recalled.
func (f *FakeMsgStore) MarkEdited(ctx context.Context, cid, msgID, senderID int64, content []byte, at int64) (*store.HotMsg, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, m := range f.Rows {
		if m.Cid == cid && m.MsgID == msgID && m.SenderID == senderID {
			if m.Recalled != 0 {
				return nil, fmt.Errorf("fake: msg cid=%d id=%d recalled: %w", cid, msgID, store.ErrNotFound)
			}
			m.Content = content
			m.Edited = at
			f.Rows[k] = m
			row := m
			return &row, nil
		}
	}
	return nil, fmt.Errorf("fake: msg cid=%d id=%d: %w", cid, msgID, store.ErrNotFound)
}

// FakeArchive implements the archiver's two ports with call recording:
// store.ArchiveSource (cold copy) and store.HotPruner (hot delete).
type FakeArchive struct {
	hot                    []store.HotMsg
	mu                     sync.Mutex
	Copied                 []store.HotMsg
	CopyCalls, DeleteCalls int
	DeletedBefore          int64
	Deleted                int64
	FailCopy               bool
	FailDelete             bool
}

func (f *FakeArchive) CopyBatch(ctx context.Context, rows []store.HotMsg) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.CopyCalls++
	if f.FailCopy {
		return 0, fmt.Errorf("fake archive down")
	}
	seen := map[string]bool{}
	for _, r := range f.Copied {
		seen[fmt.Sprintf("%d:%d", r.Cid, r.Seq)] = true
	}
	var n int64
	for _, r := range rows {
		if seen[fmt.Sprintf("%d:%d", r.Cid, r.Seq)] {
			continue // idempotent
		}
		f.Copied = append(f.Copied, r)
		if f.hot == nil {
			f.hot = append([]store.HotMsg(nil), rows...)
		}
		n++
	}
	return n, nil
}

// DeleteHotBefore prunes the HOT side only; f.Copied is the archive and stays
// immutable (that is what makes a re-run copy zero new rows).
func (f *FakeArchive) DeleteHotBefore(ctx context.Context, before int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.DeleteCalls++
	f.DeletedBefore = before
	if f.FailDelete {
		return 0, fmt.Errorf("fake hot pruner down")
	}
	var n int64
	kept := f.hot[:0]
	for _, r := range f.hot {
		if r.SentAt < before {
			n++
		} else {
			kept = append(kept, r)
		}
	}
	f.hot = kept
	f.Deleted = n
	return n, nil
}

// SeedHot populates the HOT side directly, bypassing CopyBatch. Tests need this
// to model "hot has rows the archiver has not seen yet" without polluting the
// archive recording (CopyBatch would append to Copied and only ever set hot
// from its first batch).
func (f *FakeArchive) SeedHot(rows ...store.HotMsg) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hot = append([]store.HotMsg(nil), rows...)
}

// Rows returns the remaining HOT rows (the archive is inspected via Copied).
func (f *FakeArchive) Rows() []store.HotMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.HotMsg(nil), f.hot...)
}
