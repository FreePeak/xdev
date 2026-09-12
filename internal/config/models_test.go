package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadModelsWithEnvExpansion(t *testing.T) {
	t.Setenv("TEST_GW_KEY", "sk-test-123")
	dir := t.TempDir()
	path := filepath.Join(dir, "models.yml")
	content := `providers:
  onegw:
    baseUrl: http://127.0.0.1:20128/v1
    apiKey: ${TEST_GW_KEY}
    api: openai-completions
    models:
      - id: free
        name: Free
        contextWindow: 1000000
  anthro:
    baseUrl: https://api.anthropic.com
    apiKey: inline-key
    api: anthropic-messages
    models:
      - id: claude-x
defaultModel: onegw/free
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadModels(path)
	if err != nil {
		t.Fatalf("LoadModels: %v", err)
	}
	gw := cfg.Providers["onegw"]
	if gw == nil || gw.APIKey != "sk-test-123" || gw.API != "openai-completions" {
		t.Fatalf("onegw provider = %+v", gw)
	}
	if len(gw.Models) != 1 || gw.Models[0].ID != "free" {
		t.Fatalf("models = %+v", gw.Models)
	}
	a := cfg.Providers["anthro"]
	if a == nil || a.APIKey != "inline-key" {
		t.Fatalf("anthro = %+v", a)
	}
}

func TestLoadModelsUnknownFieldsRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.yml")
	if err := os.WriteFile(path, []byte("providers:\n  x:\n  baseUrl: uknownField: 1\n"), 0o644); err == nil {
		// fallthrough: malformed content tested below
	}
	_ = os.WriteFile(path, []byte("providers:\n  x:\n    baseUrl: http://x\n    api: openai-completions\n    bogusKey: 1\n"), 0o644)
	if _, err := LoadModels(path); err == nil {
		t.Fatal("unknown field must be rejected")
	}
}

func TestParseModelRef(t *testing.T) {
	if p, m, err := ParseModelRef("onegw/free"); err != nil || p != "onegw" || m != "free" {
		t.Fatalf("got %q %q %v", p, m, err)
	}
	if _, _, err := ParseModelRef("nope"); err == nil {
		t.Fatal("want error for missing slash")
	}
	if _, _, err := ParseModelRef("/x"); err == nil {
		t.Fatal("want error for empty provider")
	}
}

func TestDefaultModelRef(t *testing.T) {
	cfg := &Config{Providers: map[string]*ProviderConfig{}}
	if got := cfg.DefaultModelRef(); got != "" {
		t.Fatalf("empty config gave %q", got)
	}
	cfg.Providers["zzz"] = &ProviderConfig{Models: []ModelConfig{{ID: "m1"}}}
	cfg.Providers["router"] = &ProviderConfig{Models: []ModelConfig{{ID: "dev"}}}
	if got := cfg.DefaultModelRef(); got != "router/dev" {
		t.Fatalf("got %q, want router/dev (preferred key)", got)
	}
	cfg.DefaultModel = "zzz/m1"
	if got := cfg.DefaultModelRef(); got != "zzz/m1" {
		t.Fatalf("explicit default lost: %q", got)
	}
}

func TestResolveMissingVar(t *testing.T) {
	if got := Resolve("x-${XDEV_NO_SUCH_VAR}-y"); got != "x--y" {
		t.Fatalf("Resolve = %q", got)
	}
}

// TestRegisterProviderValidates covers the runtime provider registration an
// extension performs (ext action `register_provider`): a payload that cannot
// produce a working adapter is refused with a fix, an unusable name never
// becomes a lookup key, and a valid block lands in the registry.
func TestRegisterProviderValidates(t *testing.T) {
	valid := func() *ProviderConfig {
		return &ProviderConfig{
			BaseURL: "http://127.0.0.1:20128/v1",
			API:     "openai-completions",
			Models:  []ModelConfig{{ID: "free"}},
		}
	}
	tests := []struct {
		name   string
		key    string
		pc     *ProviderConfig
		errHas string
	}{
		{"empty name", "", valid(), "name is required"},
		{"name with slash", "a/b", valid(), "bare provider key"},
		{"name with space", "a b", valid(), "bare provider key"},
		{"nil block", "ext", nil, "provider block is required"},
		{"missing baseUrl", "ext", &ProviderConfig{API: "openai-completions", Models: []ModelConfig{{ID: "free"}}}, "baseUrl is required"},
		{"unsupported api", "ext", &ProviderConfig{BaseURL: "http://x/v1", API: "telepathy", Models: []ModelConfig{{ID: "free"}}}, "unsupported api"},
		{"no models", "ext", &ProviderConfig{BaseURL: "http://x/v1", API: "openai-completions"}, "at least one model"},
		{"blank model id", "ext", &ProviderConfig{BaseURL: "http://x/v1", API: "openai-completions", Models: []ModelConfig{{ID: "  "}}}, "at least one model"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{}
			err := cfg.RegisterProvider(tc.key, tc.pc)
			if err == nil {
				t.Fatalf("RegisterProvider(%q) must fail", tc.key)
			}
			if !strings.Contains(err.Error(), tc.errHas) {
				t.Fatalf("error %q does not mention %q", err, tc.errHas)
			}
			if len(cfg.Providers) != 0 {
				t.Fatalf("a refused payload must not register: %v", cfg.Providers)
			}
		})
	}

	// Nil registry: an extension must not be able to panic the session.
	var nilCfg *Config
	if err := nilCfg.RegisterProvider("ext", valid()); err == nil {
		t.Fatal("nil config must be refused")
	}

	// Valid block: keyed by name, reachable through the same map every model
	// reference resolves against.
	cfg := &Config{Providers: map[string]*ProviderConfig{}}
	pc := valid()
	if err := cfg.RegisterProvider("ext", pc); err != nil {
		t.Fatal(err)
	}
	if cfg.Providers["ext"] != pc {
		t.Fatalf("provider not installed: %v", cfg.Providers)
	}
}
