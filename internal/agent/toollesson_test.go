package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// The tool-usage learning sees every tool outcome, so the agent's own
// recovery from a failure can become a recipe.

type lessonSpy struct {
	names []string
	errs  []bool
}

func (s *lessonSpy) ObserveTool(call ai.ToolCallBlock, res tool.Result) {
	s.names = append(s.names, call.Name)
	s.errs = append(s.errs, res.IsError)
}

// flakyTool fails on one value and works on another.
type flakyTool struct{}

func (flakyTool) Name() string        { return "flaky" }
func (flakyTool) Description() string { return "fails on a bad value" }
func (flakyTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`)
}

func (flakyTool) Execute(_ context.Context, args json.RawMessage) (tool.Result, error) {
	var a struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return tool.Result{}, err
	}
	if a.Text == "bad" {
		return tool.Result{Text: "flaky: bad value", IsError: true}, nil
	}
	return tool.Result{Text: "flaky: ok"}, nil
}

// flakyCallEvents scripts one assistant turn that calls the flaky tool.
func flakyCallEvents(id, text string) []ai.Event {
	args := `{"text":"` + text + `"}`
	return []ai.Event{
		{Type: ai.EventStart},
		{Type: ai.EventToolcallStart, ToolCallID: id, ToolName: "flaky", StreamIndex: 0},
		{Type: ai.EventToolcallDelta, StreamIndex: 0, PartialJSON: args},
		{Type: ai.EventToolcallEnd, StreamIndex: 0, PartialJSON: args},
		ai.Donef(ai.StopReasonStop, nil, &ai.Message{
			Role: ai.RoleAssistant,
			Content: []ai.Block{
				ai.ToolCallBlock{ID: id, Name: "flaky", Arguments: json.RawMessage(args), StreamIndex: 0},
			},
			StopReason: ai.StopReasonStop,
		}),
	}
}

// TestAgentReportsToolOutcomesToTheLessonObserver pins the seam: the loop
// reports the failure and the recovery, in order, so the backend can turn
// the pair into a recipe.
func TestAgentReportsToolOutcomesToTheLessonObserver(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: flakyCallEvents("c1", "bad")},
		{events: flakyCallEvents("c2", "good")},
		{events: doneEvents("done")},
	}}
	reg := tool.NewRegistry()
	reg.Register(flakyTool{})
	spy := &lessonSpy{}
	a := &Agent{Provider: p, Tools: reg, Hooks: TurnHooksFunc{}, Model: "m", Retry: fastRetry(), ToolLessons: spy}

	if _, err := a.Run(context.Background(), "sys", nil); err != nil {
		t.Fatal(err)
	}
	if len(spy.names) != 2 {
		t.Fatalf("want two reported outcomes, got %d: %+v", len(spy.names), spy.names)
	}
	if spy.names[0] != "flaky" || spy.names[1] != "flaky" {
		t.Fatalf("the observer must see the tool: %+v", spy.names)
	}
	if !spy.errs[0] {
		t.Fatalf("the first call failed and the observer must see it: %+v", spy.errs)
	}
	if spy.errs[1] {
		t.Fatalf("the second call worked and the observer must see it: %+v", spy.errs)
	}
}

// TestAgentReportsGarbledToolName pins the other failure the learner gets
// to see: a call to a tool that does not exist never reaches a tool, so
// the loop reports it from the miss branch. It is the mistake the
// unknown-tool message names the answer to.
func TestAgentReportsGarbledToolName(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: toolCallEvents("ash_edit", `{"file":"/tmp/a.go"}`)},
		{events: doneEvents("done")},
	}}
	reg := tool.NewRegistry()
	reg.Register(flakyTool{})
	spy := &lessonSpy{}
	a := &Agent{Provider: p, Tools: reg, Hooks: TurnHooksFunc{}, Model: "m", Retry: fastRetry(), ToolLessons: spy}

	if _, err := a.Run(context.Background(), "sys", nil); err != nil {
		t.Fatal(err)
	}
	if len(spy.names) != 1 || spy.names[0] != "ash_edit" {
		t.Fatalf("the garbled call must be reported: %+v", spy.names)
	}
	if !spy.errs[0] {
		t.Fatalf("the garbled call must be reported as a failure: %+v", spy.errs)
	}
}

// TestSubagentInheritsTheToolLessonLearner pins the child's half: a child
// does most of the session's tool work, and a mistake only a child made is
// a mistake the next session makes again.
func TestSubagentInheritsTheToolLessonLearner(t *testing.T) {
	spy := &lessonSpy{}
	tt := &TaskTool{
		Provider: &fakeProvider{calls: []fakeScript{
			{events: flakyCallEvents("c1", "bad")},
			{events: yieldEvents(`{"result":"child step 1"}`)},
		}},
		Model:       "m",
		ChildTools:  []tool.Tool{flakyTool{}},
		MaxTurns:    3,
		ToolLessons: spy,
	}
	if _, err := tt.Execute(context.Background(), json.RawMessage(`{"prompt":"do the thing"}`)); err != nil {
		t.Fatalf("task execute: %v", err)
	}
	if len(spy.names) == 0 {
		t.Fatal("the child must report its tool outcomes to the learner")
	}
	if spy.names[0] != "flaky" || !spy.errs[0] {
		t.Fatalf("the child's failed call must be reported: %+v %+v", spy.names, spy.errs)
	}
}
