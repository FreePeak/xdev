package session

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
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

// leaseEnvDir / leaseEnvID / leaseEnvMode / leaseEnvPath carry the child's
// instructions.
const (
	leaseEnvDir  = "XDEV_LEASE_TEST_DIR"
	leaseEnvID   = "XDEV_LEASE_TEST_ID"
	leaseEnvMode = "XDEV_LEASE_TEST_MODE"
	leaseEnvPath = "XDEV_LEASE_TEST_PATH"
)

// TestLeaseHolderProcess is the CHILD half of the cross-process lease tests.
// It is re-executed as a separate process (see startLeaseHolder) and is not a
// test in its own right.
//
//	mode "hold"  — take the lease and keep it until the parent kills us
//	mode "crash" — take the lease and exit at once WITHOUT releasing it
//	mode "probe" — try to take the lease and report whether it was free
//	mode "write" — open the session for writing, report the path it got and
//	               whether it was a fork, and append an entry to it
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
	case "write":
		// Open the session for writing and WRITE to it: the parent must not
		// see those bytes in its own file.
		path := os.Getenv(leaseEnvPath)
		st, err := OpenForWrite(path, Options{LeaseDir: dir})
		if err != nil {
			fmt.Println("refused:", err)
			return
		}
		defer st.Close()
		fmt.Println("path:", st.Path())
		if st.ForkedFrom() != "" {
			fmt.Println("forked: true")
		} else {
			fmt.Println("forked: false")
		}
		if err := st.Append(userMsg("", "", "written by the other process")); err != nil {
			fmt.Println("append error:", err)
			os.Exit(3)
		}
		fmt.Println("appended: ok")
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

// leaseWriterResult is what the writing child reported.
type leaseWriterResult struct {
	path     string // the file the child wrote to
	forked   bool   // it was diverted to a fork
	appended bool   // its append succeeded
	refused  string // the refusal, when the child was refused
	out      string // the child's whole output
}

// runLeaseWriter asks a child process to open the session for writing and to
// append an entry to whatever it gets. It reports the path the child wrote
// to, whether it was a fork, whether the append succeeded, and any refusal.
//
// The child is a REAL process: this is the two-writer case the lease exists
// for, and a goroutine in this process cannot reproduce it.
func runLeaseWriter(t *testing.T, dir, path string) leaseWriterResult {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLeaseHolderProcess$", "-test.v")
	cmd.Env = append(os.Environ(),
		leaseEnvDir+"="+dir,
		leaseEnvID+"=writer",
		leaseEnvMode+"=write",
		leaseEnvPath+"="+path)
	raw, err := cmd.CombinedOutput()
	res := leaseWriterResult{out: string(raw)}
	if err != nil {
		t.Fatalf("the writing child failed: %v\n%s", err, res.out)
	}
	for _, line := range strings.Split(res.out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "path: "):
			res.path = strings.TrimPrefix(line, "path: ")
		case strings.HasPrefix(line, "refused: "):
			res.refused = strings.TrimPrefix(line, "refused: ")
		case line == "forked: true":
			res.forked = true
		case line == "appended: ok":
			res.appended = true
		}
	}
	return res
}

