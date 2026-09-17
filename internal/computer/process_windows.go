//go:build windows

package computer

import (
	"os"
	"os/exec"
)

// prepareProcessGroup is a no-op on windows: JobObjects are the platform's
// group mechanism, and a desktop-control op is one short-lived command.
func prepareProcessGroup(cmd *exec.Cmd) {}

func killProcessGroup(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
