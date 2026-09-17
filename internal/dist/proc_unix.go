//go:build !windows

package dist

import (
	"os"
	"os/exec"
	"syscall"
)

// detachedCommand builds a child that keeps running after its parent exits and
// that no Ctrl-C in the parent's terminal can reach: its own session, every
// std stream on /dev/null so there is no pipe for it to block on and nothing for
// it to write to. A background update check has exactly one output — its record
// file — so a detached child needs no console at all.
func detachedCommand(exe string, args []string) *exec.Cmd {
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		// No /dev/null: leave the streams nil, which exec wires to the
		// device it opens for a child with no files 0-2.
		return &exec.Cmd{Path: exe, Args: append([]string{exe}, args...), SysProcAttr: &syscall.SysProcAttr{Setsid: true}}
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, devNull, devNull
	return cmd
}
