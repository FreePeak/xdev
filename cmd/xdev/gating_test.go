package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/config"
)

// providerFrom writes models.yml and builds the named provider, mirroring
// how the modes resolve a provider at startup.
func providerFrom(t *testing.T, provider, body string) error {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".xdev", "agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "models.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadModelsLayered()
	if err != nil {
		t.Fatal(err)
	}
	_, err = buildProvider(provider, cfg.Providers[provider], "test-model", cfg)
	return err
}

func TestDisabledProviderIsRefused(t *testing.T) {
	body := "providers:\n  onegw:\n    baseUrl: https://example.invalid/v1\n    api: openai-completions\n    apiKey: k\n    models: [{id: free}]\n"
	if err := providerFrom(t, "onegw", body); err != nil {
		t.Fatalf("enabled provider must build: %v", err)
	}
	// Settings now disable it.
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(config.DataDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.GlobalSettingsPath(),
		[]byte("disabledProviders: [onegw]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := config.LoadSettings(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	loadedSettings = s
	t.Cleanup(func() { loadedSettings = nil })
	err = s.CheckProvider("onegw", true)
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("disabled provider must be refused: %v", err)
	}
	// A different provider is unaffected, and matching ignores case.
	if err := s.CheckProvider("other", true); err != nil {
		t.Fatalf("unrelated provider refused: %v", err)
	}
	if err := s.CheckProvider("OneGW", true); err == nil {
		t.Fatal("case-insensitive match expected")
	}
}

func TestKeylessProviderNeedsNoCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	body := "providers:\n  local:\n    baseUrl: " + srv.URL + "\n    api: openai-completions\n    auth: none\n    models: [{id: llama}]\n"
	os.Unsetenv("LOCAL_API_KEY")
	os.Unsetenv("LOCAL_KEY")
	if err := providerFrom(t, "local", body); err != nil {
		t.Fatalf("auth:none must start without a credential: %v", err)
	}
	// The same provider without auth: none must complain clearly.
	bodyKeyed := "providers:\n  local:\n    baseUrl: " + srv.URL + "\n    api: openai-completions\n    models: [{id: llama}]\n"
	err := providerFrom(t, "local", bodyKeyed)
	if err == nil || !strings.Contains(err.Error(), "no credential") {
		t.Fatalf("missing credential must be an explicit error, got %v", err)
	}
	for _, fix := range []string{"models.yml", "/login", "export"} {
		if !strings.Contains(err.Error(), fix) {
			t.Errorf("error must name the fix %q: %v", fix, err)
		}
	}
}

// TestGateUsesTheChainNotACopy: an env-only key is fully configured. An
// early version of the gate re-implemented the lookup, missed the env
// source, and refused a working setup.
func TestGateUsesTheChainNotACopy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GW_API_KEY", "sk-env-only")
	body := "providers:\n  gw:\n    baseUrl: " + srv.URL + "\n    api: openai-completions\n    models: [{id: m}]\n"
	if err := providerFrom(t, "gw", body); err != nil {
		t.Fatalf("env-only credential must be accepted: %v", err)
	}
}
