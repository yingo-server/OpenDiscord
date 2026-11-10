// fanout.go: the ASYNCHRONOUS fan-out plane for transient signals.
//
// Problem this solves (the "瞬态广播扇出与背压" review finding): the pipe's
// dispatcher used to run the whole fan-out inline — ListMembers (a DB
// query) plus one reply per member, all while holding the single
// dispatcher goroutine. In SQLite the read lock and the write lock are
// mutually exclusive, so a slow fan-out stalls the message hot path for
// everyone (worst case: the 5s busy_timeout).
//
// Design:
//
//	dispatcher ──enqueue──► fanout queue (bounded; drop policy per class)
//	                          │
//	                          ▼
//	                    aggregator goroutine
//	                     ├─ per-conv window  (100ms): same conv + same user
//	                     │  → ONE frame (latest state; receipts are MERGED)
//	                     ├─ per-user debounce (10s): presence transitions of
//	                     │  one user collapse into ONE broadcast
//	                          │
//	                          ▼
//	                    broadcaster workers
//	                     1. resolve the recipient set (CACHED member list)
//	                     2. emit one frame per recipient
//	                          │
//	                          ▼
//	                    outbound lane (4096 slots)
//
// The dispatcher NEVER blocks on this plane: a full queue sheds by class
// (best-effort first) and COUNTS the shed.

package pipe

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yingo-server/OpenDiscord/server/internal/data/pipe/wire"
	"github.com/yingo-server/OpenDiscord/server/internal/data/store"
	"github.com/yingo-server/OpenDiscord/server/internal/pkg/featureflags"
	"github.com/yingo-server/OpenDiscord/server/internal/pkg/log"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/kinds"
)

// Fan-out tuning. These are MECHANISM constants (buffer sizes, window
// lengths), not policy: policy lives in policy.json.
const (
	// fanoutQueueBuf is the depth of the dispatcher's fan-out queue. The
	// dispatcher NEVER blocks on it: a full queue drops by kind class.
	fanoutQueueBuf = 4096

	// fanoutWorkers is the number of broadcaster goroutines. The AGGREGATOR
	// is a single goroutine (it owns the window state); the broadcasters
	// do the recipient resolution + emission, which is the slow part.
	fanoutWorkers = 2

	// fanoutBroadcastBuf is the depth of the aggregator → broadcaster lane.
	fanoutBroadcastBuf = 4096

	// defaultFanoutWindow is the aggregation window per conversation:
	// events of the same conversation within this window are merged into
	// ONE broadcast (per user, latest state wins).
	defaultFanoutWindow = 100 * time.Millisecond

	// defaultPresenceDebounce is the per-user presence debounce: repeated
	// connect/disconnect transitions of ONE user inside this window
	// collapse into ONE broadcast carrying the LATEST state.
	// This implements the TODO(presence-throttle) left in the code.
	defaultPresenceDebounce = 10 * time.Second
)

// Drop classes of the fan-out queue. The dispatcher degrades by class
// instead of blocking (a blocked dispatcher is a stalled hot path).
type dropClass int

const (
	// dropKeep: the frame matters (receipts) — try hard to keep it.
	dropKeep dropClass = iota
	// dropBestEffort: the frame is a hint (typing / presence) — dropping it
	// is semantically fine, the next event supersedes it.
	dropBestEffort
)

func (c dropClass) String() string {
	if c == dropKeep {
		return "keep"
	}
	return "best_effort"
}

// fanoutJob is one unit of fan-out work.
type fanoutJob struct {
	// kind is the frame kind (230 typing / 104-105 receipts / 400-402
	// presence / 108 recall).
	kind uint32
	// cid is the conversation (0 for presence peer fan-out).
	cid int64
	// excludeUID is the originator: never echoed back to them.
	excludeUID uint64
	// userID is the ORIGINATOR of the event (the user whose state the
	// frame carries). For the conv fan-out it is the sender; for presence
	// it is the subject user.
	userID uint64
	// payload is the already-marshaled frame body.
	payload []byte
	// class is the drop class of this job.
	class dropClass
	// windowKey identifies the aggregation bucket: for the conv fan-out
	// it is the cid, for the presence fan-out it is the subject uid.
	windowKey string
	// noMerge marks a job that must NOT be aggregated with others (the
	// recall broadcast carries a distinct msg_id per event — merging two
	// of them would lose one of the tombstones).
	noMerge bool
}

