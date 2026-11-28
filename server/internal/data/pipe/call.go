package pipe

// call.go: WebRTC call signaling relay (kinds 350-354).
//
// RED LINE: the data layer is a signaling switchboard ONLY. It never sees,
// inspects or relays media — there is no SFU, no transcoding, no SDP munging.
// An OFFER/ANSWER/CANDIDATE payload is an opaque JSON string carried verbatim
// to the peer; only the routing fields (callId/convId/toUserId) are read and
// only fromUserId is rewritten (to the authenticated sender, never trusted
// from the client).
//
// State is process-local (map[callId]*callState). A call is "ringing" from
// OFFER until ANSWER (then "active") or until a deadline fires, at which point
// both parties receive CALL_HANGUP{reason:"timeout"} and the entry is cleared.
//
// Deadlines (P0 leak fix): a per-call AfterFunc would leak whenever a peer
// crashes / drops the socket / closes the tab without a HANGUP, permanently
// pinning userInCall[uid] and making every later call to/from that user answer
// BUSY. To make that impossible we (a) arm a hard max-call-duration deadline on
// ANSWER so an active call can never live forever, (b) cap the table size, and
// (c) drive ALL expiry from ONE background ticker (sweep) instead of one
// timer per call — 1000 concurrent calls = 1 goroutine/timer, not 1000.
// Mechanism, not policy: no call records, no history, no quotas.

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/yingo-server/UFOT/server/internal/data/pipe/wire"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
)

const (
	callStateRinging = "ringing"
	callStateActive  = "active"

	// defaultCallRingTimeout is the ring window before a caller/callee pair is
	// auto-hung-up. Exposed as a field (callRegistry.timeout) so tests can
	// inject a short value.
	defaultCallRingTimeout = 60 * time.Second

	// defaultMaxCallDuration is the hard ceiling on an ACTIVE call. After
	// ANSWER re-arms the deadline, this bounds how long a call may stay in the
	// map even if neither side ever sends HANGUP. Exposed as a field
	// (callRegistry.maxCallDuration) for tests.
	defaultMaxCallDuration = 4 * time.Hour

	// defaultMaxCalls caps the in-process call table so anomalous/exhaustion
	// paths (many half-open calls) cannot grow it without bound. New calls past
	// the cap are rejected with CALL_BUSY. Exposed as callRegistry.max.
	defaultMaxCalls = 4096

	// defaultCallSweepInterval is how often the single background ticker scans
	// for expired calls.
	defaultCallSweepInterval = 10 * time.Second
)

// errCallExists is returned by create when the same callId is already in the
// table (a second OFFER must not silently overwrite a live call's state).
var errCallExists = errors.New("call already exists")

// errCallTableFull is returned by create when the table cap is reached.
var errCallTableFull = errors.New("call table full")

// callState is one in-progress call. convID/from/to are the routing identity.
// deadline is the absolute expiry: the ring window while ringing, then the
// max-call-duration window once active. A zero deadline never expires.
type callState struct {
	convID    int64
	from      int64
	to        int64
	startedAt time.Time
	state     string
	deadline  time.Time
}

// expiredCall is one swept-out call, handed back to the pipe so it can emit the
// timeout hangup to both parties outside the registry lock.
type expiredCall struct {
	callID    string
	convID    int64
	from      int64
	to        int64
	startedAt time.Time
}

// callRegistry is the guarded in-process call table plus a reverse index
// (userInCall: uid → callId) used to answer "is this callee already busy?".
type callRegistry struct {
	mu         sync.Mutex
	calls      map[string]*callState
	userInCall map[int64]string
	timeout    time.Duration    // ring window
	maxCallDur time.Duration    // active-call ceiling (maxCallDuration)
	maxCalls   int              // table size cap
	sweepEvery time.Duration    // ticker interval
	now        func() time.Time // injectable clock (tests advance it)
}

func newCallRegistry() *callRegistry {
	return &callRegistry{
		calls:      make(map[string]*callState),
		userInCall: make(map[int64]string),
		timeout:    defaultCallRingTimeout,
		maxCallDur: defaultMaxCallDuration,
		maxCalls:   defaultMaxCalls,
		sweepEvery: defaultCallSweepInterval,
		now:        time.Now,
	}
}

// clock returns the current time via the injectable clock (never nil).
func (c *callRegistry) clock() time.Time {
	if c.now == nil {
		return time.Now()
	}
	return c.now()
}

