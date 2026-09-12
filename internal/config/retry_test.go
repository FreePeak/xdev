package config

import (
	"strings"
	"testing"
	"time"
)

// TestSettingsRetryKeys pins the retry group layering contract (M5 #25): the
// three chain-key kinds survive KnownFields decoding, and the policy
// accessors normalize the spellings the agent consumes.
func TestSettingsRetryKeys(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	cwd := t.TempDir()
	writeFile(t, GlobalSettingsPath(), `
retry:
  fallbackChains:
    smol: ["other/dev"]
    onegw/free: ["openrouter/google/gemini-2.5-pro", "@slow"]
    "onegw/*": ["other/*"]
  reserveThreshold: auto
  reservePct: 25
  fallbackRevertPolicy: never
  fallbackCooldown: 30s
`)
	s, err := LoadSettings(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	chains := s.Retry.FallbackChains
	if len(chains) != 3 {
		t.Fatalf("chains = %v, want three keys", chains)
	}
	if got := chains["onegw/free"]; len(got) != 2 || got[0] != "openrouter/google/gemini-2.5-pro" || got[1] != "@slow" {
		t.Fatalf("model chain = %v", got)
	}
	if got := chains["onegw/*"]; len(got) != 1 || got[0] != "other/*" {
		t.Fatalf("wildcard chain = %v", got)
	}
	if s.ReservePolicy() != ReserveThresholdAuto {
		t.Fatalf("reserve policy = %q", s.ReservePolicy())
	}
	if s.RevertPolicy() != RevertNever {
		t.Fatalf("revert policy = %q", s.RevertPolicy())
	}
	if s.FallbackCooldownDuration() != 30*time.Second {
		t.Fatalf("cooldown = %v", s.FallbackCooldownDuration())
	}
	if s.ReserveFraction() != 0.25 {
		t.Fatalf("reserve fraction = %v", s.ReserveFraction())
	}

	// The shipped defaults: usage-awareness off, revert on cooldown expiry.
	t.Setenv("XDEV_AGENT_DIR", t.TempDir()) // a clean agent dir: no retry group at all
	empty, err := LoadSettings(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if empty.ReservePolicy() != ReserveThresholdOff || empty.RevertPolicy() != RevertCooldownExpiry {
		t.Fatalf("defaults = %q/%q", empty.ReservePolicy(), empty.RevertPolicy())
	}
	if empty.FallbackCooldownDuration() != DefaultFallbackCooldown || empty.ReserveFraction() != 0.1 {
		t.Fatalf("default cooldown/fraction = %v/%v", empty.FallbackCooldownDuration(), empty.ReserveFraction())
	}
}

// TestSettingsRetryValidation: a misspelled policy or an unusable chain is
// reported, never silently dropped.
func TestSettingsRetryValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"reserve threshold", "retry:\n  reserveThreshold: maybe\n", "reserveThreshold"},
		{"revert policy", "retry:\n  fallbackRevertPolicy: pinned\n", "fallbackRevertPolicy"},
		{"cooldown", "retry:\n  fallbackCooldown: soon\n", "fallbackCooldown"},
		{"empty entry", "retry:\n  fallbackChains:\n    smol: [\"\"]\n", "entry"},
		{"reserve pct", "retry:\n  reservePct: 300\n", "reservePct"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDEV_AGENT_DIR", t.TempDir())
			overlay := t.TempDir() + "/overlay.yml"
			writeFile(t, overlay, tc.body)
			_, err := LoadSettings(t.TempDir(), []string{overlay})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// TestSettingsRetryLayerMerge: chain tables deep-merge per key, so a project
// layer can add one chain without restating the global table.
func TestSettingsRetryLayerMerge(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	cwd := t.TempDir()
	writeFile(t, GlobalSettingsPath(), "retry:\n  fallbackChains:\n    smol: [\"other/dev\"]\n")
	writeFile(t, cwd+"/.xdev/config.yml", "retry:\n  fallbackChains:\n    slow: [\"third/big\"]\n")
	s, err := LoadSettings(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Retry.FallbackChains) != 2 || s.Retry.FallbackChains["smol"][0] != "other/dev" || s.Retry.FallbackChains["slow"][0] != "third/big" {
		t.Fatalf("chains = %v, want both layers", s.Retry.FallbackChains)
	}
}

// TestSettingsRetrySetRoundTrip: `xdev config set` writes the new keys into
// the shape the schema reads back.
func TestSettingsRetrySetRoundTrip(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	overlay := t.TempDir() + "/overlay.yml"
	if err := Set(overlay, "retry.reserveThreshold", "confirm"); err != nil {
		t.Fatal(err)
	}
	if err := Set(overlay, "retry.fallbackChains.smol", "other/dev,third/big"); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings(t.TempDir(), []string{overlay})
	if err != nil {
		t.Fatal(err)
	}
	if s.ReservePolicy() != ReserveThresholdConfirm {
		t.Fatalf("reserve policy = %q", s.ReservePolicy())
	}
	if got := s.Retry.FallbackChains["smol"]; len(got) != 2 || got[1] != "third/big" {
		t.Fatalf("chain = %v", got)
	}
}

// TestCredentialPoolMultiKey: the models.yml credential pool is the rotation
// order, pool[0] is exactly the key the ordinary chain picks, and an index
// past the pool is refused instead of silently reusing the primary.
func TestCredentialPoolMultiKey(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	path := writeFile(t, t.TempDir()+"/models.yml", `
providers:
  onegw:
    baseUrl: https://example.test/v1
    api: openai-completions
    apiKey: primary-key
    apiKeys: ["primary-key", "sibling-key", "  "]
    models:
      - id: free
  keysonly:
    baseUrl: https://example.test/v1
    api: openai-completions
    apiKeys: ["first-key", "second-key"]
    models:
      - id: free
`)
	cfg, err := LoadModels(path)
	if err != nil {
		t.Fatal(err)
	}
	pc := cfg.Providers["onegw"]
	if got := CredentialPool(pc, "free"); len(got) != 2 || got[0] != "primary-key" || got[1] != "sibling-key" {
		t.Fatalf("pool = %v, want the primary then the sibling (blanks and duplicates dropped)", got)
	}
	if got := CredentialKey(pc, "free", 1); got != "sibling-key" {
		t.Fatalf("CredentialKey(1) = %q", got)
	}
	if got := CredentialKey(pc, "free", 2); got != "" {
		t.Fatalf("CredentialKey(2) = %q, want empty past the pool", got)
	}

	req := CredentialRequest{Provider: "onegw", Model: "free", ProviderCfg: pc}
	primary, err := ResolveCredential(req)
	if err != nil {
		t.Fatal(err)
	}
	if primary.Value != "primary-key" || primary.Source != "models.yml" {
		t.Fatalf("primary = %+v", primary)
	}
	rotated, err := ResolveCredential(CredentialRequest{Provider: "onegw", Model: "free", ProviderCfg: pc, PoolIndex: 1})
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Value != "sibling-key" {
		t.Fatalf("rotation = %+v, want the sibling key", rotated)
	}
	if _, err := ResolveCredential(CredentialRequest{Provider: "onegw", Model: "free", ProviderCfg: pc, PoolIndex: 9}); err == nil {
		t.Fatal("an exhausted rotation index must be an error, not the primary key")
	}

	// apiKeys alone is a configured provider: its first entry is the primary.
	keyless := cfg.Providers["keysonly"]
	first, err := ResolveCredential(CredentialRequest{Provider: "keysonly", Model: "free", ProviderCfg: keyless})
	if err != nil {
		t.Fatal(err)
	}
	if first.Value != "first-key" {
		t.Fatalf("apiKeys-only provider resolved %+v", first)
	}
}
