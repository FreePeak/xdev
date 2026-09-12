package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/tool"
)

// wireAgentMode is the single place the per-mode agent seams are applied.
// Both of them were once print-only (#79 catalog bridge, #80 secrets
// redactor), which is exactly the failure this function exists to prevent: a
// mode that hand-builds an agent silently runs without them.

func TestWireAgentModeInstallsBothSeams(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".xdev"), 0o755); err != nil {
		t.Fatal(err)
	}
	const secret = "tok-live-secret-value-123"
	if err := os.WriteFile(filepath.Join(cwd, ".xdev", "secrets.yml"),
		[]byte("- name: live\n  value: "+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	reg := tool.NewRegistry()
	ag := &agent.Agent{Tools: reg, Model: "m"}
	wireAgentMode(ag, reg, &config.Config{}, &config.Settings{}, "", cwd)

	// 1. Redactor: the secret leaves context as a placeholder and comes back
	// on the way in (the reversible round trip is the whole contract).
	if ag.Redactor == nil {
		t.Fatal("no redactor installed — the mode seam is unwired again")
	}
	masked := ag.Redactor.Apply("output contains " + secret + " here")
	if strings.Contains(masked, secret) {
		t.Fatalf("secret reached provider context unredacted: %q", masked)
	}
	if !strings.Contains(masked, "$$") {
		t.Fatalf("expected a $$placeholder$$, got %q", masked)
	}
	if back := ag.Redactor.Expand(masked); !strings.Contains(back, secret) {
		t.Fatalf("placeholder did not round-trip back to the value: %q", back)
	}
}

// A nil registry must not panic (modes that build an agent without the shared
// registry still need the redactor seam).
func TestWireAgentModeToleratesNilRegistry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ag := &agent.Agent{Model: "m"}
	wireAgentMode(ag, nil, &config.Config{}, &config.Settings{}, "", t.TempDir())
	if ag.Redactor == nil {
		t.Fatal("redactor must still be installed with no registry")
	}
	wireAgentMode(nil, nil, &config.Config{}, &config.Settings{}, "", t.TempDir()) // must not panic
}

// The config the redactor reads is the project one, so a secret declared in
// $HOME's global store is honored too (the load chain is config.OpenRedactor's
// job; this pins that wiring the mode helper reaches it).
func TestWireAgentModeUsesGlobalSecrets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := config.DataDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const secret = "ghp-global-value-9999"
	if err := os.WriteFile(config.GlobalSecretsPath(),
		[]byte("secrets:\n  - name: gt\n    value: "+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ag := &agent.Agent{Model: "m"}
	wireAgentMode(ag, nil, &config.Config{}, &config.Settings{}, "", t.TempDir())
	if got := ag.Redactor.Apply("token " + secret); strings.Contains(got, secret) {
		t.Fatalf("global secret not masked: %q", got)
	}
}

// #84: a declared retry.fallbackChains order must drive the failover list —
// the engine resolved chains but no production path ever consulted settings, so
// the order a user wrote was overridden by context-window size.
func TestFailoverChainHonoursDeclaredOrder(t *testing.T) {
	cfg := &config.Config{Providers: map[string]*config.ProviderConfig{
		"small": {API: "openai-completions", BaseURL: "http://127.0.0.1:1/v1", APIKey: "k",
			Models: []config.ModelConfig{{ID: "s1", ContextWindow: 1000}}},
		"big": {API: "openai-completions", BaseURL: "http://127.0.0.1:2/v1", APIKey: "k",
			Models: []config.ModelConfig{{ID: "b1", ContextWindow: 900000}}},
		"mid": {API: "openai-completions", BaseURL: "http://127.0.0.1:3/v1", APIKey: "k",
			Models: []config.ModelConfig{{ID: "m1", ContextWindow: 50000}}},
	}}
	s := &config.Settings{Retry: config.RetrySettings{
		FallbackChains: map[string][]string{"onegw/x": {"mid/m1", "small/s1"}},
	}}
	got := failoverChain(cfg, s, "", "onegw", "x")
	if len(got) == 0 {
		t.Skip("providers unbuildable in this environment")
	}
	// Declared order wins over the window ranking (big would otherwise be first).
	if len(got) < 2 {
		t.Fatalf("chain = %d targets", len(got))
	}
	if !strings.Contains(got[0].Model, "m1") {
		t.Fatalf("first target = %q, want the declared mid/m1 (not window-ranked)", got[0].Model)
	}
	// With no chain declared, the window ranking still applies.
	plain := failoverChain(cfg, &config.Settings{}, "", "onegw", "x")
	if len(plain) == 0 || plain[0].Model != "b1" {
		t.Fatalf("undeclared chain = %+v, want the biggest window first", plain)
	}
}

// Arms the fallback state so the reserve/rotation machinery has a live object.
func TestWireAgentModeArmsFallbackState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ag := &agent.Agent{Model: "m"}
	st := wireAgentMode(ag, nil, &config.Config{}, &config.Settings{}, "smol", t.TempDir())
	if st == nil {
		t.Fatal("fallback state not armed — retry.fallbackChains/reserve/revert stay unreachable")
	}
	if st.Role != "smol" {
		t.Fatalf("role = %q, want the active role", st.Role)
	}
	if st.Rotate == nil {
		t.Fatal("credential rotation seam unwired: a spent apiKeys key fails the run")
	}
}
