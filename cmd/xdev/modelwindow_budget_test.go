package main

import (
	"testing"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/config"
)

// TestModelWindowFallbackBudget pins the other half of modelWindow: a model
// the catalog does not pin must still get a window, or compaction silently
// switches itself off for exactly the providers it exists to guard. The
// fallback is agent.ResolveMaxContextTokens() — the compiled default, or
// XDEV_MAX_CONTEXT_TOKENS when the operator sets one.
func TestModelWindowFallbackBudget(t *testing.T) {
	t.Setenv(agent.MaxContextTokensEnv, "")
	empty := &config.Config{Providers: map[string]*config.ProviderConfig{}}
	if got := modelWindow(empty, "nope", "whatever"); got != agent.MaxContextTokensDefault {
		t.Fatalf("unknown provider window = %d, want default %d", got, agent.MaxContextTokensDefault)
	}
	for _, m := range []string{"unknown-model", "llama3:8b"} {
		if got := modelWindow(empty, "nope", m); got != agent.MaxContextTokensDefault {
			t.Fatalf("unknown provider window for %q = %d, want default", m, got)
		}
	}
	// A provider whose catalog misses the model falls back the same way.
	noPin := &config.Config{Providers: map[string]*config.ProviderConfig{
		"pinned": {API: "openai-completions", BaseURL: "http://127.0.0.1:1",
			Models: []config.ModelConfig{{ID: "known", ContextWindow: 123}}},
	}}
	if got := modelWindow(noPin, "pinned", "not-in-catalog"); got != agent.MaxContextTokensDefault {
		t.Fatalf("unpinned model window = %d, want default %d", got, agent.MaxContextTokensDefault)
	}
	// The pinned model still wins over the fallback.
	if got := modelWindow(noPin, "pinned", "known"); got != 123 {
		t.Fatalf("pinned model window = %d, want 123", got)
	}
}

// TestModelWindowFallbackHonoursEnv pins the override: XDEV_MAX_CONTEXT_TOKENS
// is the operator's explicit budget for models the catalog cannot name.
func TestModelWindowFallbackHonoursEnv(t *testing.T) {
	t.Setenv(agent.MaxContextTokensEnv, "64000")
	empty := &config.Config{Providers: map[string]*config.ProviderConfig{}}
	if got := modelWindow(empty, "nope", "whatever"); got != 64000 {
		t.Fatalf("env fallback window = %d, want 64000", got)
	}
}

// TestModelWindowListedButWindowless pins the defect this file's other cases
// cannot see: a model the catalog DOES list, with no window stated. That is
// what a gateway's /v1/models produces — bare {id, object, owned_by}, so every
// discovered model arrives with ContextWindow 0 — and returning that 0 reached
// the ladder's own `ContextWindow <= 0` guard, switching compaction off for
// that model entirely. A listed id with no window is UNKNOWN, not zero.
func TestModelWindowListedButWindowless(t *testing.T) {
	t.Setenv(agent.MaxContextTokensEnv, "")
	cfg := &config.Config{Providers: map[string]*config.ProviderConfig{
		"gw": {API: "openai-completions", BaseURL: "http://127.0.0.1:1",
			// Exactly what discovery builds from a bare model list.
			Models: []config.ModelConfig{
				{ID: "pinned", ContextWindow: 200000},
				{ID: "listed-no-window"},
				{ID: "also-listed-no-window", MaxTokens: 4096},
			}},
	}}
	for _, id := range []string{"listed-no-window", "also-listed-no-window"} {
		got := modelWindow(cfg, "gw", id)
		if got != agent.MaxContextTokensDefault {
			t.Fatalf("windowless catalog entry %q window = %d, want default %d (0 disables compaction)",
				id, got, agent.MaxContextTokensDefault)
		}
		// The ladder's own gate is `ContextWindow <= 0` (compact.go
		// maybeCompact, handoff.go HandoffDue), so a positive window is
		// exactly the condition that leaves context maintenance on.
		if got <= 0 {
			t.Fatalf("window %d disables context maintenance", got)
		}
	}
	// A stated window is still honoured over the fallback.
	if got := modelWindow(cfg, "gw", "pinned"); got != 200000 {
		t.Fatalf("pinned window = %d, want 200000", got)
	}
}

// TestModelWindowProviderDefault is the middle rung: a provider-level
// contextWindow answers for every model that states none of its own. The
// live case (onegw, 2026-10-04) is a gateway whose /v1/models carries no
// window for any of its 108 ids, so every model below the stated ones fell
// through to the 200000 default while the opencode lane behind them really
// serves ~1M — the ladder then compacted far too early. A model the catalog
// does not list at all is covered here too: discovery may not have run.
func TestModelWindowProviderDefault(t *testing.T) {
	t.Setenv(agent.MaxContextTokensEnv, "")
	cfg := &config.Config{Providers: map[string]*config.ProviderConfig{
		"gwprov": {API: "openai-completions", BaseURL: "http://127.0.0.1:1",
			ContextWindow: 1_000_000,
			Models: []config.ModelConfig{
				{ID: "own-pin", ContextWindow: 128000},
				{ID: "listed-no-window"},
			}},
	}}
	for _, id := range []string{"listed-no-window", "never-catalogued"} {
		if got := modelWindow(cfg, "gwprov", id); got != 1_000_000 {
			t.Fatalf("provider default for %q = %d, want 1000000", id, got)
		}
		// A model's own stated window still outranks the provider's.
		if got := modelWindow(cfg, "gwprov", "own-pin"); got != 128000 {
			t.Fatalf("per-model pin = %d, want 128000", got)
		}
		// With no provider window the ladder still ends at the compiled default.
		none := &config.Config{Providers: map[string]*config.ProviderConfig{
			"gwbare": {API: "openai-completions", BaseURL: "http://127.0.0.1:1"},
		}}
		if got := modelWindow(none, "gwbare", "listed-no-window"); got != agent.MaxContextTokensDefault {
			t.Fatalf("no provider window = %d, want default %d", got, agent.MaxContextTokensDefault)
		}
		// An explicit per-model pin is the top rung and stays put.
		if got := modelWindow(cfg, "gwprov", "own-pin"); got != 128000 {
			t.Fatalf("per-model pin = %d, want 128000", got)
		}
	}
}
