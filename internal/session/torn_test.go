package session

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
)

// #122: what a crash mid-write leaves behind, and what a second process
// touching the same file means. The bug this closes was worse than "lose the
// last line": an unterminated final line made Open fail outright, so a session
// that was 99% intact became permanently unresumable.

// writeClean returns a three-record session file plus its exact bytes.
func writeClean(t *testing.T, path string) []byte {
	t.Helper()
	s := OpenMem(t.TempDir(), "clean")
	if _, err := s.EnsureOnDisk(path, Options{}); err != nil {
		t.Fatal(err)
	}
	for _, e := range []Entry{userMsg("", "", "one"), asstMsg("", "", "two"), userMsg("", "", "three")} {
		if err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func appendTorn(t *testing.T, path, fragment string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(fragment); err != nil {
		t.Fatal(err)
	}
}

func TestTornTailStillOpensAndReports(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	clean := writeClean(t, path)
	const torn = `{"type":"message","id":"half-writt`
	appendTorn(t, path, torn)

	// Reading must not mutate: the repair is applied by the first append.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("a torn tail must not brick the session: %v", err)
	}
	if got := len(s.Entries()); got != 3 {
		t.Fatalf("entries = %d, want the 3 intact records", got)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, append(clean, torn...)) {
		t.Fatal("Open rewrote the file; reading must be side-effect free")
	}
	notice := s.RepairNotice()
	for _, want := range []string{"interrupted write", fmt.Sprintf("%d byte", len(torn))} {
		if !strings.Contains(notice, want) {
			t.Fatalf("RepairNotice = %q, missing %q", notice, want)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFirstAppendCutsTheTornTailAndPoisonsNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	clean := writeClean(t, path)
	appendTorn(t, path, `{"type":"message","id":"half-writt`)

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(userMsg("", "", "four")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// The file is now exactly the clean session plus the new record: the
	// fragment is gone, and the new line was not glued onto it.
	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after the repair: %v", err)
	}
	if got := len(again.Entries()); got != 4 {
		t.Fatalf("entries = %d, want 4 (the poisoned-append case lost records here)", got)
	}
	if last := again.Entries()[3].(*MessageEntry); last.Message.Text() != "four" {
		t.Fatalf("last entry = %q", last.Message.Text())
	}
	if again.RepairNotice() != "" {
		t.Fatalf("a repaired file must reopen clean, got %q", again.RepairNotice())
	}
	if size := int64(len(clean)); size >= int64(len(mustRead(t, path))) {
		t.Fatalf("the repaired file should be the clean bytes plus one record, got %d vs %d", len(mustRead(t, path)), size)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestSecondWriterIsRefused: another process committed real records. Cutting
// them away would delete someone else's history and appending over them would
// fork the tree silently, so the store stops instead.
func TestSecondWriterIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	writeClean(t, path)

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// A foreign complete record appears after our committed boundary.
	appendTorn(t, path, `{"type":"message","id":"foreign","type2":2}`+"\n")

	err = s.Append(userMsg("", "", "mine"))
	if !errors.Is(err, ErrConcurrentWriter) {
		t.Fatalf("err = %v, want ErrConcurrentWriter", err)
	}
	if !strings.Contains(err.Error(), "did not write") {
		t.Errorf("err = %v, want it to name the cause", err)
	}
	// The foreign bytes survive: refusing is not the same as overwriting.
	raw := mustRead(t, path)
	if !bytes.Contains(raw, []byte("foreign")) {
		t.Fatal("the other writer's record was clobbered")
	}
	// And the latch holds: every later append reports the same thing.
	if err := s.Append(userMsg("", "", "again")); !errors.Is(err, ErrConcurrentWriter) {
		t.Fatalf("second Append err = %v, want the latch", err)
	}
	_ = s.Close()
}

// TestShrunkFileIsRefused: the file got shorter than what we committed, so the
// history we believe exists is gone. Appending would produce a file whose
// records reference entries nobody can read.
func TestShrunkFileIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	clean := writeClean(t, path)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, int64(len(clean))-20); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(userMsg("", "", "x")); !errors.Is(err, ErrConcurrentWriter) {
		t.Fatalf("err = %v, want ErrConcurrentWriter for a shrank file", err)
	}
	_ = s.Close()
}

func TestHardLinkedSessionIsNotResumable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	writeClean(t, path)
	link := filepath.Join(dir, "same-bytes.jsonl")
	if err := os.Link(path, link); err != nil {
		t.Fatalf("hard link: %v", err)
	}
	_, err := Open(path)
	if err == nil || !strings.Contains(err.Error(), "reachable by 2 paths") {
		t.Fatalf("Open of a linked session = %v, want the refusal", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err != nil {
		t.Fatalf("once the extra link is gone the session must resume: %v", err)
	}
}

// TestCrashMidWriteLeavesOnlyCompleteRecords is #122's acceptance: SIGKILL the
// writer during a real append, then require that the file reopens and holds
// exactly the records that finished. The child is this test binary re-invoked;
// the kill is unconditional, so whatever the write happened to be doing is what
// durability has to survive.
func TestCrashMidWriteLeavesOnlyCompleteRecords(t *testing.T) {
	if target := os.Getenv("XDEV_TORN_HELPER"); target != "" {
		runTornHelper(target)
		return
	}
	var tornSeen, runs int
	for attempt := 0; attempt < 12; attempt++ {
		path := filepath.Join(t.TempDir(), "crash.jsonl")
		cmd := exec.Command(os.Args[0], "-test.run=TestCrashMidWriteLeavesOnlyCompleteRecords", "-test.timeout=90s")
		cmd.Env = append(os.Environ(), "XDEV_TORN_HELPER="+path)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		// Wait() would block until the child finished, so the order matters:
		// observe growth, kill without notice, then reap. The varying pause
		// changes where the kill lands inside the next flush.
		waitForFileGrow(path, 1<<20)
		time.Sleep(time.Duration(attempt%4) * time.Millisecond)
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		_, _ = cmd.Process.Wait()
		runs++

		before := mustRead(t, path)
		s, err := Open(path)
		if err != nil {
			t.Fatalf("attempt %d: the session bricked on a crash: %v", attempt, err)
		}
		if !bytes.HasSuffix(before, []byte("\n")) {
			tornSeen++
			if s.RepairNotice() == "" {
				t.Fatalf("attempt %d: a torn tail must be reported", attempt)
			}
		}
		// Every record it returns must be complete and parseable, and the ids
		// must chain without holes: the crash may lose the tail, never the
		// meaning of what came before.
		ids := map[string]bool{}
		for _, e := range s.Entries() {
			env := e.Envelope()
			if env.ID == "" || ids[env.ID] {
				t.Fatalf("attempt %d: duplicate or missing id %q", attempt, env.ID)
			}
			ids[env.ID] = true
			if env.ParentID != "" && !ids[env.ParentID] {
				t.Fatalf("attempt %d: entry %s points at parent %s that never completed", attempt, env.ID, env.ParentID)
			}
		}
		// And the file becomes byte-clean on the next write.
		if err := s.Append(userMsg("", "", "after-crash")); err != nil {
			t.Fatalf("attempt %d: append after a crash: %v", attempt, err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("attempt %d: close: %v", attempt, err)
		}
		after := mustRead(t, path)
		if !bytes.HasSuffix(after, []byte("\n")) {
			t.Fatalf("attempt %d: the repaired file must end on a record boundary", attempt)
		}
		reopened, err := Open(path)
		if err != nil {
			t.Fatalf("attempt %d: reopen after the repair: %v", attempt, err)
		}
		if n := reopened.RepairNotice(); n != "" {
			t.Fatalf("attempt %d: the file must be clean after the repair, got %q", attempt, n)
		}
	}
	// What this run established: 12 real SIGKILLs of a writer pushing ~9 MB
	// records never produced a torn tail on this platform — the kernel commits
	// an append only after copying it. The repair is therefore tested by
	// constructing the byte state directly (TestTornTailStillOpensAndReports,
	// TestFirstAppendCutsTheTornTailAndPoisonsNothing), which is also the state a
	// power loss or a delayed-allocation filesystem leaves behind. What this loop
	// does assert is the property that matters to a user: no timing of a real
	// SIGKILL ever left a session that could not be reopened.
	t.Logf("%d real SIGKILLs, %d landed mid-line; every session reopened intact", runs, tornSeen)
}

// runTornHelper is the crash victim: it appends huge records continuously so a
// SIGKILL has the widest possible chance of landing inside one. See the note
// above for what this did and did not observe.
func runTornHelper(path string) {
	s := OpenMem(filepath.Dir(path), "crash helper")
	if _, err := s.EnsureOnDisk(path, Options{}); err != nil {
		os.Exit(2)
	}
	// ~9 MB a record: the write spans many kernel syscalls, so a kill has a
	// wide window to land inside one and leave a genuine torn tail.
	payload := strings.Repeat("half-written-payload-", 400000)
	for i := 0; i < 4000; i++ {
		if err := s.Append(&MessageEntry{Message: ai.Message{
			Role:    ai.RoleUser,
			Content: []ai.Block{ai.TextBlock{Text: payload}},
		}}); err != nil {
			return // latched: the parent is about to kill us anyway
		}
	}
}

// partialWriter forwards the first n bytes of one write to the real file and
// then fails, which is exactly what an interrupted flush leaves: bytes on disk
// that are not a whole record.
type partialWriter struct {
	w    *os.File
	keep int
}

func (p *partialWriter) Write(b []byte) (int, error) {
	if p.keep <= 0 {
		return 0, errors.New("simulated disk failure")
	}
	n := min(len(b), p.keep)
	p.keep -= n
	if _, err := p.w.Write(b[:n]); err != nil {
		return 0, err
	}
	return len(b), errors.New("simulated disk failure")
}

// TestFailedWriteRollsTheFileBack is #122's third fx step, tested
// deterministically where the SIGKILL loop cannot reach it on this platform: a
// record half-written to disk, and the write then failing. The store must latch
// AND cut the file back to its last complete record, so the next session that
// opens this file sees a clean history instead of a torn line — and the bytes
// that were refused must not be flushed a second time.
func TestFailedWriteRollsTheFileBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	clean := writeClean(t, path)

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	f := s.f
	if f == nil {
		if err := s.openWriterLocked(); err != nil {
			t.Fatal(err)
		}
		f = s.f
	}
	s.w = bufio.NewWriterSize(&partialWriter{w: f, keep: 12}, 1<<16)

	if err := s.Append(asstMsg("", "", strings.Repeat("x", 5000))); err == nil {
		t.Fatal("a failing write must return an error")
	} else if !strings.Contains(err.Error(), "simulated disk failure") {
		t.Fatalf("err = %v", err)
	}
	// The 12 forwarded bytes are rolled back, not left to poison the tail.
	if raw := mustRead(t, path); !bytes.Equal(raw, clean) {
		t.Fatalf("the file must be byte-identical to its last complete record:\n got %d bytes, want %d\n tail: %q", len(raw), len(clean), raw[len(clean):min(len(raw), len(clean)+20)])
	}
	// Latched forever, and the refused bytes are never written later.
	if err := s.Append(userMsg("", "", "later")); err == nil {
		t.Fatal("the latch must rethrow")
	}
	if raw := mustRead(t, path); !bytes.Equal(raw, clean) {
		t.Fatal("Close or a refused append must not flush bytes that were rolled back")
	}
	_ = s.Close()
	if raw := mustRead(t, path); !bytes.Equal(raw, clean) {
		t.Fatal("Close wrote the refused bytes")
	}
	if _, err := Open(path); err != nil {
		t.Fatalf("the session must still be resumable after the failed write: %v", err)
	}
}

// waitForFileGrow blocks until path exists and is at least n bytes, or a bound
// elapses. It is a scheduling aid for the kill, not an assertion.
func waitForFileGrow(path string, n int64) {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := os.Stat(path); err == nil && st.Size() >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}
