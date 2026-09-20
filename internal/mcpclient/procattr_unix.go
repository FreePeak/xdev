//go:build !windows

package mcpclient

import "syscall"

// setProcAttr returns attributes that start the child in a new
// session so it survives the parent's exit (matching
// internal/dist/proc_unix.go).
func setProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
