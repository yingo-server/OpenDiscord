package pipe

import (
	"github.com/yingo-server/OpenDiscord/server/internal/data/domain/identity"

	"context"
	"sync"

	"github.com/yingo-server/OpenDiscord/server/internal/data/cache"
	"github.com/yingo-server/OpenDiscord/server/internal/data/pipe/wire"
	"github.com/yingo-server/OpenDiscord/server/internal/data/store"
	"github.com/yingo-server/OpenDiscord/server/internal/data/store/sqlite"
	"github.com/yingo-server/OpenDiscord/server/internal/pkg/featureflags"
	"github.com/yingo-server/OpenDiscord/server/internal/pkg/log"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/kinds"
	"github.com/yingo-server/OpenDiscord/server/internal/transport"
)

// ColdDeps bundles the optional cold-store dependencies of the pipe. Any nil
// field means the feature is absent — the pipe degrades (hot-only reads, no
// audit) rather than refusing to start.
type ColdDeps struct {
	Cold  ColdReader         // read path cold-merge (optional)
	Audit store.AuditJournal // domain event audit trail (optional)
}

const (
	writeChBuf    = 128    // D-1 red line: 满则 BUSY NACK，绝不阻塞
	outChBuf      = 8192   // reply lane (acks/errors/results)
	outFanBuf     = 8192   // bulk fan-out lane
	dedupCapacity = 100000 // memory dedup LRU: 10 万
)

// HotStores is the hot-store surface the pipe needs. *sqlite.DB satisfies it;
// tests inject fakes.
//
// store.MediaStore is part of the surface because the MSG_NEW downlink
// enriches the media[] entries with the store-only fields (thumb_inline,
// waveform, status) — without it the receiving client renders empty image
// placeholders and pseudo-random audio waveforms.
type HotStores interface {
	store.MsgWriter
	store.MsgReader
	store.IdentityStore
	store.ConvStore
	store.SysStore
	store.MediaStore
	// ReportStore is the FE-1a sys_reports sink (453). *sqlite.DB satisfies
	// it; the test bundle stubs it as a no-op.
	store.ReportStore
}

// Pipe is the data layer's entry assembly: consumes internal frames from a
// transport.Peer, dispatches to domain handlers, drives the write pipeline
// (seqalloc → writech → flusher group commit) and the read router, and sends
// replies back over the peer.
//
// Goroutine group (docs: 单 goroutine 组): dispatcher loop (Run), one flusher
// goroutine, one sender goroutine. All channels bounded; backpressure is
// channel semantics.
type Pipe struct {
	peer transport.Peer
	hot  HotStores
	cold ColdDeps
	lg   *log.Logger

	writeCh chan ReadyMsg
	// outCh carries replies: ACKs, error/result frames — the client is blocked
	// on these, so they must never queue behind broadcast traffic.
	outCh chan *env
	// outFan carries bulk fan-out (MSG_NEW copies, typing, presence, receipts,
	// member updates). A 50-member group emits 50 frames per message; on one
	// shared queue those frames delayed every ACK by seconds (measured: 22%
	// ACK rate at 128 connections). Separate queue, separate sender.
	outFan chan *env
	// sendMu serialises peer.Send across the two sender goroutines.
	// REVERTED to the v1.1.1 shape after the 500-member stress test showed the
	// lock-free fan-out + ring-push path was a net regression (73% vs 98%
	// availability at 512x10; bench +4%/frame from the extra mutex+notify).
	sendMu  sync.Mutex
	fanDone chan struct{}

	// emit converts an outbound env into a transport frame. Per instance (see
	// SetEmit): a package-level global raced between two assemblies.
	emit EmitFunc

	// FrameSource converts *proto.Frame → Req (assembly installs the
	// chatpb-backed one; default is the shell source).
	FrameSource FrameSource

	dedup *Dedup
	seq   *seqAlloc
	fl    *flusher
	rr    *readRouter
	msg   *msgDomain
	conv  *convDomain
	ident *identity.Domain
	sys   *sysDomain
	trunk Trunkee       // kind 980 job triggers (optional; nil = jobs not wired)
	calls *callRegistry // in-process call-signaling state (kinds 350-354)
	cache *cache.Cache

	// mc is the cached member-list / peer-set view (fan-out recipient sets).
	mc *memberCache
	// fanout is the ASYNCHRONOUS fan-out plane: the dispatcher only enqueues
	// transient signals; the plane resolves the recipients (from the cache),
	// aggregates them into per-conv windows and emits the frames.
	fanout *fanoutPlane
	// v3 is the P3d-1 grey-split sink (nil = legacy-only, byte-identical).
	v3 *v3Split

	// recallWindow is the recall time window in seconds (injectable; 0 =
	// default 120s).
	recallWindow int64

	// editWindow is the MSG_EDIT time window in seconds (injectable; 0 =
	// default 300s). FE-1a #1.
	editWindow int64

	// flags carries the process-wide feature flags; msgDomainV2 reads
	// msg_domain_v2_enabled (default false = legacy behaviour untouched).
	// Set via SetFeatureFlags; nil = compiled defaults.
	flags *featureflags.Flags

	senderDone chan struct{}

	// outMu guards writes to writeCh/outCh against Drain closing them.
	// RLock for every send, Lock for Drain (close). Removes the classic
	// "send on closed channel" panic during SIGTERM.
	outMu    sync.RWMutex
	draining bool
}

