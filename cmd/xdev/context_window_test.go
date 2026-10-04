package main

import (
	"testing"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/config"
)

// The pin is the one thing a user sets for EVERY model, so it must outrank the
// catalog's own number for any model — otherwise /context 1m would leave a
// 128K model running on 128K and the command would be a lie.
func TestPinnedWindowOutranksTheCatalog(t *testing.T) {
	t.Setenv(agent.MaxContextTokensEnv, "")
	cfg := &config.Config{Providers: map[string]*config.ProviderConfig{
		"gw": {API: "openai-completions", BaseURL: "http://127.0.0.1:1",
			Models: []config.ModelConfig{
				{ID: "small", ContextWindow: 128_000},
				{ID: "wide", ContextWindow: 1_000_000},
				{ID: "listed-no-window"},
			}},
	}}

	// auto: the model keeps the window its catalog states.
	loadedSettings = &config.Settings{}
	if got := pinnedWindow(cfg, "gw", "small"); got != 128_000 {
		t.Fatalf("auto on a pinned model = %d, want 128000", got)
	}
	if got := pinnedWindow(cfg, "gw", "listed-no-window"); got != agent.MaxContextTokensDefault {
		t.Fatalf("auto on a windowless model = %d, want the fallback", got)
	}

	// pinned: every model, catalog or not.
	for _, want := range []string{"200k", "1m"} {
		loadedSettings = &config.Settings{Compaction: config.CompactionSettings{ContextWindow: want}}
		for _, id := range []string{"small", "wide", "listed-no-window", "not-in-catalog"} {
			if got := pinnedWindow(cfg, "gw", id); got != loadedSettings.CompactionContextWindow() {
				t.Fatalf("%s on %q = %d, want %d", want, id, got, loadedSettings.CompactionContextWindow())
			}
		}
	}
	t.Cleanup(func() { loadedSettings = nil })
}

// modelWindow itself must stay the CATALOG's answer: the pin rides the agent's
// windowPin so `/context auto` can hand a live agent back what it was built
// with, and folding it in here would make that impossible.
func TestModelWindowIgnoresThePin(t *testing.T) {
	t.Setenv(agent.MaxContextTokensEnv, "")
	cfg := &config.Config{Providers: map[string]*config.ProviderConfig{
		"gw": {API: "openai-completions", BaseURL: "http://127.0.0.1:1",
			Models: []config.ModelConfig{{ID: "small", ContextWindow: 128_000}}},
	}}
	old := loadedSettings
	t.Cleanup(func() { loadedSettings = old })
	loadedSettings = &config.Settings{Compaction: config.CompactionSettings{ContextWindow: "1m"}}
	if got := modelWindow(cfg, "gw", "small"); got != 128_000 {
		t.Fatalf("modelWindow = %d, want the catalog's 128000", got)
	}
}
