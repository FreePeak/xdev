package session

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// M8 exit criterion: a worst-case transcript must stay inside the PRD
// <100 MB RSS budget, because windowed materialization keeps only the
// post-boundary tail in memory (the pre-boundary history is already
// summarized into the compaction entry).
//
// The fixture is deliberately large: 50k assistant messages of 2 KB each
// (~109 MB on disk) with a mid-file compaction. Before windowing this
// loaded to 130 MB of heap; with it, ~65 MB.
func TestWorstCaseTranscriptStaysUnderBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("buffers a 109 MB fixture")
	}
	const (
		entries    = 50000
		bodySize   = 2000
		budgetByte = 100 << 20
	)
	dir := t.TempDir()
	path := filepath.Join(dir, "big.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, bodySize)
	for i := range body {
		body[i] = 'x'
	}
	for i := 0; i < entries; i++ {
		fmt.Fprintf(f, `{"type":"message","id":"e%d","parentId":"e%d","timestamp":"2026-09-11T00:00:00Z","message":{"role":"assistant","content":[{"type":"text","text":"%s"}],"provider":"p","model":"m"}}`+"\n", i, i-1, body)
		if i == entries/2 {
			fmt.Fprintf(f, `{"type":"compaction","id":"c%d","parentId":"e%d","timestamp":"2026-09-11T00:00:00Z","summary":{"role":"user","content":[{"type":"text","text":"summary"}]},"firstKeptEntryId":"e%d","tokensBefore":100000}`+"\n", i, i, i+1)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	entries2 := s.Entries()
	if _, err := BuildContext(entries2, s.LeafID(), SystemPrompt{}); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	retained, seen, windowed := s.WindowStats()
	if !windowed || retained >= seen {
		t.Fatalf("windowing did not engage: parsed=%d retained=%d windowed=%v", seen, retained, windowed)
	}
	// The budget is on the heap the transcript owns. Sys (the runtime's
	// virtual reservation, GC metadata included) is not the measure —
	// HeapAlloc is what the PRD budget bounds.
	if ms.HeapAlloc > budgetByte {
		t.Fatalf("heap after worst-case load = %d MB, budget = %d MB (parsed=%d retained=%d)",
			ms.HeapAlloc>>20, budgetByte>>20, seen, retained)
	}
	t.Logf("heap=%d MB retained=%d/%d after a %d MB transcript",
		ms.HeapAlloc>>20, retained, seen, fileSizeOf(path)>>20)
}

func fileSizeOf(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}