// busy reports whether uid is currently a party of a ringing/active call.
func (c *callRegistry) busy(uid int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.userInCall[uid]
	return ok
}

// create inserts a new ringing call. It returns errCallExists when callID is
// already present (no silent overwrite) and errCallTableFull when the table cap
// is reached. The ring deadline is armed from the injectable clock; expiry is
// driven by the single sweeper, NOT a per-call timer.
func (c *callRegistry) create(callID string, convID, from, to int64) (*callState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.calls[callID]; ok {
		return nil, errCallExists
	}
	if c.maxCalls > 0 && len(c.calls) >= c.maxCalls {
		return nil, errCallTableFull
	}
	now := c.clock()
	st := &callState{convID: convID, from: from, to: to, startedAt: now, state: callStateRinging}
	if c.timeout > 0 {
		st.deadline = now.Add(c.timeout)
	}
	c.calls[callID] = st
	c.userInCall[from] = callID
	c.userInCall[to] = callID
	return st, nil
}

// get returns the call state (nil when absent).
func (c *callRegistry) get(callID string) *callState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[callID]
}

// answer transitions ringing → active and re-arms the deadline to the hard
// max-call-duration ceiling. Returns false when the call is missing or was not
// ringing.
func (c *callRegistry) answer(callID string) (*callState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.calls[callID]
	if !ok {
		return nil, false
	}
	if st.state != callStateRinging {
		return st, false
	}
	st.state = callStateActive
	if c.maxCallDur > 0 {
		st.deadline = c.clock().Add(c.maxCallDur)
	} else {
		st.deadline = time.Time{}
	}
	return st, true
}

// remove deletes a call and clears the reverse index (HANGUP/BUSY). Returns the
// state (nil when absent) so the caller can forward to the peer.
func (c *callRegistry) remove(callID string) *callState {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.calls[callID]
	if !ok {
		return nil
	}
	delete(c.calls, callID)
	delete(c.userInCall, st.from)
	delete(c.userInCall, st.to)
	return st
}

// sweepExpired removes every call whose deadline has passed as of now and
// returns them for the caller to hang up. ONE pass clears all expired calls —
// this is the single-ticker sweep body (see Pipe.runCallSweeper).
func (c *callRegistry) sweepExpired(now time.Time) []expiredCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []expiredCall
	for id, st := range c.calls {
		if st.deadline.IsZero() || now.Before(st.deadline) {
			continue
		}
		delete(c.calls, id)
		delete(c.userInCall, st.from)
		delete(c.userInCall, st.to)
		out = append(out, expiredCall{callID: id, convID: st.convID, from: st.from, to: st.to, startedAt: st.startedAt})
	}
	return out
}

// Count returns the number of in-progress calls (observability).
func (c *callRegistry) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// UserInCallCount returns the number of users currently pinned in a call
// (observability). Two per active call in the normal case.
func (c *callRegistry) UserInCallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.userInCall)
}

// stopAll drops all state (Drain). There are no per-call timers to stop; the
// sweeper goroutine exits on ctx cancel.
func (c *callRegistry) stopAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = make(map[string]*callState)
	c.userInCall = make(map[int64]string)
}

// CallStats returns (activeCalls, usersInCall) for the status dump.
func (p *Pipe) CallStats() (int, int) {
	return p.calls.Count(), p.calls.UserInCallCount()
}

// runCallSweeper drives all call expiry from a single ticker. Every sweepEvery
// it scans the table and hangs up any expired call. One goroutine per process
// regardless of how many calls are in flight.
func (p *Pipe) runCallSweeper(ctx context.Context) {
	interval := p.calls.sweepEvery
	if interval <= 0 {
		interval = defaultCallSweepInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.sweepCalls(p.calls.clock())
		}
	}
}

// sweepCalls performs one expiry scan at now and emits CALL_HANGUP(timeout) to
// both parties of every expired call.
func (p *Pipe) sweepCalls(now time.Time) {
	for _, ex := range p.calls.sweepExpired(now) {
		p.sendCallHangup(ex.callID, ex.convID, ex.from, ex.to)
		// Audit the timed-out call end (metadata only, never SDP). The call is
		// already cleared, so duration comes from the swept snapshot.
		p.audit(context.Background(), "call.end", ex.from, "call:"+ex.callID, map[string]any{
			"call_id":     ex.callID,
			"conv_id":     ex.convID,
			"peer_uid":    ex.to,
			"duration_ms": now.Sub(ex.startedAt).Milliseconds(),
			"reason":      "timeout",
		})
	}
}

