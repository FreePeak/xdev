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
