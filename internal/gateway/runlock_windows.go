//go:build windows

package gateway

import "os"

// lockFile has no portable Windows spelling without a dependency (LockFileEx
// via golang.org/x/sys/windows), and the daemon is documented as a foreground
// mode there. The guard is therefore a no-op on Windows: a second daemon is
// refused by Telegram's own 409 rather than by us.
func lockFile(*os.File) error { return nil }

func unlockFile(*os.File) error { return nil }
