package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/FreePeak/xdev/internal/config"
)

// resetProviderModelCache empties the package-global catalog cache. The
// cache is keyed by provider NAME for the whole process, so one test's
// fixture for a provider must never leak into another's (and the TUI's
// warm-up goroutine must never race a test's read).
func resetProviderModelCache() {
	providerModelCacheMu.Lock()
	providerModelCache = map[string][]config.ModelConfig{}
	providerModelCacheMu.Unlock()
}

// TestProviderModelsConcurrentAccess pins the cache contract under -race:
// discovery runs once per provider per process, the network probe stays
// outside the lock, and concurrent first misses do not race the map.
func TestProviderModelsConcurrentAccess(t *testing.T) {
	resetProviderModelCache()
	t.Cleanup(resetProviderModelCache)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, `{"data":[{"id":"discovered-1"}]}`)
	}))
	defer srv.Close()

	pc := &config.ProviderConfig{
		BaseURL:   srv.URL,
		Discovery: &config.DiscoveryConfig{Type: config.DiscoveryOpenAIModels},
		Models:    []config.ModelConfig{{ID: "pinned-1"}},
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = providerModels("warm-race", pc)
		}()
	}
	wg.Wait()

	got := providerModels("warm-race", pc)
	ids := map[string]bool{}
	for _, m := range got {
		ids[m.ID] = true
	}
	if !ids["pinned-1"] || !ids["discovered-1"] {
		t.Fatalf("catalog = %v, want pinned + discovered", ids)
	}
}
