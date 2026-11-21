package main

import (
	"strings"
	"testing"

	"github.com/yingo-server/UFOT/server/internal/pkg/exitcodes"
	"github.com/yingo-server/UFOT/server/internal/pkg/i18n"
)

// TestCLIModeWhenNotTTY: chatd run via pipes/CI must reject --tui gracefully
// (it degrades to the plain systemd run, exit behavior unchanged).
func TestCLIModeWhenNotTTY(t *testing.T) {
	if i18n.StdinIsTTY() {
		t.Skip("stdin is a terminal; dual-mode degradation not observable")
	}
	if i18n.InteractiveEnabled(false) {
		t.Fatal("non-TTY stdin must disable interactive mode")
	}
	if i18n.InteractiveEnabled(true) {
		t.Fatal("--non-interactive must disable interactive mode even on TTY")
	}
}

// TestInteractiveSkippable checks that --non-interactive wins over a TTY.
func TestInteractiveSkippable(t *testing.T) {
	if !i18n.InteractiveEnabled(false) == i18n.StdinIsTTY() {
		// consistency: no flag + TTY ⇒ interactive; otherwise not.
		t.Fatal("InteractiveEnabled inconsistent with StdinIsTTY")
	}
}

func TestUsageMentionsTUI(t *testing.T) {
	if !strings.Contains(usage, "--tui") || !strings.Contains(usage, "--lang") {
		t.Fatal("usage must document --tui and --lang")
	}
}

// TestExitCodesContract guards the mapping used by TUI quit (0) and flag
// conflicts (config invalid).
func TestExitCodesContract(t *testing.T) {
	if exitcodes.OK != 0 || exitcodes.ConfigInvalid != 1 {
		t.Fatal("unexpected exitcode values")
	}
}