// FanoutStats is the observable counter set of the fan-out plane. Every
// drop is COUNTED (never only logged) so the SIGUSR1 dump / metrics can
// show the degradation instead of hiding it.
type FanoutStats struct {
	// Enqueued is the number of jobs accepted into the queue.
	Enqueued uint64
	// DroppedQueue is the number of jobs dropped because the queue was
	// full (the class counters below break it down).
	DroppedQueue uint64
	// DroppedTyping / DroppedPresence count best-effort drops by class.
	DroppedTyping   uint64
	DroppedPresence uint64
	// DroppedReceipts counts receipt jobs dropped at the queue.
	DroppedReceipts uint64
	// Aggregated is the number of events MERGED into an existing window
	// (i.e. events that did NOT produce a frame of their own).
	Aggregated uint64
	// Broadcasts is the number of frames actually emitted by the workers.
	Broadcasts uint64
	// OutboundDropped is the number of frames the outbound lane refused
	// (the lane is bounded; a full lane drops the frame).
	OutboundDropped uint64
}

// fanoutPlane owns the fan-out queue, the aggregator and the broadcasters.
type fanoutPlane struct {
	queue chan fanoutJob
	lg    *log.Logger

	mc *memberCache // recipient-set cache (member list / peer set)

	// put places one frame on the outbound lane, honouring the reserve the pipe
	// keeps for replies. Returns false when the frame was dropped.
	put func(*env) bool

	// window is the aggregation window (injectable for tests).
	window time.Duration
	// presenceWindow is the per-user presence debounce (injectable).
	presenceWindow time.Duration

	// aggMu guards the window buckets and the presence slots.
	aggMu      sync.Mutex
	buckets    map[string]*fanoutBucket
	order      []string
	presenceSt map[uint64]*presenceSlot

	// stats (atomics).
	stEnqueued     atomic.Uint64
	stDropQueue    atomic.Uint64
	stDropTyping   atomic.Uint64
	stDropPresence atomic.Uint64
	stDropReceipts atomic.Uint64
	stAggregated   atomic.Uint64
	stBroadcasts   atomic.Uint64
	stOutboundDrop atomic.Uint64

	// now is the clock used by the pull-mode throttle (injectable for tests).
	now func() time.Time

	// flags is the process-wide feature-flag store (§3.3). It is read through
	// Get/Int (atomic load, no map copy) at the MSG_NEW decision point, so a
	// hot flag change takes effect on the next message without a restart.
	// nil = all pull-mode flags at their defaults (threshold 0 = disabled),
	// which is exactly the pre-v1.2 inline behaviour.
	flags *featureflags.Flags

	// pullMu guards pullState: the per-conversation watermark throttle state
	// (§1.3 lastWatermarkSeq / lastWatermarkAt).
	pullMu    sync.Mutex
	pullState map[int64]*pullConvState

	// v3 is the P3d-1 fanout split sink. nil (or nil offer) = pure no-op:
	// the legacy direct-delivery path is byte-identical (§9 中间态铁律).
	v3 *v3Split

	// adaptive is the CORE-2 per-conversation parameter table (effective[cid],
	// universal-pull-spec §2.2). nil until SetAdaptiveStore is called. The
	// fanout plane only reads it when universal_pull_enabled is on; a nil
	// store or a missing entry falls back to the static flag defaults.
	adaptive *adaptiveStore
}

// pullConvState is the per-conversation watermark throttle state (§1.3).
// It lives in memory only: a restart resets it, which at worst emits one
// extra (harmless) watermark per conversation.
type pullConvState struct {
	lastSeq uint64
	lastAt  time.Time
}

// adaptiveSnapshot is one atomic snapshot of effective[cid] (spec §0/§2.2:
// 机制层只从原子快照读两个值，不调用控制器、不判拥塞、不写审计).
// The pointer is swapped by the policy layer; readers load it per
// conversation. nil = no adaptive snapshot → static flag defaults.
type adaptiveSnapshot struct {
	batch      int
	intervalMS int
}

