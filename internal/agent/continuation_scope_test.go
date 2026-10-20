package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// The field report (session 0b6b1cff, 2026-10-20) is the case this file
// exists for. The user asked two plain questions:
//
//	"brief me this session"
//	"which code in xdev trigger this? this is too much over engineer and overwhelm"
//
// The first turn ran `git branch -a` and `git worktree list` while
// orienting itself. The keep-going gate read those as MUTATIONS, so the
// nudge fired after the brief was already delivered and the agent produced a
// SECOND brief. The second question was nudged the same way, and the model's
// own reasoning at that point was: "Now I should stop implementing. But the
// system prompt says take the next concrete step now with tools — conflicting."
//
// Two things had to be true for that to happen, and both are pinned below.
//
// A later question must not inherit an earlier task's mutation. The gate
// used to scan the WHOLE session, so a single mutating call anywhere armed
// the nudge for every later turn. It now scans the current TASK: work done
// inside the current ask still arms it (a run that edited a file and then
// yielded owes a summary), an earlier ask's edit does not.
func TestKeepGoingDoesNotCarryAnEarlierTasksMutationIntoALaterQuestion(t *testing.T) {
	toolCallScript := func(text, name, id string) fakeScript {
		return fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
			Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
			Content: []ai.Block{
				ai.TextBlock{Text: text},
				ai.ToolCallBlock{ID: id, Name: name, Arguments: json.RawMessage(`{"text":"x"}`)},
			},
		})}}
	}
	answerScript := func(text string) fakeScript {
		return fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
			Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
			Content: []ai.Block{ai.TextBlock{Text: text}},
		})}}
	}

	// Task 1 mutated: the assistant used a tool that is not declared
	// read-only, so callMutated says the workspace changed.
	task1 := []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "edit the file"}}},
		{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop, Content: []ai.Block{
			ai.TextBlock{Text: "editing"},
			ai.ToolCallBlock{ID: "e0", Name: "echo", Arguments: json.RawMessage(`{"text":"x"}`)},
		}},
		{Role: ai.RoleToolResult, ToolCallID: "e0", ToolName: "echo", Content: []ai.Block{ai.TextBlock{Text: "x"}}},
	}

	p := &fakeProvider{calls: []fakeScript{
		toolCallScript("reading", "read", "r1"), // task 2 only reads
		answerScript("here is the answer"),
	}}
	reg := tool.NewRegistry()
	reg.Register(readOnlyTool{})
	a := &Agent{Provider: p, Tools: reg, Hooks: TurnHooksFunc{}, PromptContinuation: true}

	// Task 2's user message follows task 1's history: the question is a NEW
	// ask, so the earlier edit must not arm the gate.
	history := append(append([]ai.Message{}, task1...),
		ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "which code triggers this?"}}})

	if _, err := a.Run(context.Background(), "sys", history); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.gotReqs) != 2 {
		t.Fatalf("stream requests = %d, want 2: a question is not an unfinished task", len(p.gotReqs))
	}
	for i, r := range p.gotReqs {
		last := r.Messages[len(r.Messages)-1]
		if last.Role == ai.RoleUser && last.Attribution == PromptContinuationAttribution {
			t.Fatalf("request %d got a keep-going nudge on a plain question", i)
		}
	}
}

// The counterpart: work done INSIDE the current task still arms the gate. A
// run that edited a file and then yielded with text owes a summary, and that
// is the behaviour the all-history scan was written for.
func TestKeepGoingStillFiresForWorkInsideTheCurrentTask(t *testing.T) {
	mutate := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
		Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{
			ai.TextBlock{Text: "working"},
			ai.ToolCallBlock{ID: "e0", Name: "echo", Arguments: json.RawMessage(`{"text":"x"}`)},
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

	p := &fakeProvider{calls: []fakeScript{mutate, yield, done}}
	reg := tool.NewRegistry()
	reg.Register(echoTool{})
	a := &Agent{Provider: p, Tools: reg, Hooks: TurnHooksFunc{}, PromptContinuation: true}

	history := []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "do it"}}}}
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
		t.Fatal("a run that mutated inside the current task got no keep-going nudge")
	}
}

// The resumed-session case the all-history scan was written for: the prior
// run's tool call sits after its own user message, so it is inside the task
// boundary and still counts.
func TestKeepGoingStillFiresForAResumedSession(t *testing.T) {
	yield := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
		Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.TextBlock{Text: "still going"}},
	})}}
	done := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
		Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.TextBlock{Text: "done"}},
	})}}

	p := &fakeProvider{calls: []fakeScript{yield, done}}
	reg := tool.NewRegistry()
	reg.Register(echoTool{})
	a := &Agent{Provider: p, Tools: reg, Hooks: TurnHooksFunc{}, PromptContinuation: true}

	prior := []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "do it"}}},
		{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop, Content: []ai.Block{
			ai.TextBlock{Text: "working"},
			ai.ToolCallBlock{ID: "e0", Name: "echo", Arguments: json.RawMessage(`{"v":"y"}`)},
		}},
		{Role: ai.RoleToolResult, ToolCallID: "e0", ToolName: "echo", Content: []ai.Block{ai.TextBlock{Text: "y"}}},
	}
	if _, err := a.Run(context.Background(), "sys", prior); err != nil {
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
		t.Fatal("a resumed session whose work is unfinished got no keep-going nudge")
	}
}

// taskFloor picks the LAST real user turn. A synthetic user-role message —
// the harness's own nudge, the goal continuation, the provider cut-off
// recovery — must not count as a task boundary, or the harness's own message
// would re-arm the gate it just fired.
func TestTaskFloorIgnoresSyntheticUserMessages(t *testing.T) {
	history := []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "first ask"}}},
		{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "ok"}}},
		{Role: ai.RoleUser, Attribution: "user", Content: []ai.Block{ai.TextBlock{Text: "second ask"}}},
		{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "ok"}}},
		{Role: ai.RoleUser, Attribution: PromptContinuationAttribution, Content: []ai.Block{ai.TextBlock{Text: PromptContinuationPrompt}}},
	}
	if got := taskFloor(history); got != 2 {
		t.Fatalf("taskFloor = %d, want 2 (the last REAL user turn)", got)
	}
	// A history with no real user turn falls back to the whole history.
	if got := taskFloor([]ai.Message{
		{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "ok"}}},
	}); got != 0 {
		t.Fatalf("taskFloor = %d, want 0 when there is no real user turn", got)
	}
}