// New assembles the pipe over the real hot SQLite store. cold carries
// optional cold deps; pol is accepted for signature stability but unused by
// the pipe's mechanism (policy is the jobs'/callers' concern — mechanism/
// policy separation); lg is the component logger.
//
// p := pipe.New(peer, hot, coldDeps, pol, lg); go p.Run(ctx);
// SIGTERM: p.Drain(ctx) → exit 143.
//
// pol carries the write_path tunables (batch_size / flush_interval_ms) into
// the group-commit flusher; it accepts a policy.Policy, policy.Snapshot or
// *policy.Store, or nil for the built-in defaults.
func New(peer transport.Peer, hot *sqlite.DB, cold ColdDeps, pol any, lg *log.Logger) *Pipe {
	p := NewStores(peer, hot, cold, lg)
	p.fl.applyWritePolicy(pol)
	return p
}

// NewStores is New with the hot store behind the structural interface
// (tests inject fakes; production passes *sqlite.DB).
func NewStores(peer transport.Peer, hot HotStores, cold ColdDeps, lg *log.Logger) *Pipe {
	if lg == nil {
		lg = log.For("data")
	}
	p := &Pipe{
		peer:        peer,
		hot:         hot,
		cold:        cold,
		lg:          lg,
		writeCh:     make(chan ReadyMsg, writeChBuf),
		outCh:       make(chan *env, outChBuf),
		outFan:      make(chan *env, outFanBuf),
		fanDone:     make(chan struct{}),
		FrameSource: ShellFrameSource,
		dedup:       NewDedup(dedupCapacity),
		cache:       cache.New(cache.DefaultCapacity),
		senderDone:  make(chan struct{}),
	}
	p.seq = &seqAlloc{w: hot}
	p.fl = newFlusher(hot, hot, p.outCh, p.writeCh, p.dedup, lg)
	p.fl.putBulk = p.sendFanout
	p.fl.putAck = p.reply
	p.fl.setMediaStore(hot)
	p.mc = newMemberCache(hot)
	p.fl.mc = p.mc
	p.rr = &readRouter{r: hot, cold: cold.Cold}
	p.msg = &msgDomain{conv: hot, cache: p.cache, msg: hot, dedup: p.dedup}
	p.mc = newMemberCache(hot)
	p.conv = &convDomain{conv: hot, id: hot, cache: p.cache, members: p.mc}
	p.emit = DefaultEmit
	p.fanout = newFanoutPlane(p.mc, p.sendFanout, lg)
	p.fl.fanout = p.fanout
	p.ident = identity.New(hot, p.cache)
	p.sys = &sysDomain{sys: hot}
	p.calls = newCallRegistry()
	return p
}

