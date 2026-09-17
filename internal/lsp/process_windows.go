//go:build windows

package lsp

import (
	"os"
	"os/exec"
)

// prepareProcessGroup is a no-op on windows: JobObjects are the platform
// mechanism and xdev does not need them for a stdio LSP server.
func prepareProcessGroup(cmd *exec.Cmd) {}

func killProcessGroup(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
