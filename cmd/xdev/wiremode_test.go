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
	wireAgentMode(ag, reg, cwd)

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
	wireAgentMode(ag, nil, t.TempDir())
	if ag.Redactor == nil {
		t.Fatal("redactor must still be installed with no registry")
	}
	wireAgentMode(nil, nil, t.TempDir()) // must not panic
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
	wireAgentMode(ag, nil, t.TempDir())
	if got := ag.Redactor.Apply("token " + secret); strings.Contains(got, secret) {
		t.Fatalf("global secret not masked: %q", got)
	}
}
