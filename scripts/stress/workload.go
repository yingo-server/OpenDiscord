package main

// workload.go is the per-connection load driver. Every worker owns:
//   - its own ring buffer per op (capacity 256) — no shared latency atomic;
//   - its own pending send map (client_msg_id → send time) for ACK correlation;
//   - a distinct pre-registered token (so ACKs route to exactly one connection).
// It also tracks per-worker server_drop (RATE_LIMITED / ERROR / NACK frames
// observed on this connection) independently from client write errors.

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const ringCapacity = 256

// opLocal is one worker's stats for one operation type.
type opLocal struct {
	mu        sync.Mutex
	attempted int64
	ok        int64
	writeErr  int64
	lat       *ring
	pending   map[string]time.Time // client_msg_id → send time (msg_send only)
}

func newOpLocal() *opLocal {
	return &opLocal{lat: newRing(ringCapacity), pending: make(map[string]time.Time, 64)}
}

func (o *opLocal) attempt() {
	o.mu.Lock()
	o.attempted++
	o.mu.Unlock()
}

func (o *opLocal) addWriteErr() {
	o.mu.Lock()
	o.writeErr++
	o.mu.Unlock()
}

func (o *opLocal) addOK(d time.Duration) {
	o.mu.Lock()
	o.ok++
	o.lat.add(d)
	o.mu.Unlock()
}

// registerPending records the send timestamp for a msg_send correlation key.
func (o *opLocal) registerPending(cmid string, t0 time.Time) {
	o.mu.Lock()
	o.pending[cmid] = t0
	o.mu.Unlock()
}

// settleAck correlates an incoming ACK. It returns (rtt, found, statusOK).
func (o *opLocal) settleAck(cmid string) (time.Duration, bool) {
	o.mu.Lock()
	t0, ok := o.pending[cmid]
	if ok {
		delete(o.pending, cmid)
	}
	o.mu.Unlock()
	if !ok {
		return 0, false
	}
	return time.Since(t0), true
}

func (o *opLocal) markAcked(rtt time.Duration) {
	o.mu.Lock()
	o.ok++
	o.lat.add(rtt)
	o.mu.Unlock()
}

// snapshot copies out aggregate counters + the ring samples.
func (o *opLocal) snapshot() (attempted, ok, writeErr int64, samples []time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.attempted, o.ok, o.writeErr, o.lat.snapshot()
}

// worker is one live WS connection.
type worker struct {
	id          int
	tok         string
	uid         int64
	conv        uint64
	conn        *websocket.Conn
	serverDrop  atomic.Int64
	cmidCounter atomic.Uint64
	dead        atomic.Bool
	ops         map[string]*opLocal
	// pull-mode (§4.4): nil unless --pull-mode. Counters are per-connection
	// and merged into levelResult by the runner; lastSeq is per-conv cursor.
	pull            *pullTracker
	downWM          atomic.Int64 // watermark frames received
	downMsgNew      atomic.Int64 // MSG_NEW frames received (live + catch-up replay)
	downCtrl        atomic.Int64 // every other downstream frame (ack/receipt/typing/presence/error/...)
	downRateLimited atomic.Int64 // kind 13 RATE_LIMITED frames observed (S-6 adaptive evidence)
	syncQPS         atomic.Int64 // kind 8 uplink requests sent
	// S-4 slow-consumer injection (REL): set by the runner before run().
	isSlow   bool
	slowMult float64
}

var opNames = []string{"msg_send", "typing", "receipt", "presence"}

func newWorker(id int, tok string, uid int64, conv uint64, conn *websocket.Conn) *worker {
	w := &worker{id: id, tok: tok, uid: uid, conv: conv, conn: conn, ops: make(map[string]*opLocal, len(opNames))}
	for _, n := range opNames {
		w.ops[n] = newOpLocal()
	}
	return w
}

func (w *worker) nextCMID() string {
	return fmt.Sprintf("w%d-%d-%s", w.id, w.cmidCounter.Add(1), randHex(6))
}

