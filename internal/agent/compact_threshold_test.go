package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

// The compaction trigger is omp's rule 25, copied as it stands: the reserve
// is 15% of the window (never less), and the head-room threshold is the
// window minus that reserve.
func TestCompactionThresholdFollowsTheOMPRule(t *testing.T) {
	tests := []struct {
		name         string
		cfg          CompactionConfig
		wantReserve  int64
		wantThreshol int64
	}{
		{
			name:         "the shipped reserve is the 15% floor",
			cfg:          CompactionConfig{ContextWindow: 128000},
			wantReserve:  19200,
			wantThreshol: 108800,
		},
		{
			name:         "a small window keeps its own 15%",
			cfg:          CompactionConfig{ContextWindow: 1000},
			wantReserve:  150,
			wantThreshol: 850,
		},
		{
			name:         "a configured reserve wins when it is bigger",
			cfg:          CompactionConfig{ContextWindow: 200000, ReserveTokens: 50000},
			wantReserve:  50000,
			wantThreshol: 150000,
		},
		{
			name:         "a reserve that eats the window collapses to the 15%",
			cfg:          CompactionConfig{ContextWindow: 200000, ReserveTokens: 200000},
			wantReserve:  30000,
			wantThreshol: 170000,
		},
		{
			name:         "a small configured reserve never beats the 15%",
			cfg:          CompactionConfig{ContextWindow: 128000, ReserveTokens: 1000},
			wantReserve:  19200,
			wantThreshol: 108800,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.reserve(); got != tc.wantReserve {
				t.Errorf("reserve = %d, want %d", got, tc.wantReserve)
			}
			if got := tc.cfg.threshold(); got != tc.wantThreshol {
				t.Errorf("threshold = %d, want %d", got, tc.wantThreshol)
			}
		})
	}
}

// The trigger fires when the used context is STRICTLY above the threshold:
// at the threshold the session still fits, one token more does not.
func TestTheTriggerFiresStrictlyAboveTheThreshold(t *testing.T) {
	stubPressure(t, 0)
	cfg := CompactionConfig{ContextWindow: 1000, KeepRecentTokens: 5, Methods: []string{"soft"}}
	threshold := cfg.threshold()
	if threshold != 850 {
		t.Fatalf("threshold = %d, want 850 (the window minus its 15%%)", threshold)
	}
	// The estimator charges 4 chars per token plus 8 per message, so 3368
	// chars of user text plus two short ones is exactly 850 tokens. The
	// compaction span needs a droppable prefix, hence three messages.
	at := []ai.Message{userText(strings.Repeat("x", 3368)), userText("keep"), userText("keep")}
	if got := contextTokens(at); got != threshold {
		t.Fatalf("fixture tokens = %d, want the threshold %d", got, threshold)
	}
	p := &fakeProvider{}
	a, s, _ := storeAgent(t, p, cfg)
	for _, m := range at {
		appendMsg(t, s, m)
	}
	_ = a.maybeCompact(context.Background(), at)
	if n := len(compactionEntries(s)); n != 0 {
		t.Fatalf("compaction entries = %d, want 0 (the context sits AT the threshold)", n)
	}
	over := []ai.Message{userText(strings.Repeat("x", 3372)), userText("keep"), userText("keep")}
	if got := contextTokens(over); got != threshold+1 {
		t.Fatalf("fixture tokens = %d, want %d", got, threshold+1)
	}
	p2 := &fakeProvider{}
	a2, s2, _ := storeAgent(t, p2, cfg)
	for _, m := range over {
		appendMsg(t, s2, m)
	}
	_ = a2.maybeCompact(context.Background(), over)
	if n := len(compactionEntries(s2)); n != 1 {
		t.Fatalf("compaction entries = %d, want 1 (the context is over the threshold)", n)
	}
}
