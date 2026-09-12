//go:build !windows

package lsp

import (
	"os/exec"
	"syscall"
)

// prepareProcessGroup makes the child its own process-group leader so a
// shutdown reaches the server's descendants too (rust-analyzer spawns some).
func prepareProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcessGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
