package i18n

import "os"

// StdinIsTTY reports whether stdin is an interactive terminal. CI, pipes and
// redirects all return false → callers must run in pure flag/CLI mode.
func StdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// InteractiveEnabled is the single dual-mode decision point:
// interactive mode runs only when stdin is a TTY AND the caller has not
// passed --non-interactive. Everywhere else the flags are the truth.
func InteractiveEnabled(nonInteractiveFlag bool) bool {
	return !nonInteractiveFlag && StdinIsTTY()
}