// SetTrunkee injects the job plane (kind 980 handler). Assembly calls it after
// New; a pipe without one answers 980 with state "not_wired".
func (p *Pipe) SetTrunkee(t Trunkee) { p.trunk = t }

// SetFeatureFlags injects the process-wide feature flag store (the same
// *featureflags.Flags the 911 FEATURE_FLAGS broadcaster writes). Pull-mode
// dispatch reads pull_mode_threshold / watermark_batch /
// watermark_interval_ms from it; a hot change (SIGHUP / 911) is visible on the
// next message because Flags.Apply swaps the immutable snapshot in place.
//
// Not calling this leaves the pipe at the compiled defaults, i.e.
// pull_mode_threshold = 0 → the pre-v1.2 inline behaviour, byte for byte.
func (p *Pipe) SetFeatureFlags(flags *featureflags.Flags) {
	p.flags = flags
	if p.fl != nil {
		p.fl.flags = flags
	}
	if p.fanout != nil {
		p.fanout.setFlags(flags)
	}
	if p.conv != nil {
		p.conv.flags = flags
	}
}

// SetV3Offer installs the P3d-1 grey-split sink (docs/v3-bus-design.md §5.1/§6).
//
// The callback is supplied by the assembly layer (chatd/v3wire.go): it reads
// the LATEST flag snapshot on every call (v3_bus_enabled && percent>0), checks
// InV3Bucket(uid) — FNV-1a uid hashing, stable per uid (T-SV-7) — and writes
// the frame into MailboxHub. The pipe itself stays bus-agnostic: no import of
// internal/bus/*, no connection write loop (P3d-2).
//
// Semantics of this intermediate state:
//   - The legacy direct path is UNCHANGED and remains the only *delivery* path.
//     Frames offered to the hub are a DUAL WRITE for byte-for-byte comparison;
//     the hub does not suppress the legacy frame.
//   - offer == nil (default, and in every unit-test rig) disables the split:
//     zero calls, zero allocations, zero behaviour change.
//   - Hub errors are counted, never returned into the write path (§9: a bypass
//     failure must not affect the legacy delivery).
//
// V3SplitStats exposes the split counters for diagnostics/tests.
func (p *Pipe) SetV3Offer(offer V3Offer) {
	v := &v3Split{offer: offer}
	p.v3 = v
	if p.fl != nil {
		p.fl.v3 = v
	}
	if p.fanout != nil {
		p.fanout.setV3Split(v)
	}
}

// V3SplitStats returns the P3d-1 split counters (zero when unassembled).
func (p *Pipe) V3SplitStats() V3SplitStats { return p.v3.stats() }

// SetWritePolicy re-applies the write_path tunables (batch size / flush
// interval) from a policy snapshot. The assembly calls this on SIGHUP so a
// reloaded policy takes effect on the next batch — the in-flight batch commits
// with the old values. Safe to call at runtime (guarded by the flusher's
// config lock).
func (p *Pipe) SetWritePolicy(pol any) {
	if p.fl != nil {
		p.fl.applyWritePolicy(pol)
	}
}

// SetAdaptiveStore installs the CORE-2 effective[cid] parameter table
// (universal-pull-spec §2.2). The assembly (chatd) owns the policy loop that
// writes it from the adaptive controller; the pipe/fanout mechanism layer only
// reads per-conversation snapshots. Not calling this (or passing nil) leaves
// the static flag defaults — the deterministic fallback of spec §2.2.
func (p *Pipe) SetAdaptiveStore(a *adaptiveStore) {
	if a == nil {
		a = newAdaptiveStore()
	}
	if p.fanout != nil {
		p.fanout.setAdaptiveStore(a)
	}
}

// PullCapabilityFn 报告某个 uid 是否具备拉取模式能力（kind 14 handler）。
// 由装配层注入（cmd/chatd）：它读用户层连接注册表的能力表
// （session.PullCapableUID），返回 false = 老客户端 → 必须保持 inline 逐成员推。
type PullCapabilityFn func(uid int64) bool

