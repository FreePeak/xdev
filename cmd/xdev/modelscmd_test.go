package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/FreePeak/xdev/internal/config"
)

// modelsTestCatalog builds a models.yml-shaped config: one pinned provider and
// one that discovers its models from the httptest server.
func modelsTestCatalog(t *testing.T, baseURL string) *config.Config {
	t.Helper()
	return &config.Config{
		DefaultModel: "onegw/free",
		Providers: map[string]*config.ProviderConfig{
			"onegw": {
				BaseURL: baseURL,
				API:     "openai-completions",
				Auth:    "none",
				Models: []config.ModelConfig{
					{ID: "free", Name: "Free", ContextWindow: 1000000, MaxTokens: 65536},
				},
			},
			"local": {
				BaseURL:   baseURL,
				API:       "openai-completions",
				Auth:      "none",
				Discovery: &config.DiscoveryConfig{Type: config.DiscoveryOpenAIModels, InjectV1: true},
			},
		},
	}
}

// TestModelsCatalogMergesPinnedAndDiscovered is the core contract: the catalog
// is models.yml pinning merged with live discovery, annotated with the role
// bindings a run would resolve.
func TestModelsCatalogMergesPinnedAndDiscovered(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.URL.Path != "/v1/models" {
			t.Errorf("discovery hit %s, want /v1/models", r.URL.Path)
		}
		fmt.Fprint(w, `{"data":[{"id":"qwen3","context_length":32768,"max_tokens":8192}]}`)
	}))
	defer srv.Close()
	clear(providerModelCache)
	defer clear(providerModelCache)

	cfg := modelsTestCatalog(t, srv.URL)
	settings := &config.Settings{DefaultModel: "onegw/free"}

	rows := catalogRows(cfg, settings)
	if len(rows) != 2 {
		t.Fatalf("catalog rows = %d, want 2: %+v", len(rows), rows)
	}
	byRef := map[string]modelRow{}
	for _, r := range rows {
		byRef[r.Provider+"/"+r.Model] = r
	}
	free, ok := byRef["onegw/free"]
	if !ok {
		t.Fatalf("pinned model missing: %+v", rows)
	}
	if free.Source != "pinned" || free.ContextWindow != 1000000 || free.MaxTokens != 65536 {
		t.Fatalf("pinned row = %+v", free)
	}
	if !free.Default {
		t.Fatalf("default attribution = %+v", free)
	}
	if free.Account != "none (local server)" {
		t.Fatalf("account = %q", free.Account)
	}
	discovered, ok := byRef["local/qwen3"]
	if !ok {
		t.Fatalf("discovered model missing: %+v", rows)
	}
	if discovered.Source != "discovered" || discovered.ContextWindow != 32768 || discovered.MaxTokens != 8192 {
		t.Fatalf("discovered row = %+v", discovered)
	}
	if discovered.Default {
		t.Fatal("a non-default model was marked default")
	}
	if atomic.LoadInt32(&hits) == 0 {
		t.Fatal("discovery never hit the provider's list endpoint")
	}

	if got := modelsFilter(rows, "qwen"); len(got) != 1 || got[0].Model != "qwen3" {
		t.Fatalf("modelsFilter(qwen) = %+v", got)
	}
	if got := modelsFilter(rows, "nothing-matches"); len(got) != 0 {
		t.Fatalf("modelsFilter(no match) = %+v", got)
	}
}

// TestModelsCmdRefreshAndFilter pins the CLI surface: a query filters, and
// --refresh re-asks the servers instead of replaying the cached catalog.
func TestModelsCmdRefreshAndFilter(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		fmt.Fprint(w, `{"data":[{"id":"qwen3","context_length":32768}]}`)
	}))
	defer srv.Close()
	clear(providerModelCache)
	defer clear(providerModelCache)

	cfg := modelsTestCatalog(t, srv.URL)
	settings := &config.Settings{DefaultModel: "onegw/free"}

	var out, errOut bytes.Buffer
	if code := modelsCmd([]string{"qwen"}, cfg, settings, &out, &errOut); code != 0 {
		t.Fatalf("modelsCmd(qwen) = %d (%s)", code, errOut.String())
	}
	text := out.String()
	if !strings.Contains(text, "qwen3") || strings.Contains(text, "onegw") {
		t.Fatalf("filtered output:\n%s", text)
	}
	before := atomic.LoadInt32(&hits)

	out.Reset()
	if code := modelsCmd([]string{"--refresh", "--json"}, cfg, settings, &out, &errOut); code != 0 {
		t.Fatalf("modelsCmd(--refresh) = %d (%s)", code, errOut.String())
	}
	after := atomic.LoadInt32(&hits)
	if after <= before {
		t.Fatalf("--refresh did not re-discover (hits %d -> %d)", before, after)
	}
	var rows []modelRow
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("json catalog: %v (%s)", err, out.String())
	}
	if len(rows) != 2 {
		t.Fatalf("json rows = %d, want 2", len(rows))
	}

	// An empty catalog explains itself instead of printing an empty table.
	out.Reset()
	if code := modelsCmd([]string{}, &config.Config{}, nil, &out, &errOut); code != 0 {
		t.Fatalf("empty catalog exit = %d", code)
	}
	if !strings.Contains(out.String(), "no models configured") {
		t.Fatalf("empty catalog output:\n%s", out.String())
	}
}
