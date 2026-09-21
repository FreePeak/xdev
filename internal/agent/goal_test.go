package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// goalHooks records goal_updated events; *goalHooks implements GoalHook on top
// of the plain TurnHooksFunc surface.
type goalHooks struct {
	TurnHooksFunc
	events []Goal
}

func (h *goalHooks) OnGoalUpdated(g Goal) { h.events = append(h.events, g) }

func callGoal(t *testing.T, gt *GoalTool, args string) tool.Result {
	t.Helper()
	res, err := gt.Execute(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("goal %s: %v", args, err)
	}
	return res
}

func TestGoalToolOpRoundTrip(t *testing.T) {
	store := session.OpenMem("/proj", "goal round trip")
	t.Cleanup(func() { _ = store.Close() })
	gs := NewGoalState(store)
	gt := &GoalTool{Goals: gs}

	if res := callGoal(t, gt, `{"op":"get"}`); res.IsError || !strings.Contains(res.Text, "none") {
		t.Fatalf("empty get = %+v", res)
	}
	res := callGoal(t, gt, `{"op":"create","objective":"ship goal mode","token_budget":5000}`)
	if res.IsError || !strings.Contains(res.Text, "active") || !strings.Contains(res.Text, "ship goal mode") {
		t.Fatalf("create = %+v", res)
	}
	// A second goal is refused while one is active.
	if res := callGoal(t, gt, `{"op":"create","objective":"another"}`); !res.IsError {
		t.Fatalf("second create while active = %+v", res)
	}
	if res := callGoal(t, gt, `{"op":"create","objective":"x","token_budget":-5}`); !res.IsError || !strings.Contains(res.Text, "positive") {
		t.Fatalf("negative budget = %+v", res)
	}
	if res := callGoal(t, gt, `{"op":"get"}`); res.IsError || !strings.Contains(res.Text, "5000") || !strings.Contains(res.Text, "ship goal mode") {
		t.Fatalf("get = %+v", res)
	}
	if res := callGoal(t, gt, `{"op":"evidence","note":"go test ./internal/agent/... green"}`); res.IsError || !strings.Contains(res.Text, "1 note") {
		t.Fatalf("evidence = %+v", res)
	}
	if res := callGoal(t, gt, `{"op":"evidence"}`); !res.IsError {
		t.Fatalf("empty evidence = %+v", res)
	}
	if res := callGoal(t, gt, `{"op":"complete"}`); res.IsError || !strings.Contains(res.Text, "completed") {
		t.Fatalf("complete with recorded evidence = %+v", res)
	}
	if res := callGoal(t, gt, `{"op":"resume"}`); !res.IsError {
		t.Fatalf("resume of a completed goal = %+v", res)
	}
	if res := callGoal(t, gt, `{"op":"drop"}`); res.IsError || !strings.Contains(res.Text, "dropped") {
		t.Fatalf("drop = %+v", res)
	}
	res = callGoal(t, gt, `{"op":"resume","objective":"ship goal mode v2"}`)
	if res.IsError || !strings.Contains(res.Text, "active") || !strings.Contains(res.Text, "v2") {
		t.Fatalf("resume = %+v", res)
	}
	if res := callGoal(t, gt, `{"op":"frobnicate"}`); !res.IsError || !strings.Contains(res.Text, "unknown op") {
		t.Fatalf("unknown op = %+v", res)
	}
	// Every successful transition appended a goal_updated snapshot.
	var snapshots int
	for _, e := range store.Entries() {
		if _, ok := e.(*session.GoalUpdatedEntry); ok {
			snapshots++
		}
	}
	if snapshots < 5 {
		t.Fatalf("goal_updated entries = %d, want >= 5", snapshots)
	}
}

