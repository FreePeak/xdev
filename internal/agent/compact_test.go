package agent

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/memlimit"
	"github.com/FreePeak/xdev/internal/session"
)

func stubPressure(t *testing.T, p float64) {
	t.Helper()
	old := memPressure
	memPressure = func() float64 { return p }
	t.Cleanup(func() { memPressure = old })
}

// TestParseMethodOrder covers the compaction.methodOrder parser: order is
// preserved, unknown names are dropped (a typo must not silently disable
// compaction), duplicates collapse, and an empty or all-invalid value means
// the shipped ladder.
func TestParseMethodOrder(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", []string{"threshold", "overflow", "promotion"}},
		{"threshold", []string{"threshold"}},
		{"promotion, threshold", []string{"promotion", "threshold"}},
		{"threshold,overflow,promotion", []string{"threshold", "overflow", "promotion"}},
		{"THRESHOLD", []string{"threshold"}},
		{"bogus,threshold", []string{"threshold"}},
		{"bogus", []string{"threshold", "overflow", "promotion"}},
		{"threshold,threshold", []string{"threshold"}},
		{",,", []string{"threshold", "overflow", "promotion"}},
	}
	for _, tc := range tests {
		if got := ParseMethodOrder(tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("ParseMethodOrder(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestMethodOrderWithoutThresholdDisablesStepCompaction pins the knob's
// consumer: with `threshold` absent from the order, a step boundary must not
// compact even though the context is far over the window (the reactive
// strategies handle real overflow instead).
func TestMethodOrderWithoutThresholdDisablesStepCompaction(t *testing.T) {
	stubPressure(t, 0)
	p := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{textEvent("first"), doneEvent("first")}},
		{events: []ai.Event{textEvent("second"), doneEvent("second")}},
	}}
	cfg := CompactionConfig{ContextWindow: 1000, KeepRecentTokens: 5, Methods: []string{"overflow", "promotion"}}
	a, s, _ := storeAgent(t, p, cfg)
	a.Retry = fastRetry()

	if _, err := a.Run(context.Background(), "sys", submitHistory(t, s, strings.Repeat("x", 4000))); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if _, err := a.Run(context.Background(), "sys", submitHistory(t, s, "second user")); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if len(p.gotReqs) != 2 {
		t.Fatalf("stream calls = %d, want 2 (no summary call)", len(p.gotReqs))
	}
	for _, e := range s.Entries() {
		if _, ok := e.(*session.CompactionEntry); ok {
			t.Fatal("threshold absent from methodOrder must not compact at a step boundary")
		}
	}
}

// TestMemoryPressureForcesCompaction pins the hard backstop: at the pressure
// threshold the step boundary compacts even though the token threshold can
// never fire (huge window) and even though `threshold` is absent from the
// method order.
func TestMemoryPressureForcesCompaction(t *testing.T) {
	stubPressure(t, memlimit.HighPressure)

	p := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{textEvent("first"), doneEvent("first")}},
		{events: []ai.Event{textEvent("summary of past"), doneEvent("summary of past")}},
		{events: []ai.Event{textEvent("second"), doneEvent("second")}},
	}}
	cfg := CompactionConfig{ContextWindow: 1 << 20, KeepRecentTokens: 5, Methods: []string{"promotion"}}
	a, s, _ := storeAgent(t, p, cfg)
	a.Retry = fastRetry()

	if _, err := a.Run(context.Background(), "sys", submitHistory(t, s, strings.Repeat("x", 4000))); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	// Pressure risen before the second turn: its request must carry the
	// rebuilt, compacted context (summary call in between).
	if _, err := a.Run(context.Background(), "sys", submitHistory(t, s, "second user")); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if len(p.gotReqs) != 3 {
		t.Fatalf("stream calls = %d, want 3 (turn, summary, turn)", len(p.gotReqs))
	}
	compacted := false
	for _, e := range s.Entries() {
		if _, ok := e.(*session.CompactionEntry); ok {
			compacted = true
		}
	}
	if !compacted {
		t.Fatal("memory pressure must persist a compaction entry")
	}
}
