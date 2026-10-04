package agent

import (
	"sync"
	"testing"
)

// The pin has to reach the compaction ladder, not just the field a reader
// inspects: maybeCompact/HandoffDue both gate on `ContextWindow <= 0`, so a
// pin that only touched the literal would leave the running turn on the old
// window — which is exactly what "take effect immediately" must not do.
func TestSetContextWindowLayersOverTheLiteral(t *testing.T) {
	a := &Agent{Compaction: CompactionConfig{ContextWindow: 200_000, KeepRecentTokens: 10}}
	if got := a.compaction().ContextWindow; got != 200_000 {
		t.Fatalf("no pin = the literal, got %d", got)
	}

	a.SetContextWindow(1_000_000)
	if got := a.compaction().ContextWindow; got != 1_000_000 {
		t.Fatalf("pinned window = %d, want 1000000", got)
	}
	// The threshold follows the pin, so compaction actually moves.
	if pinned, bare := a.compaction().threshold(), (CompactionConfig{ContextWindow: 200_000}).threshold(); pinned <= bare {
		t.Fatalf("a 1M pin must raise the compaction threshold: %d vs %d", pinned, bare)
	}

	// auto hands the agent straight back to the window it was built with.
	a.SetContextWindow(0)
	if got := a.compaction().ContextWindow; got != 200_000 {
		t.Fatalf("cleared pin = %d, want the literal 200000", got)
	}
}

// windowPin is written from the TUI's key thread and read from Run's own
// goroutine, so the whole point of the atomic is that this compiles clean
// under -race with the two sides running at once.
func TestSetContextWindowConcurrentWithReads(t *testing.T) {
	a := &Agent{Compaction: CompactionConfig{ContextWindow: 200_000}}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for _, w := range []int{500_000, 0, 1_000_000, 300_000, 0} {
			a.SetContextWindow(w)
		}
	}()
	go func() {
		defer wg.Done()
		for range 200 {
			if got := a.compaction().ContextWindow; got < 0 {
				t.Errorf("window %d", got)
			}
			a.compaction().threshold()
		}
	}()
	wg.Wait()
}
