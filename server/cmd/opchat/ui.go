package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// uiState is the full mutable UI model. Render is pure over it; the input
// goroutine and the refresh goroutine both mutate it under mu (performance
// red line: key input never blocks on rendering).
type uiState struct {
	mu        sync.Mutex
	t         locale
	lang      string
	server    string
	width     int
	tab       string
	connected bool
	sound     bool

	// overview
	overview      overview
	onlineShown   float64
	msgsShown     float64
	archiveStatus string
	refreshIn     int
	refreshing    bool
	spinnerIdx    int

	// users tab
	users        []userRow
	userCursor   int
	selectedUser int64
	userDetail   map[string]any

	// reports
	reports      []reportRow
	reportCursor int

	// broadcast
	broadcastTitle string
	broadcastText  string
	broadcastReach int64

	// flags
	flags      []flagRow
	flagCursor int

	// archive
	archiveDryRows int64

	// audit
	audit       []auditRow
	auditCursor int
	auditExpand bool
	auditSince  int64

	// toast
	toast    string
	toastErr bool
	toastAt  time.Time

	fadeStep int
	help     bool
	done     bool

	lastBells int
}

func newUIState(t locale, lang, server string) *uiState {
	return &uiState{t: t, lang: lang, server: server, width: 80,
		broadcastReach: -1, archiveDryRows: -1, archiveStatus: "-"}
}

// outW is the package-level stdout writer (swappable in tests).
var outW io.Writer = os.Stdout

// lastBells records how many bells the last toast implies (1 ok / 2 error /
// 3 urgent like a new report arriving) — testable even when sound is off.
func (s *uiState) showToast(msg string, isErr bool) {
	s.toast = msg
	s.toastErr = isErr
	s.toastAt = time.Now()
	if isErr {
		s.lastBells = 2
	} else {
		s.lastBells = 1
	}
	if s.sound {
		io.WriteString(outW, bell(s.lastBells))
	}
}

func (s *uiState) toastIfFresh() {
	if s.toast != "" && time.Since(s.toastAt) > 2*time.Second {
		s.toast = ""
	}
}

// app wires the UI state with the HTTP client and terminal I/O.
type app struct {
	st     *uiState
	c      *client
	in     *bufio.Reader
	out    io.Writer
	events chan string // internal one-shot events (refresh tick)
}

func newApp(c *client, st *uiState, in io.Reader, out io.Writer) *app {
	return &app{st: st, c: c, in: bufio.NewReader(in), out: out,
		events: make(chan string, 8)}
}

// promptLine reads one line from the injected/real stdin.
func (a *app) promptLine(prompt string) string {
	fmt.Fprint(a.out, prompt)
	line, err := a.in.ReadString('\n')
	if err != nil && line == "" {
		return ""
	}
	return strings.TrimSpace(line)
}

// confirm prints what will happen and requires explicit y. Default N.
func (a *app) confirm(format string, args ...any) bool {
	fmt.Fprintf(a.out, " %s\n %s %s: ", fmt.Sprintf(format, args...),
		a.st.t.L(kConfirmHint), a.st.t.L(kYesNo))
	line := a.promptLine("")
	return strings.EqualFold(strings.TrimSpace(line), "y")
}

// apiErrorLine formats an API error with code + reason + request_id + a
// next-step suggestion (actionable-error rule).
func apiErrorLine(err error, t locale) string {
	se, ok := err.(*statusError)
	if !ok {
		return err.Error()
	}
	sug := t.L(kLoginHint)
	switch {
	case se.Status == 401 || se.Status == 403:
		sug = t.L(kLoginHint)
	case se.Status == 404:
		sug = "check the server URL and API version"
	case se.Status >= 500:
		sug = "retry later; if it persists check server logs (request_id below)"
	default:
		sug = "fix the input and retry"
	}
	return fmt.Sprintf("%s %d %s (request_id=%s) — %s %s",
		t.L(kAPIErr), se.Status, se.Reason, se.RequestID, t.L(kSuggestion), sug)
}

