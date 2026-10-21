package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The effort capability is a per-model declaration in models.yml: the levels
// the model accepts on the wire.
//
// It is OFF by default — a model that does not declare it keeps the exact
// request shape it had before the effort level existed — so the load path is
// where the default is pinned.
func TestLoadModelsReadsTheEffortCapability(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.yml")
	content := `providers:
  onegw:
    baseUrl: http://127.0.0.1:20128/v1
    api: openai-completions
    models:
      - id: with-effort
        reasoning: true
        effort:
          - low
          - medium
          - high
      - id: without-effort
        reasoning: true
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadModels(path)
	if err != nil {
		t.Fatalf("LoadModels: %v", err)
	}
	models := cfg.Providers["onegw"].Models
	if len(models) != 2 {
		t.Fatalf("models = %d, want 2", len(models))
	}
	if got := models[0].EffortWireLevels(); len(got) != 3 || got[0] != "low" || got[2] != "high" {
		t.Fatalf("effort = %v, want low/medium/high", got)
	}
	// The default is NOT supported: no list, no effort parameter, the exact
	// request xdev sent before the level existed.
	if got := models[1].EffortWireLevels(); len(got) != 0 {
		t.Fatalf("effort = %v, want none for a model that declares nothing", got)
	}
}

// The wire capability and the advertised ladder are two questions: `efforts:`
// says which rungs the model advertises (and clamps a request to), `effort:`
// says which of them the provider's own parameter takes. A model that
// advertises a rung the wire does not accept cannot reach it.
func TestEffortLevelsNarrowsTheAdvertisedRungs(t *testing.T) {
	m := ModelConfig{
		Efforts: []string{"low", "medium", "high", "xhigh"},
		Effort:  []string{"low", "medium", "high"},
	}
	got := m.EffortLevels()
	if len(got) != 3 || got[0] != "low" || got[1] != "medium" || got[2] != "high" {
		t.Fatalf("levels = %v, want low/medium/high", got)
	}
	// Only one of the two declared: that one answers.
	if got := (ModelConfig{Effort: []string{"low", "high"}}).EffortLevels(); len(got) != 2 {
		t.Fatalf("levels = %v, want the declared wire levels", got)
	}
	if got := (ModelConfig{Efforts: []string{"low", "high"}}).EffortLevels(); len(got) != 2 {
		t.Fatalf("levels = %v, want the advertised rungs", got)
	}
	// Neither declared: no opinion, and a requested rung rides out
	// unchanged (the rule ClampEffort applies to an empty list).
	if got := (ModelConfig{}).EffortLevels(); len(got) != 0 {
		t.Fatalf("levels = %v, want none", got)
	}
}
