package main

// Command stress is the CI load generator for OpenDiscord. It boots a real chatd,
// pre-registers a user pool ONCE, seeds N-member conversations, then drives the
// WS channel (and optionally HTTP) and prints an honest, auditable capacity
// report. See docs/architecture.md: tools are small and composed.
//
// Design goals (vs the previous 800-line single-file tool):
//   - registration and connection are separated (phase 0 pre-registers a pool);
//   - every worker keeps its OWN latency ring (real p50/p95/p99);
//   - availability = ACKs / attempted sends, with write_err and server_drop as
//     independent columns;
//   - --mode=mixed injects MSG_SEND/TYPING/RECEIPT/PRESENCE;
//   - --conv-members makes fanout O(N) instead of O(1);
//   - rate limiting is ON by default (production parity), --no-ratelimit opts out
//     and the run is prominently flagged as such.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	_ "github.com/mattn/go-sqlite3"
)

// ---------------------------------------------------------------------------
// HTTP identity
// ---------------------------------------------------------------------------

type client struct {
	hc  *http.Client
	tok string
	uid int64
}

func guest(base string) (*client, error) {
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, Timeout: 15 * time.Second}
	resp, err := hc.Post(base+"/api/auth/guest", "application/json",
		bytes.NewReader([]byte(`{"device_id":"stress-`+randHex(8)+`"}`)))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("guest status %d", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		UserID      int64  `json:"user_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.AccessToken == "" || out.UserID <= 0 {
		return nil, fmt.Errorf("guest returned empty token/uid")
	}
	return &client{hc: hc, tok: out.AccessToken, uid: out.UserID}, nil
}

// preregister registers n users with bounded concurrency (32), retrying a few
// times, and returns a dense pool. Registration happens exactly once — the ramp
// phase never registers (defect #1/#8).
func preregister(base string, n, conc int) ([]*client, error) {
	if conc <= 0 {
		conc = 32
	}
	pool := make([]*client, n)
	jobs := make(chan int)
	var wg sync.WaitGroup
	var failCount int64
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				var c *client
				var err error
				for attempt := 0; attempt < 5; attempt++ {
					if c, err = guest(base); err == nil {
						break
					}
					time.Sleep(50 * time.Millisecond)
				}
				if err != nil {
					atomic.AddInt64(&failCount, 1)
					continue
				}
				pool[i] = c
			}
		}()
	}
	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	if failCount > 0 {
		return pool, fmt.Errorf("%d/%d guest registrations failed", failCount, n)
	}
	return pool, nil
}

// ---------------------------------------------------------------------------
// server boot + seeding
// ---------------------------------------------------------------------------

func runChatd(dir string, rateLimit bool) (*exec.Cmd, string, error) {
	cfg := filepath.Join(dir, "chatd.toml")
	toml := fmt.Sprintf(`mode = "embedded"
listen = "127.0.0.1:18080"
admin_listen = "127.0.0.1:17780"
admin_rest_listen = "127.0.0.1:17781"
hot_db = "%[1]s/hot.db"
admin_db = "%[1]s/admin.db"
policy_path = "%[1]s/policy.json"
jwt_key_path = "%[1]s/keys/jwt.pem"
media_dir = "%[1]s/media"
log_level = "info"
dev_otp = true
rate_limit_enabled = %[2]t
`, dir, rateLimit)
	if err := os.WriteFile(cfg, []byte(toml), 0o600); err != nil {
		return nil, "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "policy.json"),
		[]byte(`{"hot_retention_days": 90}`), 0o600); err != nil {
		return nil, "", err
	}
	os.MkdirAll(filepath.Join(dir, "keys"), 0o750)
	os.MkdirAll(filepath.Join(dir, "media"), 0o750)

	cmd := exec.Command(os.Getenv("CHATD_BIN"), "-config", cfg)
	cmd.Env = append(os.Environ(), "APP_ENV=dev")
	logf, _ := os.Create(filepath.Join(dir, "chatd.log"))
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}
	base := "http://127.0.0.1:18080"
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if r, err := http.Get(base + "/healthz"); err == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				return cmd, base, nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil, "", fmt.Errorf("chatd did not become healthy")
}

// seedConvs creates numConvs conversations, each with convMembers members drawn
// from uids, directly in the hot DB. A wider member set makes MSG_NEW fanout
// actually O(members) instead of O(1) (defect #5).
func seedConvs(dbPath string, uids []int64, convMembers, numConvs int) ([]int64, error) {
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	cids := make([]int64, 0, numConvs)
	now := time.Now().Unix()
	for c := 0; c < numConvs; c++ {
		lo := c * convMembers
		hi := lo + convMembers
		if hi > len(uids) {
			hi = len(uids)
		}
		members := uids[lo:hi]
		if len(members) == 0 {
			break
		}
		var cid int64
		if err := db.QueryRow(
			`INSERT INTO conv_conversations(ctype,title,created_by,created_at) VALUES(1,'stress',?,?) RETURNING cid`,
			members[0], now).Scan(&cid); err != nil {
			return nil, err
		}
		for _, u := range members {
			if _, err := db.Exec(`INSERT INTO conv_members(cid,uid,role,joined_at) VALUES(?,?,0,?)`, cid, u, now); err != nil {
				return nil, err
			}
		}
		if _, err := db.Exec(`INSERT INTO conv_seq(cid,next_seq) VALUES(?,1)`, cid); err != nil {
			return nil, err
		}
		cids = append(cids, cid)
	}
	return cids, nil
}

// ---------------------------------------------------------------------------
// WS dial
// ---------------------------------------------------------------------------

func dialWS(base, token string) (*websocket.Conn, error) {
	u := "ws" + base[len("http"):] + "/ws"
	hdr := http.Header{"Cookie": []string{"access_token=" + token}}
	conn, _, err := websocket.DefaultDialer.Dial(u, hdr)
	return conn, err
}

// ---------------------------------------------------------------------------
// Level runner
// ---------------------------------------------------------------------------

type levelResult struct {
	Conns       int
	RampMs      int64
	Handshakes  int
	RampErr     int64
	Ops         map[string]*opAgg
	ServerDrop  int64
	PeerSamples []time.Duration
	WindowSecs  float64
	RampTimeout bool
	// pull-mode statistics (§4.4 item 2). All zero in push-mode runs.
	PullMode        bool
	DownWatermarks  int64           // watermark frames/s denominator: DownWatermarks/WindowSecs
	DownMsgNew      int64           // msg_new frames (live fanout + catch-up replay)
	DownCtrl        int64           // every other downstream frame (ack/typing/presence/error/...)
	SyncRequests    int64           // uplink kind 8 pulls (client QPS numerator)
	DownRateLimited int64           // kind 13 RATE_LIMITED frames observed (S-6 adaptive evidence)
	SlowInjected    int             // S-4: connections pacing slowFactor× slower
	Convergence     []time.Duration // watermark latestSeq → lastSeq caught-up samples
	ConvergenceOpen int64           // watermarks still unsettled at window end
}

// perSec guards division for the per-second frame rates.
func perSec(n int64, secs float64) float64 {
	if secs <= 0 {
		return 0
	}
	return float64(n) / secs
}

type levelPlan struct {
	conns       int
	convMembers int
	convIDs     []int64
	pool        []*client
	base        string
	duration    time.Duration
	interval    time.Duration
	mix         *mixer
	rampBudget  time.Duration
	pullMode    bool
	// S-4 slow-consumer injection (REL): a deterministic subset of workers
	// paces their ops slowFactor× slower. Reads are untouched — a slow
	// consumer here is a connection whose send loop stalls, exercising server
	// backpressure, not a closed TCP window.
	slowRatio  float64
	slowFactor float64
}

func runLevel(p levelPlan) *levelResult {
	res := &levelResult{Conns: p.conns, Ops: map[string]*opAgg{}, WindowSecs: p.duration.Seconds()}
	for _, n := range opNames {
		res.Ops[n] = &opAgg{}
	}

	// ---- phase A: ramp (handshake only, tokens already registered) ----
	var (
		mu      sync.Mutex
		workers []*worker
		rampErr int64
	)
	rampStart := time.Now()
	sem := make(chan struct{}, 256)
	var rwg sync.WaitGroup
	for i := 0; i < p.conns; i++ {
		rwg.Add(1)
		go func(id int) {
			defer rwg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			tok := p.pool[id].tok
			convIdx := id / p.convMembers
			if convIdx >= len(p.convIDs) {
				convIdx = len(p.convIDs) - 1
			}
			conv := uint64(p.convIDs[convIdx])
			var conn *websocket.Conn
			var err error
			for attempt := 0; attempt < 4; attempt++ {
				conn, err = dialWS(p.base, tok)
				if err == nil {
					break
				}
				time.Sleep(150 * time.Millisecond)
			}
			if err != nil {
				atomic.AddInt64(&rampErr, 1)
				return
			}
			mu.Lock()
			w := newWorker(id, tok, p.pool[id].uid, conv, conn)
			if p.pullMode {
				w.pull = newPullTracker()
			}
			workers = append(workers, w)
			mu.Unlock()
		}(i)
	}
	done := make(chan struct{})
	go func() { rwg.Wait(); close(done) }()
	budget := p.rampBudget
	if budget <= 0 {
		budget = 120 * time.Second
	}
	select {
	case <-done:
	case <-time.After(budget):
		res.RampTimeout = true
	}
	res.RampMs = time.Since(rampStart).Milliseconds()
	res.Handshakes = len(workers)
	res.RampErr = atomic.LoadInt64(&rampErr)

	if res.RampTimeout || len(workers) == 0 {
		// still drain any late workers so we don't leak conns
		for _, w := range workers {
			w.conn.Close()
		}
		return res
	}

	// ---- phase B: steady-state mixed load ----
	peer := newPeerRecorder()
	deadline := time.Now().Add(p.duration)
	var swg sync.WaitGroup
	for _, w := range workers {
		slow, mult := slowFor(w.id, len(workers), p.slowRatio, p.slowFactor)
		w.slowMult = mult
		if slow {
			w.isSlow = true
			res.SlowInjected++
		}
		go w.readLoop(peer)
		swg.Add(1)
		go func(w *worker) {
			defer swg.Done()
			w.run(deadline, p.interval, p.mix, peer)
		}(w)
	}
	swg.Wait()
	// grace: let outstanding ACKs / peer frames settle before we tear down.
	time.Sleep(800 * time.Millisecond)

	// aggregate per worker
	for _, w := range workers {
		drop := w.serverDrop.Load()
		atomic.AddInt64(&res.ServerDrop, drop)
		for _, n := range opNames {
			att, ok, werr, samples := w.ops[n].snapshot()
			a := res.Ops[n]
			a.Attempted += att
			a.OK += ok
			a.WriteErr += werr
			a.Samples = append(a.Samples, samples...)
		}
	}
	// msg_send "OK" is ACK-based; keep it in Acked for the availability rule.
	res.Ops["msg_send"].Acked = res.Ops["msg_send"].OK
	res.PeerSamples = peer.snapshot()
	res.WindowSecs = p.duration.Seconds()
	res.PullMode = p.pullMode
	if p.pullMode {
		var conv [][]time.Duration
		for _, w := range workers {
			res.DownWatermarks += w.downWM.Load()
			res.DownMsgNew += w.downMsgNew.Load()
			res.DownCtrl += w.downCtrl.Load()
			res.SyncRequests += w.syncQPS.Load()
			res.DownRateLimited += w.downRateLimited.Load()
			if w.pull != nil {
				conv = append(conv, w.pull.convergence())
			}
		}
		res.Convergence = mergeSamples(conv)
		for _, w := range workers {
			if w.pull != nil {
				res.ConvergenceOpen += int64(w.pull.outstanding())
			}
		}
	}

	for _, w := range workers {
		w.conn.Close()
	}
	return res
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	var (
		mode       = flag.String("mode", "both", "load mode: http | ws | both | mixed (mixed = WS with MSG_SEND/TYPING/RECEIPT/PRESENCE)")
		conns      = flag.Int("conns", 64, "concurrent WS connections (workers)")
		secs       = flag.Int("duration", 10, "steady-state measurement window in seconds, per level")
		interval   = flag.Duration("interval", 100*time.Millisecond, "per-worker pacing between ops (raise to stay under per-conn rate limits)")
		convMem    = flag.Int("conv-members", 2, "members per seeded conversation (fanout width); each sender is a member of its conv")
		skipBoot   = flag.Bool("no-boot", false, "use an already-running chatd at -base (set CHATD_DB to its hot.db)")
		base       = flag.String("base", "http://127.0.0.1:18080", "chatd base URL (used with -no-boot)")
		out        = flag.String("out", "result.json", "result JSON file")
		assert     = flag.Bool("assert", false, "exit 1 when thresholds are missed")
		ladder     = flag.String("ladder", "", "auto-scale: 'start=N,max=M'; doubles conns until availability < 70%")
		noRL       = flag.Bool("no-ratelimit", false, "DISABLE rate limiting (raw-ceiling mode; the run is flagged)")
		metrics    = flag.String("metrics-url", "", "optional admin REST /api/v1/metrics URL to scrape server-side counters")
		sendR      = flag.Int("send-ratio", 70, "mixed mode: % of MSG_SEND")
		typingR    = flag.Int("typing-ratio", 15, "mixed mode: % of TYPING")
		receiptR   = flag.Int("receipt-ratio", 10, "mixed mode: % of RECEIPT")
		presR      = flag.Int("presence-ratio", 5, "mixed mode: % of PRESENCE")
		pullMode   = flag.Bool("pull-mode", false, "enable pull-mode client behaviour: handle kind 14 WATERMARK, reply with 8 SYNC pulls (spec pull-mode-spec §4.4); default false = push-mode, byte-identical to pre-§4.4 behaviour")
		univPull   = flag.Bool("universal-pull", false, "REL/lts-1.4: universal-pull stress mode — ALL sessions watermark-driven (kind 14 → 8 SYNC, reuses pullTracker); implies --pull-mode. Reports watermark/s, SYNC QPS, convergence latency and the 3-way downstream frame split (spec universal-pull-spec §4)")
		slowRatio  = flag.Float64("slow-ratio", 0, "S-4: fraction of connections injected as slow consumers (0..1), e.g. 0.05 = 50/1000")
		slowFactor = flag.Float64("slow-factor", 5, "S-4: pacing multiplier applied to slow-consumer connections (>=1)")
		adaptCmp   = flag.String("adaptive-compare", "", "S-6: compare two recorded --universal-pull result JSONs (static.json,adaptive.json), print batch/interval evidence + rate-limit hits, then exit (no load is generated)")
	)
	flag.Parse()

	// S-6 offline comparison: no load, just merge the two recorded runs.
	if *adaptCmp != "" {
		runAdaptiveCompare(*adaptCmp)
		return
	}

	// universal-pull is watermark-driven end to end, so it implies pull-mode.
	if *univPull {
		*pullMode = true
	}

	rateLimit := !*noRL

	if *mode == "restart-storm" {
		runRestartStorm(*base, *conns, *convMem, *secs, *interval, rateLimit, *skipBoot, *out)
		return
	}

	mkMixer := func() *mixer {
		if *mode == "mixed" || *mode == "both" || *mode == "ws" {
			if *mode == "mixed" {
				return newMixer(*sendR, *typingR, *receiptR, *presR)
			}
		}
		return newMixer(100, 0, 0, 0) // pure msg_send
	}

	// ---- decide pool size & conv plan ----
	convMembers := *convMem
	if convMembers < 2 {
		convMembers = 2
	}

	if *ladder != "" {
		start, maxConns := parseLadder(*ladder)
		numConvs := (maxConns + convMembers - 1) / convMembers
		poolSize := numConvs * convMembers
		runLadder(*base, start, maxConns, *secs, *interval, convMembers, poolSize, rateLimit, mkMixer(), *out, *metrics)
		return
	}

	numConvs := (*conns + convMembers - 1) / convMembers
	poolSize := numConvs * convMembers
	if poolSize < *conns {
		poolSize = *conns
	}

	dir, _ := os.MkdirTemp("", "ocstress-")
	defer os.RemoveAll(dir)
	var chatd *exec.Cmd
	dbPath := filepath.Join(dir, "hot.db")
	if !*skipBoot {
		var err error
		chatd, *base, err = runChatd(dir, rateLimit)
		if err != nil {
			fmt.Println("boot:", err)
			os.Exit(2)
		}
		defer func() {
			if chatd != nil && chatd.Process != nil {
				chatd.Process.Signal(os.Interrupt)
				chatd.Process.Wait()
			}
		}()
	} else {
		dbPath = os.Getenv("CHATD_DB")
	}

	fmt.Printf("== OpenDiscord stress: %s mode=%s conns=%d duration=%ds conv_members=%d rate_limit=%s ==\n",
		*base, *mode, *conns, *secs, convMembers, rlLabel(rateLimit))

	// phase 0: pre-register the pool (once)
	t0 := time.Now()
	pool, perr := preregister(*base, poolSize, 32)
	preregDur := time.Since(t0)
	if perr != nil {
		fmt.Printf("pre-register: %v (proceeding with %d ok)\n", perr, countNonNil(pool))
		pool = compact(pool)
		if len(pool) < poolSize {
			poolSize = len(pool)
			numConvs = (poolSize + convMembers - 1) / convMembers
		}
	}
	uids := make([]int64, len(pool))
	for i, c := range pool {
		uids[i] = c.uid
	}
	fmt.Printf("pre-registered users: %d (in %s, concurrency 32)\n", len(pool), preregDur.Round(time.Millisecond))

	// seed conversations (once)
	convIDs, err := seedConvs(dbPath, uids, convMembers, numConvs)
	if err != nil {
		fmt.Println("seed convs:", err)
		os.Exit(2)
	}
	fmt.Printf("seeded conversations: %d (members/conv=%d)\n", len(convIDs), convMembers)

	// optional HTTP phase
	if *mode == "http" || *mode == "both" {
		runHTTPPhase(*base, pool, *conns, *secs)
	}

	var res *levelResult
	if *mode != "http" {
		res = runLevel(levelPlan{
			conns: *conns, convMembers: convMembers, convIDs: convIDs, pool: pool,
			base: *base, duration: time.Duration(*secs) * time.Second,
			interval: *interval, mix: mkMixer(), rampBudget: 120 * time.Second,
			pullMode:  *pullMode,
			slowRatio: *slowRatio, slowFactor: *slowFactor,
		})
		fmt.Printf("\nws handshakes: %d / %d\n", res.Handshakes, *conns)
		printLevelDetail(res, convMembers, rateLimit)
		printPullStats(res)
		writeJSON(*out, false, []*levelResult{res}, len(pool), rateLimit, convMembers)
		fmt.Println("wrote", *out)

		if *metrics != "" {
			scrapeMetrics(*metrics)
		}
		printCredibility(len(pool), []*levelResult{res}, rateLimit)

		if *assert {
			ms := res.Ops["msg_send"]
			if len(ms.Samples) > 0 && percentile(ms.Samples, 0.99) > 500*time.Millisecond {
				fmt.Println("ASSERT FAIL: msg_send p99 > 500ms")
				os.Exit(1)
			}
			if availability(ms.Acked, ms.Attempted) < 99 && ms.Attempted > 0 {
				fmt.Println("ASSERT FAIL: availability < 99%")
				os.Exit(1)
			}
		}
	}
}

// osExit is an indirection for os.Exit so testable files (storm.go) can call
// it without making the whole package exit during tests.
func osExit(code int) { os.Exit(code) }

// dbPathFromEnv returns the hot.db path for -no-boot runs (empty otherwise).
func dbPathFromEnv() string { return os.Getenv("CHATD_DB") }

// writeJSONAny writes an arbitrary JSON document (used by the S-5 subcommand;
// the ladder/mixed modes use writeJSON with their level-row schema).
func writeJSONAny(path string, v any) {
	blob, _ := json.MarshalIndent(v, "", "  ")
	os.WriteFile(path, blob, 0o644)
}

func parseLadder(s string) (start, maxConns int) {
	start, maxConns = 256, 1<<20
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if _, err := fmt.Sscanf(part, "start=%d", &start); err == nil {
			continue
		}
		fmt.Sscanf(part, "max=%d", &maxConns)
	}
	return
}

// runLadder boots one chatd, pre-registers the whole pool once, seeds the max
// conv set once, then doubles concurrency until availability < 70% or ramp
// fails. Identity/conversation tables stay fixed across levels (defect #8).
func runLadder(base string, start, maxConns, secs int, interval time.Duration, convMembers, poolSize int, rateLimit bool, mix *mixer, out, metricsURL string) {
	dir, _ := os.MkdirTemp("", "ocladder-")
	defer os.RemoveAll(dir)
	chatd, baseURL, err := runChatd(dir, rateLimit)
	if err != nil {
		fmt.Println("boot:", err)
		os.Exit(2)
	}
	defer func() {
		if chatd != nil && chatd.Process != nil {
			chatd.Process.Signal(os.Interrupt)
			chatd.Process.Wait()
		}
	}()
	base = baseURL

	fmt.Printf("== OpenDiscord stress ladder: start=%d max=%d duration=%ds conv_members=%d rate_limit=%s ==\n",
		start, maxConns, secs, convMembers, rlLabel(rateLimit))

	t0 := time.Now()
	pool, perr := preregister(base, poolSize, 32)
	if perr != nil {
		fmt.Printf("pre-register: %v\n", perr)
		pool = compact(pool)
	}
	uids := make([]int64, len(pool))
	for i, c := range pool {
		uids[i] = c.uid
	}
	numConvsMax := (poolSize + convMembers - 1) / convMembers
	fmt.Printf("pre-registered users: %d (in %s)\n", len(pool), time.Since(t0).Round(time.Millisecond))

	convIDs, err := seedConvs(filepath.Join(dir, "hot.db"), uids, convMembers, numConvsMax)
	if err != nil {
		fmt.Println("seed convs:", err)
		os.Exit(2)
	}
	fmt.Printf("seeded conversations: %d (members/conv=%d)\n\n", len(convIDs), convMembers)

	fmt.Printf("%8s %10s %10s %8s %9s %8s %8s %7s %8s %-22s %10s %6s\n",
		"conns", "sends", "acks", "avail%", "write_err", "srv_drop", "ramp_ms", "samples", "peer_p95", "send->ack p50/p95/p99", "rpm", "verdict")
	var rows []*levelResult
	for n := start; n <= maxConns; n *= 2 {
		convsNeeded := (n + convMembers - 1) / convMembers
		useConvs := convIDs
		if convsNeeded < len(useConvs) {
			useConvs = convIDs[:convsNeeded]
		}
		res := runLevel(levelPlan{
			conns: n, convMembers: convMembers, convIDs: useConvs,
			pool: pool[:n], base: base, duration: time.Duration(secs) * time.Second,
			interval: interval, mix: mix, rampBudget: rampBudgetFor(n),
		})
		rows = append(rows, res)
		ms := res.Ops["msg_send"]
		avail := availability(ms.Acked, ms.Attempted)
		rpm := float64(ms.Attempted) / res.WindowSecs * 60
		peerP95 := "-"
		if len(res.PeerSamples) > 0 {
			peerP95 = fmtDur(percentile(res.PeerSamples, 0.95))
		}
		verdict := "PASS"
		if res.RampTimeout {
			verdict = "RAMP_TIMEOUT"
		} else if res.Handshakes < n {
			verdict = "SATURATED"
		} else if avail < 70 {
			verdict = "FAIL"
		}
		fmt.Printf("%8d %10d %10d %7.1f%% %9d %8d %8d %7d %8s %-22s %10.0f %6s\n",
			n, ms.Attempted, ms.Acked, avail, totalWriteErr(res), res.ServerDrop,
			res.RampMs, len(ms.Samples), peerP95, fmtLat3(ms.Samples), rpm, verdict)
		writeJSON(out, true, rows, len(pool), rateLimit, convMembers)
		if verdict != "PASS" {
			break
		}
	}

	writeJSON(out, true, rows, len(pool), rateLimit, convMembers)
	fmt.Println("wrote", out)
	if metricsURL != "" {
		scrapeMetrics(metricsURL)
	}
	printCredibility(len(pool), rows, rateLimit)
}

func rampBudgetFor(n int) time.Duration {
	if n > 2048 {
		return 180 * time.Second
	}
	return 90 * time.Second
}

func totalWriteErr(r *levelResult) int64 {
	var t int64
	for _, a := range r.Ops {
		t += a.WriteErr
	}
	return t
}

func countNonNil(pool []*client) int {
	n := 0
	for _, c := range pool {
		if c != nil {
			n++
		}
	}
	return n
}

func compact(pool []*client) []*client {
	out := pool[:0]
	for _, c := range pool {
		if c != nil {
			out = append(out, c)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// reporting (level detail + JSON + credibility)
// ---------------------------------------------------------------------------

func printLevelDetail(r *levelResult, convMembers int, rateLimit bool) {
	fmt.Printf("\n=== level: conns=%d  members/conv=%d  rate_limit=%s ===\n",
		r.Conns, convMembers, rlLabel(rateLimit))
	fmt.Printf("ramp: %d ms, handshakes=%d, ramp_err=%d, ramp_timeout=%v\n",
		r.RampMs, r.Handshakes, r.RampErr, r.RampTimeout)
	fmt.Printf("%-10s %10s %10s %8s %10s %9s %8s  %-24s\n",
		"op", "attempted", "acked/ok", "ok%", "write_err", "srv_drop", "samples", "p50/p95/p99")
	for _, n := range opNames {
		a := r.Ops[n]
		num := a.OK
		if n == "msg_send" {
			num = a.Acked
		}
		fmt.Printf("%-10s %10d %10d %7.1f%% %10d %9d %8d  %-24s\n",
			n, a.Attempted, num, a.successRate(), a.WriteErr, a.ServerDrop,
			len(a.Samples), fmtLat3(a.Samples))
	}
	ms := r.Ops["msg_send"]
	avail := availability(ms.Acked, ms.Attempted)
	rpm := float64(ms.Attempted) / r.WindowSecs * 60
	peer := "-"
	if len(r.PeerSamples) > 0 {
		peer = fmt.Sprintf("samples=%d p50/p95/p99=%s", len(r.PeerSamples), fmtLat3(r.PeerSamples))
	}
	fmt.Printf("availability(ACK/send)= %.1f%%   rpm= %.0f   write_err(total)=%d   server_drop=%d\n",
		avail, rpm, totalWriteErr(r), r.ServerDrop)
	fmt.Printf("send->peer_recv: %s\n", peer)
	if r.SlowInjected > 0 {
		fmt.Printf("S-4 slow-consumer injection: %d/%d conns slowed (read loops untouched)\n",
			r.SlowInjected, r.Conns)
	}
	var notes []string
	if len(ms.Samples) < 1000 {
		notes = append(notes, "insufficient samples (<1000): p95/p99 unreliable")
	}
	if r.RampTimeout {
		notes = append(notes, "RAMP TIMEOUT")
	}
	if r.Handshakes < r.Conns {
		notes = append(notes, fmt.Sprintf("%d/%d handshakes", r.Handshakes, r.Conns))
	}
	if len(notes) == 0 {
		notes = append(notes, "ok")
	}
	fmt.Printf("notes: %s\n", strings.Join(notes, "; "))
}

type jsonRow struct {
	Conns        int               `json:"conns"`
	Members      int               `json:"members_per_conv"`
	RampMs       int64             `json:"ramp_ms"`
	Handshakes   int               `json:"handshakes"`
	RampErr      int64             `json:"ramp_err"`
	RampTimeout  bool              `json:"ramp_timeout"`
	MsgAttempted int64             `json:"msg_send_attempted"`
	MsgAcks      int64             `json:"msg_send_acks"`
	AvailPct     float64           `json:"availability_pct"`
	WriteErr     int64             `json:"write_err"`
	ServerDrop   int64             `json:"server_drop"`
	Samples      int               `json:"samples"`
	P50ms        float64           `json:"p50_ms"`
	P95ms        float64           `json:"p95_ms"`
	P99ms        float64           `json:"p99_ms"`
	PeerSamples  int               `json:"peer_recv_samples"`
	PeerP95ms    float64           `json:"peer_recv_p95_ms"`
	RPM          float64           `json:"rpm"`
	Ops          map[string]jsonOp `json:"ops"`
}

type jsonOp struct {
	Attempted int64   `json:"attempted"`
	OK        int64   `json:"ok"`
	SuccessPc float64 `json:"success_pct"`
	WriteErr  int64   `json:"write_err"`
	Samples   int     `json:"samples"`
	P50ms     float64 `json:"p50_ms"`
	P95ms     float64 `json:"p95_ms"`
	P99ms     float64 `json:"p99_ms"`
}

func writeJSON(path string, ladder bool, rows []*levelResult, preregUsers int, rateLimit bool, convMembers int) {
	outRows := make([]jsonRow, 0, len(rows))
	for _, r := range rows {
		ms := r.Ops["msg_send"]
		jr := jsonRow{
			Conns: r.Conns, Members: convMembers, RampMs: r.RampMs,
			Handshakes: r.Handshakes, RampErr: r.RampErr, RampTimeout: r.RampTimeout,
			MsgAttempted: ms.Attempted, MsgAcks: ms.Acked,
			AvailPct: availability(ms.Acked, ms.Attempted),
			WriteErr: totalWriteErr(r), ServerDrop: r.ServerDrop,
			Samples: len(ms.Samples),
			P50ms:   msDur(ms.Samples, 0.50), P95ms: msDur(ms.Samples, 0.95), P99ms: msDur(ms.Samples, 0.99),
			PeerSamples: len(r.PeerSamples), PeerP95ms: msDur(r.PeerSamples, 0.95),
			RPM: float64(ms.Attempted) / r.WindowSecs * 60,
			Ops: map[string]jsonOp{},
		}
		for _, n := range opNames {
			a := r.Ops[n]
			num := a.OK
			if n == "msg_send" {
				num = a.Acked
			}
			jr.Ops[n] = jsonOp{
				Attempted: a.Attempted, OK: num, SuccessPc: a.successRate(),
				WriteErr: a.WriteErr, Samples: len(a.Samples),
				P50ms: msDur(a.Samples, 0.50), P95ms: msDur(a.Samples, 0.95), P99ms: msDur(a.Samples, 0.99),
			}
		}
		outRows = append(outRows, jr)
	}
	doc := map[string]any{
		"ladder": ladder, "rate_limit_enabled": rateLimit,
		"pre_registered_users": preregUsers, "conv_members": convMembers,
		"generated_at": time.Now().UTC().Format(time.RFC3339), "levels": outRows,
	}
	// REL/universal-pull: persist the pull-mode measurement block so the S-6
	// static-vs-adaptive comparison (-adaptive-compare) can replay it.
	if len(rows) > 0 && rows[0].PullMode {
		pullRows := make([]pullJSONRow, 0, len(rows))
		for _, r := range rows {
			pullRows = append(pullRows, pullRowFromLevel(r))
		}
		doc["pull_levels"] = pullRows
	}
	blob, _ := json.MarshalIndent(doc, "", "  ")
	os.WriteFile(path, blob, 0o644)
}

func msDur(samples []time.Duration, p float64) float64 {
	return float64(percentile(samples, p).Microseconds()) / 1000.0
}

func printCredibility(preregUsers int, rows []*levelResult, rateLimit bool) {
	fmt.Println("\n== 可信度声明 / credibility statement ==")
	fmt.Printf("pre-registered users: %d\n", preregUsers)
	if len(rows) > 0 {
		r := rows[len(rows)-1]
		ms := r.Ops["msg_send"]
		fmt.Printf("ws handshakes (last level): %d / %d\n", r.Handshakes, r.Conns)
		fmt.Printf("rate limit: %s\n", rlLabel(rateLimit))
		ok := len(ms.Samples) >= 1000
		fmt.Printf("msg_send samples (last level): %d -> %s\n", len(ms.Samples), sampleVerdict(ok))
		if r.RampTimeout {
			fmt.Println("ramp: TIMEOUT (level saturated before measurement)")
		} else {
			fmt.Println("ramp: within budget")
		}
	}
	fmt.Println("availability denominator = attempted msg_send sends; acked = MSG_ACK status 0.")
	fmt.Println("send->ack is server round trip on one connection; send->peer_recv needs >=2 members (conv-members).")
}

func sampleVerdict(ok bool) string {
	if ok {
		return "sufficient"
	}
	return "INSUFFICIENT"
}

func rlLabel(rateLimit bool) string {
	if rateLimit {
		return "ENABLED (production parity)"
	}
	return "DISABLED (raw ceiling — NOT production)"
}

// ---------------------------------------------------------------------------
// optional metrics scrape
// ---------------------------------------------------------------------------

func scrapeMetrics(url string) {
	fmt.Printf("\n== server-side metrics (%s) ==\n", url)
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		fmt.Printf("metrics unavailable: %v\n", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	lines := strings.Split(string(body), "\n")
	shown := 0
	for _, ln := range lines {
		low := strings.ToLower(ln)
		if strings.HasPrefix(ln, "#") || strings.TrimSpace(ln) == "" {
			continue
		}
		if strings.Contains(low, "drop") || strings.Contains(low, "nack") ||
			strings.Contains(low, "fanout") || strings.Contains(low, "egress") {
			fmt.Println("  " + ln)
			shown++
		}
	}
	if shown == 0 {
		fmt.Println("  (no drop/nack/fanout/egress counters exposed by this endpoint)")
	}
}

// ---------------------------------------------------------------------------
// lightweight HTTP phase (kept from the original tool; not the focus)
// ---------------------------------------------------------------------------

func runHTTPPhase(base string, pool []*client, conns, secs int) {
	fmt.Println("== phase: HTTP API ==")
	aggs := map[string]*opAgg{"http.healthz": {}, "http.guest": {}, "http.media": {}}
	var mu sync.Mutex
	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	var wg sync.WaitGroup
	for i := 0; i < conns && i < len(pool); i++ {
		wg.Add(1)
		go func(c *client) {
			defer wg.Done()
			for time.Now().Before(deadline) {
				t0 := time.Now()
				r, err := c.hc.Get(base + "/healthz")
				d := time.Since(t0)
				mu.Lock()
				st := aggs["http.healthz"]
				st.Attempted++
				if err != nil {
					st.WriteErr++
				} else {
					io.Copy(io.Discard, r.Body)
					r.Body.Close()
					st.OK++
					st.Samples = append(st.Samples, d)
				}
				mu.Unlock()
				time.Sleep(100 * time.Millisecond)
			}
		}(pool[i])
	}
	wg.Wait()
	for _, n := range []string{"http.healthz", "http.guest", "http.media"} {
		a := aggs[n]
		fmt.Printf("  %-14s attempted=%d ok=%d p95=%s\n", n, a.Attempted, a.OK, fmtDur(percentile(a.Samples, 0.95)))
	}
}
