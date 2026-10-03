package stats

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
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

// TestModelRowCarriesTheCacheWriteBucket pins R-USE-3's gap: the whole-store
// Totals carried CacheWrite while the per-model row did not, so the dashboard's
// "which model writes the cache" question had no answer — and the write side is
// the expensive half at Anthropic's 1.25x the input rate. The row is also the
// place a cache-key audit looks: a model whose share of the store's cache
// writes is not its share of its reads is a session pinning the wrong prefix.
//
// The rollup cache is what makes this a real fix and not just a struct field:
// modelCounters is serialized into stats/rollup.json keyed on file size+mtime,
// so a field added without bumping rollupVersion reads back as 0 from every
// session scanned before the change.
func TestModelRowCarriesTheCacheWriteBucket(t *testing.T) {
	dataDir := t.TempDir()
	ts := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	write := &session.MessageEntry{
		Env: session.Envelope{ID: "w1", Timestamp: ts},
		Message: ai.Message{
			Role: ai.RoleAssistant, Model: "writer", StopReason: ai.StopReasonStop,
			Content: []ai.Block{ai.TextBlock{Text: "ok"}},
			Usage: &ai.Usage{
				Input: 300, Output: 60, CacheRead: 900, CacheWrite: 1_200, TotalTokens: 2_460,
				Cost: &ai.UsageCost{Total: 0.0021},
			},
		},
	}
	read := assistantEntry("r1", "reader", ts.Add(time.Second), "read")
	writeSessionFile(t, dataDir, "-tmp-fixture", "sess-w", "/tmp/fixture", "auto", ts, []session.Entry{
		userEntry("u1", ts, "go"), write, read,
	})

	rep, err := Scan(Options{DataDir: dataDir, NoRollup: true})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	byModel := map[string]ModelStat{}
	for _, m := range rep.Models {
		byModel[m.Model] = m
	}
	w, ok := byModel["writer"]
	if !ok {
		t.Fatalf("models = %+v, want a writer row", rep.Models)
	}
	if w.CacheWrite != 1_200 {
		t.Errorf("writer cacheWrite = %d, want 1200", w.CacheWrite)
	}
	if w.CacheRead != 900 || w.TotalTokens != 2_460 {
		t.Errorf("writer row = %+v, want read 900 / total 2460", w)
	}
	// The row's own buckets must add up to its own total, the same invariant
	// the store-wide totals hold — that is what makes a per-model number
	// auditable against the whole store.
	if got := w.Input + w.Output + w.CacheRead + w.CacheWrite; got != w.TotalTokens {
		t.Errorf("writer buckets sum to %d, want the row's total %d", got, w.TotalTokens)
	}
	// The model with no writes stays at zero rather than inheriting the other
	// model's — the per-model split is the point.
	r, ok := byModel["reader"]
	if !ok {
		t.Fatalf("models = %+v, want a reader row", rep.Models)
	}
	if r.CacheWrite != 0 {
		t.Errorf("reader cacheWrite = %d, want 0", r.CacheWrite)
	}
	// The store-wide bucket is unchanged: this only moves where it is reported.
	if rep.Totals.CacheWrite != 1_200 {
		t.Errorf("totals cacheWrite = %d, want 1200", rep.Totals.CacheWrite)
	}
}