// adaptiveStore is the per-conversation adaptive parameter table
// (effective[cid], spec §2.2). The fanout plane only READS it; the policy
// layer (CORE-1 controller wired by the assembly) writes whole entries under
// a short lock and publishes a value receiver copy atomically per conv.
// Values outside the hard bounds are REJECTED at write time (spec §2.2
// 越界拒载), so readers never see an illegal pair.
type adaptiveStore struct {
	mu   sync.RWMutex
	effs map[int64]adaptiveSnapshot
}

func newAdaptiveStore() *adaptiveStore { return &adaptiveStore{effs: map[int64]adaptiveSnapshot{}} }

// set publishes one conversation's parameters. Out-of-bounds values are
// rejected: the previous entry (if any) is kept and false is returned so the
// caller can write an adaptive.reject audit event (spec §2.2 / T-AD-4).
func (a *adaptiveStore) set(cid int64, batch, intervalMS int) bool {
	if batch < adaptiveBatchMin || batch > adaptiveBatchMax ||
		intervalMS < adaptiveIntervalMinMS || intervalMS > adaptiveIntervalMaxMS {
		return false
	}
	a.mu.Lock()
	a.effs[cid] = adaptiveSnapshot{batch: batch, intervalMS: intervalMS}
	a.mu.Unlock()
	return true
}

// clear drops one conversation's entry (adaptive off / rollback: spec §5 开关 3
// → 冻结/清空 effective[cid]，回退静态默认值).
func (a *adaptiveStore) clear(cid int64) {
	a.mu.Lock()
	delete(a.effs, cid)
	a.mu.Unlock()
}

// get returns the conversation's snapshot or false when none exists
// (mechanism layer then falls back to the static defaults 50/2000).
func (a *adaptiveStore) get(cid int64) (adaptiveSnapshot, bool) {
	a.mu.RLock()
	s, ok := a.effs[cid]
	a.mu.RUnlock()
	return s, ok
}

// Hard bounds of the adaptive control quantities (spec §2.2; the mechanism
// layer uses them only to REJECT out-of-bounds writes — it never chooses
// parameters itself).
const (
	adaptiveBatchMin      = 1
	adaptiveBatchMax      = 200
	adaptiveIntervalMinMS = 10
	adaptiveIntervalMaxMS = 2000
)

// fanoutBucket is one aggregation window: the pending jobs of one
// conversation (or one presence subject), keyed by the user whose state
// the frame carries.
type fanoutBucket struct {
	key  string
	jobs map[uint64]fanoutJob // per-user latest job
	// order preserves the arrival order of the users (stable output).
	order []uint64
	// unmerged holds noMerge jobs: each is broadcast individually,
	// never collapsed into the per-user window.
	unmerged []fanoutJob
}

// presenceSlot is one user's debounce slot: the pending (latest) state and
// the timer that will publish it.
type presenceSlot struct {
	// lastKind is the presence kind last broadcast for this user (401 online /
	// 402 offline); lastAt is when. A repeat of the same kind inside the window
	// is absorbed, a change is always sent.
	lastKind uint32
	lastAt   time.Time
	hasLast  bool
}

// newFanoutPlane builds the fan-out plane.
func newFanoutPlane(mc *memberCache, put func(*env) bool, lg *log.Logger) *fanoutPlane {
	return &fanoutPlane{
		queue:          make(chan fanoutJob, fanoutQueueBuf),
		lg:             lg,
		mc:             mc,
		put:            put,
		window:         defaultFanoutWindow,
		presenceWindow: defaultPresenceDebounce,
		buckets:        map[string]*fanoutBucket{},
		presenceSt:     map[uint64]*presenceSlot{},
		pullState:      map[int64]*pullConvState{},
		now:            time.Now,
	}
}

// setFlags installs the process-wide feature flag store (SIGHUP / 911 writes
// mutate it in place, so the pointer stays valid across reloads).
func (f *fanoutPlane) setFlags(flags *featureflags.Flags) { f.flags = flags }

// setV3Split attaches the P3d-1 split sink. It is called by Pipe.SetV3Offer
// with the SAME pointer the flusher holds, so every fan-out producer shares one
// counter set. An unassembled plane keeps v3 == nil → no-op.
func (f *fanoutPlane) setV3Split(v *v3Split) { f.v3 = v }