// SetPullCapability installs the old-client capability probe
// (lts-1.4-acceptance §B).
//
// universal_pull_enabled=true 时，MSG_NEW 的分流判定从「整会话一个口径」
// 变成「按收件人能力口径」：
//   - fn(uid) == true  → 该 uid 收水位线（kind 14）+ 拉取；
//   - fn(uid) == false → 该 uid 收 inline 102（老客户端，宁可多发不漏消息）。
//
// 三条边界：
//   - fn == nil（未注入 / 单测装配）→ 全部 uid 走 inline，行为与 v1.2.1 一致；
//   - universal_pull_enabled=false → 本回调根本不被调用（S-7 逐字节基线）；
//   - 本回调只做「读能力」，不写 DB、不发帧、不改 flag。
func (p *Pipe) SetPullCapability(fn PullCapabilityFn) {
	if p.fanout != nil {
		p.fanout.setPullCapability(fn)
	}
}

// NewAdaptiveStore 创建空的 effective[cid] 表（装配层持有并经 SetAdaptiveStore
// 注入；adaptiveWrite 允许装配层的控制循环写入条目，机制层只读）。
// adaptive_control_enabled=false（默认）时装配层不创建、不注入——机制层
// 保持静态 flag 默认（50/2000，spec §2.2 确定回退）。
func NewAdaptiveStore() *adaptiveStore { return newAdaptiveStore() }

// AdaptiveStoreHandle 是 NewAdaptiveStore 返回值的导出别名（cmd/ 装配层持有
// 句柄用；机制层内部类型 adaptiveStore 不直接暴露，读写都经 Pipe 方法）。
type AdaptiveStoreHandle = adaptiveStore

// AdaptiveWrite 写入一条 effective[cid]（装配层控制循环专用，CORE-1 接线）。
// 越界值由 adaptiveStore.set 拒绝（返回 false，保留旧值——spec §2.2 拒载），
// 调用方据此写 adaptive.reject 审计事件。cid<=0 视为非法直接拒绝。
func (p *Pipe) AdaptiveWrite(st *adaptiveStore, cid int64, batch, intervalMS int) bool {
	if st == nil || cid <= 0 {
		return false
	}
	return st.set(cid, batch, intervalMS)
}

// AdaptiveClear 清除一条 effective[cid]（控制器关闭/回滚：冻结后回退静态
// 默认，spec §5 开关 3）。
func (p *Pipe) AdaptiveClear(st *adaptiveStore, cid int64) {
	if st != nil {
		st.clear(cid)
	}
}

// FanoutStatsSnapshot 暴露扇出计数快照（控制器输入信号：队列深度/丢弃增量，
// universal-pull-spec §2.1 同名计数器纪律；不新增探针）。
func (p *Pipe) FanoutStatsSnapshot() FanoutStats {
	if p.fanout == nil {
		return FanoutStats{}
	}
	return p.fanout.Stats()
}

// Outbound exposes the outbound env lane (tests/assembly tapping).
func (p *Pipe) Outbound() <-chan *env { return p.outCh }

// OutboundFan exposes the bulk fan-out lane (tests assert broadcast frames).
func (p *Pipe) OutboundFan() <-chan *env { return p.outFan }

// Run starts the goroutine group and blocks until ctx is canceled or the
// peer's frame stream closes.
func (p *Pipe) Run(ctx context.Context) {
	fctx, fcancel := context.WithCancel(ctx)
	defer fcancel()
	go p.fl.run(fctx)
	go p.fanout.start(fctx)
	go p.sender()
	go p.fanSender()
	go p.runCallSweeper(fctx)

	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-p.peer.Frames():
			if !ok {
				return
			}
			p.dispatch(ctx, p.FrameSource(f))
		}
	}
}

// sender drains outCh into the peer via Emit. Send errors are logged, not
// fatal (crash-only; acks are recoverable through dedup + DB constraint).
func (p *Pipe) sender() {
	defer close(p.senderDone)
	for e := range p.outCh {
		p.sendOne(e)
	}
}

