package config

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// TestPricingDecodesAndPricesOneRequest is the whole feature: a model
// declares its per-million rates, and one request's buckets are priced at
// their own rates — a cached turn at the cache rate, not the input rate.
func TestPricingDecodesAndPricesOneRequest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.yml")
	content := `providers:
  onegw:
    baseUrl: http://127.0.0.1:20128/v1
    apiKey: k
    api: openai-completions
    models:
      - id: claude-x
        pricing:
          input: 3
          output: 15
          cacheRead: 0.3
          cacheWrite: 3.75
      - id: free
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadModels(path)
	if err != nil {
		t.Fatalf("LoadModels: %v", err)
	}

	p := cfg.Pricing("claude-x")
	if p.Zero() {
		t.Fatal("claude-x has no declared price")
	}
	// 1M input at 3 · 100k output at 15 · 2M cached at 0.3 · 500k written at 3.75
	want := 3.0 + 1.5 + 0.6 + 1.875
	if got := p.USD(1_000_000, 100_000, 2_000_000, 500_000, 0); math.Abs(got-want) > 1e-9 {
		t.Errorf("USD = %v, want %v", got, want)
	}
	// A provider that reports no total is priced as the sum of its buckets.
	if got := p.USD(1_000_000, 100_000, 0, 0, 0); math.Abs(got-4.5) > 1e-9 {
		t.Errorf("USD without a reported total = %v, want 4.5", got)
	}
	// A model with no pricing block is not priced, rather than priced at zero.
	if !cfg.Pricing("free").Zero() {
		t.Error("a model with no pricing block must read as unpriced")
	}
}

// TestPricingMatchesNamespacedModelIds: a gateway prefixes its own namespace,
// so "onegw/claude-x" and "openrouter/anthropic/claude-x" both have to reach
// the pinned "claude-x" entry — and the longest matching id wins, so a pinned
// "claude-x-mini" is not shadowed by "claude-x".
func TestPricingMatchesNamespacedModelIds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.yml")
	content := `providers:
  onegw:
    baseUrl: http://127.0.0.1:20128/v1
    api: openai-completions
    models:
      - id: claude-x
        pricing: {input: 3}
      - id: claude-x-mini
        pricing: {input: 1}
  router:
    baseUrl: https://router.example
    api: openai-completions
    models:
      - id: claude-x
        pricing: {input: 9}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadModels(path)
	if err != nil {
		t.Fatalf("LoadModels: %v", err)
	}

	if got := cfg.Pricing("onegw/claude-x"); math.Abs(got.Input-3) > 1e-9 {
		t.Errorf("namespaced lookup = %v, want 3", got.Input)
	}
	// The prefix WINS over the longest pin: `router` pins claude-x at 9, and
	// "router/claude-x" must not be priced by onegw's 3.
	if got := cfg.Pricing("router/claude-x"); math.Abs(got.Input-9) > 1e-9 {
		t.Errorf("router-namespaced lookup = %v, want 9", got.Input)
	}
	// A namespace that is NOT a provider key falls through to every provider:
	// "openrouter/anthropic/claude-x" carries no configured provider, so the
	// longest matching pin answers, and a tie goes to a deterministic one.
	if got := cfg.Pricing("openrouter/anthropic/claude-x"); got.Zero() {
		t.Error("a namespaced id with no provider prefix must still find a pin")
	}
	// The longest pinned id wins over the shorter one it contains.
	if got := cfg.Pricing("claude-x-mini"); math.Abs(got.Input-1) > 1e-9 {
		t.Errorf("mini lookup = %v, want 1 (the longest matching id)", got.Input)
	}
	// A model nothing was pinned for is unpriced, not the default of some
	// other entry.
	if !cfg.Pricing("mystery-model").Zero() {
		t.Error("an unpinned model must read as unpriced")
	}
}

// TestPricingIsNilSafe: the lookup is on the scan's hot path and is called for
// every model in the store, including one with no provider block.
func TestPricingIsNilSafe(t *testing.T) {
	var nilCfg *Config
	if !nilCfg.Pricing("anything").Zero() {
		t.Error("a nil config must read as unpriced")
	}
	empty := Config{}
	if !empty.Pricing("").Zero() {
		t.Error("an empty model id must read as unpriced")
	}
}
