package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// A model that garbles a real tool's name used to get four words back —
// `unknown tool "ash_g2"` — which names what is NOT there and tells it
// nothing to do. Measured 2026-10-10 over every stored session on this box
// (275 files): 17 of the 26 calls that hit runOneTool's miss branch were
// one garbled name, `ash_edit` (= `ast_edit` minus a character), and the
// sessions that received it answered with more junk names — ash_g1,
// ash_g2, ash_g3, ash_g4, each with `{}` for arguments, one full round
// trip each. The miss text now names the near-miss and lists the deferred
// catalog, so the recovery is one call.
//
// The suggestion is text only. This test pins that a near-miss NEVER runs
// the tool it suggested: only the model can re-send, and a re-sent call
// takes the ordinary path (plan-mode gate, approval policy, interceptor
// chain) like any other call.
func TestUnknownToolNameSuggestsTheNearMiss(t *testing.T) {
	var ran []string
	reg := tool.NewRegistry()
	for _, n := range []string{"read", "edit", "bash", "grep", "todo"} {
		reg.Register(nameStub{name: n, ran: &ran})
	}
	reg.Register(nameStub{name: "ast_edit", ran: &ran})
	reg.Defer("ast_edit", "AST-aware codemod rewrites", "edit", "codemod")
	a := &Agent{Tools: reg, Hooks: &hookLog{}, Model: "m"}

	msg := a.runOneTool(context.Background(), ai.ToolCallBlock{Name: "ash_edit"})
	got := msg.Text()
	if !strings.Contains(got, "unknown tool") {
		t.Fatalf("a miss is still a miss: %q", got)
	}
	if !strings.Contains(got, `"ast_edit"`) {
		t.Fatalf("the near-miss must be named so the model can re-send: %q", got)
	}
	if len(ran) != 0 {
		t.Fatalf("a suggested tool ran: %v — the harness must never pick which tool a mangled name meant", ran)
	}
}

// A name with no near-miss gets the honest answer plus the bridge, and the
// deferred names it cannot see in its tool schema — the same separation
// internal/tool's missText makes for the bridge path.
func TestUnknownToolWithoutAMatchAnswersAndPointsAtTheBridge(t *testing.T) {
	var ran []string
	reg := tool.NewRegistry()
	reg.Register(nameStub{name: "read", ran: &ran})
	reg.Register(nameStub{name: "bash", ran: &ran})
	reg.Register(nameStub{name: "github", ran: &ran})
	reg.Defer("github", "GitHub operations: PRs, issues, files", "git", "pr")
	a := &Agent{Tools: reg, Hooks: &hookLog{}, Model: "m"}

	msg := a.runOneTool(context.Background(), ai.ToolCallBlock{Name: "ash_g2"})
	got := msg.Text()
	for _, want := range []string{
		"unknown tool", tool.ToolSearchName, "github",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("miss text missing %q:\n%s", want, got)
		}
	}
	// A far name must not be force-fit onto a real tool: ash_g2 is not a
	// typo of anything here, and guessing would send a call somewhere the
	// model never asked for.
	if strings.Contains(got, "closest registered tool") {
		t.Fatalf("a far name was given a suggestion it cannot verify: %q", got)
	}
	if len(ran) != 0 {
		t.Fatalf("tool ran for an unknown name: %v", ran)
	}
}

// The rule that keeps the suggestion honest is distance, and it is the
// repo's own near-miss rule (fallback_chain.go): edit distance ≤ 2 with a
// length delta ≤ 2. `ash_g2` is 4 edits from every real tool here; `gash`
// is 1 from bash and must be told so.
func TestNearToolNameIsBoundedAndDeterministic(t *testing.T) {
	var ran []string
	reg := tool.NewRegistry()
	for _, n := range []string{"bash", "grep", "read", "ast_edit"} {
		reg.Register(nameStub{name: n, ran: &ran})
	}
	for _, tc := range []struct {
		name string
		want string
		ok   bool
	}{
		{"ash_edit", "ast_edit", true},
		{"gash", "bash", true},
		{"brep", "grep", true},
		{"ash_g2", "", false}, // 4 edits from everything
		{"", "", false},
	} {
		got, ok := nearToolName(reg, tc.name)
		if ok != tc.ok || got != tc.want {
			t.Errorf("nearToolName(%q) = %q, %v; want %q, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// nameStub is a registry tool that records its execution.
type nameStub struct {
	name string
	ran  *[]string
}

func (s nameStub) Name() string        { return s.name }
func (s nameStub) Description() string { return "stub for one registry name" }
func (s nameStub) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (s nameStub) Execute(_ context.Context, args json.RawMessage) (tool.Result, error) {
	*s.ran = append(*s.ran, s.name+" "+string(args))
	return tool.Result{Text: s.name + " ran"}, nil
}