// fanSender drains the bulk lane. It is a separate goroutine so a burst of
// broadcast frames can neither fill nor delay the reply lane.
func (p *Pipe) fanSender() {
	defer close(p.fanDone)
	for e := range p.outFan {
		p.sendOne(e)
	}
}

// sendOne delivers one outbound env to the peer.
//
// 并发安全论证（perf-spec 补充模块 B，扇出路径去全局锁）：
//
// 扇出调用方：fanSender goroutine（outFan 消费者）+ fanoutPlane 的投递路径，
// 一条大群消息就是几百次连续 Send。Send 本身是「入队 + signal」级操作：
//   - transport/embedded busPeer.Send：bridge.mu 只保护 links 快照拷贝，
//     实际投递是带 100ms 超时的 channel send（select，非阻塞语义）；
//   - transport/embedded layerPeer.Send：单 channel send；
//   - transport/remote remotePeer.Send：EncodeFrame 后走 connWriter.write，
//     connWriter 自带 sync.Mutex（remote/codec.go：与 FrameRW 共享的连接写
//     锁契约）串行化对同一 socket 的所有写；
//   - transport/remote ReconnectingPeer.Send：mu 保护 inner 快照后转发。
//   - transport.Peer 的接口契约即注明「实现必须可并发安全调用」。
//
// 因此扇出侧无需 pipe 级锁：每条连接的写串行化由远端 WS 层自己的
// gorillaWriter/gorilla 内部锁完成（每连接一把，互不干扰），pipe 再持全局
// 锁只是把 500 次微秒级入队变成 500 次锁竞争，并把 reply sender 一起拖住。
//
// reply 路径（outCh 消费者 → ACK）：保留 replySendMu。ACK 是客户端在等的
// 帧，走与扇出相同的 peer；虽然 Send 并发安全，reply 路径仍独占小锁以确保
// ACK 的编码-发送在扇出流水中不被穿插观测（可调试性 > 微秒级开销），且
// 持锁范围只有一个 Send 调用，不会阻塞扇出侧（扇出根本不碰这把锁）。
func (p *Pipe) sendOne(e *env) {
	p.sendMu.Lock()
	err := p.peer.Send(p.emit(e))
	p.sendMu.Unlock()
	// sharedFrame 引用计数：成功路径也要释放（b5b63d2 的 revert 误删了这一行，
	// 导致成功投递的扇出帧 refs 永不归零——审计 2026-11-07 #2）。
	e.releaseShared()
	if err != nil {
		p.lg.Warn("pipe: peer send failed", "err", err.Error())
	}
}

// Drain implements SIGTERM semantics: stop accepting writes, flush every
// buffered message with one final group commit, then let the outbound lane
// empty. The 30s budget is the CALLER's deadline (pass ctx with timeout).
//
// Idempotent and safe to call concurrently with dispatch: all senders hold
// outMu.RLock, Drain takes outMu.Lock before closing the channels.
func (p *Pipe) Drain(ctx context.Context) {
	p.outMu.Lock()
	if p.draining {
		p.outMu.Unlock()
		return
	}
	p.draining = true
	close(p.writeCh) // flusher: final flush on channel close, then exits
	p.outMu.Unlock()

	// Stop any armed ring timers so no AfterFunc fires into a closed lane.
	p.calls.stopAll()

	select {
	case <-p.fl.done:
	case <-ctx.Done():
		p.lg.Warn("drain: flusher deadline exceeded")
	}

	p.outMu.Lock()
	close(p.outCh)
	close(p.outFan)
	p.outMu.Unlock()

	select {
	case <-p.senderDone:
	case <-ctx.Done():
		p.lg.Warn("drain: sender deadline exceeded; outbound lane may hold acks")
	}
}

