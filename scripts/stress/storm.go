package main

// storm.go implements scenario S-5 (重启风暴 / restart storm, spec §4.4 item 3):
//
//	phase 1  connect all workers, hold a 5s steady state (baseline pull QPS)
//	phase 2  disconnect every worker at once (the "storm")
//	phase 3  reconnect all of them and measure the 10s window right after:
//	         per-second kind-8 SYNC QPS curve, 5xx / error-frame count,
//	         reconnect success rate.
//
// The assertion printed at the end is: zero 5xx-equivalent frames (kind 10
// ERROR / kind 13 RATE_LIMITED) during the post-reconnect window, plus the
// observed peak QPS as the capacity upper bound for the reconnect burst.

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type stormResult struct {
	Requested     int       `json:"requested_conns"`
	Connected1st  int       `json:"first_connect_ok"`
	Reconnected   int       `json:"reconnect_ok"`
	ReconnectFail int       `json:"reconnect_fail"`
	ReconnectMs   int64     `json:"reconnect_all_ms"`
	SteadySyncs   int64     `json:"steady_state_syncs"` // 5s baseline window
	WindowSyncs   []int64   `json:"post_reconnect_qps"` // per-second kind-8 count, 10 entries
	ErrorFrames   int64     `json:"error_frames"`       // kind 10/13 seen post-reconnect
	DownWM        int64     `json:"down_watermarks"`
	DownMsgNew    int64     `json:"down_msg_new"`
	DownCtrl      int64     `json:"down_ctrl"`
	Convergence   []float64 `json:"convergence_p50_p95_ms"`
	OpenWM        int64     `json:"unsettled_watermarks"`
	PeakQPS       float64   `json:"peak_qps"`
	NoError       bool      `json:"no_error_frames"`
}

