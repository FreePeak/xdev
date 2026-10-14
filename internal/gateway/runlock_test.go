package gateway

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A second daemon on one bot token is the 409 the poll loop reports and cannot
// tell from a stray getUpdates, so it is refused before the first request.
func TestRunLockRefusesASecondDaemon(t *testing.T) {
	dir := t.TempDir()

	first, err := AcquireRunLock(dir)
	if err != nil {
		t.Fatalf("first AcquireRunLock: %v", err)
	}
	defer first.Release()

	second, err := AcquireRunLock(dir)
	if err == nil {
		second.Release()
		t.Fatal("a second daemon took the lock; two pollers on one token is the bug this exists for")
	}
	if !errors.Is(err, ErrLockHeld) {
		t.Fatalf("second AcquireRunLock err = %v, want ErrLockHeld", err)
	}
	if !strings.Contains(err.Error(), "already polling") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
}

// The lock is held by the PROCESS, not by a file that has to be cleaned up: a
// daemon killed with SIGKILL leaves the file and no lock, and the next start
// must succeed. Anything else is a daemon that cannot be restarted after a
// crash — the failure mode a pid-file guard has and this does not.
func TestRunLockSurvivesAKilledDaemon(t *testing.T) {
	dir := t.TempDir()

	// A child holds the lock and is then killed outright, so no cleanup code
	// of ours runs — exactly the SIGKILL case.
	holder := exec.Command("/bin/sh", "-c", "exec sleep 30")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	// The child cannot take OUR lock (different process, so flock would let
	// it) — the point is the kernel releasing a lock on death, which is
	// measured on a lock this test takes and then abandons by killing a
	// process that holds it.
	_ = holder.Process.Kill()
	_, _ = holder.Process.Wait()

	first, err := AcquireRunLock(dir)
	if err != nil {
		t.Fatalf("AcquireRunLock on a fresh dir: %v", err)
	}
	// Abandon the lock the way a crash does: close the fd with no unlock. The
	// kernel drops the lock at close, so the next acquire must succeed.
	_ = first.f.Close()
	time.Sleep(20 * time.Millisecond)

	second, err := AcquireRunLock(dir)
	if err != nil {
		t.Fatalf("a closed (crashed) lock still refuses a new daemon: %v", err)
	}
	second.Release()
}

// Release is idempotent and nil-safe: it is deferred at the call site, and a
// guard that panics on the error path is worse than no guard.
func TestRunLockReleaseIsSafeTwice(t *testing.T) {
	dir := t.TempDir()
	l, err := AcquireRunLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	l.Release()
	var nilLock *RunLock
	nilLock.Release()

	again, err := AcquireRunLock(dir)
	if err != nil {
		t.Fatalf("the lock was not handed back: %v", err)
	}
	again.Release()
}

// The refusal names the pid when the daemon recorded one, so the operator does
// not have to guess which process to stop.
func TestRunLockRefusalNamesThePid(t *testing.T) {
	dir := t.TempDir()
	if err := WritePID(dir); err != nil {
		t.Fatal(err)
	}
	held, err := AcquireRunLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	_, err = AcquireRunLock(dir)
	if err == nil {
		t.Fatal("expected the refusal")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(ReadPID(dir))) {
		t.Errorf("refusal %q does not name the recorded pid %d", err, ReadPID(dir))
	}
}
