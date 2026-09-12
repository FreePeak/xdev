package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"context"
	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/memory"
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

// #82: compaction.idleAfter / compaction.async were parsed, validated and
// listed in `config list` — and copied nowhere, so neither trigger could fire.
func TestWireAgentModeAppliesCompactionSettings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	on := true
	s := &config.Settings{}
	s.Compaction.IdleAfter = "90s"
	s.Compaction.Async = &on
	ag := &agent.Agent{Model: "m", Compaction: agent.CompactionConfig{ContextWindow: 1000}}
	wireAgentMode(ag, nil, &config.Config{}, s, "", t.TempDir())
	if ag.Compaction.IdleAfter != 90*time.Second {
		t.Fatalf("IdleAfter = %v, want 90s from settings", ag.Compaction.IdleAfter)
	}
	if !ag.Compaction.Async {
		t.Fatal("Async not applied")
	}
	// The fields the build site already set must survive the seam.
	if ag.Compaction.ContextWindow != 1000 {
		t.Fatalf("ContextWindow clobbered: %d", ag.Compaction.ContextWindow)
	}
	if len(ag.Compaction.Methods) != 0 {
		t.Fatal("Methods unexpectedly changed")
	}
}

// An abort must drop the in-flight background summarize; wiring the cancel
// into the loop's abort path is what makes the async trigger safe.
func TestAbortCancelsAsyncCompaction(t *testing.T) {
	ag := &agent.Agent{Model: "m"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Run must return the cancellation and not panic on the (absent) job.
	_, err := ag.Run(ctx, "sys", []ai.Message{{Role: ai.RoleUser,
		Content: []ai.Block{ai.TextBlock{Text: "x"}}}})
	if err == nil {
		t.Fatal("a canceled context must end the run with an error")
	}
	ag.CancelAsyncCompaction() // safe with nothing in flight
}

// #86: the remote memory backend's turn cadence and its compaction context
// both had no production caller in the interactive path.
func TestWireAgentModeCarriesMemoryContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ag := &agent.Agent{Model: "m"}
	wireAgentMode(ag, nil, &config.Config{}, &config.Settings{}, "", t.TempDir())
	// Backend off: no seam, and nothing panics.
	if ag.MemoryContext != nil {
		t.Fatal("MemoryContext set with no remote backend configured")
	}
}

// #89: the friction detector only ever saw print-mode turns. The shared feed
// helper is what the TUI submit path calls now, so it is pinned here: only the
// sharpshooter backend observes, an empty turn is not scored, and a turn after
// a failure carries the AfterFailure signal the detector weights.
func TestObserveFrictionFeedsSharpshooter(t *testing.T) {
	dir := t.TempDir()
	ss := &memory.SharpShooter{Dir: dir, Threshold: 2}
	if observeFriction(ss, "use tabs", false) {
		t.Fatal("a first statement is not friction")
	}
	observeFriction(ss, "   ", false) // no text: no score
	// The restatement (+2, its own threshold) is what a friction feed exists
	// to catch.
	if !observeFriction(ss, "use tabs", true) {
		t.Fatal("the repeated instruction did not cross the threshold: the feed is inert")
	}
	// Every other backend is a no-op (and must not panic).
	if observeFriction(&memory.Backend{Dir: dir}, "x", false) {
		t.Fatal("markdown backend reported friction")
	}
	if observeFriction(nil, "x", false) {
		t.Fatal("nil backend reported friction")
	}
}
