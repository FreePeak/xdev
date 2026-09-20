//go:build windows

package mcpclient

import "syscall"

// setProcAttr on Windows is a no-op; child processes are
// naturally detached from the console.
func setProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}
