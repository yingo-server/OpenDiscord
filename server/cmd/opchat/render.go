package main

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ANSI helpers (pure functions — all unit-testable).
const (
	ansiReset   = "\x1b[0m"
	ansiDim     = "\x1b[2m"
	ansiBold    = "\x1b[1m"
	ansiRed     = "\x1b[31m"
	ansiGreen   = "\x1b[32m"
	ansiYellow  = "\x1b[33m"
	ansiCyan    = "\x1b[36m"
	ansiInverse = "\x1b[7m"
)

// stripANSI removes SGR sequences for width calc.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦"}

// fadeIn frames: 3-frame fade for tab switches (~120ms total: 40ms/frame).
var fadeInFrames = []float64{0.2, 0.5, 1.0}

// renderFrame draws the full screen for a given state. Pure: no I/O.
// tab order: 1 overview, 2 users, 3 reports, 4 broadcast, 5 flags, 6 archive, 7 audit.
func renderFrame(s *uiState) string {
	// The refresh goroutine mutates the same uiState while the renderer reads
	// it; take the state lock for the whole frame so a status-bar read can
	// never race a refresh write (CI -race caught exactly that).
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	b.WriteString(renderHeader(s))
	b.WriteString("\n")
	b.WriteString(renderTabBar(s))
	b.WriteString("\n")
	b.WriteString(strings.Repeat("─", s.width) + "\n")
	body := renderTabBody(s)
	if s.fadeStep >= 0 && s.fadeStep < len(fadeInFrames) {
		body = applyFade(body, fadeInFrames[s.fadeStep])
	}
	b.WriteString(body)
	b.WriteString("\n" + strings.Repeat("─", s.width) + "\n")
	b.WriteString(renderStatusBar(s))
	if s.toast != "" {
		b.WriteString("\n" + renderToast(s))
	}
	b.WriteString("\n")
	return b.String()
}

func renderHeader(s *uiState) string {
	status := ansiRed + "○" + ansiReset
	if s.connected {
		status = ansiGreen + "●" + ansiReset
	}
	line := fmt.Sprintf("┌─ %s %s %s %s%s ── %s:%s ", s.t.L(kTitle), status,
		s.t.L(kConnected), ansiDim, s.server, s.t.L(kLang), s.lang)
	pad := s.width - 1 - utf8.RuneCountInString(stripANSI(line))
	if pad > 0 {
		line += strings.Repeat("─", pad)
	} else {
		line += "─"
	}
	return line
}

func renderTabBar(s *uiState) string {
	tabs := []struct {
		key, label string
	}{
		{"1", s.t.L(kTabOverview)}, {"2", s.t.L(kTabUsers)},
		{"3", s.t.L(kTabReports)}, {"4", s.t.L(kTabBroadcast)},
		{"5", s.t.L(kTabFlags)}, {"6", s.t.L(kTabArchive)},
		{"7", s.t.L(kTabAudit)}, {"q", s.t.L(kQuit)},
	}
	var parts []string
	for _, tb := range tabs {
		label := "[" + tb.key + "]" + tb.label
		if tb.key == s.tab {
			parts = append(parts, ansiInverse+label+ansiReset)
		} else {
			parts = append(parts, label)
		}
	}
	return strings.Join(parts, " ")
}

func renderStatusBar(s *uiState) string {
	sp := ""
	if s.refreshing {
		sp = spinnerFrames[s.spinnerIdx%len(spinnerFrames)] + " "
	}
	extra := fmt.Sprintf("%s %ds", s.t.L(kRefreshIn), s.refreshIn)
	if s.connected {
		extra = fmt.Sprintf("%s: %d · %s: %d · %s: %s · %s %ds",
			s.t.L(kOverviewOnline), int64(s.onlineShown),
			s.t.L(kOverviewMsgs), int64(s.msgsShown),
			s.t.L(kOverviewArchive), s.archiveStatus, s.t.L(kRefreshIn), s.refreshIn)
	}
	return fmt.Sprintf("%s│ %s%s │ %s", ansiDim, sp, extra, s.t.L(kSelectHint)) + ansiReset
}