// captureStderr runs fn with os.Stderr redirected to a pipe and returns what
// was written to it.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stderr = old
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// TestACreatedSessionHoldsTheLease: a session xdev CREATES is materialised on
// its first assistant message, and from that moment it is an ordinary file on
// disk that any second xdev can find with --continue or --resume. So the
// CREATOR takes the ownership lease when it materialises — otherwise the
// prevention layer is absent for exactly the case it was built for.
//
// Two REAL processes: this one creates the session and writes to it, a child
// opens the same session for writing. The child must be diverted to a FORK,
// and this process's next append must still succeed.
func TestACreatedSessionHoldsTheLease(t *testing.T) {
	dir := t.TempDir()
	leaseDir := filepath.Join(dir, "session-owners")
	path := filepath.Join(dir, "s.jsonl")

	// The creator: a brand-new session, materialised on its first message.
	s := OpenMem(dir, "created")
	if _, err := s.EnsureOnDisk(path, Options{LeaseDir: leaseDir}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(userMsg("", "", "mine")); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// A second, REAL process opens the same session for writing.
	got := runLeaseWriter(t, leaseDir, path)
	if !got.forked {
		t.Fatalf("the second process was not diverted to a fork: %s", got.out)
	}
	if !got.appended {
		t.Fatalf("the second process could not write to its fork: %s", got.out)
	}
	if got.path == path {
		t.Fatalf("the second process wrote to the live session: %s", got.out)
	}

	// The creator's NEXT append must succeed: the other process wrote to its
	// own fork, not to this file.
	if err := s.Append(userMsg("", "", "mine again")); err != nil {
		t.Fatalf("the creator's append was killed by the other process: %v", err)
	}
	// And this file still holds only this process's records.
	raw := mustRead(t, path)
	if bytes.Contains(raw, []byte("written by the other process")) {
		t.Fatal("the other process's record landed in the live session")
	}
	// The lease file exists: the creator holds it.
	if _, err := os.Stat(filepath.Join(leaseDir, s.ID()+".lock")); err != nil {
		t.Fatalf("the creator did not take the lease: %v", err)
	}
}

// TestAnUnusableLeaseDirectoryDoesNotFailAWrite: the lease is ADVISORY. A
// lease root that the ENVIRONMENT cannot host — the parent cannot be
// written to, so the lease directory can never be created — must not turn a
// write path that used to work into a failure. The committed-byte boundary
// still protects the file.
//
// (A lease root that is THERE but cannot host a TRUSTWORTHY lease is a
// different failure, and it REFUSES: see TestAnUntrustedLeaseRootIsRefused.)
func TestAnUnusableLeaseDirectoryDoesNotFailAWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so the lease root would be creatable")
	}
	dir := t.TempDir()
	// A parent that cannot be written to: the lease directory can never be
	// created under it.
	parent := filepath.Join(dir, "readonly")
	if err := os.Mkdir(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	leaseDir := filepath.Join(parent, "session-owners")
	path := filepath.Join(dir, "s.jsonl")
	writeClean(t, path)

	var st *Store
	var err error
	out := captureStderr(t, func() {
		st, err = OpenForWrite(path, Options{LeaseDir: leaseDir})
	})
	if err != nil {
		t.Fatalf("an unusable lease directory failed the write path: %v", err)
	}
	defer st.Close()
	if st.Path() != path {
		t.Fatalf("path = %q, want the session itself", st.Path())
	}
	// The session still works: the boundary is the real guard.
	if err := st.Append(userMsg("", "", "mine")); err != nil {
		t.Fatalf("the write failed without a lease: %v", err)
	}
	// And the degradation is LOUD: a user whose lease root is unusable is
	// running without the prevention layer, and that must not be silent.
	if !strings.Contains(out, "session lease unavailable") {
		t.Fatalf("the degradation was silent: stderr = %q", out)
	}
	// And attaching a lease to an existing store degrades the same way.
	plain, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if err := plain.AttachLease(leaseDir); err != nil {
		t.Fatalf("AttachLease failed on an unusable lease directory: %v", err)
	}
}

