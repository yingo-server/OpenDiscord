package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 1. pure render tests

func TestRenderFrameOverviewAndTabHighlight(t *testing.T) {
	st := newUIState(localeFor("zh-CN"), "zh-CN", "http://127.0.0.1:7780")
	st.connected = true
	st.overview = overview{Online: 1234, Users: 5000, MsgToday: 88213, Archive: `{"last_run":"ok"}`}
	st.onlineShown, st.msgsShown = 1234, 88213
	st.archiveStatus = `{"last_run":"ok"}`
	st.tab = "1"
	out := renderFrame(st)
	if !strings.Contains(out, "1234") {
		t.Fatal("overview online number missing")
	}
	if !strings.Contains(out, "88,213") && !strings.Contains(out, "88213") {
		t.Fatal("messages number missing")
	}
	if !strings.Contains(out, "[1]概览") {
		t.Fatal("tab bar missing zh label")
	}
	if !strings.Contains(out, "\x1b[7m[1]概览\x1b[0m") {
		t.Fatal("active tab not highlighted (inverse video)")
	}
}

func TestRenderFrameToast(t *testing.T) {
	st := newUIState(localeFor("zh-CN"), "zh-CN", "srv")
	st.showToast("封禁成功 uid=42", false)
	out := renderFrame(st)
	if !strings.Contains(out, "封禁成功 uid=42") {
		t.Fatal("toast text missing")
	}
	if !strings.Contains(out, "\x1b[32m") {
		t.Fatal("success toast should be green")
	}
}

func TestRenderTabSwitchEnglish(t *testing.T) {
	st := newUIState(localeFor("en"), "en", "srv")
	st.tab = "3"
	out := renderTabBar(st)
	if !strings.Contains(out, "[3]Reports") {
		t.Fatalf("en tab label missing: %s", out)
	}
}

// 2. i18n

func TestI18nZhAndEn(t *testing.T) {
	if zh := localeFor("zh-CN").L(kTabUsers); zh != "用户" {
		t.Fatalf("zh key wrong: %q", zh)
	}
	if en := localeFor("en").L(kTabUsers); en != "Users" {
		t.Fatalf("en key wrong: %q", en)
	}
	if localeFor("zh-CN").L(kNoCreds) == kNoCreds {
		t.Fatal("zh fallback to key — translation missing")
	}
}

func TestDetectLangFromEnv(t *testing.T) {
	os.Setenv("LANG", "zh_CN.UTF-8")
	defer os.Unsetenv("LANG")
	if detectLang() != "zh-CN" {
		t.Fatal("zh not detected from LANG")
	}
	os.Setenv("LANG", "en_US.UTF-8")
	if detectLang() != "en" {
		t.Fatal("en not detected from LANG")
	}
}

// 3. number roll animation frames

func TestRollSequenceMonotonicAndEndpoint(t *testing.T) {
	seq := rollSequence(1000, 1234)
	if len(seq) < 2 {
		t.Fatalf("need multiple frames, got %d", len(seq))
	}
	for i := 1; i < len(seq); i++ {
		if seq[i] < seq[i-1] {
			t.Fatalf("frames not monotonic non-decreasing at %d: %v < %v", i, seq[i], seq[i-1])
		}
	}
	if seq[len(seq)-1] != 1234 {
		t.Fatalf("last frame must equal target, got %v", seq[len(seq)-1])
	}
	// downward roll
	down := rollSequence(5000, 4000)
	for i := 1; i < len(down); i++ {
		if down[i] > down[i-1] {
			t.Fatalf("downward roll increased at %d", i)
		}
	}
	if down[len(down)-1] != 4000 {
		t.Fatal("downward endpoint wrong")
	}
	// equal values → single frame
	if eq := rollSequence(7, 7); len(eq) != 1 || eq[0] != 7 {
		t.Fatal("equal roll should be one frame")
	}
}

// 5. non-TTY → usage + exit 64

func TestNonTTYPrintsUsageExit64(t *testing.T) {
	// run() with stdout not a char device: in `go test` stdout is a pipe, so
	// run() itself must take the usage path before touching state.
	code := run([]string{"--lang", "en"})
	if code != 64 {
		t.Fatalf("exit code = %d, want 64", code)
	}
}

// 6. sound toggle

func TestBellOnWhenSoundEnabled(t *testing.T) {
	st := newUIState(localeFor("zh-CN"), "zh-CN", "srv")
	st.sound = true
	var out strings.Builder
	old := outW
	outW = &out
	defer func() { outW = old }()
	st.showToast("ok", false)
	if !strings.Contains(out.String(), "\a") {
		t.Fatal("sound on: bell expected")
	}
	// error → 2 bells
	out.Reset()
	st.showToast("err", true)
	if got := strings.Count(out.String(), "\a"); got != 2 {
		t.Fatalf("error toast should emit 2 bells, got %d", got)
	}
}

func TestBellOffByDefault(t *testing.T) {
	st := newUIState(localeFor("zh-CN"), "zh-CN", "srv")
	var out strings.Builder
	old := outW
	outW = &out
	defer func() { outW = old }()
	st.showToast("ok", false)
	if strings.Contains(out.String(), "\a") {
		t.Fatal("sound must be off by default")
	}
	if st.lastBells != 1 {
		t.Fatal("bell metadata should still be tracked when muted")
	}
}

func TestBellUrgentThreeShort(t *testing.T) {
	if got := bell(3); strings.Count(got, "\a") != 3 {
		t.Fatalf("urgent notification should be 3 short bells, got %q", got)
	}
}

// client helpers

func TestCredPathSharedWithAdminctl(t *testing.T) {
	dir := t.TempDir()
	os.Setenv("UFOT_ADMIN_CREDENTIALS", filepath.Join(dir, "adminctl.json"))
	defer os.Unsetenv("UFOT_ADMIN_CREDENTIALS")
	if err := saveCredentials(credentials{Server: "http://x", Token: "t", CSRF: "c"}); err != nil {
		t.Fatal(err)
	}
	c, err := loadCredentials()
	if err != nil || c.Token != "t" || c.CSRF != "c" {
		t.Fatalf("roundtrip failed: %+v %v", c, err)
	}
}

func TestParseMetrics(t *testing.T) {
	m := parseMetrics("# c\nufot_online_connections 1234\njunk\nufot_users_total 5000\n")
	if m["ufot_online_connections"] != 1234 || m["ufot_users_total"] != 5000 {
		t.Fatalf("parse wrong: %v", m)
	}
}

func TestAPIErrorLine(t *testing.T) {
	err := &statusError{Status: 401, Reason: "unauthorized", RequestID: "req-9"}
	line := apiErrorLine(err, localeFor("zh-CN"))
	for _, want := range []string{"401", "unauthorized", "req-9", "adminctl login"} {
		if !strings.Contains(line, want) {
			t.Fatalf("error line missing %q: %s", want, line)
		}
	}
}

func TestMaskContact(t *testing.T) {
	if got := maskContact("13800001111"); got != "138******11" {
		t.Fatalf("mask wrong: %q", got)
	}
	if got := maskContact("ab@x.com"); got == "ab@x.com" {
		t.Fatalf("short contact not masked: %q", got)
	}
}

func TestHelpOverlay(t *testing.T) {
	st := newUIState(localeFor("en"), "en", "srv")
	if !strings.Contains(renderHelpOverlay(st), "keybindings") {
		t.Fatal("help overlay missing title")
	}
}