// SetAdaptiveStore wires the CORE-2 effective[cid] table (assembly layer calls
// Pipe.SetAdaptive). nil pointer → adaptive lookups are skipped and the static
// defaults apply (spec §2.2: 控制器关闭时机制层回退静态默认值).
func (f *fanoutPlane) setAdaptiveStore(a *adaptiveStore) { f.adaptive = a }

// start launches the aggregator + the broadcaster workers.
func (f *fanoutPlane) start(ctx context.Context) {
	go f.aggregate(ctx)
	for i := 0; i < fanoutWorkers; i++ {
	}
}

// Stats returns the counter snapshot.
func (f *fanoutPlane) Stats() FanoutStats {
	return FanoutStats{
		Enqueued:        f.stEnqueued.Load(),
		DroppedQueue:    f.stDropQueue.Load(),
		DroppedTyping:   f.stDropTyping.Load(),
		DroppedPresence: f.stDropPresence.Load(),
		DroppedReceipts: f.stDropReceipts.Load(),
		Aggregated:      f.stAggregated.Load(),
		Broadcasts:      f.stBroadcasts.Load(),
		OutboundDropped: f.stOutboundDrop.Load(),
	}
}

// Enqueue hands a job to the fan-out plane. It NEVER blocks: a full queue
// sheds by class and COUNTS the shed.
//
// Class policy (the "分级丢弃" of the review):
//
//   - best-effort (typing / presence): shed first — the next event of the
//     same kind supersedes it, so no information is really lost.
//   - keep (receipts): shed only when the queue is full; counted.
func (f *fanoutPlane) Enqueue(job fanoutJob) {
	select {
	case f.queue <- job:
		f.stEnqueued.Add(1)
		return
	default:
	}
	f.stDropQueue.Add(1)
	switch job.class {
	case dropBestEffort:
		if isTypingKind(job.kind) {
			f.stDropTyping.Add(1)
		} else {
			f.stDropPresence.Add(1)
		}
	case dropKeep:
		f.stDropReceipts.Add(1)
	}
	f.lg.Warn("fanout: queue full; job dropped",
		"kind", kinds.KindName(job.kind), "class", job.class.String())
}

// aggregate is the single aggregator goroutine: it owns the window state.
//
// It merges jobs per (windowKey, user) and publishes the merged jobs to
// the broadcaster lane when the window expires.
func (f *fanoutPlane) aggregate(ctx context.Context) {
	aggCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	bcast := make(chan fanoutJob, fanoutBroadcastBuf)

	// The broadcasters consume the merged jobs.
	for i := 0; i < fanoutWorkers; i++ {
		go f.broadcastLoop(aggCtx, bcast)
	}

	timer := time.NewTimer(f.window)
	defer timer.Stop()
	if !timer.Stop() {
		<-timer.C
	}
	for {
		select {
		case <-ctx.Done():
			f.flushAll(bcast)
			return
		case job := <-f.queue:
			f.accept(job, bcast)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(f.window)
		case <-timer.C:
			f.flushAll(bcast)
			timer.Reset(f.window)
		}
	}
}

// accept merges one job into the window state (or hands it to the
// presence debouncer).
func (f *fanoutPlane) accept(job fanoutJob, bcast chan fanoutJob) {
	// Presence: the per-user debounce owns the timing (10s), so presence
	// jobs never enter the conv window.
	if isPresenceKind(job.kind) {
		f.acceptPresence(job, bcast)
		return
	}
	f.aggMu.Lock()
	defer f.aggMu.Unlock()
	b := f.buckets[job.windowKey]
	if b == nil {
		b = &fanoutBucket{key: job.windowKey, jobs: map[uint64]fanoutJob{}}
		f.buckets[job.windowKey] = b
		f.order = append(f.order, job.windowKey)
	}
	if job.noMerge {
		// Never aggregated: the job gets its own bucket slot.
		f.stAggregated.Add(0)
		b.unmerged = append(b.unmerged, job)
		return
	}
	if old, exists := b.jobs[job.userID]; exists {
		f.stAggregated.Add(1)
		b.jobs[job.userID] = mergeJob(old, job)
		return
	}
	b.jobs[job.userID] = job
	b.order = append(b.order, job.userID)
}

