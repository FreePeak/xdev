//go:build windows

package gateway

import "syscall"

// sysProcAttr has no session concept on Windows; a worker is started in the
// same process group, and `xdev gateway run` is documented as a foreground
// mode there.
func sysProcAttr() *syscall.SysProcAttr { return nil }
