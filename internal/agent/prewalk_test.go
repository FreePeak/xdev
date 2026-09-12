package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// E2E: a prewalk agent runs on a scripted provider. Turn 1: model calls
// write. Turn 2: the StreamRequest.Model must be the prewalk target.
// The registry has no todo tool, so this is the documented ungated
// fallback (TestPrewalkGateFallbacks covers that decision directly).
func TestPrewalkSwitchesAfterFirstEdit(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: toolCallEvents("write", `{"path":"a.txt","content":"x"}`)},
	}}
	reg := tool.NewRegistry()
	reg.Register(tool.NewWriteTool())
	tgt := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{
			{Type: ai.EventStart, Provider: "fake", API: "fake-api", Model: "smol-model"},
			{Type: ai.EventTextDelta, Delta: "all done"},
			ai.Donef(ai.StopReasonStop, nil, &ai.Message{Role: ai.RoleAssistant,
				Content: []ai.Block{ai.TextBlock{Text: "all done"}}, StopReason: ai.StopReasonStop}),
		}},
	}}
	ag := &Agent{
		Provider: p, Tools: reg, Model: "big-model", MaxTurns: 3,
		Prewalk: &Prewalk{Target: FailoverTarget{Provider: tgt, Model: "smol-model"}},
	}
	_, err := ag.Run(context.Background(), "sys", []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "write the file"}}},
	})
	if err != nil && !strings.Contains(err.Error(), "script exhausted") {
		t.Fatal(err)
	}
	if !ag.prewalk.done {
		t.Fatal("prewalk did not switch after the write")
	}
	if ag.Model != "smol-model" {
		t.Fatalf("Model = %q, want smol-model", ag.Model)
	}
	// The first request goes to the primary on big-model; after the switch
	// the second goes to the target provider on smol-model.
	if len(p.gotReqs) != 1 {
		t.Fatalf("primary saw %d requests, want 1", len(p.gotReqs))
	}
	if p.gotReqs[0].Model != "big-model" {
		t.Fatalf("primary model = %q", p.gotReqs[0].Model)
	}
	if len(tgt.gotReqs) != 1 {
		t.Fatalf("target saw %d requests, want 1", len(tgt.gotReqs))
	}
	if tgt.gotReqs[0].Model != "smol-model" {
		t.Fatalf("target model = %q, want smol-model", tgt.gotReqs[0].Model)
	}
}

// Without a successful edit/write, prewalk must never fire — a read-only
// or failed-write run stays on the primary model.
func TestPrewalkStaysUnarmedWithoutMutation(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: toolCallEvents("read", `{}`)},
		{events: []ai.Event{
			{Type: ai.EventStart},
			ai.Donef(ai.StopReasonStop, nil, &ai.Message{Role: ai.RoleAssistant,
				Content: []ai.Block{ai.TextBlock{Text: "all read"}}, StopReason: ai.StopReasonStop}),
		}},
	}}
	reg := tool.NewRegistry()
	reg.Register(tool.NewReadTool())
	tgt := &fakeProvider{}
	ag := &Agent{
		Provider: p, Tools: reg, Model: "big-model", MaxTurns: 3,
		Prewalk: &Prewalk{Target: FailoverTarget{Provider: tgt, Model: "smol-model"}},
	}
	if _, err := ag.Run(context.Background(), "sys", []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "read a file"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if ag.prewalk.done || len(tgt.gotReqs) != 0 {
		t.Fatalf("prewalk fired without a mutation (done=%v target reqs=%d)", ag.prewalk.done, len(tgt.gotReqs))
	}
}

// --- the plan gate (issue #41): a todo plan must exist before the first
// successful edit/write hands the run off --------------------------------

// toolCallSpec is one tool call inside a scripted assistant turn.
type toolCallSpec struct{ name, args string }

// toolCallsEvents scripts one assistant turn that calls several tools at
// once; the whole results batch reaches prewalkNote together, which is how
// plan-and-write-in-one-turn is exercised.
func toolCallsEvents(specs ...toolCallSpec) []ai.Event {
	events := []ai.Event{{Type: ai.EventStart}}
	blocks := make([]ai.Block, 0, len(specs))
	for i, s := range specs {
		id := fmt.Sprintf("c%d", i+1)
		events = append(events,
			ai.Event{Type: ai.EventToolcallStart, ToolCallID: id, ToolName: s.name, StreamIndex: i},
			ai.Event{Type: ai.EventToolcallDelta, StreamIndex: i, PartialJSON: s.args},
			ai.Event{Type: ai.EventToolcallEnd, StreamIndex: i, PartialJSON: s.args},
		)
		blocks = append(blocks, ai.ToolCallBlock{ID: id, Name: s.name,
			Arguments: json.RawMessage(s.args), StreamIndex: i})
	}
	return append(events, ai.Donef(ai.StopReasonStop, nil, &ai.Message{
		Role: ai.RoleAssistant, Content: blocks, StopReason: ai.StopReasonStop,
	}))
}