// readLoop consumes server frames for the whole test window.
func (w *worker) readLoop(peer *peerRecorder) {
	for {
		_, data, err := w.conn.ReadMessage()
		if err != nil {
			return
		}
		env, err := decodeEnvelope(data)
		if err != nil {
			continue
		}
		switch env.Kind {
		case kindMsgAck:
			w.downCtrl.Add(1)
			var a msgAckPayload
			if json.Unmarshal(env.Payload, &a) == nil && a.ClientMsgID != "" {
				if rtt, found := w.ops["msg_send"].settleAck(a.ClientMsgID); found {
					if a.Status == 0 {
						w.ops["msg_send"].markAcked(rtt)
					} else {
						// server-side NACK (e.g. data-layer rate limit)
						w.serverDrop.Add(1)
					}
				}
			}
		case kindRateLimited, kindError:
			w.downCtrl.Add(1)
			w.serverDrop.Add(1)
			if env.Kind == kindRateLimited {
				w.downRateLimited.Add(1)
			}
		case kindMsgNew:
			w.downMsgNew.Add(1)
			var m msgNewPayload
			if json.Unmarshal(env.Payload, &m) == nil && len(m.Content) > 0 {
				var c msgNewContent
				if json.Unmarshal(m.Content, &c) == nil && c.ClientMsgID != "" {
					if peer != nil {
						peer.recv(c.ClientMsgID)
					}
				}
				if w.pull != nil && m.Seq > 0 {
					// Catch-up replay frames carry seq but usually a client_msg_id
					// we do not own; ingest the seq regardless so lastSeq advances.
					w.pull.onMsgNew(w.conv, m.Seq, time.Now())
				}
			}
		case kindWatermark:
			// §4.4 item 1. Without --pull-mode this branch is unreachable in a
			// correct deployment (the server only sends 14 to pull-mode convs),
			// but if the flag is mis-set we count it as a control frame so the
			// report exposes the mismatch instead of silently dropping it.
			if w.pull == nil {
				w.downCtrl.Add(1)
				break
			}
			var wm watermarkPayload
			if json.Unmarshal(env.Payload, &wm) != nil {
				w.downCtrl.Add(1)
				break
			}
			w.downWM.Add(1)
			conv := wm.ConvID
			if conv == 0 {
				conv = env.ConvID
			}
			if req := w.pull.onWatermark(conv, wm.LatestSeq, time.Now()); req != nil {
				w.sendSync(req)
			}
		default:
			w.downCtrl.Add(1)
		}
	}
}

// sendSync writes one kind 8 SYNC request (the uplink pull). Counted for the
// client-side 8/920 QPS statistic. A write error marks the worker dead (the
// read loop will exit on the broken socket anyway).
func (w *worker) sendSync(req *syncRequestPayload) {
	pl, err := encodeSyncRequest(req)
	if err != nil {
		return
	}
	env := &envelope{Kind: kindSyncRequest, ConvID: req.ConvID, Payload: pl}
	b, err := encodeEnvelope(env)
	if err != nil {
		return
	}
	if err := w.conn.WriteMessage(websocket.TextMessage, b); err != nil {
		w.dead.Store(true)
		return
	}
	w.syncQPS.Add(1)
}

// run is the send loop: pick an op per the mixer, write one frame, pace.
func (w *worker) run(deadline time.Time, interval time.Duration, m *mixer, peer *peerRecorder) {
	i := 0
	for time.Now().Before(deadline) {
		if w.dead.Load() {
			return
		}
		w.doOp(m.next(i), peer)
		i++
		if interval > 0 {
			pause := interval
			if w.slowMult > 1 {
				// S-4: slow consumers pace slower; read loop untouched.
				pause = time.Duration(float64(pause) * w.slowMult)
			}
			time.Sleep(pause)
		}
	}
}

