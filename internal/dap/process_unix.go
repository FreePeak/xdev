//go:build !windows

package dap

import (
	"os/exec"
	"syscall"
)

// prepareProcessGroup makes the adapter its own process-group leader so a
// teardown reaches the debuggee too (debugpy and lldb-dap spawn the inferior
// as a child; killing only the adapter would leave it running).
func prepareProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcessGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
