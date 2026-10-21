//go:build !windows

package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestTheLeaseLockIsARealFlock: the in-process refcount makes two stores on
// one session SHARE the lease — so the refcount could be hiding a lock that
// does not lock at all. This proves the lock is real: a SECOND open file
// description on the same lease file cannot take it while the lease is
// held, exactly as another process would be refused.
func TestTheLeaseLockIsARealFlock(t *testing.T) {
	path, leaseDir, id, _ := leaseFixture(t)
	st, err := OpenForWrite(path, Options{LeaseDir: leaseDir})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	lock := filepath.Join(leaseDir, id+".lock")
	f, err := os.OpenFile(lock, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		t.Fatal("a second file description took the lease: the lock does not lock")
	}
	if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
		t.Fatalf("flock err = %v, want EWOULDBLOCK", err)
	}
}
