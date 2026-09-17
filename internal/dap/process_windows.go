//go:build windows

package dap

import (
	"os"
	"os/exec"
)

// prepareProcessGroup is a no-op on windows: JobObjects are the platform's
// group mechanism and xdev does not need them for a stdio DAP adapter.
func prepareProcessGroup(cmd *exec.Cmd) {}

func killProcessGroup(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