// onCall is the dispatch entry for kinds 350-354.
func (p *Pipe) onCall(ctx context.Context, req Req) {
	if !p.callsEnabled(ctx) {
		p.reply(buildErrEnv(req, 403, "calls disabled"))
		return
	}
	var sig wire.CallSignal
	if len(req.Payload) > 0 {
		if err := wire.Unmarshal(req.Payload, &sig); err != nil {
			p.reply(buildErrEnv(req, 400, "bad payload"))
			return
		}
	}
	sender := int64(req.UserID)

	switch req.Kind {
	case uint32(kinds.KindCallOffer):
		p.onCallOffer(ctx, req, sig, sender)
	case uint32(kinds.KindCallAnswer):
		p.onCallAnswer(ctx, req, sig, sender)
	case uint32(kinds.KindCallCandidate):
		p.onCallRelay(ctx, req, sig, sender)
	case uint32(kinds.KindCallBusy), uint32(kinds.KindCallHangup):
		p.onCallTeardown(ctx, req, sig, sender)
	default:
		p.reply(buildErrEnv(req, 400, "bad call kind"))
	}
}

// onCallOffer: permission-gate both parties, auto-BUSY a busy callee, reject a
// duplicate callId (409) or a full table (BUSY), else open a ringing call and
// relay the offer to the callee.
func (p *Pipe) onCallOffer(ctx context.Context, req Req, sig wire.CallSignal, sender int64) {
	convID := int64(sig.ConvID)
	if convID == 0 {
		convID = int64(req.ConvID)
	}
	to := int64(sig.ToUserID)
	if convID == 0 || to == 0 || sender == 0 || sig.CallID == "" {
		p.reply(buildErrEnv(req, 400, "bad offer"))
		return
	}
	if !p.bothMembers(ctx, convID, sender, to) {
		p.reply(buildErrEnv(req, 403, "not a member"))
		return
	}
	// Callee already in a call → answer the caller with CALL_BUSY(busy).
	if p.calls.busy(to) {
		p.replyCallBusy(sig.CallID, convID, sender)
		return
	}
	if _, err := p.calls.create(sig.CallID, convID, sender, to); err != nil {
		switch {
		case errors.Is(err, errCallExists):
			// Do not overwrite the live call: conflict.
			p.reply(buildErrEnv(req, 409, "call already exists"))
		case errors.Is(err, errCallTableFull):
			// Table cap reached: surface as busy so the client backs off.
			p.replyCallBusy(sig.CallID, convID, sender)
		default:
			p.reply(buildErrEnv(req, 500, "call create failed"))
		}
		return
	}
	p.relayCall(req, sig, sender, to)
	// Audit call establishment: metadata only, NEVER SDP/candidate content.
	p.audit(ctx, "call.start", sender, "call:"+sig.CallID, map[string]any{
		"call_id":     sig.CallID,
		"conv_id":     convID,
		"peer_uid":    to,
		"duration_ms": int64(0),
		"reason":      "offer",
	})
}
func (p *Pipe) replyCallBusy(callID string, convID, to int64) {
	pl, _ := wire.Marshal(wire.CallSignal{CallID: callID, Reason: "busy", FromUserID: uint64(to)})
	p.reply(&env{Kind: uint32(kinds.KindCallBusy), ConvID: uint64(convID), UserID: uint64(to), Payload: pl})
}

// onCallAnswer: the call must exist and still be ringing; flip to active (which
// re-arms the hard max-duration deadline) and relay the answer to the caller.
func (p *Pipe) onCallAnswer(ctx context.Context, req Req, sig wire.CallSignal, sender int64) {
	st := p.calls.get(sig.CallID)
	if st == nil {
		p.reply(buildErrEnv(req, 404, "no such call"))
		return
	}
	// Authorize BEFORE mutating state (a forged answer must not flip the call).
	peer := peerOf(st, sender)
	if !p.bothMembers(ctx, st.convID, sender, peer) {
		p.reply(buildErrEnv(req, 403, "not a member"))
		return
	}
	if _, ok := p.calls.answer(sig.CallID); !ok {
		p.reply(buildErrEnv(req, 404, "no such call"))
		return
	}
	p.relayCall(req, sig, sender, peer)
}