func TestGoalCompleteRequiresEvidence(t *testing.T) {
	gs := NewGoalState(nil)
	gt := &GoalTool{Goals: gs}
	if res := callGoal(t, gt, `{"op":"create","objective":"needs proof"}`); res.IsError {
		t.Fatalf("create = %+v", res)
	}
	res := callGoal(t, gt, `{"op":"complete"}`)
	if !res.IsError || !strings.Contains(res.Text, "evidence") {
		t.Fatalf("evidence-free complete must be rejected: %+v", res)
	}
	if v, ok := gs.View(); !ok || v.Status != GoalActive {
		t.Fatalf("status after rejected complete = %+v", v)
	}
	res = callGoal(t, gt, `{"op":"complete","evidence":["verified: go test -run TestGoalCompleteRequiresEvidence"]}`)
	if res.IsError || !strings.Contains(res.Text, "completed") {
		t.Fatalf("complete with evidence = %+v", res)
	}
	if v, _ := gs.View(); v.Status != GoalCompleted || len(v.Evidence) != 1 {
		t.Fatalf("view after complete = %+v", v)
	}
}

// TestGoalBudgetExhaustionStopsReminders drives three turns: the first two
// spend 60 tokens each against a 100-token budget, so the third turn's request
// must carry no reminder and the flip must be persisted + published.
func TestGoalBudgetExhaustionStopsReminders(t *testing.T) {
	store := session.OpenMem("/proj", "budget")
	t.Cleanup(func() { _ = store.Close() })
	gs := NewGoalState(store)
	if _, err := gs.Create("bounded objective", 100); err != nil {
		t.Fatal(err)
	}
	hooks := &goalHooks{}
	call := func(usage int64) fakeScript {
		return fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, &ai.Usage{TotalTokens: usage}, &ai.Message{
			Role: ai.RoleAssistant,
			Content: []ai.Block{
				ai.TextBlock{Text: "calling"},
				ai.ToolCallBlock{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{"text":"pong"}`)},
			},
			StopReason: ai.StopReasonStop,
		})}}
	}
	p := &fakeProvider{calls: []fakeScript{call(60), call(60), {events: []ai.Event{ai.Donef(ai.StopReasonStop, nil,
		&ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
			Content: []ai.Block{ai.TextBlock{Text: "done"}}})}}}}
	reg := tool.NewRegistry()
	reg.Register(echoTool{})
	a := &Agent{Provider: p, Tools: reg, Hooks: hooks, Goals: gs}
	if _, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	v, ok := gs.View()
	if !ok || v.Status != GoalBudgetExhausted || v.Spent != 120 {
		t.Fatalf("view = %+v (ok=%v)", v, ok)
	}
	if len(hooks.events) != 1 || hooks.events[0].Status != GoalBudgetExhausted {
		t.Fatalf("goal events = %+v", hooks.events)
	}
	if gs.Reminder() != "" {
		t.Fatal("reminder must stop once the budget is exhausted")
	}
	if s := p.gotReqs[0].System; !strings.Contains(s, "100 of 100 remaining") {
		t.Fatalf("turn-1 system = %q", s)
	}
	if s := p.gotReqs[1].System; !strings.Contains(s, "40 of 100 remaining") {
		t.Fatalf("turn-2 system = %q", s)
	}
	if s := p.gotReqs[2].System; strings.Contains(s, "goal reminder") {
		t.Fatalf("turn-3 system still reminds: %q", s)
	}
	var last *session.GoalUpdatedEntry
	for _, e := range store.Entries() {
		if gu, ok := e.(*session.GoalUpdatedEntry); ok {
			last = gu
		}
	}
	if last == nil || last.Goal.Status != GoalBudgetExhausted || last.Goal.Spent != 120 {
		t.Fatalf("persisted snapshot = %+v", last)
	}
}

func TestGoalReminderInjectedEachTurn(t *testing.T) {
	gs := NewGoalState(nil)
	if _, err := gs.Create("remind me every turn", 0); err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
			Role: ai.RoleAssistant,
			Content: []ai.Block{
				ai.TextBlock{Text: "calling"},
				ai.ToolCallBlock{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{"text":"pong"}`)},
			},
			StopReason: ai.StopReasonStop,
		})}},
		{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
			Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
			Content: []ai.Block{ai.TextBlock{Text: "done"}},
		})}},
	}}
	reg := tool.NewRegistry()
	reg.Register(echoTool{})
	a := &Agent{Provider: p, Tools: reg, Hooks: TurnHooksFunc{}, Goals: gs}
	if _, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.gotReqs) != 2 {
		t.Fatalf("stream calls = %d", len(p.gotReqs))
	}
	for i, req := range p.gotReqs {
		if !strings.Contains(req.System, "goal reminder — objective: remind me every turn") {
			t.Fatalf("turn %d system missing the reminder: %q", i+1, req.System)
		}
	}
}

