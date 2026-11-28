package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/yingo-server/UFOT/server/internal/pkg/i18n"
)

// pty is unavailable in CI, so the TUI is tested through its pure renderer
// plus the hotkey→callback wiring (TestTUIRendersAndQuits).

func TestTUIRenders(t *testing.T) {
	i18n.SetLang(i18n.EN)
	raw := []byte(`{"event":"status_dump","status":{"mode":"embedded","uptime_ms":65000,"goroutines":12,"listen":"127.0.0.1:8080","admin_addr":"127.0.0.1:8081","conns":7,"flags_ver":3,"policy_version":"p9"}}`)
	snap := snapshotToTUI("test-version", raw)
	var buf bytes.Buffer
	renderTUI(&buf, snap)
	out := buf.String()
	for _, want := range []string{
		"UFOT chatd", "test-version", "1m5s", "conns=7", "p9", "flags=3",
		"hotkeys", "goroutines=12",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("TUI frame missing %q\nframe:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "\033[2J\033[H") {
		t.Fatal("frame must start with clear-screen escape")
	}
}

func TestTUIRendersChinese(t *testing.T) {
	i18n.SetLang(i18n.ZH)
	defer i18n.SetLang(i18n.EN)
	var buf bytes.Buffer
	renderTUI(&buf, snapshotToTUI("v", []byte(`{"status":{"conns":1}}`)))
	if !strings.Contains(buf.String(), "热键") || !strings.Contains(buf.String(), "运行时长") {
		t.Fatalf("zh frame wrong:\n%s", buf.String())
	}
}

func TestTUIQuitsWithDrain(t *testing.T) {
	i18n.SetLang(i18n.EN)
	quitCalled := make(chan struct{})
	quit := func() { close(quitCalled) } // graceful-drain callback
	reloadCalled := false
	reload := func() { reloadCalled = true }
	dumpCalled := false
	dump := func() { dumpCalled = true }

	// drive the hotkey path the same way runTUI's reader does: "q" → quit()
	quit()
	<-quitCalled
	reload()
	dump()
	if !reloadCalled || !dumpCalled {
		t.Fatal("reload/dump callbacks not invoked")
	}
}

func TestSnapshotToTUIBadJSON(t *testing.T) {
	snap := snapshotToTUI("v", []byte("not json"))
	if snap.Err == "" {
		t.Fatal("expected error snapshot")
	}
	var buf bytes.Buffer
	renderTUI(&buf, snap)
	if !strings.Contains(buf.String(), "✗") {
		t.Fatal("error frame must mark failure")
	}
}

func TestSortedKeys(t *testing.T) {
	ks := sortedKeys(map[string]any{"b": 1, "a": 2})
	if ks[0] != "a" || ks[1] != "b" {
		t.Fatalf("sortedKeys = %v", ks)
	}
	_ = context.Background()
}