// onCallRelay: CANDIDATE — no state change; locate the peer via the call table
// and forward verbatim.
func (p *Pipe) onCallRelay(ctx context.Context, req Req, sig wire.CallSignal, sender int64) {
	st := p.calls.get(sig.CallID)
	if st == nil {
		p.reply(buildErrEnv(req, 404, "no such call"))
		return
	}
	peer := peerOf(st, sender)
	if !p.bothMembers(ctx, st.convID, sender, peer) {
		p.reply(buildErrEnv(req, 403, "not a member"))
		return
	}
	p.relayCall(req, sig, sender, peer)
}

// onCallTeardown: BUSY/HANGUP — permission-gate, clear state, relay to peer.
func (p *Pipe) onCallTeardown(ctx context.Context, req Req, sig wire.CallSignal, sender int64) {
	st := p.calls.get(sig.CallID)
	if st == nil {
		p.reply(buildErrEnv(req, 404, "no such call"))
		return
	}
	peer := peerOf(st, sender)
	if !p.bothMembers(ctx, st.convID, sender, peer) {
		p.reply(buildErrEnv(req, 403, "not a member"))
		return
	}
	p.calls.remove(sig.CallID)
	p.relayCall(req, sig, sender, peer)
	// Audit call end on an explicit HANGUP (BUSY is a pre-call rejection, not a
	// call end). Metadata only — duration/reason/peer, never SDP.
	if req.Kind == uint32(kinds.KindCallHangup) {
		reason := sig.Reason
		if reason == "" {
			reason = "hangup"
		}
		p.audit(ctx, "call.end", sender, "call:"+sig.CallID, map[string]any{
			"call_id":     sig.CallID,
			"conv_id":     st.convID,
			"peer_uid":    peer,
			"duration_ms": time.Since(st.startedAt).Milliseconds(),
			"reason":      reason,
		})
	}
}

// relayCall rewrites FromUserID to the real sender and emits the frame to
// targetUID. The env.UserID IS the delivery target (installEmit turns it into
// Envelope.UserId, and the user gateway's downstream router delivers by uid).
func (p *Pipe) relayCall(req Req, sig wire.CallSignal, sender, targetUID int64) {
	sig.FromUserID = uint64(sender)
	pl, _ := wire.Marshal(sig)
	p.reply(&env{Kind: req.Kind, ConvID: sig.ConvID, UserID: uint64(targetUID), Payload: pl})
}

// sendCallHangup emits CALL_HANGUP(reason="timeout") to both parties. Called by
// the sweeper after the entry has already been removed.
func (p *Pipe) sendCallHangup(callID string, convID, from, to int64) {
	pl, _ := wire.Marshal(wire.CallSignal{
		CallID: callID, ConvID: uint64(convID), Reason: "timeout", FromUserID: uint64(from),
	})
	p.reply(&env{Kind: uint32(kinds.KindCallHangup), ConvID: uint64(convID), UserID: uint64(from), Payload: pl})
	p.reply(&env{Kind: uint32(kinds.KindCallHangup), ConvID: uint64(convID), UserID: uint64(to), Payload: pl})
}

// bothMembers is the security red line: BOTH the sender and the peer (callee or
// the other party) must belong to convID, else the signal is dropped (403).
func (p *Pipe) bothMembers(ctx context.Context, convID, a, b int64) bool {
	if a == 0 || b == 0 {
		return false
	}
	if _, err := p.hot.CheckMember(ctx, convID, a); err != nil {
		return false
	}
	if _, err := p.hot.CheckMember(ctx, convID, b); err != nil {
		return false
	}
	return true
}

// peerOf returns the OTHER party of a two-party call given one side.
func peerOf(st *callState, who int64) int64 {
	if who == st.to {
		return st.from
	}
	return st.to
}

// callsEnabled reads sys_feature_flags.call_enabled; absent = false (default
// off). The flag row holds JSON ("true"/"false") as written by bootstrapFlags.
func (p *Pipe) callsEnabled(ctx context.Context) bool {
	flags, err := p.hot.AllFlags(ctx)
	if err != nil {
		return false
	}
	for _, f := range flags {
		if f.Key != "call_enabled" {
			continue
		}
		var b bool
		if err := json.Unmarshal([]byte(f.Value), &b); err == nil {
			return b
		}
		return f.Value == "true"
	}
	return false
}
