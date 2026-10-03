package stats

import (
	"math"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// compactionEntry builds a compaction entry whose summary carries a billed
// request's usage — what the summarize / handoff path persists after the
// compaction-billing fix. side is nil for a deterministic member (shake, soft,
// snapcompact), which makes no provider call.
func compactionEntry(id, anchor, model string, ts time.Time, side *ai.Usage) session.Entry {
	return &session.CompactionEntry{
		Env: session.Envelope{ID: id, Timestamp: ts},
		Summary: ai.Message{
			Role: ai.RoleAssistant, Model: model, StopReason: ai.StopReasonStop,
			Content: []ai.Block{ai.TextBlock{Text: "summary of what came before"}},
			Usage:   side,
		},
		FirstKeptEntryID: &anchor,
		TokensBefore:     9_000,
		Method:           "handoff",
	}
}

// TestScanChargesCompactionUsage is the billing contract on the scan side: the
// summarize / handoff-document call is a provider request the session paid
// for, and the scan only ever read message entries — so its tokens and
// dollars reached no counter and a session that compacted read as if it had
// spent nothing. It is deliberately NOT a turn: "turns" answers how many
// times the model was asked to advance the work, and a summarize asks it to
// compress instead.
func TestScanChargesCompactionUsage(t *testing.T) {
	dataDir := t.TempDir()
	ts := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	side := &ai.Usage{
		Input: 40, Output: 8, CacheRead: 2_000, CacheWrite: 0, TotalTokens: 2_048,
		Cost: &ai.UsageCost{Total: 0.0006},
	}
	entries := []session.Entry{
		userEntry("u1", ts, "do it"),
		assistantEntry("a1", "m1", ts.Add(time.Second), "read"),
		compactionEntry("c1", "a1", "m1", ts.Add(2*time.Second), side),
	}
	writeSessionFile(t, dataDir, "-tmp-fixture", "sess-compact", "/tmp/fixture", "auto", ts, entries)

	rep, err := Scan(Options{DataDir: dataDir})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	tot := rep.Totals
	// The turn's 115 plus the summarize's 2_048.
	if tot.TotalTokens != 115+2_048 {
		t.Errorf("totalTokens = %d, want %d (the side call is charged)", tot.TotalTokens, 115+2_048)
	}
	if tot.Input != 100+40 || tot.CacheRead != 5+2_000 || tot.Output != 10+8 {
		t.Errorf("buckets = in %d out %d read %d, want 140/18/2005", tot.Input, tot.Output, tot.CacheRead)
	}
	if math.Abs(tot.CostUSD-0.0016) > 1e-9 {
		t.Errorf("cost = %v, want 0.0016", tot.CostUSD)
	}
	// PricedTurns counts PRICED BILLED REQUESTS, so the side call counts here
	// — but Turns must not, or "turns" would claim the compaction was work.
	if tot.Turns != 1 {
		t.Errorf("turns = %d, want 1 — a summarize is not a turn", tot.Turns)
	}
	if tot.PricedTurns != 2 {
		t.Errorf("pricedTurns = %d, want 2", tot.PricedTurns)
	}
	// The per-model row carries the tokens under the model that was billed,
	// with its turn count unmoved.
	if len(rep.Models) != 1 {
		t.Fatalf("models = %d, want 1", len(rep.Models))
	}
	m := rep.Models[0]
	if m.Model != "m1" || m.Turns != 1 || m.TotalTokens != 115+2_048 {
		t.Errorf("model row = %+v, want m1 / 1 turn / %d tokens", m, 115+2_048)
	}
	// The day rollup takes tokens and dollars; its turn count stays 1.
	if len(rep.Days) != 1 {
		t.Fatalf("days = %d, want 1", len(rep.Days))
	}
	for _, d := range rep.Days {
		if d.Turns != 1 || d.Tokens != 115+2_048 {
			t.Errorf("day row = %+v, want 1 turn / %d tokens", d, 115+2_048)
		}
	}
}

// TestScanIgnoresCompactionWithoutUsage is the other half of the contract: a
// deterministic member (shake, soft, snapcompact) makes no provider call, so
// its summary carries no usage and must contribute exactly nothing. A
// synthesized zero would make the cache-hit rate and the cost line wobble on
// every offline compaction.
func TestScanIgnoresCompactionWithoutUsage(t *testing.T) {
	dataDir := t.TempDir()
	ts := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	entries := []session.Entry{
		userEntry("u1", ts, "do it"),
		assistantEntry("a1", "m1", ts.Add(time.Second), "read"),
		compactionEntry("c1", "a1", "m1", ts.Add(2*time.Second), nil),
	}
	writeSessionFile(t, dataDir, "-tmp-fixture", "sess-shake", "/tmp/fixture", "auto", ts, entries)

	rep, err := Scan(Options{DataDir: dataDir})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if rep.Totals.TotalTokens != 115 {
		t.Errorf("totalTokens = %d, want 115 (an offline compaction costs nothing)", rep.Totals.TotalTokens)
	}
	if rep.Totals.PricedTurns != 1 || rep.Totals.Turns != 1 {
		t.Errorf("turns/priced = %d/%d, want 1/1", rep.Totals.Turns, rep.Totals.PricedTurns)
	}
}
