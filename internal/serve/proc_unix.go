//go:build !windows

package serve

import (
	"os"
	"syscall"
)

// processAlive reports whether pid is a live process. Signal 0 delivers
// nothing and is the portable liveness probe: it fails when the process is
// gone or belongs to another user.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