// foreignTodo shadows the todo tool name with a non-*tool.TodoTool so the
// gate's type-assert fallback is reachable.
type foreignTodo struct{}

func (foreignTodo) Name() string        { return "todo" }
func (foreignTodo) Description() string { return "not the todo tool" }
func (foreignTodo) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (foreignTodo) Execute(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{}, nil
}

// Todo call payloads. emptyTodoArgs takes the items-only init path, which
// is the one that can build a phase with no tasks.
const (
	planTodoArgs  = `{"op":"init","list":[{"phase":"Foundation","items":["wire the gate","prove it with a test"]}]}`
	emptyTodoArgs = `{"op":"init","phase":"Foundation","items":["  "]}`
	clearTodoArgs = `{"op":"rm"}`
	doneTodoArgs  = `{"op":"done"}`
)

// prewalkFixture builds the gate scenario: write + todo tools in the
// registry, a scripted primary provider, and an empty handoff target.
func prewalkFixture(primary *fakeProvider) (*Agent, *tool.TodoTool, *fakeProvider) {
	reg := tool.NewRegistry()
	reg.Register(tool.NewWriteTool())
	tt := tool.NewTodoTool()
	reg.Register(tt)
	tgt := &fakeProvider{}
	ag := &Agent{
		Provider: primary, Tools: reg, Model: "big-model", MaxTurns: 8,
		Prewalk: &Prewalk{Target: FailoverTarget{Provider: tgt, Model: "smol-model"}},
	}
	return ag, tt, tgt
}

// prewalkRun drives one scripted session to completion.
func prewalkRun(t *testing.T, ag *Agent) {
	t.Helper()
	if _, err := ag.Run(context.Background(), "sys", []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "do the work"}}},
	}); err != nil {
		t.Fatal(err)
	}
}

// writeArg is the write payload for a fresh temp file.
func writeArg(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf(`{"path":%q,"content":"x"}`, filepath.Join(t.TempDir(), "out.txt"))
}

// A plan todo list in the same batch as the write arms the gate — the
// snapshot is read after the batch, so plan+execute in one turn hands off.
func TestPrewalkFiresWithPlanInSameBatch(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: toolCallsEvents(
			toolCallSpec{"todo", planTodoArgs},
			toolCallSpec{"write", writeArg(t)},
		)},
	}}
	ag, _, tgt := prewalkFixture(p)
	tgt.calls = []fakeScript{{events: doneEvents("finished on the small model")}}
	prewalkRun(t, ag)
	if !ag.prewalk.done || ag.Model != "smol-model" {
		t.Fatalf("prewalk did not fire with a plan (done=%v model=%q)", ag.prewalk.done, ag.Model)
	}
	if len(tgt.gotReqs) != 1 || tgt.gotReqs[0].Model != "smol-model" {
		t.Fatalf("target requests = %d, want 1 on smol-model", len(tgt.gotReqs))
	}
}

// No plan list → no prewalk, no switch: the run finishes on the original
// model and the target provider is never asked.
func TestPrewalkHoldsWithoutPlan(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: toolCallEvents("write", writeArg(t))},
		{events: doneEvents("finished on the big model")},
	}}
	ag, _, tgt := prewalkFixture(p)
	prewalkRun(t, ag)
	if ag.prewalk.done || ag.Model != "big-model" {
		t.Fatalf("prewalk fired without a plan (done=%v model=%q)", ag.prewalk.done, ag.Model)
	}
	if len(tgt.gotReqs) != 0 {
		t.Fatalf("target saw %d requests, want 0", len(tgt.gotReqs))
	}
	if len(p.gotReqs) != 2 {
		t.Fatalf("primary saw %d requests, want 2", len(p.gotReqs))
	}
}