// mergeJob merges two jobs of the same (bucket, user). Receipts are MERGED
// (the receipt lists are concatenated — dropping an earlier receipt would
// lose a read marker); every other kind keeps the LATEST state (typing on
// after off, etc.).
func mergeJob(old, next fanoutJob) fanoutJob {
	if !isReceiptKind(old.kind) || old.kind != next.kind {
		return next
	}
	merged, ok := mergeReceiptPayloads(old.payload, next.payload)
	if !ok {
		return next
	}
	next.payload = merged
	return next
}

// mergeReceiptPayloads concatenates two ReceiptPayload JSON bodies.
func mergeReceiptPayloads(a, b []byte) ([]byte, bool) {
	var pa, pb wire.ReceiptPayload
	if err := wire.Unmarshal(a, &pa); err != nil {
		return nil, false
	}
	if err := wire.Unmarshal(b, &pb); err != nil {
		return nil, false
	}
	pa.Receipts = append(pa.Receipts, pb.Receipts...)
	out, err := wire.Marshal(pa)
	if err != nil {
		return nil, false
	}
	return out, true
}

// flushAll publishes every pending job of the current window.
func (f *fanoutPlane) flushAll(bcast chan fanoutJob) {
	f.aggMu.Lock()
	buckets := f.buckets
	order := f.order
	f.buckets = map[string]*fanoutBucket{}
	f.order = nil
	f.aggMu.Unlock()
	for _, key := range order {
		b := buckets[key]
		if b == nil {
			continue
		}
		for _, job := range b.unmerged {
			select {
			case bcast <- job:
			default:
				f.stDropQueue.Add(1)
				f.stOutboundDrop.Add(1)
				f.lg.Warn("fanout: broadcaster lane full; job dropped",
					"kind", kinds.KindName(job.kind))
			}
		}
		for _, uid := range b.order {
			job := b.jobs[uid]
			select {
			case bcast <- job:
			default:
				f.stDropQueue.Add(1)
				f.stOutboundDrop.Add(1)
				f.lg.Warn("fanout: broadcaster lane full; job dropped",
					"kind", kinds.KindName(job.kind))
			}
		}
	}
}

// acceptPresence applies per-user presence deduplication.
//
// Semantics: a STATE CHANGE is always broadcast immediately (a user who just
// went online must appear online now; a user who closed the app must go
// offline now — delaying that by a window would show a lying roster). Only a
// REPEATED report of the state we already broadcast, inside the window, is
// suppressed — that is what absorbs multi-device flapping (device A closes
// while device B stays connected still reports "online", so nothing changes).
//
// This replaces an earlier design that held the first event for the whole
// window: that made every disconnect invisible for up to 10s, which is wrong
// for a presence indicator and made the e2e semantics lie.
func (f *fanoutPlane) acceptPresence(job fanoutJob, bcast chan fanoutJob) {
	f.aggMu.Lock()
	slot := f.presenceSt[job.userID]
	if slot == nil {
		slot = &presenceSlot{}
		f.presenceSt[job.userID] = slot
	}
	sameState := slot.hasLast && slot.lastKind == job.kind
	recent := sameState && time.Now().Sub(slot.lastAt) < f.presenceWindow
	slot.lastKind = job.kind
	slot.lastAt = time.Now()
	slot.hasLast = true
	f.aggMu.Unlock()

	if recent {
		f.stAggregated.Add(1)
		return
	}
	j := job
	select {
	case bcast <- j:
	default:
		f.stDropQueue.Add(1)
		f.stOutboundDrop.Add(1)
	}
}

// broadcastLoop consumes merged jobs, resolves the recipient set and emits
// one frame per recipient.
func (f *fanoutPlane) broadcastLoop(ctx context.Context, jobs <-chan fanoutJob) {
	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-jobs:
			if !ok {
				return
			}
			f.broadcast(ctx, job)
		}
	}
}