func (p *Pipe) dispatch(ctx context.Context, req Req) {
	switch req.Kind {
	case uint32(kinds.KindMsgSend): // 101 on the internal lane
		p.onMsgSend(ctx, req)
	case uint32(kinds.KindSyncRequest): // 8: SYNC gap recovery (断线补拉)
		p.onSyncRequest(ctx, req)
	case uint32(kinds.KindReadHistory): // 920
		p.onReadHistory(ctx, req)
	case uint32(kinds.KindConvDataRequest): // 930
		p.onConvData(ctx, req)
	case uint32(kinds.KindIdentityRequest): // 940
		p.onIdentity(ctx, req)
	case uint32(kinds.KindFlagGet), uint32(kinds.KindFlagSet): // 970/971
		p.onFlag(ctx, req)
	case uint32(kinds.KindJobTrigger): // 980 → jobber (optional Trunkee)
		p.onJobTrigger(ctx, req)
	case uint32(kinds.KindCallOffer), uint32(kinds.KindCallAnswer), // 350/351
		uint32(kinds.KindCallCandidate), uint32(kinds.KindCallBusy), // 352/353
		uint32(kinds.KindCallHangup): // 354
		p.onCall(ctx, req)
	case uint32(kinds.KindMsgRead), uint32(kinds.KindMsgDelivered): // 104/105 receipts (transient)
		p.onReceipt(ctx, req)
	case uint32(kinds.KindTyping): // 230 typing (transient)
		p.onTyping(ctx, req)
	case uint32(kinds.KindPresenceUpdate): // 400 client presence status
		p.onPresenceUp(ctx, req)
	case uint32(kinds.KindPresenceOnline), uint32(kinds.KindPresenceOffline): // 401/402 user-layer connect/disconnect
		p.onPresenceConnect(ctx, req)
	case uint32(kinds.KindMsgDelete), uint32(kinds.KindMsgRecall): // 107/108 recall (persisted)
		p.onRecall(ctx, req)
	case uint32(kinds.KindMsgEdit): // 106 edit (FE-1a #1, flag msg_domain_v2_enabled)
		if p.msgDomainV2() {
			p.onMsgEdit(ctx, req)
			return
		}
		p.lg.Debug("pipe: unhandled kind", "kind", kinds.KindName(req.Kind))
	case uint32(kinds.KindMsgForward): // 109 forward metadata (FE-1a #2, flag-gated)
		if p.msgDomainV2() {
			p.onMsgForward(ctx, req)
			return
		}
		p.lg.Debug("pipe: unhandled kind", "kind", kinds.KindName(req.Kind))
	case uint32(kinds.KindReport): // 453 report → sys_reports (FE-1a #4, flag-gated)
		if p.msgDomainV2() {
			p.onReport(ctx, req)
			return
		}
		p.lg.Debug("pipe: unhandled kind", "kind", kinds.KindName(req.Kind))
	default:
		p.lg.Debug("pipe: unhandled kind", "kind", kinds.KindName(req.Kind))
	}
}

// onMsgSend: write-path gates then writech enqueue (docs §1.2).
func (p *Pipe) onMsgSend(ctx context.Context, req Req) {
	var pld wire.MessagePayload
	if len(req.Payload) > 0 {
		if err := wire.Unmarshal(req.Payload, &pld); err != nil {
			p.reply(buildErrEnv(req, 400, "bad payload"))
			return
		}
	}
	cid, sender := int64(req.ConvID), int64(req.UserID)

	admit := p.msg.admit(ctx, cid, sender, pld, nowMilli())
	switch {
	case admit.Replay != nil:
		// dedup hit: replay the ORIGINAL ack (same msg_id/seq).
		p.reply(buildAckEnv(sender, cid, pld.ClientMsgID, admit.Replay.MsgID, admit.Replay.Seq, 0))
		return
	case admit.Code != 0:
		// 403 etc: result composed HERE; the message never enters writech.
		p.reply(buildErrEnv(req, admit.Code, admit.Reason))
		return
	}

	// seqalloc: allocate the seq (per-conv grouping happens when multiple
	// messages share one dispatch pass; here one message per frame).
	msgs := []store.PendingMsg{{
		Cid: cid, SenderID: sender, ClientMsgID: pld.ClientMsgID,
		Content: req.Payload, SentAt: nowSec(),
	}}
	if err := p.seq.assign(ctx, msgs); err != nil {
		p.reply(buildErrEnv(req, 500, "seq alloc failed"))
		return
	}
	ready := ReadyMsg{Msg: msgs[0], Trace: req.TraceID}

	// D-1 red line: writech full → immediate BUSY NACK; never block.
	// The RLock makes the send safe against Drain closing writeCh
	// (assembly contract: Drain is called from another goroutine on SIGTERM).
	p.outMu.RLock()
	if p.draining {
		p.outMu.RUnlock()
		return // shutting down: the peer is gone, nobody to answer
	}
	select {
	case p.writeCh <- ready:
		p.outMu.RUnlock()
	default:
		p.outMu.RUnlock()
		p.reply(buildErrEnv(req, 503, "busy"))
	}
}