// An active goal must keep the run going. Before the continuation a turn that
// yielded plain text ended the run, so a goal created interactively was never
// acted on: the reminder only ever rode along with a turn the user started.
func TestGoalContinuationKeepsTheRunGoing(t *testing.T) {
	store := session.OpenMem("/proj", "goal continuation")
	t.Cleanup(func() { _ = store.Close() })
	gs := NewGoalState(store)
	if _, err := gs.Create("keep working", 0); err != nil {
		t.Fatal(err)
	}
	yield := func(text string) fakeScript {
		return fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
			Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
			Content: []ai.Block{ai.TextBlock{Text: text}},
		})}}
	}
	// Turn 2 completes the goal (with the evidence completion demands), which
	// is what ends the run — not the yield in turn 3.
	complete := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
		Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{
			ai.TextBlock{Text: "wrapping up"},
			ai.ToolCallBlock{ID: "g1", Name: GoalToolName, Arguments: json.RawMessage(`{"op":"complete","evidence":["proved"]}`)},
		},
	})}}
	reg := tool.NewRegistry()
	reg.Register(&GoalTool{Goals: gs})
	p := &fakeProvider{calls: []fakeScript{yield("just a plan"), complete, yield("all done"), yield("second run")}}
	a := &Agent{Provider: p, Tools: reg, Hooks: TurnHooksFunc{}, Goals: gs, GoalContinuation: true, Store: store}
	if _, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "go"}}}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.gotReqs) != 3 {
		t.Fatalf("stream requests = %d, want 3 (yield, goal complete, final)", len(p.gotReqs))
	}
	last := p.gotReqs[1].Messages[len(p.gotReqs[1].Messages)-1]
	if last.Role != ai.RoleUser || !strings.Contains(last.Text(), "goal continuation") {
		t.Fatalf("the continuation never reached the model: %+v", last)
	}
	// Hidden but persisted: a store rebuild keeps the turn it produced, and
	// the transcript can tell it apart from something the user typed.
	var hidden int
	for _, e := range store.Entries() {
		if me, ok := e.(*session.MessageEntry); ok && me.Message.Attribution == GoalContinuationAttribution {
			hidden++
		}
	}
	if hidden != 1 {
		t.Fatalf("persisted continuations = %d, want 1", hidden)
	}

	// The goal is completed: a later yield ends the run instead of looping.
	before := len(p.gotReqs)
	if _, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(p.gotReqs) - before; got != 1 {
		t.Fatalf("a completed goal kept continuing: %d requests", got)
	}
}

// Modes that did not opt in (print/RPC/ACP) must not spend turns of their own:
// the same active goal ends the run at the yield.
func TestGoalContinuationOffByDefault(t *testing.T) {
	gs := NewGoalState(nil)
	if _, err := gs.Create("quiet goal", 0); err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{calls: []fakeScript{{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
		Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.TextBlock{Text: "done"}},
	})}}}}
	reg := tool.NewRegistry()
	reg.Register(echoTool{})
	reg.Register(&GoalTool{Goals: gs})
	a := &Agent{Provider: p, Tools: reg, Hooks: TurnHooksFunc{}, Goals: gs}
	if _, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.gotReqs) != 1 {
		t.Fatalf("stream requests = %d, want 1 (no continuation without opt-in)", len(p.gotReqs))
	}
}