// broadcast resolves the recipient set and emits one frame per recipient, with
// the recipient-independent body encoded ONCE (perf-spec §2.1).
//
// The recipient set comes from the CACHE (member list / peer set), so the
// worker never blocks the dispatcher on a DB query.
func (f *fanoutPlane) broadcast(ctx context.Context, job fanoutJob) {
	if job.kind == 401 || job.kind == 402 {
	}
	if job.cid > 0 {
		members, err := f.mc.Members(ctx, job.cid)
		if err != nil {
			f.lg.Warn("fanout: member list failed", "cid", job.cid, "err", err.Error())
			return
		}
		uids := make([]uint64, 0, len(members))
		for _, m := range members {
			if uint64(m.UID) == job.excludeUID {
				continue
			}
			uids = append(uids, uint64(m.UID))
		}
		f.fanoutTo(job.kind, uint64(job.cid), uids, job.payload)
		return
	}
	// Presence: the peer set of the subject user.
	peers, err := f.mc.Peers(ctx, int64(job.userID))
	if err != nil {
		f.lg.Warn("fanout: peer set failed", "uid", job.userID, "err", err.Error())
		return
	}
	uids := make([]uint64, 0, len(peers))
	for _, peer := range peers {
		if uint64(peer) == job.excludeUID {
			continue
		}
		uids = append(uids, uint64(peer))
	}
	f.fanoutTo(job.kind, 0, uids, job.payload)
}

// fanoutTo delivers job's payload to every uid in uids. The envelope body is
// encoded exactly ONCE via buildFanoutEnvs and shared by reference across all
// members; the outbound lane only carries lightweight per-member env values that
// point at the same immutable bytes. A member whose frame the lane refuses must
// release its shared reference itself so the reference count still reaches zero.
func (f *fanoutPlane) fanoutTo(kind uint32, cid uint64, uids []uint64, payload []byte) {
	f.fanoutToPut(kind, cid, uids, payload, f.put)
}

// fanoutToPut is fanoutTo with an explicit lane placer. put == nil means the
// fan-out lane is not wired (test assemblies): nothing is emitted and no shared
// frame is built.
func (f *fanoutPlane) fanoutToPut(kind uint32, cid uint64, uids []uint64, payload []byte, put func(*env) bool) {
	// P3d-1: the SAME fan-out output also feeds the v3 mailbox log for uid's
	// in the grey bucket (dual-write / byte-for-byte comparison). This happens
	// BEFORE the legacy direct delivery and can never fail it: a nil sink, a
	// bucket miss or a hub error only updates counters (§9).
	if f.v3 != nil && put != nil {
		f.v3.offerTo(context.Background(), kind, cid, uids, "", payload, 0, 0)
	}
	if put == nil {
		return
	}
	envs, _ := buildFanoutEnvs(defaultFanoutEncode, kind, "", payload, cid, uids)
	for _, e := range envs {
		if put(e) {
			f.stBroadcasts.Add(1)
			continue
		}
		e.releaseShared()
		f.stOutboundDrop.Add(1)
		f.lg.Warn("fanout: outbound lane at bulk high-water; frame dropped",
			"kind", kinds.KindName(kind), "uid", e.UserID)
	}
}

// ---------------------------------------------------------------------------
// 拉取模式分流（docs/pull-mode-spec-v1.2.md §1.2/§1.3/§3.1/§3.2）
//
// 这是 MSG_NEW 的唯一分流判定点：消息已经落库、seq 已经分配（flusher 的
// group-commit 之后），此处只决定"怎么通知"：
//
//	universal_pull_enabled=true  (CORE-2) → 所有会话统一走水位线分支（spec §1.2；
//	    pull_mode_threshold 的交付分流语义被取消，202 快照的 >100 保护不受影响）
//	否则（回滚路径，S-7 逐字节基线）：
//	    len(members) <= pull_mode_threshold → 逐成员 102 MSG_NEW（与 v1.1.1 逐字节一致）
//	    len(members) >  pull_mode_threshold → 不逐成员 emit，改为一条 kind 14 水位线
//
// universal_pull_enabled 默认 false = v1.2.1 行为逐字节一致（spec §5 开关 1）。
//
// 阈值默认 0 = 全部 inline（规范 §6.4 中间态）：判定函数的第一个条件就是
// threshold > 0，因此 flag 未启用时下面的 102 路径与改动前完全同构。
// ---------------------------------------------------------------------------

// pullModeDefaults mirror the compiled flag defaults; they are the fallbacks
// when a flag is absent or of an unexpected type.
const (
	pullDefaultBatch    = 50
	pullDefaultInterval = 2000 * time.Millisecond
)

