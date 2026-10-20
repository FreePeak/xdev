package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FreePeak/xdev/internal/config"
)

// TestCatalogDoesNotRepeatAModel: the list /model shows must not name the
// same model twice. The picker turns every entry into a button, so a
// duplicate is a duplicate button — and the text list gets a repeated line.
//
// The cause was the merge: the discovery result already carries the pinned
// models (pinned wins on a collision), and the pinned list was prepended to
// it again.
func TestCatalogDoesNotRepeatAModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"pinned"},{"id":"discovered"}]}`))
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{}
	cfg := &config.Config{Providers: map[string]*config.ProviderConfig{
		"probe": {
			BaseURL:   srv.URL,
			Discovery: &config.DiscoveryConfig{Type: "openai-models-list"},
			Models:    []config.ModelConfig{{ID: "pinned"}},
		},
	}}

	refs := d.catalog(cfg)
	seen := map[string]int{}
	for _, ref := range refs {
		seen[ref]++
	}
	for ref, n := range seen {
		if n > 1 {
			t.Errorf("%q appears %d times in %v, want once", ref, n, refs)
		}
	}
	if len(refs) != 2 {
		t.Errorf("catalog = %v, want the pinned and the discovered model, each once", refs)
	}
}
