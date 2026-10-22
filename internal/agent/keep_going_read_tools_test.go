package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// bashStub is a tool NAMED "bash" that returns a canned result.
//
// The gate judges a bash call by its COMMAND LINE — tool.BashCallIsReadOnly
// over the `command` argument — not by what the tool does, so the stub never
// runs a shell and the test never touches the network.
type bashStub struct{ echoTool }

func (bashStub) Name() string { return "bash" }
func (bashStub) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)
}

// multiOpStub is a tool whose ops split into reads and writes, the shape the
// github tool and the hub tool have. It never runs anything: the gate asks
// the op, not the result.
type multiOpStub struct {
	echoTool
	name    string
	readOps []string
}

func (s multiOpStub) Name() string { return s.name }
func (s multiOpStub) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"op":{"type":"string"}}}`)
}
func (s multiOpStub) ReadOnlyOps() []string { return s.readOps }

// The field report this file exists for.
//
// Live 2026-10-21, session 4c4addab: the user asked "list me all pr urls
// openningin xdev". The model ran `gh pr list` twelve times and the github
// tool once, and at 22:44:46.566 it DELIVERED the answer — the open PR list,
// with the URLs:
//
//	**`FreePeak/xdev` has zero open PRs** — nothing to review there right now.
//	| PR | Title |
//	| https://github.com/FreePeak/xdev-private2/pull/3 | chore(deps): ... |
//
// In the SAME millisecond the harness injected a keep-going nudge:
//
//	continue the unfinished work: take the next concrete step with tools. If
//
// this turn already delivered the answer or report the user asked for, say so
// in one sentence and stop. Do not start implementing after a research or
// report request.
//
// The model obeyed the nudge and spent the next three minutes reading the
// diffs of two unrelated worktrees. That is the "very unrelated work".
//
// The cause was the gate's definition of "changed": `gh` was not in the
// read-only command table, so every `gh pr list` was reported as a mutation
// and a research turn armed the nudge.
//
// The gate's own history says this is the bug it exists to prevent. Commit
// 89e06aa: "A research session reads twenty files, so every one of them
// earned a nudge whose own text reads 'take the next concrete step now with
// tools. Do not stop at a plan or a status report': precisely what the
// request did not ask for."
func TestKeepGoingStaysOffAfterAGhReadOnlyTurn(t *testing.T) {
	readThenAnswer := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
		Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{
			ai.TextBlock{Text: "listing the open PRs"},
			ai.ToolCallBlock{ID: "g1", Name: "bash", Arguments: json.RawMessage(
				`{"command":"gh pr list --repo FreePeak/xdev --state open --limit 100 --json number,url"}`)},
		},
	})}}
	answer := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
		Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.TextBlock{Text: "**FreePeak/xdev** has zero open PRs."}},
	})}}

	p := &fakeProvider{calls: []fakeScript{readThenAnswer, answer}}
	reg := tool.NewRegistry()
	reg.Register(bashStub{})
	a := &Agent{Provider: p, Tools: reg, Hooks: TurnHooksFunc{}, PromptContinuation: true}

	history := []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "list me all pr urls openningin xdev"}}}}
	if _, err := a.Run(context.Background(), "sys", history); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.gotReqs) != 2 {
		t.Fatalf("stream requests = %d, want 2: the answer was the deliverable", len(p.gotReqs))
	}
	for i, r := range p.gotReqs {
		last := r.Messages[len(r.Messages)-1]
		if last.Role == ai.RoleUser && last.Attribution == PromptContinuationAttribution {
			t.Fatalf("request %d got a keep-going nudge after the answer was delivered", i)
		}
	}
}

// The other direction, which must not rot: a `gh` line that WRITES still
// arms the gate. `gh pr merge` changes a repository, so a run that merged a
// PR and then yielded owes a summary.
func TestKeepGoingStillFiresForAGhWrite(t *testing.T) {
	mergeThenYield := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
		Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{
			ai.TextBlock{Text: "merging"},
			ai.ToolCallBlock{ID: "m1", Name: "bash", Arguments: json.RawMessage(
				`{"command":"gh pr merge 123 --repo FreePeak/xdev --squash"}`)},
		},
	})}}
	yield := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
		Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.TextBlock{Text: "still going"}},
	})}}
	done := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
		Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.TextBlock{Text: "done"}},
	})}}

	p := &fakeProvider{calls: []fakeScript{mergeThenYield, yield, done}}
	reg := tool.NewRegistry()
	reg.Register(bashStub{})
	a := &Agent{Provider: p, Tools: reg, Hooks: TurnHooksFunc{}, PromptContinuation: true}

	history := []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "merge pr 123"}}}}
	if _, err := a.Run(context.Background(), "sys", history); err != nil {
		t.Fatalf("Run: %v", err)
	}
	sawNudge := false
	for _, r := range p.gotReqs {
		last := r.Messages[len(r.Messages)-1]
		if last.Role == ai.RoleUser && last.Attribution == PromptContinuationAttribution {
			sawNudge = true
		}
	}
	if !sawNudge {
		t.Fatal("a run that merged a PR got no keep-going nudge")
	}
}

// The same rule for a TOOL that mixes reads and writes.
//
// Live 2026-10-21, session 4c4addab: the model answered "list me all pr
// urls openningin xdev" with the github tool's search_prs op and with
// `gh pr list` in bash. Both calls counted as mutations — neither the tool
// nor the shell command was in the classifier — so the gate armed on a turn
// that only read, and the harness pushed the model past the answer.
func TestKeepGoingStaysOffAfterAReadOnlyToolOp(t *testing.T) {
	readThenAnswer := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
		Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{
			ai.TextBlock{Text: "searching the PRs"},
			ai.ToolCallBlock{ID: "s1", Name: "github", Arguments: json.RawMessage(
				`{"op":"search_prs","repo":"FreePeak/xdev","limit":50}`)},
		},
	})}}
	answer := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
		Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.TextBlock{Text: "**FreePeak/xdev** has zero open PRs."}},
	})}}

	p := &fakeProvider{calls: []fakeScript{readThenAnswer, answer}}
	reg := tool.NewRegistry()
	reg.Register(multiOpStub{name: "github", readOps: []string{"search_prs", "repo_view"}})
	a := &Agent{Provider: p, Tools: reg, Hooks: TurnHooksFunc{}, PromptContinuation: true}

	history := []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "list me all pr urls openningin xdev"}}}}
	if _, err := a.Run(context.Background(), "sys", history); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.gotReqs) != 2 {
		t.Fatalf("stream requests = %d, want 2: the answer was the deliverable", len(p.gotReqs))
	}
	for i, r := range p.gotReqs {
		last := r.Messages[len(r.Messages)-1]
		if last.Role == ai.RoleUser && last.Attribution == PromptContinuationAttribution {
			t.Fatalf("request %d got a keep-going nudge after the answer was delivered", i)
		}
	}
}

// The other direction for a mixed tool: its WRITE op still arms the gate.
func TestKeepGoingStillFiresForAWriteToolOp(t *testing.T) {
	createThenYield := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
		Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{
			ai.TextBlock{Text: "opening the PR"},
			ai.ToolCallBlock{ID: "c1", Name: "github", Arguments: json.RawMessage(
				`{"op":"pr_create","title":"x","body":"y"}`)},
		},
	})}}
	yield := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
		Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.TextBlock{Text: "still going"}},
	})}}
	done := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
		Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.TextBlock{Text: "done"}},
	})}}

	p := &fakeProvider{calls: []fakeScript{createThenYield, yield, done}}
	reg := tool.NewRegistry()
	reg.Register(multiOpStub{name: "github", readOps: []string{"search_prs", "repo_view"}})
	a := &Agent{Provider: p, Tools: reg, Hooks: TurnHooksFunc{}, PromptContinuation: true}

	history := []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "open a PR"}}}}
	if _, err := a.Run(context.Background(), "sys", history); err != nil {
		t.Fatalf("Run: %v", err)
	}
	sawNudge := false
	for _, r := range p.gotReqs {
		last := r.Messages[len(r.Messages)-1]
		if last.Role == ai.RoleUser && last.Attribution == PromptContinuationAttribution {
			sawNudge = true
		}
	}
	if !sawNudge {
		t.Fatal("a run that created a PR got no keep-going nudge")
	}
}
