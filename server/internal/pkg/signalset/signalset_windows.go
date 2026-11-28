//go:build windows

package signalset

import (
	"os"
	"syscall"
)

// DefaultMapping on Windows: only SIGINT and SIGTERM exist. Windows has no
// SIGHUP/SIGUSR1/SIGUSR2, so reload and snapshot are not signal-triggered
// there — they are reachable through the CLI (`ufotctl`, `chatd --tui`).
// Returning a smaller table is honest: the process simply never receives
// those signals.
func DefaultMapping() map[os.Signal]Semantics {
	return map[os.Signal]Semantics{
		syscall.SIGTERM: Quit,
		syscall.SIGINT:  Quit,
	}
}