// The goal tool is discovered from the registry when the agent was wired
// without an explicit state (Run's fallback path).
func TestGoalDiscoveredFromRegistry(t *testing.T) {
	store := session.OpenMem("/proj", "discovery")
	t.Cleanup(func() { _ = store.Close() })
	gs := NewGoalState(store)
	if _, err := gs.Create("registry objective", 0); err != nil {
		t.Fatal(err)
	}
	reg := tool.NewRegistry()
	reg.Register(echoTool{})
	reg.Register(&GoalTool{Goals: gs})
	p := &fakeProvider{calls: []fakeScript{{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
		Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.TextBlock{Text: "done"}},
	})}}}}
	a := &Agent{Provider: p, Tools: reg, Hooks: TurnHooksFunc{}}
	if _, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if s := p.gotReqs[0].System; !strings.Contains(s, "registry objective") {
		t.Fatalf("system = %q", s)
	}
}

func TestGoalPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goal.jsonl")
	store := session.OpenMem("/proj", "persist")
	if _, err := store.EnsureOnDisk(path, session.Options{}); err != nil {
		t.Fatal(err)
	}
	gs := NewGoalState(store)
	if _, err := gs.Create("persistent objective", 2000); err != nil {
		t.Fatal(err)
	}
	if _, err := gs.AddEvidence("go test ./internal/agent/... green"); err != nil {
		t.Fatal(err)
	}
	gs.AddUsage(500)
	if _, err := gs.Complete(nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := session.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	gs2 := NewGoalState(reopened)
	v, ok := gs2.View()
	if !ok {
		t.Fatal("goal must survive a reopen")
	}
	if v.Objective != "persistent objective" || v.Status != GoalCompleted || v.TokenBudget != 2000 || v.Spent != 500 || len(v.Evidence) != 1 {
		t.Fatalf("reopened goal = %+v", v)
	}
	if !strings.Contains(gs2.Describe(), "persistent objective") {
		t.Fatalf("Describe = %q", gs2.Describe())
	}
	// A session switch rebinds: the new session starts with its own goal.
	gs2.Bind(session.OpenMem("/proj", "other"))
	if _, ok := gs2.View(); ok {
		t.Fatal("a session switch must not leak the previous session's goal")
	}
}

func TestPromptContinuationKeepsTheRunGoing(t *testing.T) {
	yield := func(text string) fakeScript {
		return fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
			Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
			Content: []ai.Block{ai.TextBlock{Text: text}},
		})}}
	}
	withTools := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
		Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{
			ai.TextBlock{Text: "working"},
			ai.ToolCallBlock{ID: "e1", Name: "echo", Arguments: json.RawMessage(`{"v":"x"}`)},
		},
	})}}
	p := &fakeProvider{calls: []fakeScript{withTools, yield("status only"), yield("still going"), yield("done")}}
	reg := tool.NewRegistry()
	reg.Register(echoTool{})
	a := &Agent{Provider: p, Tools: reg, Hooks: TurnHooksFunc{}, PromptContinuation: true}
	if _, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "do it"}}}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.gotReqs) != 3 {
		t.Fatalf("stream requests = %d, want 3 (tools, first yield, nudged second yield)", len(p.gotReqs))
	}
	last := p.gotReqs[2].Messages[len(p.gotReqs[2].Messages)-1]
	if last.Role != ai.RoleUser || last.Attribution != PromptContinuationAttribution {
		t.Fatalf("prompt continuation never reached the model: %+v", last)
	}
}

func TestPromptContinuationOffWithoutTools(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
		Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.TextBlock{Text: "hello"}},
	})}}}}
	reg := tool.NewRegistry()
	reg.Register(echoTool{})
	a := &Agent{Provider: p, Tools: reg, Hooks: TurnHooksFunc{}, PromptContinuation: true}
	if _, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.gotReqs) != 1 {
		t.Fatalf("plain greeting continued: %d requests", len(p.gotReqs))
	}
}
