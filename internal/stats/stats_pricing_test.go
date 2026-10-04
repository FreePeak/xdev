package stats

import (
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// flatPrice is one model's rates per million tokens.
type flatPrice struct{ in, out, cacheRead, cacheWrite float64 }

func (p flatPrice) USD(in, out, cr, cw, total int64) float64 {
	if total <= 0 {
		total = in + out + cr + cw
	}
	return (float64(in)/1e6)*p.in + (float64(out)/1e6)*p.out +
		(float64(cr)/1e6)*p.cacheRead + (float64(cw)/1e6)*p.cacheWrite
}

// unpricedAssistantEntry is a turn whose provider reported tokens but no cost —
// the gateway/proxy/local-server case the price table exists for.
func unpricedAssistantEntry(id, model string, ts time.Time, u *ai.Usage) session.Entry {
	return &session.MessageEntry{
		Env: session.Envelope{ID: id, Timestamp: ts},
		Message: ai.Message{
			Role: ai.RoleAssistant, Model: model, StopReason: ai.StopReasonStop,
			Content: []ai.Block{ai.TextBlock{Text: "ok"}},
			Usage:   u,
		},
	}
}

// priceTable prices only the models it names; anything else returns nil.
type priceTable map[string]flatPrice

func (p priceTable) lookup(model string) Pricer {
	if pr, ok := p[model]; ok {
		return pr
	}
	return nil
}

// TestScanPricesUnpricedRequestsLocally is the point of the table: a provider
// that reports tokens and no cost leaves the store at $0.00 for work that was
// paid for. The estimate fills exactly that gap — every bucket priced on its
// own rate, so a cached turn is not charged the fresh-input rate.
func TestScanPricesUnpricedRequestsLocally(t *testing.T) {
	dataDir := t.TempDir()
	ts := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	entries := []session.Entry{
		userEntry("u1", ts, "do it"),
		unpricedAssistantEntry("a1", "m1", ts.Add(time.Second), &ai.Usage{
			Input: 1_000_000, Output: 100_000, CacheRead: 0, TotalTokens: 1_100_000,
		}),
	}
	writeSessionFile(t, dataDir, "-tmp-fixture", "sess-priced", "/tmp/fixture", "auto", ts, entries)

	// 3/M input · 15/M output · 3.75/M cache write.
	table := priceTable{"m1": {in: 3, out: 15, cacheWrite: 3.75}}
	rep, err := Scan(Options{DataDir: dataDir, NoRollup: true, Pricer: table.lookup})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got, want := rep.Totals.CostUSD, 3.0+1.5; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("cost = %v, want %v (1M in at 3 + 100k out at 15)", got, want)
	}
	if rep.Totals.CostReported != 0 {
		t.Errorf("costReported = %v, want 0 — nothing was reported here", rep.Totals.CostReported)
	}
	if rep.Totals.CostEstimated < 4.49 || rep.Totals.CostEstimated > 4.51 {
		t.Errorf("costEstimated = %v, want 4.5", rep.Totals.CostEstimated)
	}
	if rep.Totals.PricedRequests != 1 || rep.Totals.BilledRequests != 1 {
		t.Errorf("priced/billed = %d/%d, want 1/1", rep.Totals.PricedRequests, rep.Totals.BilledRequests)
	}
	if len(rep.Models) != 1 || rep.Models[0].CostUSD < 4.49 || rep.Models[0].CostUSD > 4.51 {
		t.Errorf("model row cost = %+v, want ~4.5 on m1", rep.Models)
	}
}

// TestScanTrustsTheProviderOverTheTable is the precedence rule: a reported
// price is believed and the local table never touches it. A price table is a
// gap-filler, not a second opinion — otherwise correcting a table would
// silently rewrite history that was actually billed.
func TestScanTrustsTheProviderOverTheTable(t *testing.T) {
	dataDir := t.TempDir()
	ts := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	entries := []session.Entry{
		userEntry("u1", ts, "do it"),
		assistantEntry("a1", "m1", ts.Add(time.Second)), // carries Cost 0.001
		unpricedAssistantEntry("a2", "m1", ts.Add(2*time.Second), &ai.Usage{
			Input: 1_000_000, TotalTokens: 1_000_000,
		}),
	}
	writeSessionFile(t, dataDir, "-tmp-fixture", "sess-mixed", "/tmp/fixture", "auto", ts, entries)

	table := priceTable{"m1": {in: 3, out: 15}}
	rep, err := Scan(Options{DataDir: dataDir, NoRollup: true, Pricer: table.lookup})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if rep.Totals.CostReported < 0.0009 || rep.Totals.CostReported > 0.0011 {
		t.Errorf("costReported = %v, want 0.001 (only the priced turn)", rep.Totals.CostReported)
	}
	// 0.001 reported + 3.0 estimated.
	if rep.Totals.CostUSD < 2.999 || rep.Totals.CostUSD > 3.003 {
		t.Errorf("cost = %v, want 3.001", rep.Totals.CostUSD)
	}
	if rep.Totals.PricedRequests != 2 || rep.Totals.BilledRequests != 2 {
		t.Errorf("priced/billed = %d/%d, want 2/2", rep.Totals.PricedRequests, rep.Totals.BilledRequests)
	}
}