// A plan created mid-run arms a later edit: the write before the plan does
// not fire, the write after it does.
func TestPrewalkFiresWhenPlanCreatedMidRun(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: toolCallEvents("write", writeArg(t))},
		{events: toolCallEvents("todo", planTodoArgs)},
		{events: toolCallEvents("write", writeArg(t))},
	}}
	ag, tt, tgt := prewalkFixture(p)
	tgt.calls = []fakeScript{{events: doneEvents("finished on the small model")}}
	prewalkRun(t, ag)
	if !ag.prewalk.done || ag.Model != "smol-model" {
		t.Fatalf("prewalk did not fire after the mid-run plan (done=%v model=%q)", ag.prewalk.done, ag.Model)
	}
	if !ag.prewalk.held {
		t.Fatal("the pre-plan write should have logged a hold")
	}
	if n := len(tt.Snapshot()); n != 1 || len(tt.Snapshot()[0].Tasks) != 2 {
		t.Fatalf("plan snapshot = %d phase(s), want 2 tasks in 1 phase", n)
	}
}

// An empty todo list is not a plan: init with blank items leaves a phase
// with no tasks, so the edit does not hand off.
func TestPrewalkHoldsOnEmptyTodoList(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: toolCallEvents("todo", emptyTodoArgs)},
		{events: toolCallEvents("write", writeArg(t))},
		{events: doneEvents("finished on the big model")},
	}}
	ag, tt, tgt := prewalkFixture(p)
	prewalkRun(t, ag)
	snap := tt.Snapshot()
	if len(snap) != 1 || len(snap[0].Tasks) != 0 {
		t.Fatalf("fixture did not build an empty list: %+v", snap)
	}
	if ag.prewalk.done || ag.Model != "big-model" || len(tgt.gotReqs) != 0 {
		t.Fatalf("prewalk fired on an empty list (done=%v model=%q target reqs=%d)",
			ag.prewalk.done, ag.Model, len(tgt.gotReqs))
	}
}

// A cleared todo list is not a plan either: `rm` keeps the phases but drops
// every task, so the following edit stays on the primary model.
func TestPrewalkHoldsOnClearedTodoList(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: toolCallEvents("todo", planTodoArgs)},
		{events: toolCallEvents("todo", clearTodoArgs)},
		{events: toolCallEvents("write", writeArg(t))},
		{events: doneEvents("finished on the big model")},
	}}
	ag, tt, tgt := prewalkFixture(p)
	prewalkRun(t, ag)
	snap := tt.Snapshot()
	if len(snap) != 1 || len(snap[0].Tasks) != 0 {
		t.Fatalf("fixture did not clear the list: %+v", snap)
	}
	if ag.prewalk.done || ag.Model != "big-model" || len(tgt.gotReqs) != 0 {
		t.Fatalf("prewalk fired on a cleared list (done=%v model=%q target reqs=%d)",
			ag.prewalk.done, ag.Model, len(tgt.gotReqs))
	}
}

// A finished plan still counts as a plan: the gate asks whether a plan list
// exists, not whether work is outstanding.
func TestPrewalkFiresWithCompletedPlan(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: toolCallEvents("todo", planTodoArgs)},
		{events: toolCallEvents("todo", doneTodoArgs)},
		{events: toolCallEvents("write", writeArg(t))},
	}}
	ag, tt, tgt := prewalkFixture(p)
	tgt.calls = []fakeScript{{events: doneEvents("finished on the small model")}}
	prewalkRun(t, ag)
	if tasks := tt.Snapshot()[0].Tasks; len(tasks) != 2 || tasks[0].Status != tool.TodoCompleted {
		t.Fatalf("fixture did not complete the plan: %+v", tasks)
	}
	if !ag.prewalk.done || ag.Model != "smol-model" {
		t.Fatalf("prewalk did not fire on a completed plan (done=%v model=%q)", ag.prewalk.done, ag.Model)
	}
}

// The gate reports gated=false — keeping the pre-todo open behavior — for a
// foreign tool under the todo name, and for an empty or absent todo tool it
// reports gated=true with planned=false.
func TestPrewalkGateFallbacks(t *testing.T) {
	noReg := &Agent{}
	if planned, gated := noReg.prewalkGate(); planned || gated {
		t.Fatalf("nil registry: planned=%v gated=%v, want false/false", planned, gated)
	}
	foreign := tool.NewRegistry()
	foreign.Register(foreignTodo{})
	ag := &Agent{Tools: foreign}
	if planned, gated := ag.prewalkGate(); planned || gated {
		t.Fatalf("foreign todo tool: planned=%v gated=%v, want false/false", planned, gated)
	}
	empty := tool.NewRegistry()
	empty.Register(tool.NewTodoTool())
	ag.Tools = empty
	if planned, gated := ag.prewalkGate(); planned || !gated {
		t.Fatalf("empty todo tool: planned=%v gated=%v, want false/true", planned, gated)
	}
}
