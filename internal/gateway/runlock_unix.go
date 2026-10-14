//go:build !windows

package gateway

import (
	"errors"
	"os"
	"syscall"
)

// lockFile takes an exclusive, non-blocking lock on f. It returns ErrLockHeld
// when another process holds it, which is the only outcome AcquireRunLock
// interprets; any other error is a real failure to lock.
//
// flock is the right primitive here rather than a lock file with a pid inside:
// the kernel releases it when the holder exits, however it exits, so there is
// no staleness to detect and no window where a killed daemon's lock reads as
// live. It is per open file description, so a second fd in the SAME process
// does not satisfy it either — a doubled start inside one process is refused
// exactly like a rival process.
func lockFile(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return ErrLockHeld
		}
		return err
	}
	return nil
}

// unlockFile releases the lock. Closing the file would release it too; this is
// explicit so Release reads as what it is, and so a caller that keeps the file
// open (a status read) still hands the lock back.
func unlockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
