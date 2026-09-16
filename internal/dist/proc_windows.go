//go:build windows

package dist

import (
	"os/exec"
	"syscall"
)

// detachedCommand builds a child that keeps running after its parent exits,
// with no console: xdev is a console binary, and CREATE_NO_WINDOW stops the
// check from flashing a window on the user's desktop. Its std streams are the
// nil files exec.CommandContext leaves them as, which exec wires to the null
// device — the same shape as the unix helper, one argument at a time.
func detachedCommand(exe string, args []string) *exec.Cmd {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
	return cmd
}

const createNoWindow = 0x0800_0000 // CREATE_NO_WINDOW
