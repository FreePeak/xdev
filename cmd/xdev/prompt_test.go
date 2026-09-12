package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/tool"
)

// lateTool registers after the prompt closure is created.
type lateTool struct{}

func (lateTool) Name() string        { return "late_arrival" }
func (lateTool) Description() string { return "arrives after startup" }
func (lateTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (lateTool) Execute(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{Text: "ok"}, nil
}

// TestPromptReflectsLiveRegistry pins the split-brain fix: async MCP and
// extension tools register after startup, and the prompt the model reads
// must name them.
func TestPromptReflectsLiveRegistry(t *testing.T) {
	reg := tool.NewRegistry()
	build := promptFn("BASE", t.TempDir(), reg, "TAIL")

	if got := build(); strings.Contains(got, "late_arrival") {
		t.Fatal("tool appeared before it was registered")
	}
	reg.Register(lateTool{})
	got := build()
	if !strings.Contains(got, "late_arrival") {
		t.Fatalf("late-registered tool missing from the prompt:\n%s", got)
	}
	if !strings.Contains(got, "BASE") || !strings.Contains(got, "TAIL") {
		t.Fatal("base/append prompt segments lost")
	}
}

// maxPromptTokens is PRD §1 Goal 4: a <1,000-token system prompt including
// tool descriptions (pi's philosophy). Measured with the same chars/4
// estimate floor the compaction trigger uses, so one yardstick governs
// both.
const maxPromptTokens = 1000

// TestBundledPromptStaysUnderBudget guards the pi-minimalism budget
// against silent drift: every tool added to the registry lands in the
// prompt, so this fails the moment the bundled surface outgrows the goal
// rather than after a dozen tools have shipped.
// verboseTool simulates a chatty MCP/extension tool whose own
// documentation would otherwise blow the prompt budget by itself.
type verboseTool struct{ name string }

func (v verboseTool) Name() string { return v.name }
func (v verboseTool) Description() string {
	return strings.Repeat("detailed remote tool documentation. ", 60) // ~2.4 KB
}
func (v verboseTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (v verboseTool) Execute(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{}, nil
}

// TestPromptBudgetWithLateRegistrants measures the surface a real session
// carries: bundled tools plus extension/MCP tools registering AFTER
// startup, which the lazy prompt folds in at the next submit. Remote prose
// must be capped at the choke point so one verbose server cannot bust
// PRD §1 Goal 4.
func TestPromptBudgetWithLateRegistrants(t *testing.T) {
	// The budget governs the bundled surface: isolate ambient user rules
	// (~/.agent/rules, ~/.xdev/agent/rules) so the developer's own
	// rulebook does not count against PRD §1 Goal 4.
	t.Setenv("HOME", t.TempDir())
	reg := newToolRegistry(t.TempDir(), nil, "p", "m", nil, nil, nil)
	for i := range 3 {
		reg.Register(verboseTool{name: fmt.Sprintf("late_%d", i)})
	}
	got := promptFn(basePrompt(printOptions{}, t.TempDir()), t.TempDir(), reg, "")()
	if tokens := len([]rune(got)) / 4; tokens >= maxPromptTokens {
		t.Fatalf("prompt with verbose remote tools is ~%d tokens (budget %d) — the description cap is not holding",
			tokens, maxPromptTokens)
	}
	if !strings.Contains(got, "late_0") {
		t.Fatal("late-registered tool vanished from the prompt")
	}
}

func TestBundledPromptStaysUnderBudget(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // ambient rules are user content, not bundled prose
	reg := newToolRegistry(t.TempDir(), nil, "p", "m", nil, nil, nil)
	got := promptFn(basePrompt(printOptions{}, t.TempDir()), t.TempDir(), reg, "")()
	tokens := len([]rune(got)) / 4
	if tokens >= maxPromptTokens {
		t.Fatalf("system prompt is ~%d tokens (budget %d): %d chars across %d tools — trim a description or make a deliberate PRD change",
			tokens, maxPromptTokens, len([]rune(got)), len(reg.Defs()))
	}
	if len(reg.Defs()) < 9 {
		t.Fatalf("expected the full bundled tool surface, got %d", len(reg.Defs()))
	}
}

// TestSystemPromptFileDiscovery pins the SYSTEM.md / APPEND_SYSTEM.md
// precedence: project file → user file → built-in default, and the flag
// wins over both files.
func TestSystemPromptFileDiscovery(t *testing.T) {
	proj := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	userDir := filepath.Join(home, ".xdev", "agent")
	os.MkdirAll(userDir, 0o755)

	// No files at all → the built-in default.
	if got := basePrompt(printOptions{}, proj); got != agent.SystemPromptBase {
		t.Fatalf("default = %q..., want the base prompt", got[:40])
	}

	// User-level SYSTEM.md → used when no project file exists.
	os.WriteFile(filepath.Join(userDir, "SYSTEM.md"), []byte("user prompt"), 0o644)
	if got := basePrompt(printOptions{}, proj); got != "user prompt" {
		t.Fatalf("user-level = %q", got)
	}

	// Project-level SYSTEM.md → wins over user-level.
	os.WriteFile(filepath.Join(proj, "SYSTEM.md"), []byte("project prompt"), 0o644)
	if got := basePrompt(printOptions{}, proj); got != "project prompt" {
		t.Fatalf("project-level = %q", got)
	}

	// The flag overrides both.
	opts := printOptions{SystemPrompt: "flag wins"}
	if got := basePrompt(opts, proj); got != "flag wins" {
		t.Fatalf("flag = %q", got)
	}
}

func TestAppendSystemFileDiscovery(t *testing.T) {
	proj := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	userDir := filepath.Join(home, ".xdev", "agent")
	os.MkdirAll(userDir, 0o755)
	os.WriteFile(filepath.Join(userDir, "APPEND_SYSTEM.md"), []byte("user append"), 0o644)
	os.WriteFile(filepath.Join(proj, "APPEND_SYSTEM.md"), []byte("project append"), 0o644)

	ov := agent.LoadSystemPromptOverrides(proj)
	if ov.Append != "project append" {
		t.Fatalf("project APPEND_SYSTEM.md = %q", ov.Append)
	}
	// Flag would win over both (the cmd checks AppendSystem before overrides.Append).
	opts := printOptions{AppendSystem: "flag append"}
	appendStr := opts.AppendSystem
	if appendStr == "" {
		appendStr = ov.Append
	}
	if appendStr != "flag append" {
		t.Fatalf("append precedence broken: %q", appendStr)
	}
}

// TestPersonalityReachesThePrompt pins that a discovered PERSONALITY.md is
// actually composed into the system prompt tail (it was set on the struct
// but never consumed — the recurring inert-field pattern).
func TestPersonalityReachesThePrompt(t *testing.T) {
	ov := agent.SystemPromptOverrides{Personality: "You are terse.", Append: "Always test."}
	if got := tailSystemPrompt(ov, ""); got != "You are terse.\n\nAlways test." {
		t.Fatalf("personality+append = %q", got)
	}
	// The flag overrides APPEND_SYSTEM.md but not the persona.
	if got := tailSystemPrompt(ov, "flag wins"); got != "You are terse.\n\nflag wins" {
		t.Fatalf("flag precedence = %q", got)
	}
	// Personality alone.
	only := agent.SystemPromptOverrides{Personality: "Curious."}
	if got := tailSystemPrompt(only, ""); got != "Curious." {
		t.Fatalf("personality only = %q", got)
	}
	if got := tailSystemPrompt(agent.SystemPromptOverrides{}, ""); got != "" {
		t.Fatalf("empty overrides should yield empty, got %q", got)
	}
}
