//go:build windows

package main

import "os"

// Windows has no SIGTSTP and no SIGHUP to restore a console for, and
// syscall.Kill there does not deliver a POSIX signal, so there is nothing to
// do. Returning nil tells the caller not to assume the terminal was restored
// on a signal — it is exactly the pre-existing behaviour on this platform.
// onSignal is never called here: no signal is ever caught.
func watchTerminalRoutes(fini func(), onSignal func(sig os.Signal, code int)) (stop func()) {
	return nil
}