// TestScanLeavesUnpricedModelsAtZero is the honesty guard: a model with no
// local price stays at $0 and is not counted as priced, so the report says
// what it does not know instead of inventing a number.
func TestScanLeavesUnpricedModelsAtZero(t *testing.T) {
	dataDir := t.TempDir()
	ts := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	entries := []session.Entry{
		userEntry("u1", ts, "do it"),
		unpricedAssistantEntry("a1", "mystery", ts.Add(time.Second), &ai.Usage{
			Input: 1_000_000, TotalTokens: 1_000_000,
		}),
	}
	writeSessionFile(t, dataDir, "-tmp-fixture", "sess-unknown", "/tmp/fixture", "auto", ts, entries)

	rep, err := Scan(Options{DataDir: dataDir, NoRollup: true, Pricer: priceTable{"m1": {in: 3}}.lookup})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if rep.Totals.CostUSD != 0 || rep.Totals.CostEstimated != 0 {
		t.Errorf("cost = %v / estimated %v, want 0/0", rep.Totals.CostUSD, rep.Totals.CostEstimated)
	}
	if rep.Totals.PricedRequests != 0 || rep.Totals.BilledRequests != 1 {
		t.Errorf("priced/billed = %d/%d, want 0/1", rep.Totals.PricedRequests, rep.Totals.BilledRequests)
	}
}

// TestScanWithoutAPricerIsUnchanged: a scan with no table (no models.yml) is
// exactly the old behaviour — reported costs only, everything else an honest
// zero.
func TestScanWithoutAPricerIsUnchanged(t *testing.T) {
	dataDir := t.TempDir()
	ts := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	entries := []session.Entry{
		userEntry("u1", ts, "do it"),
		assistantEntry("a1", "m1", ts.Add(time.Second)),
		unpricedAssistantEntry("a2", "m1", ts.Add(2*time.Second), &ai.Usage{
			Input: 5_000_000, TotalTokens: 5_000_000,
		}),
	}
	writeSessionFile(t, dataDir, "-tmp-fixture", "sess-nopr", "/tmp/fixture", "auto", ts, entries)

	rep, err := Scan(Options{DataDir: dataDir, NoRollup: true})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if rep.Totals.CostUSD < 0.0009 || rep.Totals.CostUSD > 0.0011 {
		t.Errorf("cost = %v, want the reported 0.001 and nothing more", rep.Totals.CostUSD)
	}
	if rep.Totals.PricedRequests != 1 || rep.Totals.BilledRequests != 2 {
		t.Errorf("priced/billed = %d/%d, want 1/2", rep.Totals.PricedRequests, rep.Totals.BilledRequests)
	}
}

// TestRollupCacheSurvivesAPriceChange: the cache holds the reported/unpriced
// split, never the estimate, so editing models.yml between two scans changes
// the number without a rescan — and a cached scan prices identically to a
// fresh one.
func TestRollupCacheSurvivesAPriceChange(t *testing.T) {
	dataDir := t.TempDir()
	ts := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	entries := []session.Entry{
		userEntry("u1", ts, "do it"),
		unpricedAssistantEntry("a1", "m1", ts.Add(time.Second), &ai.Usage{
			Input: 1_000_000, TotalTokens: 1_000_000,
		}),
	}
	writeSessionFile(t, dataDir, "-tmp-fixture", "sess-cache", "/tmp/fixture", "auto", ts, entries)

	cached, err := Scan(Options{DataDir: dataDir, Pricer: priceTable{"m1": {in: 3}}.lookup})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if cached.CacheHits != 0 {
		t.Fatalf("the first scan should read the file, cache hits = %d", cached.CacheHits)
	}
	again, err := Scan(Options{DataDir: dataDir, Pricer: priceTable{"m1": {in: 30}}.lookup})
	if err != nil {
		t.Fatalf("Scan 2: %v", err)
	}
	if again.CacheHits != 1 {
		t.Errorf("the second scan missed the cache: %d hits", again.CacheHits)
	}
	// A new price, same cached counters: 3 → 30.
	if again.Totals.CostUSD < 29.99 || again.Totals.CostUSD > 30.01 {
		t.Errorf("cost = %v, want 30 — the estimate is applied at fold time", again.Totals.CostUSD)
	}
	fresh, err := Scan(Options{DataDir: dataDir, NoRollup: true, Pricer: priceTable{"m1": {in: 30}}.lookup})
	if err != nil {
		t.Fatalf("Scan 3: %v", err)
	}
	if fresh.Totals.CostUSD != again.Totals.CostUSD {
		t.Errorf("cached %v != fresh %v", again.Totals.CostUSD, fresh.Totals.CostUSD)
	}
}
