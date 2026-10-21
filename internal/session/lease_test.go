package session

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The cross-process session ownership lease.
//
// Every test here that needs a SECOND WRITER runs one: a goroutine in this
// process cannot prove cross-process exclusivity, and the in-process
// refcount deliberately makes two stores on one session SHARE the lease. So
// the other writer is this test binary, re-executed as a child process.

// leaseEnvDir / leaseEnvID / leaseEnvMode carry the child's instructions.
const (
	leaseEnvDir  = "XDEV_LEASE_TEST_DIR"
	leaseEnvID   = "XDEV_LEASE_TEST_ID"
	leaseEnvMode = "XDEV_LEASE_TEST_MODE"
)

// TestLeaseHolderProcess is the CHILD half of the cross-process lease tests.
// It is re-executed as a separate process (see startLeaseHolder) and is not a
// test in its own right.
//
//	mode "hold"  — take the lease and keep it until the parent kills us
//	mode "crash" — take the lease and exit at once WITHOUT releasing it
//	mode "probe" — try to take the lease and report whether it was free
func TestLeaseHolderProcess(t *testing.T) {
	dir := os.Getenv(leaseEnvDir)
	id := os.Getenv(leaseEnvID)
	if dir == "" || id == "" {
		t.Skip("not the holder run")
	}
	switch os.Getenv(leaseEnvMode) {
	case "probe":
		_, err := acquireLease(dir, id)
		switch {
		case err == nil:
			fmt.Println("acquired")
		case errors.Is(err, ErrSessionOwned):
			fmt.Println("owned")
		default:
			fmt.Println("error:", err)
			os.Exit(2)
		}
	case "crash":
		if _, err := acquireLease(dir, id); err != nil {
			fmt.Println("error:", err)
			os.Exit(2)
		}
		fmt.Println("ready")
		// Exit WITHOUT releasing: the kernel drops the lock when the
		// descriptor closes, so a crashed holder leaves nothing behind.
		os.Exit(0)
	default:
		if _, err := acquireLease(dir, id); err != nil {
			fmt.Println("error:", err)
			os.Exit(2)
		}
		fmt.Println("ready")
		time.Sleep(10 * time.Minute)
	}
}

// startLeaseHolder runs the lease-holder child and waits until it holds the
// lease. The returned function stops the child.
func startLeaseHolder(t *testing.T, dir, id, mode string) func() {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLeaseHolderProcess$", "-test.v")
	cmd.Env = append(os.Environ(),
		leaseEnvDir+"="+dir,
		leaseEnvID+"="+id,
		leaseEnvMode+"="+mode)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	br := bufio.NewReader(stdout)
	line, err := br.ReadString('\n')
	for err == nil && !strings.HasPrefix(line, "ready") {
		line, err = br.ReadString('\n')
	}
	if err != nil || !strings.HasPrefix(line, "ready") {
		stop()
		t.Fatalf("the lease holder never reported ready: %v (%q)", err, line)
	}
	if mode == "crash" {
		// The holder exits at once without releasing. Wait for the process to
		// be GONE — the kernel drops the lock when its descriptor closes, so
		// the lease is only free once the process has actually exited.
		if err := cmd.Wait(); err != nil {
			t.Fatalf("the crashed holder exited badly: %v", err)
		}
		stopped = true
		return func() {}
	}
	t.Cleanup(stop)
	return stop
}

// runLeaseProbe asks a child process to try the lease and reports what it
// got: "acquired" (the lease was free) or "owned" (another live process
// holds it).
func runLeaseProbe(t *testing.T, dir, id string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLeaseHolderProcess$", "-test.v")
	cmd.Env = append(os.Environ(),
		leaseEnvDir+"="+dir,
		leaseEnvID+"="+id,
		leaseEnvMode+"=probe")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("lease probe: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		switch strings.TrimSpace(line) {
		case "acquired", "owned":
			return strings.TrimSpace(line)
		}
	}
	t.Fatalf("the lease probe said nothing useful: %q", out)
	return ""
}

// leaseFixture writes a session file plus the id it carries.
func leaseFixture(t *testing.T) (path, leaseDir, id string, before []byte) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "s.jsonl")
	before = writeClean(t, path)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id = s.ID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return path, filepath.Join(dir, "session-owners"), id, before
}

// TestLeaseIsExclusiveAcrossProcesses: the lease is a CROSS-process lock. A
// second live process is refused it.
func TestLeaseIsExclusiveAcrossProcesses(t *testing.T) {
	path, leaseDir, id, _ := leaseFixture(t)
	stop := startLeaseHolder(t, leaseDir, id, "hold")
	defer stop()

	if _, err := acquireLease(leaseDir, id); !errors.Is(err, ErrSessionOwned) {
		t.Fatalf("a live holder was not refused the lease: %v", err)
	}
	// And the refusal is the SENTINEL, so a caller can tell "someone else
	// owns it" from "the lease dir is broken".
	if _, err := acquireLease(leaseDir, id); !strings.Contains(err.Error(), "another xdev is writing this session") {
		t.Fatalf("err = %v, want it to name the conflict", err)
	}
	_ = path
}

