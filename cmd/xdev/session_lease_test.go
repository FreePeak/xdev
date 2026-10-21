package main

import (
	"bytes"
	"os"
	"path/filepath"
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

// TestTheStartupForksAnOwnedSessionOnlyOnce: the TUI opens the live session
// and then the restored tab set, and the tab set usually names the SAME
// session. When another live xdev owns it, ONE fork must be made — two forks
// are two orphan files and two tabs showing one conversation.
func TestTheStartupForksAnOwnedSessionOnlyOnce(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dataDir)
	cwd := t.TempDir()
	path := compressTestSession(t, dataDir, cwd)

	// Another live process owns the session.
	stop := startLeaseHolder(t, path, sessionLeaseDir())
	defer stop()

	// The tab set names the same session (the TUI reopens it).
	saveTabSet(cwd, []string{path})

	// The TUI startup: the live session, then the restored tabs.
	live, err := openStartupSession(cwd, printOptions{ResumePrefix: compressTestShortID(path)})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	restored := savedTabSessions(cwd)
	defer func() {
		for _, st := range restored {
			_ = st.Close()
		}
	}()

	// Exactly ONE fork: the original and one fork in the bucket.
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
		t.Fatalf("the session bucket holds %d files, want the original and ONE fork", jsonl)
	}
	// And ONE tab: the restored store carries the live session's id, so the
	// TUI's dedupe check drops it instead of opening a second tab.
	if len(restored) != 1 {
		t.Fatalf("restored tabs = %d, want 1", len(restored))
	}
	if restored[0].ID() != live.ID() {
		t.Fatalf("the restored tab is a different session: %q vs %q", restored[0].ID(), live.ID())
	}
	if restored[0].Path() != live.Path() {
		t.Fatalf("the restored tab is a different file: %q vs %q", restored[0].Path(), live.Path())
	}
}

// TestCompressSucceedsWithoutAUsableLeaseDirectory: the lease is ADVISORY, so
// a lease root that the ENVIRONMENT cannot host — the state root cannot be
// written to, so the lease directory can never be created — must not take the
// write path down. The base branch compacted this session; the branch must
// compact it too, and SAY that the lease is off.
func TestCompressSucceedsWithoutAUsableLeaseDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so the lease root would be creatable")
	}
	dataDir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dataDir)
	cwd := t.TempDir()
	path := compressTestSession(t, dataDir, cwd)
	// The state root cannot be written to: the lease directory can never be
	// created under it.
	if err := os.Chmod(dataDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dataDir, 0o700) })

	var out, errOut bytes.Buffer
	var code int
	stderr := captureStderr(t, func() {
		code = compressCmd([]string{"--session", path, "--keep-recent", "30", "--force"}, cwd, nil, &out, &errOut)
	})
	if code != 0 {
		t.Fatalf("compress exit = %d (stderr: %s)", code, errOut.String())
	}
	if !compressTestHasCompaction(t, path) {
		t.Fatal("the compaction was not written")
	}
	// The degradation is LOUD: running without the prevention layer must not
	// be silent.
	if !strings.Contains(stderr, "session lease unavailable") {
		t.Fatalf("the degradation was silent: stderr = %q", stderr)
	}
}

// TestCompressRefusesAnUntrustedLeaseRoot: a lease root that is THERE but
// cannot host a TRUSTWORTHY lease is REFUSED, not degraded. `xdev compress`
// must say so and change nothing — a lease another user could take is not a
// lease, and running without it is what an adversary wants.
func TestCompressRefusesAnUntrustedLeaseRoot(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dataDir)
	cwd := t.TempDir()
	path := compressTestSession(t, dataDir, cwd)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A FILE where the lease directory belongs: it can never be a lease host.
	if err := os.WriteFile(filepath.Join(dataDir, "session-owners"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := compressCmd([]string{"--session", path, "--keep-recent", "30", "--force"}, cwd, nil, &out, &errOut)
	if code != 1 {
		t.Fatalf("compress exit = %d, want 1 (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "not a trustworthy lease host") {
		t.Fatalf("the refusal does not name the cause:\n%s", errOut.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the session was modified by a refused compaction")
	}
	if compressTestHasCompaction(t, path) {
		t.Fatal("the refusal still wrote a compaction entry")
	}
}

// TestTheExportRefusesTwoForeignSelectors: the base branch refused
// --from-claude and --from-codex together, and the export path must keep
// refusing them.
func TestTheExportRefusesTwoForeignSelectors(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dataDir)
	cwd := t.TempDir()

	if _, err := exportSourceSession(cwd, printOptions{FromClaude: "a", FromCodex: "b"}); err == nil {
		t.Fatal("the export accepted two foreign selectors")
	} else if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err = %v, want the mutual-exclusion refusal", err)
	}
}

// TestTheExportResolvesTheForeignImportBeforeResume: the base branch
// resolved the FOREIGN import when both --resume and --from-* were given.
// The export path must keep that precedence.
func TestTheExportResolvesTheForeignImportBeforeResume(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dataDir)
	cwd := t.TempDir()
	path := compressTestSession(t, dataDir, cwd)

	_, err := exportSourceSession(cwd, printOptions{
		ResumePrefix: compressTestShortID(path),
		FromClaude:   "no-such-transcript",
	})
	if err == nil {
		t.Fatal("the foreign import should have been resolved (and failed)")
	}
	if !strings.Contains(err.Error(), "claude") {
		t.Fatalf("err = %v, want the foreign import to be resolved BEFORE --resume", err)
	}
}

// TestTheExportOfAContinuationWithNoHistoryKeepsItsTitle: the base branch
// exported the empty session a --continue would have started, under the
// continuation title. The exported document must keep that title.
func TestTheExportOfAContinuationWithNoHistoryKeepsItsTitle(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dataDir)
	cwd := t.TempDir()

	st, err := exportSourceSession(cwd, printOptions{ContinueLast: true})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if !strings.HasPrefix(st.Title(), "continued print ") {
		t.Fatalf("title = %q, want the continuation title", st.Title())
	}
}