func renderToast(s *uiState) string {
	color := ansiGreen
	if s.toastErr {
		color = ansiRed
	}
	return color + " ✔ " + s.toast + ansiReset
}

func renderTabBody(s *uiState) string {
	switch s.tab {
	case "1":
		return renderOverview(s)
	case "2":
		return renderUsers(s)
	case "3":
		return renderReports(s)
	case "4":
		return renderBroadcast(s)
	case "5":
		return renderFlags(s)
	case "6":
		return renderArchive(s)
	case "7":
		return renderAudit(s)
	}
	return ""
}

func renderOverview(s *uiState) string {
	var b strings.Builder
	ov := s.overview
	fmt.Fprintf(&b, " %s: %d\n", s.t.L(kOverviewOnline), int64(s.onlineShown))
	fmt.Fprintf(&b, " %s: %d\n", s.t.L(kOverviewUsers), int64(ov.Users))
	fmt.Fprintf(&b, " %s: %d\n", s.t.L(kOverviewMsgs), int64(s.msgsShown))
	fmt.Fprintf(&b, " %s: %s\n", s.t.L(kOverviewArchive), ov.Archive)
	if ov.Raw != nil {
		keys := sortedKeys(ov.Raw)
		b.WriteString(" " + ansiDim + "-- /api/v1/metrics --" + ansiReset + "\n")
		for i, k := range keys {
			if i >= 8 {
				break
			}
			fmt.Fprintf(&b, "   %s = %v\n", k, ov.Raw[k])
		}
	}
	return b.String()
}

func sortedKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}

func renderUsers(s *uiState) string {
	var b strings.Builder
	if s.userDetail != nil {
		fmt.Fprintf(&b, " %s uid=%d\n", s.t.L(kUserDetail), s.selectedUser)
		for _, k := range sortedUserKeys(s.userDetail) {
			v := s.userDetail[k]
			switch strings.Contains(k, "phone") || strings.Contains(k, "mobile") {
			case true:
				v = maskContact(fmt.Sprint(v))
			}
			if strings.Contains(k, "email") {
				v = maskContact(fmt.Sprint(v))
			}
			fmt.Fprintf(&b, "   %s = %v\n", k, v)
		}
		fmt.Fprintf(&b, " [b]%s [u]%s [k]%s [Esc]%s\n",
			s.t.L(kOpBan), s.t.L(kOpUnban), s.t.L(kOpKick), s.t.L(kBack))
		return b.String()
	}
	if len(s.users) == 0 {
		b.WriteString(" " + s.t.L(kNoResults) + "\n")
	}
	for i, u := range s.users {
		marker := " "
		if i == s.userCursor {
			marker = ">"
		}
		fmt.Fprintf(&b, " %s uid=%-6d %-16s status=%d\n", marker, u.ID, u.Username, u.Status)
	}
	fmt.Fprintf(&b, " [Enter]%s [b]%s [u]%s [k]%s\n",
		s.t.L(kUserDetail), s.t.L(kOpBan), s.t.L(kOpUnban), s.t.L(kOpKick))
	return b.String()
}

func sortedUserKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}

// maskContact masks phone/email: keep first 3 and last 2 chars.
func maskContact(s string) string {
	r := []rune(s)
	if len(r) <= 5 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:3]) + strings.Repeat("*", len(r)-5) + string(r[len(r)-2:])
}

func renderReports(s *uiState) string {
	var b strings.Builder
	if len(s.reports) == 0 {
		b.WriteString(" " + s.t.L(kNoResults) + "\n")
	}
	for i, rp := range s.reports {
		marker := " "
		if i == s.reportCursor {
			marker = ">"
		}
		fmt.Fprintf(&b, " %s id=%-6d status=%-3d %s\n", marker, rp.ID, rp.Status, rp.Reason)
	}
	fmt.Fprintf(&b, " [Enter]%s [f]%s\n", s.t.L(kReportsAction), s.t.L(kReportsFilter))
	return b.String()
}

