package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/session"
)

// TestResumeForksAnOwnedSession: a resume of a session another live xdev owns
// writes to a FORK — the original file belongs to the other process — and the
// user is told where their entries are going.
//
// The wording is omp's, because the fact it reports is the only thing that
// explains a resumed session that no longer writes to the file the user is
// looking at.
func TestResumeForksAnOwnedSession(t *testing.T) {
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

	st, err := resolveSession(cwd, false, compressTestShortID(path))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if st.Path() == path {
		t.Fatal("the owned session was resumed instead of forked")
	}
	if st.ForkedFrom() != path {
		t.Fatalf("ForkedFrom = %q, want %q", st.ForkedFrom(), path)
	}

	notice := sessionForkNotice(st)
	for _, want := range []string{
		"is open for writing in another xdev process",
		"so this session now saves to",
		st.Path(),
	} {
		if !strings.Contains(notice, want) {
			t.Fatalf("the notice is missing %q:\n%s", want, notice)
		}
	}

	// The session the other process owns is untouched.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the owned session's file was modified by the resume")
	}

	// A store that was not forked has no notice.
	plain, err := session.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if sessionForkNotice(plain) != "" {
		t.Fatalf("a plain store reported a fork notice: %q", sessionForkNotice(plain))
	}
}

// TestExportOfAnOwnedSessionIsNotRefused: the export fast path only READS.
// It is never refused — not even when another live xdev owns the session —
// and it leaves no fork behind.
func TestExportOfAnOwnedSessionIsNotRefused(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dataDir)
	cwd := t.TempDir()
	path := compressTestSession(t, dataDir, cwd)

	stop := startLeaseHolder(t, path, sessionLeaseDir())
	defer stop()

	st, err := exportSourceSession(cwd, printOptions{ResumePrefix: compressTestShortID(path)})
	if err != nil {
		t.Fatalf("the export was refused: %v", err)
	}
	defer st.Close()
	if st.Path() != path {
		t.Fatalf("the export forked the session: %s", st.Path())
	}
	if st.ForkedFrom() != "" {
		t.Fatal("the export reported a fork")
	}
}

// TestExportTakesNoLease: a read-only export of a session nobody owns
// creates no lease file at all. The lease belongs to the WRITE path.
func TestExportTakesNoLease(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dataDir)
	cwd := t.TempDir()
	path := compressTestSession(t, dataDir, cwd)

	st, err := exportSourceSession(cwd, printOptions{ResumePrefix: compressTestShortID(path)})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	entries, err := os.ReadDir(sessionLeaseDir())
	if err == nil && len(entries) != 0 {
		t.Fatalf("the read-only export took a lease: %v", entries)
	}
}