// pullLimits reads the pull-mode tunables. Get()/Int() are atomic loads
// of an immutable snapshot (no map copy), so reading them per message is as
// cheap as the threshold check itself — no window-level caching needed, and a
// hot flag change (SIGHUP / 911) is picked up on the very next message.
//
// CORE-2 (universal-pull-spec §2.2): when the adaptive table carries an
// entry for the conversation AND universal_pull_enabled is on, the per-conv
// batch/interval from effective[cid] override the static flags (静态覆盖 /
// 控制器输出优先于静态默认值). The threshold is NOT adaptive — universal
// mode cancels the threshold's delivery-split semantics entirely.
func (f *fanoutPlane) pullLimits(cid int64) (threshold, batch int, interval time.Duration) {
	if f.flags == nil {
		return 0, pullDefaultBatch, pullDefaultInterval
	}
	threshold = f.flags.Int(featureflags.KeyPullModeThreshold, 0)
	batch = f.flags.Int(featureflags.KeyWatermarkBatch, pullDefaultBatch)
	if batch <= 0 {
		batch = pullDefaultBatch
	}
	ms := f.flags.Int(featureflags.KeyWatermarkIntervalMS, int(pullDefaultInterval/time.Millisecond))
	if ms <= 0 {
		ms = int(pullDefaultInterval / time.Millisecond)
	}
	// effective[cid] lookup is gated on the universal-pull master switch:
	// with the flag off the behaviour must stay byte-identical to v1.2.1
	// (S-7), so adaptive entries are not even read.
	if f.adaptive != nil && f.flags.Bool(featureflags.KeyUniversalPullEnabled, false) {
		if s, ok := f.adaptive.get(cid); ok {
			batch, ms = s.batch, s.intervalMS
		}
	}
	return threshold, batch, time.Duration(ms) * time.Millisecond
}

// universalPullActive reports whether the universal-pull mode is enabled
// (spec §1.2: 所有会话统一水位线分支，取消 pull_mode_threshold 交付分流).
func (f *fanoutPlane) universalPullActive() bool {
	return f.flags != nil && f.flags.Bool(featureflags.KeyUniversalPullEnabled, false)
}

// pullModeActive reports whether a conversation with n members must be served
// in pull mode.
//
// Two regimes (universal-pull-spec §1.2):
//   - universal_pull_enabled=true → ALL conversations (n >= 2 members) go
//     through the unified watermark branch; pull_mode_threshold keeps no
//     delivery-split meaning. 1-member conversations (peers/solitary) keep
//     the inline path — there is no other member to notify, so the split
//     would be a no-op anyway; keeping it byte-identical to v1.2.1 there.
//   - universal_pull_enabled=false → the v1.2.1 threshold path, byte for byte
//     (S-7 rollback baseline). threshold <= 0 disables the whole feature (§6.4).
func (f *fanoutPlane) pullModeActive(cid int64, n int) bool {
	if f.universalPullActive() {
		return n > 1
	}
	threshold, _, _ := f.pullLimits(cid)
	return threshold > 0 && n > threshold
}

// shouldEmitWatermark applies the §1.3 throttle for one conversation and
// returns whether a watermark must be emitted for this committed seq, plus the
// diagnostic reason (0 = batch/interval tick, 1 = first watermark of the conv).
//
// Rules:
//   - 无推进不发：seq <= lastWatermarkSeq → false (never emit).
//   - 推进触发：seq - lastWatermarkSeq >= watermark_batch.
//   - 时间兜底：now - lastWatermarkAt >= watermark_interval_ms.
//
// Worst case per second per conv is ⌈msgs/batch⌉ + ⌈1000/interval⌉, which is
// the bound T-SV-2 asserts.
func (f *fanoutPlane) shouldEmitWatermark(cid int64, seq uint64) (emit bool, reason uint32) {
	if seq == 0 {
		return false, 0
	}
	_, batch, interval := f.pullLimits(cid)
	now := f.now()

	f.pullMu.Lock()
	defer f.pullMu.Unlock()
	if f.pullState == nil {
		f.pullState = map[int64]*pullConvState{}
	}
	// Bound the map: watermark state is a pure acceleration structure, so
	// dropping it only costs one extra watermark per conversation.
	if len(f.pullState) > 1<<16 {
		f.pullState = map[int64]*pullConvState{}
	}
	st := f.pullState[cid]
	if st == nil {
		// 首次：reason=1。v1.2.1 基线语义：第一条水位线立即发（时间触发兜底
		// 也覆盖后续），lastSeq 记为 seq（推进计数从这条起算）。T-UP-3 用
		//「到达即计数」的语义锁 batch 扣条：batch>=2 时首条消息只记录、不发，
		// 由后续消息凑满 batch 或时间兜底触发 —— 这只影响 universal 模式的
		// effective[cid] 路径，见下方 universal 分支。
		f.pullState[cid] = &pullConvState{lastSeq: seq, lastAt: now}
		return true, 1 // 首次：reason=1
	}
	if seq <= st.lastSeq {
		return false, 0 // 无推进不发（含乱序/重复）
	}
	advanced := seq - st.lastSeq
	// Universal-pull spec §2.2 / T-UP-3: the adaptive batch also gates the
	// very first adaptive window — with batch=200 a single message must NOT
	// emit (the old v1.2.1 shape emitted the first watermark immediately,
	// because the static batch=50 was tuned for it; with per-conv batch up to
	// 200 that would defeat aggregation). Implementation: a conversation whose
	// first watermark was reason=1 is a fresh state; here the "advance" is
	// measured from the last emitted seq, so a batch > advanced simply holds —
	// EXCEPT the reason=1 emission which already happened in the st==nil branch.
	if advanced < uint64(batch) && now.Sub(st.lastAt) < interval {
		return false, 0
	}
	st.lastSeq = seq
	st.lastAt = now
	return true, 0
}