// runRestartStorm executes S-5 against a running chatd (boots its own only
// when base is empty — with the default flow main() already booted one when
// -no-boot is not set, but storm always drives against -base for clarity).
func runRestartStorm(base string, conns, convMembers, secs int, interval time.Duration, rateLimit bool, skipBoot bool, out string) {
	_ = skipBoot // storm reuses the same -base contract as the other modes
	if convMembers < 2 {
		convMembers = 2
	}
	if secs <= 0 {
		secs = 10
	}
	fmt.Printf("== OpenDiscord stress S-5 restart-storm: base=%s conns=%d members/conv=%d rate_limit=%s ==\n",
		base, conns, convMembers, rlLabel(rateLimit))

	// identity + convs (storm only needs a member set, no send load)
	pool, perr := preregister(base, conns, 32)
	if perr != nil {
		fmt.Printf("pre-register: %v (proceeding with %d ok)\n", perr, countNonNil(pool))
		pool = compact(pool)
		if len(pool) < conns {
			conns = len(pool)
		}
	}
	uids := make([]int64, len(pool))
	for i, c := range pool {
		uids[i] = c.uid
	}
	numConvs := (conns + convMembers - 1) / convMembers
	if numConvs < 1 {
		numConvs = 1
	}
	var convIDs []int64
	if db := dbPathFromEnv(); db != "" {
		var err error
		convIDs, err = seedConvs(db, uids, convMembers, numConvs)
		if err != nil {
			fmt.Println("seed convs:", err)
			osExit(2)
			return
		}
	} else {
		// no DB access: fall back to conv 0 (server tolerates unknown conv for
		// pure WS connect; watermark paths simply never fire)
		convIDs = []int64{0}
	}
	if len(convIDs) == 0 {
		convIDs = []int64{0}
	}

	dial := func(n int) []*worker {
		var (
			mu      sync.Mutex
			ws      []*worker
			sem     = make(chan struct{}, 256)
			wg      sync.WaitGroup
			rampErr int64
		)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				convIdx := id / convMembers
				if convIdx >= len(convIDs) {
					convIdx = len(convIDs) - 1
				}
				var conn *websocket.Conn
				var err error
				for attempt := 0; attempt < 4; attempt++ {
					conn, err = dialWS(base, pool[id].tok)
					if err == nil {
						break
					}
					time.Sleep(150 * time.Millisecond)
				}
				if err != nil {
					atomic.AddInt64(&rampErr, 1)
					return
				}
				w := newWorker(id, pool[id].tok, pool[id].uid, uint64(convIDs[convIdx]), conn)
				w.pull = newPullTracker() // storm always runs pull-mode
				mu.Lock()
				ws = append(ws, w)
				mu.Unlock()
			}(i)
		}
		wg.Wait()
		if rampErr > 0 {
			fmt.Printf("  dial errors: %d\n", rampErr)
		}
		return ws
	}

	// ---- phase 1: connect + 5s steady state ----
	ws := dial(conns)
	res := &stormResult{Requested: conns, Connected1st: len(ws)}
	if len(ws) == 0 {
		fmt.Println("storm: no connections established")
		osExit(2)
		return
	}
	for _, w := range ws {
		go w.readLoop(newPeerRecorder())
	}
	steadyDeadline := time.Now().Add(5 * time.Second)
	for _, w := range ws {
		go w.run(steadyDeadline, interval, newMixer(100, 0, 0, 0), newPeerRecorder())
	}
	time.Sleep(5 * time.Second)
	var steadySyncs int64
	for _, w := range ws {
		steadySyncs += w.syncQPS.Load()
	}
	res.SteadySyncs = steadySyncs

	// ---- phase 2: storm — everyone drops at once ----
	for _, w := range ws {
		w.conn.Close()
	}
	fmt.Printf("phase 2: disconnected %d connections\n", len(ws))
	time.Sleep(300 * time.Millisecond) // let the server reap the sockets

	// ---- phase 3: reconnect all, measure 10s window ----
	t0 := time.Now()
	ws2 := dial(conns)
	res.ReconnectMs = time.Since(t0).Milliseconds()
	res.Reconnected = len(ws2)
	res.ReconnectFail = conns - len(ws2)

	var errFrames, wm, mn, ctl int64
	var convParts [][]time.Duration
	var open int64
	stopQPS := make([]int64, secs)
	var qpsWG sync.WaitGroup
	stopAfter := time.Now().Add(time.Duration(secs) * time.Second)
	for _, w := range ws2 {
		go w.readLoop(newPeerRecorder())
		qpsWG.Add(1)
		go func(w *worker) {
			defer qpsWG.Done()
			w.run(stopAfter, interval, newMixer(100, 0, 0, 0), newPeerRecorder())
		}(w)
	}
	// sample syncQPS once per second (the QPS curve)
	for s := 0; s < secs; s++ {
		time.Sleep(time.Second)
		var cur int64
		for _, w := range ws2 {
			cur += w.syncQPS.Load()
		}
		stopQPS[s] = cur // cumulative; converted to per-second below
	}
	qpsWG.Wait()
	time.Sleep(300 * time.Millisecond)

	prev := int64(0)
	peak := 0.0
	for s := 0; s < secs; s++ {
		delta := stopQPS[s] - prev
		prev = stopQPS[s]
		res.WindowSyncs = append(res.WindowSyncs, delta)
		if float64(delta) > peak {
			peak = float64(delta)
		}
	}
	res.PeakQPS = peak
	for _, w := range ws2 {
		errFrames += w.serverDrop.Load()
		wm += w.downWM.Load()
		mn += w.downMsgNew.Load()
		ctl += w.downCtrl.Load()
		if w.pull != nil {
			convParts = append(convParts, w.pull.convergence())
			open += int64(w.pull.outstanding())
		}
	}
	res.ErrorFrames = errFrames
	res.DownWM, res.DownMsgNew, res.DownCtrl = wm, mn, ctl
	res.OpenWM = open
	res.NoError = errFrames == 0
	conv := mergeSamples(convParts)
	res.Convergence = []float64{msDur(conv, 0.50), msDur(conv, 0.95)}

	printStormReport(res)
	if out != "" {
		writeJSONAny(out, res)
	}
}

// printStormReport renders the S-5 verdict block.
func printStormReport(r *stormResult) {
	total := int64(0)
	for _, v := range r.WindowSyncs {
		total += v
	}
	fmt.Printf(`
=== S-5 restart-storm report ===
first_connect:  %d/%d
reconnect:      %d/%d ok in %d ms (%d failed)
steady_state:   %d kind-8 requests in 5s (avg %.1f QPS)
post-reconnect 10s kind-8 QPS curve: %v
  total=%d  avg=%.1f QPS  peak=%.1f QPS (per-cluster-second upper bound observed)
down frames:    watermark=%d msg_new=%d ctrl=%d
error frames:   %d (kind 10/13, post-reconnect)
convergence:    p50=%v p95=%v (unsettled=%d)
VERDICT: %s
`,
		r.Connected1st, r.Requested,
		r.Reconnected, r.Requested, r.ReconnectMs, r.ReconnectFail,
		r.SteadySyncs, float64(r.SteadySyncs)/5.0,
		r.WindowSyncs, total, float64(total)/float64(maxInt(1, len(r.WindowSyncs))), r.PeakQPS,
		r.DownWM, r.DownMsgNew, r.DownCtrl,
		r.ErrorFrames,
		fmtDur(time.Duration(r.Convergence[0]*float64(time.Millisecond))),
		fmtDur(time.Duration(r.Convergence[1]*float64(time.Millisecond))),
		r.OpenWM,
		map[bool]string{true: "PASS (no 5xx/error frames)", false: "FAIL (error frames observed)"}[r.NoError])
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
