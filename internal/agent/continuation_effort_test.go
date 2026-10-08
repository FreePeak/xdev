package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// TestReadOnlyBashDoesNotEarnAKeepGoingNudge is the end-to-end half of the
// overthinking fix, and it is the one that would have caught the bug in the
// shape users hit it: an interactive run whose ONLY tool use is a shell read.
//
// The unit table (TestReadOnlyCommandClassifiesShellLines) proves the
// classifier; TestCallMutatedReadsTheCapsManifest proves the gate consults it.
// Neither proves the LOOP changes behaviour, because both call callMutated
// directly. This drives a real Run with the REAL bash tool (not a spy) so the
// verdict has to survive the tool call, the argument plumbing and the gate:
//
//   - with the old "bash is always mutating", the second yield below collected
//     a prompt-continuation and the run made three requests;
//   - with bash judged by its arguments, the run stops after the report, which
//     is what "match the effort to the ask" means in practice.
//
// Measured context for why this is worth a test: 649 prompt-continuations
// against 180 real user turns in the 80 newest session files (3.6 per turn),
// and `git status` is the most common bash call in a session.
func TestReadOnlyBashDoesNotEarnAKeepGoingNudge(t *testing.T) {
	dir := t.TempDir()
	// The model reads the repo, then reports. Nothing was changed.
	readThenYield := fakeScript{events: toolCallEvents("bash", `{"command":"git status --short"}`)}
	report := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
		Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.TextBlock{Text: "nothing is modified — that is the report"}},
	})}}
	p := &fakeProvider{calls: []fakeScript{readThenYield, report, report}}

	reg := tool.NewRegistry()
	reg.Register(tool.NewBashTool(dir))
	a := &Agent{Provider: p, Tools: reg, Hooks: TurnHooksFunc{}, PromptContinuation: true}
	if _, err := a.Run(context.Background(), "sys",
		[]ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "is anything modified?"}}}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.gotReqs) != 2 {
		t.Fatalf("stream requests = %d, want 2 (the read, then the report) — a shell read must not buy a keep-going nudge", len(p.gotReqs))
	}
	for i, r := range p.gotReqs {
		last := r.Messages[len(r.Messages)-1]
		if last.Role == ai.RoleUser && last.Attribution == PromptContinuationAttribution {
			t.Fatalf("request %d got a keep-going nudge after a run that only read", i)
		}
	}
}

// TestMutatingBashStillEarnsAKeepGoingNudge is the guard in the other
// direction, and the one that keeps the fix from silently disabling keep-going
// for real work. Same shape, one different command: the run stages a file, so
// the model has changed something and the gate must keep pushing until it
// yields twice.
func TestMutatingBashStillEarnsAKeepGoingNudge(t *testing.T) {
	dir := t.TempDir()
	stage := fakeScript{events: toolCallEvents("bash", `{"command":"git init -q . && git add -A"}`)}
	yield := func(text string) fakeScript {
		return fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
			Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
			Content: []ai.Block{ai.TextBlock{Text: text}},
		})}}
	}
	p := &fakeProvider{calls: []fakeScript{stage, yield("staged"), yield("done"), yield("extra")}}

	reg := tool.NewRegistry()
	reg.Register(tool.NewBashTool(dir))
	a := &Agent{Provider: p, Tools: reg, Hooks: TurnHooksFunc{}, PromptContinuation: true}
	if _, err := a.Run(context.Background(), "sys",
		[]ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "stage everything"}}}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.gotReqs) != 3 {
		t.Fatalf("stream requests = %d, want 3 (stage, yield, nudged yield) — a mutating bash command must still keep the run going", len(p.gotReqs))
	}
	last := p.gotReqs[2].Messages[len(p.gotReqs[2].Messages)-1]
	if last.Role != ai.RoleUser || last.Attribution != PromptContinuationAttribution {
		t.Fatalf("a run that staged files got no keep-going nudge: %+v", last)
	}
	_ = json.Marshal
}

// TestOpenTodosDoNotEarnAKeepGoingNudge is the 724e3fbb regression: a research
// run that opens a todo list, reads the codebase, and yields the report must
// STOP — even when the "write the report" todo is still pending. openTodos used
// to OR into the keep-going gate, so the nudge's "do not stop at a status
// report" text then pushed the model into a worktree and an implementation it
// was never asked for. Soft nagging still rides todoReminder(); this gate only
// asks whether the workspace already changed.
func TestOpenTodosDoNotEarnAKeepGoingNudge(t *testing.T) {
	initTodos := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
		Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{
			ai.TextBlock{Text: "planning the review"},
			ai.ToolCallBlock{ID: "t1", Name: "todo", Arguments: json.RawMessage(
				`{"op":"init","list":[{"phase":"report","items":["Write full issue report"]}]}`,
			)},
		},
	})}}
	readThenYield := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
		Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{
			ai.TextBlock{Text: "reading"},
			ai.ToolCallBlock{ID: "r1", Name: "read", Arguments: json.RawMessage(`{"path":"README.md"}`)},
		},
	})}}
	report := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
		Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.TextBlock{Text: "# live issue report\n\n**No code changes** — report only."}},
	})}}
	p := &fakeProvider{calls: []fakeScript{initTodos, readThenYield, report, report}}

	reg := tool.NewRegistry()
	reg.Register(tool.NewTodoTool())
	reg.Register(readOnlyTool{})
	a := &Agent{Provider: p, Tools: reg, Hooks: TurnHooksFunc{}, PromptContinuation: true}
	if _, err := a.Run(context.Background(), "sys",
		[]ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "write me the fully report list all the current issues"}}}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.gotReqs) != 3 {
		t.Fatalf("stream requests = %d, want 3 (todo, read, report) — open todos must not buy a keep-going nudge after a report", len(p.gotReqs))
	}
	for i, r := range p.gotReqs {
		last := r.Messages[len(r.Messages)-1]
		if last.Role == ai.RoleUser && last.Attribution == PromptContinuationAttribution {
			t.Fatalf("request %d got a keep-going nudge after a report-only run with open todos", i)
		}
	}
}

// TestPromptContinuationPromptLetsAReportStop pins the nudge copy: the old
// text said "Do not stop at a plan or a status report", which is exactly the
// wrong instruction after a research yield. The new text must name the report
// as a valid stop and must not tell the model to keep going past one.
func TestPromptContinuationPromptLetsAReportStop(t *testing.T) {
	if strings.Contains(PromptContinuationPrompt, "Do not stop at a plan or a status report") {
		t.Fatal("nudge still forbids stopping at a status report — that is the 724e3fbb instruction that started the implement pass")
	}
	if !strings.Contains(PromptContinuationPrompt, "delivered the answer or report") {
		t.Fatal("nudge must tell the model a delivered report is a valid stop")
	}
	if !strings.Contains(PromptContinuationPrompt, "Do not start implementing after a research or report request") {
		t.Fatal("nudge must forbid starting an implement pass after a report")
	}
}
