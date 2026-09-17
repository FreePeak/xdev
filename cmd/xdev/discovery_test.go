package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/FreePeak/xdev/internal/config"
)

// TestModelWindowUsesDiscovery pins the wiring, not just the engine: a model
// that exists only on the server must still get its context window, or
// compaction silently switches itself off for exactly the providers
// discovery exists to serve.
func TestModelWindowUsesDiscovery(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		fmt.Fprint(w, `{"data":[{"id":"llama3:8b","context_length":8192}]}`)
	}))
	t.Cleanup(srv.Close)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".xdev", "agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "models.yml"),
		[]byte("providers:\n  local:\n    baseUrl: "+srv.URL+"\n    api: openai-completions\n    auth: none\n    discovery: { type: openai-models-list }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadModelsLayered()
	if err != nil {
		t.Fatal(err)
	}
	delete(providerModelCache, "local")
	if got := modelWindow(cfg, "local", "llama3:8b"); got != 8192 {
		t.Fatalf("discovered model window = %d, want 8192", got)
	}
	// Second lookup must be served from cache, not a second HTTP round-trip.
	modelWindow(cfg, "local", "llama3:8b")
	if hits != 1 {
		t.Fatalf("discovery re-ran per lookup: %d requests", hits)
	}
	// And a still-down server does not break resolution or repeat forever.
	delete(providerModelCache, "local")
	bad := &config.Config{Providers: map[string]*config.ProviderConfig{
		"dead": {API: "openai-completions", BaseURL: "http://127.0.0.1:1",
			Models:    []config.ModelConfig{{ID: "pinned", ContextWindow: 123}},
			Discovery: &config.DiscoveryConfig{Type: "openai-models-list"}},
	}}
	delete(providerModelCache, "dead")
	if got := modelWindow(bad, "dead", "pinned"); got != 123 {
		t.Fatalf("pinned model lost after discovery failure: %d", got)
	}
}