func (p *Pipe) onReadHistory(ctx context.Context, req Req) {
	var r wire.ReadHistory
	if err := wire.Unmarshal(req.Payload, &r); err != nil {
		p.reply(buildErrEnv(req, 400, "bad payload"))
		return
	}
	if _, err := p.msg.memberCheck(ctx, int64(r.ConvID), int64(req.UserID)); err != nil {
		p.reply(buildErrEnv(req, 403, "not a member"))
		return
	}
	res, err := p.rr.handle(ctx, r, int64(req.UserID))
	if err != nil {
		p.reply(buildErrEnv(req, 500, "read failed"))
		return
	}
	p.reply(buildResultEnv(req, uint32(kinds.KindReadHistoryResult), readHistoryResultPayload(res)))
}

// onSyncRequest serves kind 8 (SYNC_REQUEST) — the gap-recovery path a client
// triggers when it detects a seq gap (engine.onGap → SYNC{sinceSeq}).
//
// The reply is NOT a wrapped result frame: every missed message is re-sent as
// a plain KindMsgNew(102) addressed to the requesting user, which the client's
// ingest path already understands (it auto-fills the gap). This reuses the
// 920 read machinery (readrouter) but reads ASCENDING after sinceSeq.
//
// Security: the requester MUST be a member of the conversation (403 otherwise).
func (p *Pipe) onSyncRequest(ctx context.Context, req Req) {
	var r wire.ReadHistory // {convId, sinceSeq, limit} — same JSON shape as 920
	if len(req.Payload) > 0 {
		if err := wire.Unmarshal(req.Payload, &r); err != nil {
			p.reply(buildErrEnv(req, 400, "bad payload"))
			return
		}
	}
	cid := int64(r.ConvID)
	if cid == 0 {
		cid = int64(req.ConvID)
	}
	if cid == 0 {
		p.reply(buildErrEnv(req, 400, "missing conv"))
		return
	}
	uid := int64(req.UserID)
	if _, err := p.msg.memberCheck(ctx, cid, uid); err != nil {
		p.reply(buildErrEnv(req, 403, "not a member"))
		return
	}
	rows, err := p.rr.catchup(ctx, cid, int64(r.SinceSeq), int(r.Limit))
	if err != nil {
		p.reply(buildErrEnv(req, 500, "read failed"))
		return
	}
	for _, m := range rows {
		pl := p.fl.buildMsgNewPayload(hotToPending(m))
		p.reply(&env{Kind: uint32(kinds.KindMsgNew), ConvID: uint64(cid), UserID: req.UserID, Payload: pl})
	}
}

// hotToPending adapts a read row to the PendingMsg view buildMsgNewPayload
// consumes (both share the same message fields; MsgNew payload is identical
// whether produced by the live fan-out or a sync replay). The soft-delete
// marker (Recalled) rides along so a gap-fill replay also carries the
// tombstone state.
func hotToPending(m store.HotMsg) store.PendingMsg {
	return store.PendingMsg{
		MsgID: m.MsgID, Cid: m.Cid, Seq: m.Seq, SenderID: m.SenderID,
		ClientMsgID: m.ClientMsgID, Content: m.Content, SentAt: m.SentAt,
		Recalled: m.Recalled,
	}
}