// TestLeaseIsReleasedWhenTheHolderProcessDies: a holder that dies WITHOUT
// releasing leaves NO stale lease. The kernel drops the lock when the
// descriptor closes — that is the whole reason the lease is a flock and not
// a pid file.
func TestLeaseIsReleasedWhenTheHolderProcessDies(t *testing.T) {
	_, leaseDir, id, _ := leaseFixture(t)
	startLeaseHolder(t, leaseDir, id, "crash")

	l, err := acquireLease(leaseDir, id)
	if err != nil {
		t.Fatalf("a crashed holder left a stale lease behind: %v", err)
	}
	l.release()
}

// TestOpenForWriteTakesTheLease: the lease is taken around the store's
// lifetime, and released when the store closes.
func TestOpenForWriteTakesTheLease(t *testing.T) {
	path, leaseDir, id, _ := leaseFixture(t)
	st, err := OpenForWrite(path, Options{LeaseDir: leaseDir})
	if err != nil {
		t.Fatal(err)
	}
	if st.Path() != path {
		t.Fatalf("path = %q, want the session itself", st.Path())
	}
	if got := runLeaseProbe(t, leaseDir, id); got != "owned" {
		t.Fatalf("a child process could take the lease of an open store: %s", got)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if got := runLeaseProbe(t, leaseDir, id); got != "acquired" {
		t.Fatalf("the lease was not released on Close: %s", got)
	}
}

// TestLeaseFileSurvivesRelease: the lease FILE is never unlinked. Unlinking
// a flock'd file races (the unlinked inode can still be locked), so the file
// stays and its presence means nothing.
func TestLeaseFileSurvivesRelease(t *testing.T) {
	path, leaseDir, id, _ := leaseFixture(t)
	st, err := OpenForWrite(path, Options{LeaseDir: leaseDir})
	if err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(leaseDir, id+".lock")
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("the lease file is missing: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("the lease file was unlinked on release: %v", err)
	}
}

// TestOpenDoesNotTakeALease: a read-only open is lease-free and is NEVER
// refused. Export, search, gallery and share only read — refusing to read
// because someone else writes would be a usability regression with no
// safety benefit.
func TestOpenDoesNotTakeALease(t *testing.T) {
	path, leaseDir, id, _ := leaseFixture(t)
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := os.Stat(leaseDir); !os.IsNotExist(err) {
		t.Fatalf("a read-only open created the lease dir: %v", err)
	}
	// And it is not refused while another process owns the session.
	stop := startLeaseHolder(t, leaseDir, id, "hold")
	defer stop()
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("a read-only open of an owned session was refused: %v", err)
	}
	_ = st2.Close()
}

// TestOpenForWriteForksWhenTheSessionIsOwned: the owned session is FORKED,
// not refused and not written into. The original file is left byte-identical
// for the process that owns it.
func TestOpenForWriteForksWhenTheSessionIsOwned(t *testing.T) {
	path, leaseDir, id, before := leaseFixture(t)
	stop := startLeaseHolder(t, leaseDir, id, "hold")
	defer stop()

	fork, err := OpenForWrite(path, Options{LeaseDir: leaseDir})
	if err != nil {
		t.Fatal(err)
	}
	defer fork.Close()

	if fork.Path() == path {
		t.Fatal("the owned session was opened instead of forked")
	}
	if fork.ID() == id {
		t.Fatal("the fork reused the original session id")
	}
	if fork.ForkedFrom() != path {
		t.Fatalf("ForkedFrom = %q, want %q", fork.ForkedFrom(), path)
	}
	if fork.ForkedFromID() != id {
		t.Fatalf("ForkedFromID = %q, want %q", fork.ForkedFromID(), id)
	}
	if fork.ForkReason() != "owned" {
		t.Fatalf("ForkReason = %q, want %q", fork.ForkReason(), "owned")
	}
	// The fork lives beside the session it branched from.
	if filepath.Dir(fork.Path()) != filepath.Dir(path) {
		t.Fatalf("the fork landed in %q, want the session's own directory %q", filepath.Dir(fork.Path()), filepath.Dir(path))
	}
	// The fork's header carries parentSession = the original id.
	raw, err := os.ReadFile(fork.Path())
	if err != nil {
		t.Fatal(err)
	}
	lines := splitLines(raw)
	if len(lines) < 2 {
		t.Fatal("the fork has no header")
	}
	h, ok := ParseHeader(lines[1])
	if !ok {
		t.Fatal("the fork's header does not parse")
	}
	if h.ParentSession != id {
		t.Fatalf("parentSession = %q, want %q", h.ParentSession, id)
	}
	// The transcript came along.
	if got := len(fork.Entries()); got != 3 {
		t.Fatalf("the fork carries %d entries, want the original's 3", got)
	}
	// The ORIGINAL is untouched — not written, not truncated, not unlinked.
	after := mustRead(t, path)
	if !bytes.Equal(after, before) {
		t.Fatal("the owned session's file was modified by the fork")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the owned session's file was removed: %v", err)
	}
	// The fork holds its OWN lease.
	if got := runLeaseProbe(t, leaseDir, fork.ID()); got != "owned" {
		t.Fatalf("the fork does not hold its lease: %s", got)
	}
}