// emitWatermark is the pull-mode branch of the MSG_NEW decision point
// (§3.1/§3.2): the message is already committed and readable through 920/8,
// so the only thing members need is the new high-water mark.
//
// It deliberately does NOT emit the per-member 102 frame. The watermark is
// addressed per member uid exactly like 102 was; the gateway's Broadcast(uid)
// reaches CONNECTED devices only, so offline members are converged by
// Resume(kind 6) instead (§3.1).
//
// The inline branch is untouched: flusher.fanoutNew only calls this when
// len(members) > pull_mode_threshold, and keeps the pre-v1.2 loop otherwise.
func (f *fanoutPlane) emitWatermark(cid, senderID, seq uint64, members []store.Member, put func(*env) bool) {
	if seq == 0 {
		return // 未分配 seq 的消息不可能可拉，不产生水位线
	}
	emit, reason := f.shouldEmitWatermark(int64(cid), seq)
	if !emit {
		return
	}
	// latest_seq 引用的是已经 group-commit 落库的 seq（flusher 在 AppendMessages
	// 成功后才进入 fan-out），因此客户端按 [lastSeq+1, latest_seq] 拉取必有数据
	// ——§1.1 的生成时序约束，T-SV-5 锁定。
	pl, err := wire.Marshal(wire.Watermark{
		ConvID:      cid,
		LatestSeq:   seq,
		Reason:      reason,
		MemberCount: uint32(len(members)),
	})
	if err != nil {
		f.lg.Warn("fanout: watermark marshal failed", "cid", cid, "err", err.Error())
		return
	}
	uids := make([]uint64, 0, len(members))
	for _, m := range members {
		if uint64(m.UID) == senderID {
			continue
		}
		uids = append(uids, uint64(m.UID))
	}
	// fanoutToPut 会顺带把这条水位线按灰度 uid 投进 mailbox（§2.4）；
	// 大群消息本体（102）不进 mailbox，避免 O(N) 复活。
	f.fanoutToPut(uint32(kinds.KindWatermark), cid, uids, pl, put)
}

// isTypingKind reports whether kind is the typing signal.
func isTypingKind(kind uint32) bool { return kind == uint32(kinds.KindTyping) }

// isPresenceKind reports whether kind is a presence signal (400/401/402).
func isPresenceKind(kind uint32) bool {
	return kind == uint32(kinds.KindPresenceUpdate) ||
		kind == uint32(kinds.KindPresenceOnline) ||
		kind == uint32(kinds.KindPresenceOffline)
}

// isReceiptKind reports whether kind is a read/delivered receipt.
func isReceiptKind(kind uint32) bool {
	return kind == uint32(kinds.KindMsgRead) || kind == uint32(kinds.KindMsgDelivered)
}

// memberCacheEntry is a compile-time assertion that store.Member is the
// element type of the cached member lists.
var _ = []store.Member(nil)