func (w *worker) doOp(name string, peer *peerRecorder) {
	op := w.ops[name]
	if op == nil {
		return
	}
	env := &envelope{ConvID: w.conv}
	switch name {
	case "msg_send":
		cmid := w.nextCMID()
		pl, _ := json.Marshal(msgSendPayload{ClientMsgID: cmid, Text: "l"})
		env.Kind = kindMsgSend
		env.Payload = pl
		now := time.Now()
		op.attempt()
		op.registerPending(cmid, now)
		peer.mark(cmid, now)
	case "typing":
		pl, _ := json.Marshal(typingPayload{ConvID: w.conv, Typing: true})
		env.Kind = kindTyping
		env.Payload = pl
		op.attempt()
	case "receipt":
		pl, _ := json.Marshal(receiptPayload{Receipts: []receiptItem{{MsgID: 1}}, Rtype: 0})
		env.Kind = kindMsgRead
		env.Payload = pl
		op.attempt()
	case "presence":
		pl, _ := json.Marshal(presencePayload{UserID: uint64(w.uid), Status: "online", At: time.Now().UnixMilli()})
		env.Kind = kindPresence
		env.Payload = pl
		op.attempt()
	default:
		return
	}
	b, err := encodeEnvelope(env)
	if err != nil {
		op.addWriteErr()
		return
	}
	t0 := time.Now()
	if werr := w.conn.WriteMessage(websocket.TextMessage, b); werr != nil {
		op.addWriteErr()
		w.dead.Store(true)
		return
	}
	// Non-ACK ops: success == the socket accepted the write. (Their true server
	// effect is reported via write_err / server_drop and, for msg_send, ACK rate.)
	if name != "msg_send" {
		op.addOK(time.Since(t0))
	}
}

// peerRecorder measures send→peer_recv: when any member connection receives a
// MSG_NEW whose embedded client_msg_id matches one this process sent, we record
// the round trip. This is the "peer fanout actually arrived" latency, distinct
// from send→ack.
type peerRecorder struct {
	mu      sync.Mutex
	pending map[string]time.Time
	samples []time.Duration
}

func newPeerRecorder() *peerRecorder {
	return &peerRecorder{pending: make(map[string]time.Time, 1024)}
}

func (p *peerRecorder) mark(cmid string, t0 time.Time) {
	p.mu.Lock()
	if len(p.pending) < 500000 {
		p.pending[cmid] = t0
	}
	p.mu.Unlock()
}

func (p *peerRecorder) recv(cmid string) {
	p.mu.Lock()
	t0, ok := p.pending[cmid]
	if ok {
		delete(p.pending, cmid)
		if len(p.samples) < 500000 {
			p.samples = append(p.samples, time.Since(t0))
		}
	}
	p.mu.Unlock()
}

func (p *peerRecorder) snapshot() []time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]time.Duration, len(p.samples))
	copy(out, p.samples)
	return out
}

// mixer emits ops in a smooth weighted round-robin order, so even a short run
// (a few hundred ops) hits every operation type in roughly the right proportion
// — a plain block-by-ratio list starves TYPING/PRESENCE until index 70+.
type mixer struct {
	ops []string
}

func newMixer(send, typing, receipt, presence int) *mixer {
	weights := []int{send, typing, receipt, presence}
	names := []string{"msg_send", "typing", "receipt", "presence"}
	total := 0
	for _, w := range weights {
		if w > 0 {
			total += w
		}
	}
	if total <= 0 {
		return &mixer{ops: []string{"msg_send"}}
	}
	// smooth weighted round-robin (nginx): deterministic, proportional, interleaved.
	current := make([]int, len(weights))
	ops := make([]string, 0, total)
	for step := 0; step < total; step++ {
		best, bestIdx := -1, -1
		for i, w := range weights {
			current[i] += w
			if current[i] > best {
				best, bestIdx = current[i], i
			}
		}
		current[bestIdx] -= total
		ops = append(ops, names[bestIdx])
	}
	return &mixer{ops: ops}
}

func (m *mixer) next(i int) string { return m.ops[i%len(m.ops)] }