// TestRollupCacheInvalidatesOnNewCounter is the other half: the per-model
// counters ride the on-disk rollup cache, which is keyed on file size+mtime —
// neither of which changes when a new field is added to the struct. Without the
// version bump, every session scanned before the upgrade reports cacheWrite 0
// forever, and the bug this fixes silently comes back for existing users.
func TestRollupCacheInvalidatesOnNewCounter(t *testing.T) {
	dataDir := t.TempDir()
	ts := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	entries := []session.Entry{
		userEntry("u1", ts, "go"),
		&session.MessageEntry{
			Env: session.Envelope{ID: "w1", Timestamp: ts},
			Message: ai.Message{
				Role: ai.RoleAssistant, Model: "writer", StopReason: ai.StopReasonStop,
				Content: []ai.Block{ai.TextBlock{Text: "ok"}},
				Usage:   &ai.Usage{Input: 10, Output: 5, CacheWrite: 700, TotalTokens: 715},
			},
		},
	}
	path := writeSessionFile(t, dataDir, "-tmp-fixture", "sess-w", "/tmp/fixture", "auto", ts, entries)

	// First scan writes the cache.
	if _, err := Scan(Options{DataDir: dataDir}); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "stats", "rollup.json")); err != nil {
		t.Fatalf("the first scan wrote no rollup cache: %v", err)
	}
	// Second scan reads it: same numbers, from the cache.
	rep, err := Scan(Options{DataDir: dataDir})
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if len(rep.Models) != 1 || rep.Models[0].CacheWrite != 700 {
		t.Fatalf("cached scan = %+v, want one row with cacheWrite 700", rep.Models)
	}

	// Forge an OLD cache — the previous schema version, carrying the counters
	// as they looked THEN: a valid entry keyed on this file's real size and
	// mtime, whose per-model row has no cacheWrite field at all. That is
	// exactly what a user upgrading with a rollup.json already on disk has,
	// and it is the only way to see the bug this bump prevents: `get` matches
	// on size+mtime (both unchanged), so without the version check the stale
	// counters are trusted and the model reports 0 cache writes forever.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the fixture: %v", err)
	}
	staleCounters := counters{Turns: 1, TotalTokens: 715}
	if staleCounters.Models == nil {
		staleCounters.Models = map[string]modelCounters{}
	}
	staleCounters.Models["writer"] = modelCounters{Turns: 1, Input: 10, Output: 5, TotalTokens: 715}
	// Pinned, not computed: this test is only meaningful if the constant
	// below IS the previous schema. rollupVersion-1 would still pass if the
	// constant were ever bumped twice (the forged file would then be a schema
	// nobody ever wrote, and loadRollup's mismatch check would reject it for
	// the wrong reason).
	const previousRollupVersion = 2
	if rollupVersion != previousRollupVersion+1 {
		t.Fatalf("rollupVersion = %d; this fixture forges %d and only reproduces the bug if they differ by one",
			rollupVersion, previousRollupVersion)
	}
	stale := rollupFile{Version: previousRollupVersion, Entries: rollup{
		path: {Size: info.Size(), Mod: info.ModTime().UnixNano(), C: staleCounters},
	}}
	body, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "stats", "rollup.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	// Prove the fixture is a real hit candidate. get() is what Scan calls and
	// it does NOT look at the version — so if this returns true, the entry is
	// a genuine hit candidate and the version check in loadRollup is the only
	// thing standing between it and a permanently under-reported number. This
	// is the assertion that fails if the bump is ever reverted: loadRollup
	// would then hand the same entry to Scan, get() would match on size+mtime,
	// and the stale counters (no cacheWrite) would be folded as fact.
	if _, ok := (rollup(stale.Entries)).get(session.SessionMeta{
		Path: path, SizeBytes: info.Size(), ModTime: info.ModTime(),
	}); !ok {
		t.Fatal("the forged entry does not match on size+mtime, so it cannot reproduce the bug")
	}

	rep, err = Scan(Options{DataDir: dataDir})
	if err != nil {
		t.Fatalf("Scan over a stale cache: %v", err)
	}
	if len(rep.Models) != 1 || rep.Models[0].CacheWrite != 700 {
		t.Fatalf("a stale cache was trusted: models = %+v, want cacheWrite 700 from a full rescan", rep.Models)
	}
	if rep.CacheHits != 0 {
		t.Errorf("cacheHits = %d, want 0 — a foreign-version cache must be dropped whole", rep.CacheHits)
	}
}
