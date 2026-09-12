//go:build windows

package eval

import "os/exec"

// prepareProcessGroup is a no-op on windows: there is no Setpgid.
func prepareProcessGroup(cmd *exec.Cmd) { _ = cmd }

// interruptGroup kills the kernel on windows: SIGINT cannot be delivered, so
// the escalation is immediate and the next cell restarts the interpreter.
func interruptGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// killGroup kills the kernel on windows.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
