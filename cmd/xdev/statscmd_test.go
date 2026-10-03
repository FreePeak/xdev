package main

import (
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/stats"
)

// TestStatsByModelShowsBothCacheBuckets is the CLI half of R-USE-3: the
// per-model rows reported the store's cache READ but not its cache WRITE, so
// "which model is writing the cache" had no answer in the one command a user
// runs to audit spend. The write side is the expensive half — Anthropic
// charges it at 1.25× the input rate — so hiding it is hiding the cost.
func TestStatsByModelShowsBothCacheBuckets(t *testing.T) {
	rep := stats.Report{
		Totals: stats.Totals{Turns: 2, PricedTurns: 2},
		Models: []stats.ModelStat{
			{Model: "writer", Sessions: 1, Turns: 1, Input: 300, Output: 60,
				CacheRead: 900, CacheWrite: 1_200, TotalTokens: 2_460, CostUSD: 0.0021},
			{Model: "reader", Sessions: 1, Turns: 1, Input: 400, Output: 40,
				CacheRead: 5_000, CacheWrite: 0, TotalTokens: 5_440, CostUSD: 0.001},
		},
	}
	out := formatStatsReport(&rep)
	if !strings.Contains(out, "CACHE R") || !strings.Contains(out, "CACHE W") {
		t.Fatalf("the by-model header has no cache columns:\n%s", out)
	}
	// The writer's write bucket must be visible as its own figure.
	if !strings.Contains(out, "1.2k") {
		t.Fatalf("the model's cache write is not in the report:\n%s", out)
	}
	// And the reader's zero must be printed rather than elided: a column that
	// only appears when nonzero is how the writer's cost stayed invisible in
	// the first place. This is the row that says "reads a lot, writes nothing".
	var reader string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "reader") {
			reader = line
		}
	}
	if reader == "" {
		t.Fatalf("the reader row is missing:\n%s", out)
	}
	if !strings.Contains(reader, "5.0k") {
		t.Errorf("the reader row lost its cache read: %q", reader)
	}
}