// loginWizard implements the guided login: URL → user → password → TOTP.
// Uses the same /api/v1/login exchange and credential file as adminctl.
func loginWizard(a *app, base string) bool {
	t := a.st.t
	fmt.Fprintf(a.out, "── %s ──\n", t.L(kLoginWizard))
	if base == "" {
		base = a.promptLine(t.L(kLoginURL))
		if base == "" {
			base = "http://127.0.0.1:7780"
		}
	}
	user := a.promptLine(t.L(kLoginUser))
	if user == "" {
		a.st.showToast(t.L(kLoginFail)+": username empty", true)
		return false
	}
	pass := a.promptLine(t.L(kLoginPass))
	totp := a.promptLine(t.L(kLoginTOTP))
	if err := login(base, user, pass, totp); err != nil {
		fmt.Fprintf(a.out, "%s\n", apiErrorLine(err, t))
		a.st.showToast(t.L(kLoginFail), true)
		return false
	}
	c := newClient(base, "", "")
	if creds, err := loadCredentials(); err == nil {
		c = newClient(base, creds.Token, creds.CSRF)
	}
	a.c = c
	a.st.server = base
	a.st.connected = true
	a.st.showToast(t.L(kLoginOK), false)
	return true
}

// run is the main loop: input goroutine + refresh goroutine + render.
// In headless/test mode (out is not a TTY) raw ANSI home/clear is skipped
// by the caller driving renderFrame directly; here we full-redraw.
func (a *app) run(refreshEvery time.Duration, stop <-chan struct{}) {
	st := a.st
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	refreshTick := time.NewTicker(refreshEvery)
	defer refreshTick.Stop()
	count := int(refreshEvery / (100 * time.Millisecond))
	lastRender := ""
	for !st.done {
		select {
		case <-stop:
			st.done = true
		case <-refreshTick.C:
			go a.refresh()
		case <-ticker.C:
			st.mu.Lock()
			st.refreshIn = refreshCountdown(st.refreshIn, count)
			if st.refreshing {
				st.spinnerIdx++
			}
			st.toastIfFresh()
			if st.fadeStep >= 0 {
				st.fadeStep++
				if st.fadeStep >= len(fadeInFrames) {
					st.fadeStep = -1
				}
			}
			st.mu.Unlock()
			// renderFrame takes the lock itself; calling it while holding it
			// would deadlock (and did, once the renderer was made safe).
			frame := renderFrame(st)
			if frame != lastRender {
				fmt.Fprint(a.out, "\x1b[H\x1b[2J"+frame)
				lastRender = frame
			}
		default:
			// non-blocking single-key read
			if a.in.Buffered() > 0 {
				line, _ := a.in.ReadString('\n')
				a.handleKeys(strings.TrimRight(line, "\r\n"))
				frame := renderFrame(st)
				fmt.Fprint(a.out, "\x1b[H\x1b[2J"+frame)
				lastRender = frame
				continue
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// handleKeys processes a (possibly multi-char) key sequence. ESC handling:
// any Esc/cancel path never discards uncommitted edits (broadcast text etc.
// stay in state).
func (a *app) handleKeys(k string) {
	st := a.st
	st.mu.Lock()
	defer st.mu.Unlock()
	switch k {
	case "q", "\x1b\x1b":
		st.done = true
	case "\x03": // ctrl+c
		st.done = true
	case "?":
		st.help = !st.help
	case "s":
		st.sound = !st.sound
		st.showToast(map[bool]string{true: st.t.L(kSoundOn), false: st.t.L(kSoundOff)}[st.sound], false)
	case "1", "2", "3", "4", "5", "6", "7":
		if st.tab != k {
			st.fadeStep = 0
		}
		st.tab = k
	case "r":
		go a.refresh()
	}
	if st.help {
		return
	}
	switch st.tab {
	case "1":
	case "2":
		a.handleUsersKeys(k)
	case "3":
		a.handleReportsKeys(k)
	case "4":
		a.handleBroadcastKeys(k)
	case "5":
		a.handleFlagsKeys(k)
	case "6":
		a.handleArchiveKeys(k)
	case "7":
		a.handleAuditKeys(k)
	}
}

// --- per-tab key handlers (network calls made inline; tests inject a fake
// server so these are integration-tested) ---

func (a *app) handleUsersKeys(k string) {
	st := a.st
	switch k {
	case "g": // search flow
		q := a.promptLine(st.t.L(kSearchPrompt))
		rows, err := listUsers(a.c, q, 20)
		if err != nil {
			fmt.Fprintf(a.out, "%s\n", apiErrorLine(err, st.t))
			st.showToast(st.t.L(kToastErr), true)
			return
		}
		st.users = rows
		st.userCursor = 0
		st.selectedUser = 0
	case "\x1b[A": // up
		if st.userCursor > 0 {
			st.userCursor--
		}
	case "\x1b[B", "j": // down
		if st.userCursor < len(st.users)-1 {
			st.userCursor++
		}
	case "\r", "\n", "Enter": // detail
		if st.userCursor < len(st.users) {
			u := st.users[st.userCursor]
			st.selectedUser = u.ID
			m, err := getUser(a.c, u.ID)
			if err != nil {
				fmt.Fprintf(a.out, "%s\n", apiErrorLine(err, st.t))
				return
			}
			st.userDetail = m
		}
	case "b": // ban (destructive: shows API + params, confirms)
		if st.selectedUser == 0 && st.userCursor < len(st.users) {
			st.selectedUser = st.users[st.userCursor].ID
		}
		if st.selectedUser == 0 {
			return
		}
		lv := a.promptLine(st.t.L(kBanLevel))
		dys := a.promptLine(st.t.L(kBanDays))
		rs := a.promptLine(st.t.L(kBanReason))
		level, _ := strconv.Atoi(orDefault(lv, "1"))
		days, _ := strconv.Atoi(orDefault(dys, "0"))
		api := fmt.Sprintf("POST /api/v1/users/%d/ban {level:%d, days:%d, reason:%q}", st.selectedUser, level, days, rs)
		if a.confirm("%s %s", st.t.L(kWillCall), api) {
			if err := banUser(a.c, st.selectedUser, level, days, rs); err != nil {
				fmt.Fprintf(a.out, "%s\n", apiErrorLine(err, st.t))
				st.showToast(st.t.L(kToastErr), true)
			} else {
				st.showToast(fmt.Sprintf("%s uid=%d", st.t.L(kOpBan), st.selectedUser), false)
			}
		} else {
			st.showToast(st.t.L(kCancelled), false)
		}
	case "u": // unban
		if st.selectedUser == 0 {
			return
		}
		rs := a.promptLine(st.t.L(kBanReason))
		api := fmt.Sprintf("POST /api/v1/users/%d/unban {reason:%q}", st.selectedUser, rs)
		if a.confirm("%s %s", st.t.L(kWillCall), api) {
			if err := unbanUser(a.c, st.selectedUser, rs); err != nil {
				st.showToast(st.t.L(kToastErr), true)
			} else {
				st.showToast(fmt.Sprintf("%s uid=%d", st.t.L(kOpUnban), st.selectedUser), false)
			}
		}
	case "k": // kick
		if st.selectedUser == 0 {
			return
		}
		api := fmt.Sprintf("POST /api/v1/users/%d/kick", st.selectedUser)
		if a.confirm("%s %s", st.t.L(kWillCall), api) {
			if err := kickUser(a.c, st.selectedUser, "opchat"); err != nil {
				st.showToast(st.t.L(kToastErr), true)
			} else {
				st.showToast(fmt.Sprintf("%s uid=%d", st.t.L(kOpKick), st.selectedUser), false)
			}
		}
	case "\x1b", "Esc":
		st.userDetail = nil
		st.selectedUser = 0
	}
}

func (a *app) handleReportsKeys(k string) {
	st := a.st
	switch k {
	case "g":
		sf := a.promptLine(st.t.L(kReportsFilter))
		status, _ := strconv.Atoi(orDefault(sf, "-1"))
		rows, err := listReports(a.c, status, 20)
		if err != nil {
			fmt.Fprintf(a.out, "%s\n", apiErrorLine(err, st.t))
			return
		}
		st.reports = rows
	case "\x1b[A":
		if st.reportCursor > 0 {
			st.reportCursor--
		}
	case "\x1b[B", "j":
		if st.reportCursor < len(st.reports)-1 {
			st.reportCursor++
		}
	case "f", "\r", "\n", "Enter":
		if st.reportCursor >= len(st.reports) {
			return
		}
		id := st.reports[st.reportCursor].ID
		action := a.promptLine(st.t.L(kReportsAction))
		if action == "" {
			return
		}
		note := a.promptLine(st.t.L(kReportsNote))
		api := fmt.Sprintf("POST /api/v1/reports/%d/handle {action:%q, note:%q}", id, action, note)
		if a.confirm("%s %s", st.t.L(kWillCall), api) {
			if err := handleReport(a.c, id, action, note); err != nil {
				fmt.Fprintf(a.out, "%s\n", apiErrorLine(err, st.t))
				st.showToast(st.t.L(kToastErr), true)
			} else {
				st.showToast(fmt.Sprintf("report %d %s", id, action), false)
			}
		} else {
			st.showToast(st.t.L(kCancelled), false)
		}
	}
}

func (a *app) handleBroadcastKeys(k string) {
	st := a.st
	switch k {
	case "e": // edit
		st.broadcastTitle = a.promptLine(st.t.L(kBroadcastTitle))
		st.broadcastText = a.promptLine(st.t.L(kBroadcastText))
	case "p": // preview (dry_run first)
		if st.broadcastText == "" {
			st.broadcastText = a.promptLine(st.t.L(kBroadcastText))
			if st.broadcastText == "" {
				return
			}
		}
		raw, err := broadcast(a.c, st.broadcastText, true)
		if err != nil {
			fmt.Fprintf(a.out, "%s\n", apiErrorLine(err, st.t))
			st.showToast(st.t.L(kToastErr), true)
			return
		}
		var pr struct {
			Reach  int64  `json:"reach"`
			Recips int64  `json:"recipients"`
			Count  int64  `json:"count"`
			DryRun bool   `json:"dry_run"`
			Note   string `json:"note"`
		}
		_ = json.Unmarshal(raw, &pr)
		st.broadcastReach = firstNonZero(pr.Reach, pr.Recips, pr.Count)
		st.showToast(fmt.Sprintf("%s: %d (dry_run)", st.t.L(kBroadcastReach), st.broadcastReach), false)
	case "s": // send (destructive: dry-run then confirm)
		if st.broadcastText == "" {
			return
		}
		if _, err := broadcast(a.c, st.broadcastText, true); err != nil {
			fmt.Fprintf(a.out, "%s\n", apiErrorLine(err, st.t))
			return
		}
		api := fmt.Sprintf("POST /api/v1/broadcast {text:%q, dry_run:false}", st.broadcastText)
		if a.confirm("%s %s", st.t.L(kWillCall), api) {
			if _, err := broadcast(a.c, st.broadcastText, false); err != nil {
				fmt.Fprintf(a.out, "%s\n", apiErrorLine(err, st.t))
				st.showToast(st.t.L(kToastErr), true)
			} else {
				st.showToast(st.t.L(kBroadcastSent), false)
			}
		} else {
			st.showToast(st.t.L(kCancelled), false)
		}
	case "\x1b", "Esc":
		// uncommitted text intentionally kept (rule 3)
		st.showToast(st.t.L(kUncommitted), false)
	}
}

func (a *app) handleFlagsKeys(k string) {
	st := a.st
	switch k {
	case "g":
		rows, err := listFlags(a.c)
		if err != nil {
			fmt.Fprintf(a.out, "%s\n", apiErrorLine(err, st.t))
			return
		}
		st.flags = rows
	case "\x1b[A":
		if st.flagCursor > 0 {
			st.flagCursor--
		}
	case "\x1b[B", "j":
		if st.flagCursor < len(st.flags)-1 {
			st.flagCursor++
		}
	case "\r", "\n", "Enter", "e":
		if st.flagCursor >= len(st.flags) {
			return
		}
		f := st.flags[st.flagCursor]
		val := a.promptLine(st.t.L(kFlagsValue))
		if val == "" {
			return
		}
		if !json.Valid([]byte(val)) {
			st.showToast(st.t.L(kFlagsBadJSON), true)
			return
		}
		api := fmt.Sprintf("PUT /api/v1/flags {key:%q, value:%s}", f.Key, val)
		if a.confirm("%s %s", st.t.L(kWillCall), api) {
			if err := setFlag(a.c, f.Key, val); err != nil {
				fmt.Fprintf(a.out, "%s\n", apiErrorLine(err, st.t))
				st.showToast(st.t.L(kToastErr), true)
			} else {
				st.showToast(fmt.Sprintf("flag %s saved", f.Key), false)
			}
		} else {
			st.showToast(st.t.L(kCancelled), false)
		}
	}
}

func (a *app) handleArchiveKeys(k string) {
	st := a.st
	switch k {
	case "d": // dry-run
		raw, err := archiveRun(a.c, true)
		if err != nil {
			fmt.Fprintf(a.out, "%s\n", apiErrorLine(err, st.t))
			return
		}
		var pr struct {
			Rows     int64 `json:"rows"`
			Count    int64 `json:"count"`
			ToMove   int64 `json:"to_move"`
			Estimate int64 `json:"estimate"`
		}
		_ = json.Unmarshal(raw, &pr)
		st.archiveDryRows = firstNonZero(pr.Rows, pr.Count, pr.ToMove, pr.Estimate)
		st.showToast(fmt.Sprintf("%s: %d", st.t.L(kArchiveDry), st.archiveDryRows), false)
	case "r": // run (destructive)
		api := "POST /api/v1/archive/run {dry_run:false}"
		if a.confirm("%s — %s", st.t.L(kArchiveRun), api) {
			if _, err := archiveRun(a.c, false); err != nil {
				fmt.Fprintf(a.out, "%s\n", apiErrorLine(err, st.t))
				st.showToast(st.t.L(kToastErr), true)
			} else {
				st.showToast("archive ok", false)
			}
		} else {
			st.showToast(st.t.L(kCancelled), false)
		}
	}
}

func (a *app) handleAuditKeys(k string) {
	st := a.st
	switch k {
	case "g", "r":
		rows, err := listAudit(a.c, 0, 50)
		if err != nil {
			fmt.Fprintf(a.out, "%s\n", apiErrorLine(err, st.t))
			return
		}
		st.audit = rows
		st.auditCursor = 0
	case "\x1b[A":
		if st.auditCursor > 0 {
			st.auditCursor--
		}
	case "\x1b[B", "j":
		if st.auditCursor < len(st.audit)-1 {
			st.auditCursor++
		}
	case "u":
		st.auditExpand = !st.auditExpand
	case "m", "l": // load more via since_id
		if len(st.audit) == 0 {
			return
		}
		since := st.audit[len(st.audit)-1].ID
		rows, err := listAudit(a.c, since, 50)
		if err != nil {
			fmt.Fprintf(a.out, "%s\n", apiErrorLine(err, st.t))
			return
		}
		st.audit = append(st.audit, rows...)
	}
}

// refresh pulls overview data in the background (never blocks input).
func (a *app) refresh() {
	st := a.st
	st.mu.Lock()
	st.refreshing = true
	st.mu.Unlock()
	ov, err := fetchOverview(a.c)
	st.mu.Lock()
	defer st.mu.Unlock()
	st.refreshing = false
	if err != nil {
		st.connected = false
		return
	}
	st.connected = true
	st.overview = ov
	// number roll target update (renderer animates via rollSequence frames)
	st.onlineShown = ov.Online
	st.msgsShown = ov.MsgToday
	st.archiveStatus = ov.Archive
}

// refreshCountdown ticks the visible countdown toward 0 at 10Hz.
func refreshCountdown(cur, periodUnits int) int {
	next := cur - periodUnits/10
	if next < 0 {
		return 0
	}
	return next
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func firstNonZero(vals ...int64) int64 {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}
