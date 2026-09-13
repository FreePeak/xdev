package session

import (
	"bufio"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// The ai-title fallback (4fb70ef) made rename land on a LIVE session: the
// generator runs on its own goroutine while the turn goroutine keeps
// appending. Rename rewrites byte 0..TitleSlotWidth with WriteAt while the
// buffered writer appends — so assert the overlap cannot deadlock, tear a
// line, or make the file unparseable.
func TestRenameRacingAppendsStaysConsistent(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/s.jsonl"
	st := OpenMem("/proj", "original")
	if _, err := st.EnsureOnDisk(p, Options{}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	stop := make(chan struct{})

	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 400; i++ {
			if err := st.Append(userMsg("m", "", strings.Repeat("payload", 16))); err != nil {
				errs <- err
				return
			}
			if i%16 == 0 {
				runtime.Gosched()
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 400; i++ {
			title := "title number"
			if i%2 == 0 {
				title = "a far longer manual title than the slot's original content was"
			}
			if err := st.Rename(title, TitleSourceManual); err != nil {
				errs <- err
				return
			}
			if i%16 == 0 {
				runtime.Gosched()
			}
		}
	}()

	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case err := <-errs:
		close(stop)
		t.Fatalf("concurrent rename/append error: %v", err)
	case <-time.After(8 * time.Second):
		close(stop)
		t.Fatal("rename and append did not finish: the store serialises them badly enough to stall the session (rename runs on the TUI's own goroutine)")
	}
	close(stop)
	wg.Wait()

	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	n := 0
	for sc.Scan() {
		line := sc.Text()
		if n == 0 {
			// The slot is TitleSlotWidth bytes INCLUDING its newline; the
			// scanner strips the newline, so the line measures one less.
			if len(line) != TitleSlotWidth-1 {
				t.Fatalf("title slot width = %d, want %d (slot minus newline)", len(line), TitleSlotWidth-1)
			}
		} else if !strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}") {
			t.Fatalf("line %d torn by the offset-0 rewrite: %.60q", n, line)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan after concurrent rename: %v", err)
	}
	if n < 3 {
		t.Fatalf("only %d line(s) survived", n)
	}
	reopened, err := Open(p)
	if err != nil {
		t.Fatalf("file no longer opens after the race: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}
