//go:build !windows

package computer

import (
	"os/exec"
	"syscall"
)

// prepareProcessGroup makes the child its own process-group leader, so a
// cancelled op reaches its descendants too: the platform tools are shells and
// scripting hosts (osascript, xdotool) that fork the real work.
func prepareProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup SIGKILLs the group and the child itself, so the op dies
// whether or not the group took effect.
func killProcessGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