func renderBroadcast(s *uiState) string {
	var b strings.Builder
	fmt.Fprintf(&b, " %s %s\n", s.t.L(kBroadcastTitle), s.broadcastTitle)
	fmt.Fprintf(&b, " %s %s\n", s.t.L(kBroadcastText), s.broadcastText)
	if s.broadcastReach >= 0 {
		fmt.Fprintf(&b, " %s: %d\n", s.t.L(kBroadcastReach), s.broadcastReach)
	}
	fmt.Fprintf(&b, " [p]%s [s]%s\n", "preview", "send")
	return b.String()
}

func renderFlags(s *uiState) string {
	var b strings.Builder
	if len(s.flags) == 0 {
		b.WriteString(" " + s.t.L(kNoResults) + "\n")
	}
	for i, f := range s.flags {
		marker := " "
		if i == s.flagCursor {
			marker = ">"
		}
		fmt.Fprintf(&b, " %s %-24s = %-20s v%d\n", marker, f.Key, string(f.Value), f.Version)
	}
	fmt.Fprintf(&b, " [Enter]%s\n", s.t.L(kFlagsValue))
	return b.String()
}

func renderArchive(s *uiState) string {
	var b strings.Builder
	fmt.Fprintf(&b, " %s: %s\n", s.t.L(kOverviewArchive), s.archiveStatus)
	if s.archiveDryRows >= 0 {
		fmt.Fprintf(&b, " %s: %d\n", s.t.L(kArchiveDry), s.archiveDryRows)
	}
	fmt.Fprintf(&b, " [d]%s [r]%s\n", "dry-run", s.t.L(kArchiveRun))
	return b.String()
}

func renderAudit(s *uiState) string {
	var b strings.Builder
	for i, a := range s.audit {
		marker := " "
		if i == s.auditCursor {
			marker = ">"
		}
		fmt.Fprintf(&b, " %s id=%-8d %-12s %s\n", marker, a.ID, a.Act, a.At)
		if s.auditExpand && i == s.auditCursor {
			b.WriteString("   " + ansiDim + compactJSON(a.Raw) + ansiReset + "\n")
		}
	}
	b.WriteString(" " + s.t.L(kAuditMore) + " · " + s.t.L(kAuditExpand) + "\n")
	return b.String()
}

// renderHelpOverlay returns the floating help sheet for the '?' key.
func renderHelpOverlay(s *uiState) string {
	lines := []string{
		"1-7/←→  " + s.t.L(kTabOverview) + ".." + s.t.L(kTabAudit),
		"r       " + s.t.L(kRefresh),
		"s       " + s.t.L(kSoundOn) + "/" + s.t.L(kSoundOff),
		"?       " + s.t.L(kHelpTitle),
		"Esc/Ctrl+C " + s.t.L(kQuit),
	}
	return ansiInverse + strings.Join(lines, "\n") + ansiReset
}

// applyFade dims the whole body frame (fade simulation via ANSI dim/inverse
// intensity steps). Pure string transform.
func applyFade(body string, intensity float64) string {
	if intensity >= 1.0 {
		return body
	}
	return ansiDim + body + ansiReset
}

// rollSequence returns the intermediate frames for a number rolling from old
// to new over ~400ms at ~30fps (12 frames), with ease-out pacing. The result
// is monotonic non-decreasing for old<new (non-increasing for old>new) and
// always ends exactly at new.
func rollSequence(oldV, newV float64) []float64 {
	const frames = 12
	if oldV == newV {
		return []float64{newV}
	}
	out := make([]float64, 0, frames)
	for i := 1; i <= frames; i++ {
		t := float64(i) / frames
		e := 1 - (1-t)*(1-t)*(1-t) // ease-out cubic
		v := oldV + (newV-oldV)*e
		out = append(out, v)
	}
	out[len(out)-1] = newV
	return out
}

func formatInt(v float64) string {
	return strconv.FormatInt(int64(v), 10)
}

// bell emits n short terminal bells separated by pauses (all inside one
// write; terminal decides whether to play).
func bell(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("\a")
		if i < n-1 {
			b.WriteString(timeStroke)
		}
	}
	return b.String()
}

const timeStroke = " "
