package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/session"
)

// The lease holder child: this test binary, re-executed as a SEPARATE
// process that takes the session's ownership lease and keeps it. A goroutine
// in this process cannot prove cross-process exclusivity — the in-process
// refcount deliberately makes two stores on one session SHARE the lease.
const (
	leaseHolderEnvPath = "XDEV_LEASE_HOLDER_PATH"
	leaseHolderEnvDir  = "XDEV_LEASE_HOLDER_DIR"
)

// TestLeaseHolderProcess is the CHILD half of the lease tests. It is not a
// test in its own right: it only does something when the parent asked for
// it through the environment.
func TestLeaseHolderProcess(t *testing.T) {
	path := os.Getenv(leaseHolderEnvPath)
	dir := os.Getenv(leaseHolderEnvDir)
	if path == "" || dir == "" {
		t.Skip("not the holder run")
	}
	st, err := session.OpenForWrite(path, session.Options{LeaseDir: dir})
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(2)
	}
	defer st.Close()
	fmt.Println("ready")
	time.Sleep(10 * time.Minute)
}

// startLeaseHolder runs the lease-holder child and waits until it holds the
// lease. The returned function stops the child.
func startLeaseHolder(t *testing.T, path, leaseDir string) func() {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLeaseHolderProcess$", "-test.v")
	cmd.Env = append(os.Environ(),
		leaseHolderEnvPath+"="+path,
		leaseHolderEnvDir+"="+leaseDir)
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
	t.Cleanup(stop)
	return stop
}

// TestCompressRefusesAnOwnedSession: `xdev compress` modifies an EXISTING
// session file out of band, so it takes the session's ownership lease. When
// another live xdev owns the session it REFUSES — it does not fork (a
// compaction written to a file nobody is looking at is invisible damage),
// it says why, and nothing is changed.
func TestCompressRefusesAnOwnedSession(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dataDir)
	cwd := t.TempDir()
	path := compressTestSession(t, dataDir, cwd)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	stop := startLeaseHolder(t, path, sessionLeaseDir())
	defer stop()

	var out, errOut bytes.Buffer
	// --force skips the mtime "looks live" guard, so the refusal under test
	// is the LEASE and not the age heuristic.
	code := compressCmd([]string{"--session", path, "--keep-recent", "30", "--force"}, cwd, nil, &out, &errOut)
	if code != 1 {
		t.Fatalf("compress exit = %d, want 1 (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "another xdev is writing this session") {
		t.Fatalf("refusal output:\n%s", errOut.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the owned session's file was modified")
	}
	if compressTestHasCompaction(t, path) {
		t.Fatal("the refusal still wrote a compaction entry")
	}

	// The engine refuses the same way (the CLI is a thin shell over it).
	if _, err := compressSession(context.Background(), path, "shake", 30, false, nil, ""); err == nil {
		t.Fatal("compressSession wrote to a session another xdev owns")
	} else if !strings.Contains(err.Error(), "another xdev is writing this session") {
		t.Fatalf("engine refusal = %v", err)
	}

	// A dry run only READS: it is not refused, and it writes nothing.
	out.Reset()
	errOut.Reset()
	if code := compressCmd([]string{"--session", path, "--keep-recent", "30", "--dry-run", "--force"}, cwd, nil, &out, &errOut); code != 0 {
		t.Fatalf("the dry run of an owned session was refused: %d (%s)", code, errOut.String())
	}
}
