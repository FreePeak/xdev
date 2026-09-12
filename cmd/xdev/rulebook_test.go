package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/rules"
	"github.com/FreePeak/xdev/internal/tool"
)

// #108: glob-scoped rulebooks were discovered, listed in the prompt, and never
// consumed — rules.ForPath had no caller. A rule whose globs match the file
// being edited now rides that tool result.

type writeStubProvider struct{ scripts [][]ai.Event }

func (p *writeStubProvider) Stream(_ context.Context, _ ai.StreamRequest) (<-chan ai.Event, error) {
	ev := p.scripts[0]
	p.scripts = p.scripts[1:]
	ch := make(chan ai.Event, len(ev))
	for _, e := range ev {
		ch <- e
	}
	close(ch)
	return ch, nil
}
func (p *writeStubProvider) Name() string { return "stub" }
func (p *writeStubProvider) API() string  { return "stub" }

func TestRulebookNoteRidesEditToolResult(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	// A glob-scoped rulebook rule for Go files.
	if err := os.MkdirAll(filepath.Join(dir, ".cursor", "rules"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Base-name glob: the documented matcher dialect (path.Match has no real
	// "**", so "**/*.go" is not a supported form here).
	if err := os.WriteFile(filepath.Join(dir, ".cursor", "rules", "go-style.mdc"),
		[]byte("---\ndescription: Go house style\nglobs: \"*.go\"\nalwaysApply: false\n---\nAlways table-drive tests in this repo.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := rules.Active()
	t.Cleanup(func() { rules.Set(prev) })
	rules.Set(rules.Discover(dir, nil))

	// The consumption point: a scoped rule surfaces for the matching path…
	if note := rulebookNoteFor(filepath.Join(dir, "pkg", "a.go")); !strings.Contains(note, "table-drive") {
		t.Fatalf("glob-scoped rule never reached the model for a matching path:\n%q", note)
	} else if !strings.Contains(note, "Go house style") {
		t.Fatalf("the rule description should head the notice:\n%q", note)
	}
	// A non-matching path still gets the always-apply rules (correct: the
	// builtin rulebook applies everywhere) but NOT the glob-scoped one — the
	// distinction the whole feature turns on.
	if note := rulebookNoteFor(filepath.Join(dir, "README.md")); strings.Contains(note, "table-drive") {
		t.Fatalf("a *.go rule must not fire for a markdown file:\n%q", note)
	}
	// Nothing discovered at all renders no notice (no transcript noise).
	rules.Set(nil)
	if note := rulebookNoteFor(filepath.Join(dir, "a.go")); note != "" {
		t.Fatalf("no rules must mean no notice: %q", note)
	}
}

// End to end through the agent loop: the notice lands in the tool result the
// model reads back.
func TestAgentInjectsRulebookNotice(t *testing.T) {
	prov := &writeStubProvider{scripts: [][]ai.Event{
		{
			{Type: ai.EventStart, Provider: "stub", Model: "m"},
			{Type: ai.EventToolcallStart, ToolCallID: "c1", ToolName: "echo-write", StreamIndex: 0},
			{Type: ai.EventToolcallEnd, StreamIndex: 0, PartialJSON: `{"path":"pkg/a.go"}`},
			ai.Donef(ai.StopReasonStop, nil, &ai.Message{
				Role: ai.RoleAssistant,
				Content: []ai.Block{ai.ToolCallBlock{ID: "c1", Name: "edit",
					Arguments: json.RawMessage(`{"path":"pkg/a.go"}`), StreamIndex: 0}},
				StopReason: ai.StopReasonStop,
			}),
		},
		{
			{Type: ai.EventStart, Provider: "stub", Model: "m"},
			ai.Donef(ai.StopReasonStop, nil, &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
				Content: []ai.Block{ai.TextBlock{Text: "done"}}}),
		},
	}}
	reg := tool.NewRegistry()
	reg.Register(rulebookProbeTool{})
	var results []string
	ag := &agent.Agent{
		Provider: prov, Tools: reg, Model: "m",
		// The loop's own tool-result seam is what the model actually reads.
		Hooks: agent.TurnHooksFunc{OnToolResultMsgF: func(m *ai.Message) {
			results = append(results, m.Text())
		}},
		Rulebook: func(path string) string { return "USE TABLE-DRIVEN TESTS for " + path },
	}
	_, err := ag.Run(context.Background(), "sys", []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "go"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(results, "\n")
	if !strings.Contains(joined, "USE TABLE-DRIVEN TESTS for pkg/a.go") {
		t.Fatalf("rulebook notice never reached the tool result: %s", joined)
	}
	if !strings.Contains(joined, "edited 1 file") {
		t.Fatalf("the notice replaced the tool's own output: %s", joined)
	}
}

// rulebookProbeTool is a mutating tool whose result the loop decorates.
type rulebookProbeTool struct{}

func (rulebookProbeTool) Name() string { return "edit" }
func (rulebookProbeTool) Description() string {
	return "probe edit tool standing in for the real writer"
}
func (rulebookProbeTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)
}
func (rulebookProbeTool) Tier() tool.Tier { return tool.TierWrite }
func (rulebookProbeTool) Execute(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{Text: "edited 1 file"}, nil
}
