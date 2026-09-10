package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

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
	reg := newToolRegistry(t.TempDir(), nil, "p", "m", nil)
	for i := range 3 {
		reg.Register(verboseTool{name: fmt.Sprintf("late_%d", i)})
	}
	got := promptFn(basePrompt(printOptions{}), t.TempDir(), reg, "")()
	if tokens := len([]rune(got)) / 4; tokens >= maxPromptTokens {
		t.Fatalf("prompt with verbose remote tools is ~%d tokens (budget %d) — the description cap is not holding",
			tokens, maxPromptTokens)
	}
	if !strings.Contains(got, "late_0") {
		t.Fatal("late-registered tool vanished from the prompt")
	}
}

func TestBundledPromptStaysUnderBudget(t *testing.T) {
	reg := newToolRegistry(t.TempDir(), nil, "p", "m", nil)
	got := promptFn(basePrompt(printOptions{}), t.TempDir(), reg, "")()
	tokens := len([]rune(got)) / 4
	if tokens >= maxPromptTokens {
		t.Fatalf("system prompt is ~%d tokens (budget %d): %d chars across %d tools — trim a description or make a deliberate PRD change",
			tokens, maxPromptTokens, len([]rune(got)), len(reg.Defs()))
	}
	if len(reg.Defs()) < 9 {
		t.Fatalf("expected the full bundled tool surface, got %d", len(reg.Defs()))
	}
}