func (p *Pipe) onConvData(ctx context.Context, req Req) {
	var raw struct {
		Op        uint32 `json:"op,omitempty"`
		ConvID    uint64 `json:"convId,omitempty"`
		Body      []byte `json:"body,omitempty"`
		RequestID string `json:"requestId,omitempty"`
	}
	if err := wire.Unmarshal(req.Payload, &raw); err != nil {
		p.reply(buildErrEnv(req, 400, "bad payload"))
		return
	}
	cid := raw.ConvID
	if cid == 0 {
		cid = req.ConvID
	}
	r := p.conv.handle(ctx, int64(req.UserID), raw.Op, cid, raw.Body)
	// Membership mutations land in the audit journal (best-effort, ids only).
	if r.OK {
		p.auditConvMember(ctx, int64(req.UserID), raw.Op, int64(cid), raw.Body)
	}
	pl, _ := wire.Marshal(wire.DataResult{
		Op: raw.Op, Ok: r.OK, Error: r.Error, Body: r.Payload, RequestID: raw.RequestID,
	})
	p.reply(buildResultEnv(req, uint32(kinds.KindConvDataResult), pl))
	// Membership changes fan out one 202 per member (never a conv topic
	// publish): each frame is addressed by user_id so every member — online
	// or not — gets their own delivery attempt at the gateway.
	for _, ev := range r.Events {
		p.reply(&env{Kind: ev.Kind, ConvID: ev.ConvID, UserID: ev.UserID, Payload: ev.Payload})
	}
}

func (p *Pipe) onIdentity(ctx context.Context, req Req) {
	var raw struct {
		Op        uint32 `json:"op,omitempty"`
		UserID    uint64 `json:"userId,omitempty"`
		Body      []byte `json:"body,omitempty"`
		RequestID string `json:"requestId,omitempty"`
	}
	if err := wire.Unmarshal(req.Payload, &raw); err != nil {
		p.reply(buildErrEnv(req, 400, "bad payload"))
		return
	}
	uid := raw.UserID
	if uid == 0 {
		uid = req.UserID
	}
	r := p.ident.Handle(ctx, raw.Op, uid, raw.Body)
	pl, _ := wire.Marshal(wire.DataResult{Op: raw.Op, Ok: r.OK, Error: r.Error, RequestID: raw.RequestID})
	p.reply(buildResultEnv(req, uint32(kinds.KindIdentityResult), pl))
}

func (p *Pipe) onFlag(ctx context.Context, req Req) {
	var fr wire.FlagRequest
	if err := wire.Unmarshal(req.Payload, &fr); err != nil {
		p.reply(buildErrEnv(req, 400, "bad payload"))
		return
	}
	if req.Kind == uint32(kinds.KindFlagSet) {
		fr.Write = true
	}
	res, err := p.sys.handleFlag(ctx, fr)
	if err != nil {
		p.reply(buildErrEnv(req, 500, err.Error()))
		return
	}
	pl, _ := wire.Marshal(res)
	p.reply(buildResultEnv(req, uint32(kinds.KindFlagResult), pl))
}

// reply places one env on the outbound lane (non-blocking: the lane is
// bounded; a full lane drops the reply — clients retry idempotently).
func (p *Pipe) reply(e *env) {
	p.outMu.RLock()
	defer p.outMu.RUnlock()
	if p.draining {
		return // outCh is (or is about to be) closed
	}
	select {
	case p.outCh <- e:
	default:
		p.lg.Warn("pipe: reply lane full; reply dropped")
	}
}

// sendFanout places one bulk frame on the fan-out lane. Returns false when the
// frame was dropped (counted by the caller).
func (p *Pipe) sendFanout(e *env) bool {
	p.outMu.RLock()
	defer p.outMu.RUnlock()
	if p.draining {
		return false
	}
	select {
	case p.outFan <- e:
		return true
	default:
		return false
	}
}
