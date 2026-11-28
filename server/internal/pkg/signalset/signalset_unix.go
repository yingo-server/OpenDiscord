//go:build !windows

package signalset

import (
	"os"
	"syscall"
)

// DefaultMapping is the documented mapping on unix: SIGHUP reloads, SIGTERM
// and SIGINT quit, SIGUSR1 dumps a snapshot and SIGUSR2 is reserved.
func DefaultMapping() map[os.Signal]Semantics {
	return map[os.Signal]Semantics{
		syscall.SIGHUP:  Reload,
		syscall.SIGUSR1: Snapshot,
		syscall.SIGUSR2: Custom,
		syscall.SIGTERM: Quit,
		syscall.SIGINT:  Quit,
	}
}