// TestLeaseIsRefcountedWithinOneProcess: two stores on one session in one
// process SHARE the lease instead of conflicting — flock conflicts between
// two open file descriptions even inside one process, and the TUI's own tab
// set plus a resume of the same session must not fork each other.
func TestLeaseIsRefcountedWithinOneProcess(t *testing.T) {
	path, leaseDir, id, _ := leaseFixture(t)
	first, err := OpenForWrite(path, Options{LeaseDir: leaseDir})
	if err != nil {
		t.Fatal(err)
	}
	second, err := OpenForWrite(path, Options{LeaseDir: leaseDir})
	if err != nil {
		t.Fatal(err)
	}
	if second.Path() != path {
		t.Fatalf("the second store forked a session this process owns: %q", second.Path())
	}
	if second.ForkedFrom() != "" {
		t.Fatal("the second store in the same process reported a fork")
	}
	// One holder is not enough to release the lease.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if got := runLeaseProbe(t, leaseDir, id); got != "owned" {
		t.Fatalf("the lease was released while a holder was still open: %s", got)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if got := runLeaseProbe(t, leaseDir, id); got != "acquired" {
		t.Fatalf("the lease was not released after the last holder closed: %s", got)
	}
}

// TestLeaseDirIsPrivate: the lease directory is 0700 and owned by this user.
// A lease another user could take is not a lease.
func TestLeaseDirIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows exposes no unix owner/mode pair")
	}
	dir := t.TempDir()
	groupReadable := filepath.Join(dir, "group-readable")
	if err := os.MkdirAll(groupReadable, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(groupReadable, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireLease(groupReadable, "abc"); err == nil {
		t.Fatal("a group-readable lease dir was accepted")
	}

	// The directory OpenForWrite creates is private.
	path, leaseDir, _, _ := leaseFixture(t)
	st, err := OpenForWrite(path, Options{LeaseDir: leaseDir})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st2, err := os.Stat(leaseDir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st2.Mode().Perm(); perm != 0o700 {
		t.Fatalf("the lease dir mode is %04o, want 0700", perm)
	}
}

// TestCommittedBoundaryStillFiresForALeaseHolder: the lease is ADVISORY. It
// cannot stop a writer that does not take it, so the committed-byte boundary
// stays the authoritative layer — a lease holder that finds foreign records
// still refuses to append, and the foreign bytes survive.
func TestCommittedBoundaryStillFiresForALeaseHolder(t *testing.T) {
	path, leaseDir, _, _ := leaseFixture(t)
	st, err := OpenForWrite(path, Options{LeaseDir: leaseDir})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// A writer that does NOT take the lease appends a complete record.
	appendTorn(t, path, `{"type":"message","id":"foreign"}`+"\n")
	err = st.Append(userMsg("", "", "mine"))
	if !errors.Is(err, ErrConcurrentWriter) {
		t.Fatalf("err = %v, want ErrConcurrentWriter (the lease must not mask the boundary)", err)
	}
	if raw := mustRead(t, path); !bytes.Contains(raw, []byte("foreign")) {
		t.Fatal("the other writer's record was clobbered")
	}
}

// TestForkedStoreKeepsTheAnchorResolvable: the fork copies the entries
// verbatim, so a compaction anchor still resolves in the fork — the fork did
// not orphan the kept window.
func TestForkedStoreKeepsTheAnchorResolvable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	kept := "44444444"
	s := OpenMem(dir, "compacted")
	if _, err := s.EnsureOnDisk(path, Options{}); err != nil {
		t.Fatal(err)
	}
	for _, e := range []Entry{
		userMsg("11111111", "", "dropped"),
		asstMsg("22222222", "11111111", "dropped too"),
		mkCompaction("33333333", "22222222", "summary text", &kept),
		userMsg("44444444", "33333333", "kept A"),
	} {
		if err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	leaseDir := filepath.Join(dir, "session-owners")
	orig, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id := orig.ID()
	_ = orig.Close()

	stop := startLeaseHolder(t, leaseDir, id, "hold")
	defer stop()

	fork, err := OpenForWrite(path, Options{LeaseDir: leaseDir})
	if err != nil {
		t.Fatal(err)
	}
	defer fork.Close()
	res, err := BuildContext(fork.Entries(), fork.LeafID(), SystemPrompt{})
	if err != nil {
		t.Fatalf("the fork's context does not rebuild: %v", err)
	}
	if len(res.Messages) == 0 {
		t.Fatal("the fork rebuilt an empty context")
	}
	if res.Messages[0].Text() != "summary text" {
		t.Fatalf("the compaction anchor did not survive the fork: %q", res.Messages[0].Text())
	}
}