// TestAnUntrustedLeaseRootIsRefused: a lease root that is THERE but cannot
// host a TRUSTWORTHY lease is REFUSED, not degraded. A lease taken in such a
// root could be taken by someone else, and running without the prevention
// layer is exactly what an adversary wants.
//
// Three shapes, all refused: the root is a FILE, the root is a SYMLINK (a
// symlink to a private directory PASSES the privacy check, so resolving it
// would let a swapped root redirect the lease onto another inode), and the
// root is readable by other users.
func TestAnUntrustedLeaseRootIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	writeClean(t, path)

	// (a) the lease root is a FILE.
	fileRoot := filepath.Join(dir, "as-a-file")
	if err := os.WriteFile(fileRoot, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenForWrite(path, Options{LeaseDir: fileRoot}); !errors.Is(err, ErrLeaseUntrusted) {
		t.Fatalf("err = %v, want ErrLeaseUntrusted (a lease root that is a file is not a lease host)", err)
	}

	// (b) the lease root is readable by other users.
	openRoot := filepath.Join(dir, "open-to-others")
	if err := os.Mkdir(openRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(openRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenForWrite(path, Options{LeaseDir: openRoot}); !errors.Is(err, ErrLeaseUntrusted) {
		t.Fatalf("err = %v, want ErrLeaseUntrusted (a lease another user could take is not a lease)", err)
	}

	// (c) the lease root is a SYMLINK.
	if runtime.GOOS != "windows" {
		symRoot := filepath.Join(dir, "as-a-symlink")
		target := filepath.Join(dir, "target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, symRoot); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenForWrite(path, Options{LeaseDir: symRoot}); !errors.Is(err, ErrLeaseUntrusted) {
			t.Fatalf("err = %v, want ErrLeaseUntrusted (a symlinked lease root can redirect the lease)", err)
		}
	}
}

// TestTheHolderKeepsItsLeaseWhenTheLeaseRootIsReplaced: the adversary's
// case. A REAL process holds the lease on a session; the lease root is then
// replaced (here by a file); a second REAL process opens the same session
// for writing.
//
// The second process must be REFUSED. It must not be let into the live file
// (that is the failure the lease exists to prevent), and the holder must
// keep its lease and its file untouched.
func TestTheHolderKeepsItsLeaseWhenTheLeaseRootIsReplaced(t *testing.T) {
	dir := t.TempDir()
	leaseDir := filepath.Join(dir, "session-owners")
	path := filepath.Join(dir, "s.jsonl")
	before := writeClean(t, path)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	_ = s.Close()

	// A REAL process holds the lease.
	stop := startLeaseHolder(t, leaseDir, id, "hold")
	defer stop()

	// The adversary replaces the lease root with a file.
	if err := os.RemoveAll(leaseDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leaseDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A second REAL process opens the same session for writing: REFUSED.
	got := runLeaseWriter(t, leaseDir, path)
	if got.refused == "" {
		t.Fatalf("the second process was not refused: %s", got.out)
	}
	if !strings.Contains(got.refused, "not a trustworthy lease host") {
		t.Fatalf("the refusal does not name the cause: %q", got.refused)
	}
	if got.forked || got.appended {
		t.Fatalf("the second process got in anyway: %s", got.out)
	}
	// The holder's file is untouched.
	if after := mustRead(t, path); !bytes.Equal(after, before) {
		t.Fatal("the held session's file was modified")
	}
}

// TestOneOwnedSessionIsForkedOnce: ONE fork per owned session, not one per
// open-for-write call. The TUI opens the live session and then the restored
// tab set, and the tab set usually names the SAME session: two calls, two
// forks, two orphan files and two tabs showing one conversation.
func TestOneOwnedSessionIsForkedOnce(t *testing.T) {
	path, leaseDir, id, _ := leaseFixture(t)
	stop := startLeaseHolder(t, leaseDir, id, "hold")
	defer stop()

	first, err := OpenForWrite(path, Options{LeaseDir: leaseDir})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenForWrite(path, Options{LeaseDir: leaseDir})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	if first.Path() != second.Path() {
		t.Fatalf("the owned session was forked twice: %q and %q", first.Path(), second.Path())
	}
	if first.ID() != second.ID() {
		t.Fatalf("the two forks carry different ids: %q and %q", first.ID(), second.ID())
	}
	// Exactly ONE file was added to the directory: the original and one fork.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	jsonl := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			jsonl++
		}
	}
	if jsonl != 2 {
		t.Fatalf("the directory holds %d session files, want the original and ONE fork", jsonl)
	}
}

// TestAnImportedSessionHoldsTheLease: the import MATERIALISES a new session
// file and writes the imported records into it, so it is a write path like
// any other — the importer holds the lease from the moment the file exists,
// not after the import has finished.
func TestAnImportedSessionHoldsTheLease(t *testing.T) {
	dir := t.TempDir()
	leaseDir := filepath.Join(dir, "session-owners")
	res, err := ImportClaude(testdataPath(t, "claude", "11111111-2222-3333-4444-555555555555.jsonl"),
		dir, "/tmp/work", Options{LeaseDir: leaseDir})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Store.Close()
	if _, err := os.Stat(filepath.Join(leaseDir, res.Store.ID()+".lock")); err != nil {
		t.Fatalf("the imported session did not take the lease: %v", err)
	}
}

// TestAnUntrustedLeaseRootRefusesToMaterialiseASession: a session written
// without a lease is a session two processes write into — the exact thing the
// lease exists to prevent — so an untrusted lease root means the session is
// NOT materialised, and the file it would have created does not exist.
//
// (The OPEN path refuses the same way: TestAnUntrustedLeaseRootIsRefused.)
func TestAnUntrustedLeaseRootRefusesToMaterialiseASession(t *testing.T) {
	dir := t.TempDir()

	roots := map[string]string{}
	// (a) the lease root is a FILE.
	fileRoot := filepath.Join(dir, "as-a-file")
	if err := os.WriteFile(fileRoot, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	roots["file"] = fileRoot
	// (b) the lease root is readable by other users.
	openRoot := filepath.Join(dir, "open-to-others")
	if err := os.Mkdir(openRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(openRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	roots["group-readable"] = openRoot
	// (c) the lease root is a SYMLINK.
	if runtime.GOOS != "windows" {
		symRoot := filepath.Join(dir, "as-a-symlink")
		target := filepath.Join(dir, "target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, symRoot); err != nil {
			t.Fatal(err)
		}
		roots["symlink"] = symRoot
	}

	for name, root := range roots {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name+".jsonl")
			s := OpenMem(dir, "new")
			_, err := s.EnsureOnDisk(path, Options{LeaseDir: root})
			if !errors.Is(err, ErrLeaseUntrusted) {
				t.Fatalf("err = %v, want ErrLeaseUntrusted (a session nobody can protect is not written)", err)
			}
			// And the file the materialisation created is GONE: the rollback
			// removes it, so a half-created session is never left behind.
			if _, serr := os.Stat(path); !os.IsNotExist(serr) {
				t.Fatalf("the session file exists under an untrusted lease root: %v", serr)
			}
			_ = s.Close()
		})
	}
}

// TestTheUntrustedRefusalsCarryTheirRemedy: a user who hits a refusal must
// be able to act without reading the source. The message names the path, the
// cause, AND the fix.
func TestTheUntrustedRefusalsCarryTheirRemedy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	writeClean(t, path)

	// The mode refusal names the chmod.
	openRoot := filepath.Join(dir, "open-to-others")
	if err := os.Mkdir(openRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(openRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := OpenForWrite(path, Options{LeaseDir: openRoot})
	if err == nil {
		t.Fatal("a group-readable lease root was accepted")
	}
	if !strings.Contains(err.Error(), "chmod 700 "+openRoot) {
		t.Fatalf("the refusal does not carry the remedy: %v", err)
	}

	// The file refusal names the removal.
	fileRoot := filepath.Join(dir, "as-a-file")
	if err := os.WriteFile(fileRoot, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = OpenForWrite(path, Options{LeaseDir: fileRoot})
	if err == nil {
		t.Fatal("a lease root that is a file was accepted")
	}
	if !strings.Contains(err.Error(), "rm "+fileRoot) {
		t.Fatalf("the refusal does not carry the remedy: %v", err)
	}

	// The symlink refusal names the removal too.
	if runtime.GOOS != "windows" {
		symRoot := filepath.Join(dir, "as-a-symlink")
		target := filepath.Join(dir, "target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, symRoot); err != nil {
			t.Fatal(err)
		}
		_, err = OpenForWrite(path, Options{LeaseDir: symRoot})
		if err == nil {
			t.Fatal("a symlinked lease root was accepted")
		}
		if !strings.Contains(err.Error(), "rm "+symRoot) {
			t.Fatalf("the refusal does not carry the remedy: %v", err)
		}
	}
}
