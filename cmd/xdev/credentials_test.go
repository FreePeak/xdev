package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
)

// TestCredentialChainReachesTheWire proves the resolution order with the
// header the provider actually sends, not just "no error": CLI key beats
// models.yml beats env.
func TestCredentialChainReachesTheWire(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)

	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".xdev", "agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(dir, "models.yml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfgOf := func() *config.Config {
		cfg, err := config.LoadModelsLayered()
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	pc := func() *config.ProviderConfig { return cfgOf().Providers["onegw"] }

	providerFor := func(t *testing.T) ai.Provider {
		t.Helper()
		p, err := buildProvider("onegw", pc(), "free", nil)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	// 1) env fallback (no key anywhere else)
	write("providers:\n  onegw:\n    baseUrl: " + srv.URL + "\n    api: openai-completions\n    models: [{id: free}]\ndefaultModel: onegw/free\n")
	t.Setenv("ONEGW_API_KEY", "sk-from-env")
	p := providerFor(t)
	stream(t, p)

	// 2) models.yml wins over env
	write("providers:\n  onegw:\n    baseUrl: " + srv.URL + "\n    api: openai-completions\n    apiKey: sk-from-yml\n    models: [{id: free}]\n")
	t.Setenv("XDEV_TEST_UNUSED", "1")
	p = providerFor(t)
	stream(t, p)

	// 3) CLI key wins over both
	cliKeyValue = "sk-from-cli"
	t.Cleanup(func() { cliKeyValue = "" })
	p = providerFor(t)
	stream(t, p)

	mu.Lock()
	defer mu.Unlock()
	want := []string{"Bearer sk-from-env", "Bearer sk-from-yml", "Bearer sk-from-cli"}
	if len(auths) != len(want) {
		t.Fatalf("auth headers = %v", auths)
	}
	for i, w := range want {
		if auths[i] != w {
			t.Fatalf("request %d sent %q, want %q", i, auths[i], w)
		}
	}
}

func stream(t *testing.T, p ai.Provider) {
	t.Helper()
	ch, err := p.Stream(context.Background(), ai.StreamRequest{
		Model:    "free",
		Messages: []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		if strings.Contains(err.Error(), "credential") {
			t.Fatalf("resolution failed before the request: %v", err)
		}
		t.Fatal(err)
	}
	for range ch {
	}
}

func TestAuthHeaderConfigReachesTheWire(t *testing.T) {
	// openai-completions authenticates with `Authorization: Bearer` by
	// default, so a custom authHeader that lands there instead proves the
	// setting is honored rather than an adapter's own convention (an earlier
	// version of this test used anthropic, which hardcodes x-api-key, and
	// passed without the config being read at all).
	var got http.Header
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Clone()
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".xdev", "agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "models.yml"),
		[]byte("providers:\n  gw:\n    baseUrl: "+srv.URL+"\n    api: openai-completions\n    apiKey: sk-custom\n    authHeader: X-Gateway-Key\n    models: [{id: m1}]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadModelsLayered()
	if err != nil {
		t.Fatal(err)
	}
	p, err := buildProvider("gw", cfg.Providers["gw"], "claude-x", cfg)
	if err != nil {
		t.Fatal(err)
	}
	stream(t, p)
	mu.Lock()
	defer mu.Unlock()
	if got.Get("X-Gateway-Key") != "sk-custom" {
		t.Fatalf("authHeader ignored: %v", got)
	}
	if got.Get("Authorization") != "" {
		t.Fatalf("the adapter's default header should be suppressed: %q", got.Get("Authorization"))
	}
}

// TestAuthHeaderDefaultsToBearer: without the setting, nothing changes.
func TestAuthHeaderDefaultsToBearer(t *testing.T) {
	var got http.Header
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Clone()
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".xdev", "agent")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "models.yml"),
		[]byte("providers:\n  gw:\n    baseUrl: "+srv.URL+"\n    api: openai-completions\n    apiKey: sk-plain\n    models: [{id: m1}]\n"), 0o644)
	cfg, err := config.LoadModelsLayered()
	if err != nil {
		t.Fatal(err)
	}
	p, err := buildProvider("gw", cfg.Providers["gw"], "claude-x", cfg)
	if err != nil {
		t.Fatal(err)
	}
	stream(t, p)
	mu.Lock()
	defer mu.Unlock()
	if got.Get("Authorization") != "Bearer sk-plain" {
		t.Fatalf("default bearer path broken: %v", got)
	}
}

// TestPerModelOverrideReachesTheWire pins the models.yml override contract
// (M9 #10 models.yml box): a `models[].apiKey` must replace the
// provider-level key on the wire. Without this test the declared fields are
// dead weight — the earlier version of buildProvider never read them.
func TestPerModelOverrideReachesTheWire(t *testing.T) {
	var auths []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)

	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".xdev", "agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Provider-level key, and a per-model key on "dev".
	body := "providers:\n  onegw:\n    baseUrl: " + srv.URL + "\n    api: openai-completions\n    apiKey: sk-provider-level\n    models:\n      - id: free\n      - id: dev\n        apiKey: sk-model-level\ndefaultModel: onegw/free\n"
	if err := os.WriteFile(filepath.Join(dir, "models.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadModelsLayered()
	if err != nil {
		t.Fatal(err)
	}
	mk := func(model string) ai.Provider {
		p, err := buildProvider("onegw", cfg.Providers["onegw"], model, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	// "free" (no per-model key) → provider-level key.
	stream(t, mk("free"))
	// "dev" (per-model key) → model-level key.
	stream(t, mk("dev"))

	mu.Lock()
	defer mu.Unlock()
	want := []string{"Bearer sk-provider-level", "Bearer sk-model-level"}
	if len(auths) != len(want) {
		t.Fatalf("auth headers = %v, want %v", auths, want)
	}
	for i, w := range want {
		if auths[i] != w {
			t.Fatalf("request %d sent %q, want %q", i, auths[i], w)
		}
	}
}
