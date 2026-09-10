package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// taskTool builds a TaskTool over a scripted provider.
func taskTool(p *fakeProvider) *TaskTool {
	return &TaskTool{Provider: p, Model: "m", ChildTools: []tool.Tool{echoTool{}}}
}

func TestTaskToolRendersYieldForParent(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: yieldEvents(`{"result":"found 3 callers in a.go, b.go, c.go"}`)},
	}}
	tt := taskTool(p)
	res, err := tt.Execute(context.Background(), json.RawMessage(`{"prompt":"count the callers","name":"scout"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res)
	}
	if !strings.Contains(res.Text, "found 3 callers") {
		t.Fatalf("handoff text = %q", res.Text)
	}
	// The transcript surface stays out: no message bodies beyond the yield.
	if strings.Contains(res.Text, "calling") {
		t.Fatalf("child transcript leaked: %q", res.Text)
	}
	details, ok := res.Details.(*SubagentResult)
	if !ok || details.Status != "yielded" {
		t.Fatalf("details = %#v", res.Details)
	}
}

func TestTaskToolRequiresPrompt(t *testing.T) {
	res, err := taskTool(&fakeProvider{}).Execute(context.Background(), json.RawMessage(`{"prompt":"  "}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "prompt is required") {
		t.Fatalf("res = %+v", res)
	}
}

func TestTaskToolMalformedArgs(t *testing.T) {
	res, err := taskTool(&fakeProvider{}).Execute(context.Background(), json.RawMessage(`{oops`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "malformed arguments") {
		t.Fatalf("res = %+v", res)
	}
}

// TestTaskToolChildCannotSpawnGrandchildren pins the structural depth
// guard: the child registry is built from ChildTools + yield only, so a
// nested task tool can never exist inside a child.
func TestTaskToolChildCannotSpawnGrandchildren(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: yieldEvents(`{"result":"ok"}`)},
	}}
	tt := taskTool(p)
	reg := tool.NewRegistry()
	reg.Register(&yieldTool{})
	for _, c := range tt.ChildTools {
		reg.Register(c)
	}
	if _, has := reg.Get(TaskToolName); has {
		t.Fatal("child registry exposes the task tool (recursion possible)")
	}
	if _, has := reg.Get("yield"); !has {
		t.Fatal("child registry must include yield")
	}
}

// TestTaskToolEndToEndIsolation drives a PARENT agent that calls the task
// tool, asserting the exit criterion: the parent receives the yield
// payload, and the child's transcript never enters the parent history.
func TestTaskToolEndToEndIsolation(t *testing.T) {
	secret := "CHILD-ONLY-DETAIL"
	// Child: says the secret, then yields a clean payload.
	child := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{
			{Type: ai.EventStart},
			{Type: ai.EventTextStart}, textEvent(secret),
			{Type: ai.EventToolcallStart, ToolCallID: "y1", ToolName: "yield", StreamIndex: 0},
			{Type: ai.EventToolcallDelta, StreamIndex: 0, PartialJSON: `{"result":"3 callers found"}`},
			{Type: ai.EventToolcallEnd, StreamIndex: 0, PartialJSON: `{"result":"3 callers found"}`},
			ai.Donef(ai.StopReasonStop, nil, &ai.Message{
				Role: ai.RoleAssistant,
				Content: []ai.Block{
					ai.TextBlock{Text: secret},
					ai.ToolCallBlock{ID: "y1", Name: "yield", Arguments: json.RawMessage(`{"result":"3 callers found"}`), StreamIndex: 0},
				},
				StopReason: ai.StopReasonStop,
			}),
		}},
	}}
	// Parent: calls task, then answers.
	parent := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{
			{Type: ai.EventStart},
			{Type: ai.EventToolcallStart, ToolCallID: "t1", ToolName: TaskToolName, StreamIndex: 0},
			{Type: ai.EventToolcallDelta, StreamIndex: 0, PartialJSON: `{"prompt":"count callers"}`},
			{Type: ai.EventToolcallEnd, StreamIndex: 0, PartialJSON: `{"prompt":"count callers"}`},
			ai.Donef(ai.StopReasonStop, nil, &ai.Message{
				Role: ai.RoleAssistant,
				Content: []ai.Block{
					ai.ToolCallBlock{ID: "t1", Name: TaskToolName, Arguments: json.RawMessage(`{"prompt":"count callers"}`), StreamIndex: 0},
				},
				StopReason: ai.StopReasonStop,
			}),
		}},
		{events: []ai.Event{ai.Event{Type: ai.EventStart}, textEvent("done: 3 callers"), doneEvent("done: 3 callers")}},
	}}

	// One provider per side: route by model name.
	router := &routingProvider{parent: parent, child: child, childModel: "child"}
	reg := tool.NewRegistry()
	reg.Register(&TaskTool{Provider: router, Model: "child", ChildTools: []tool.Tool{echoTool{}}})
	a := &Agent{Provider: router, Tools: reg, Model: "parent", Retry: fastRetry(), Hooks: TurnHooksFunc{}}

	final, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "delegate it"}}}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Text() != "done: 3 callers" {
		t.Fatalf("final = %q", final.Text())
	}
	// The parent's provider only ever saw the yield-derived handoff.
	for _, req := range parent.gotReqs {
		for _, m := range req.Messages {
			if strings.Contains(m.Text(), secret) {
				t.Fatalf("child transcript leaked into parent context: %q", m.Text())
			}
		}
	}
	// And the handoff text did arrive.
	joined := ""
	for _, req := range parent.gotReqs {
		for _, m := range req.Messages {
			joined += m.Text() + "\n"
		}
	}
	if !strings.Contains(joined, "3 callers found") {
		t.Fatalf("parent never saw the yield payload:\n%s", joined)
	}
}

// routingProvider sends parent-model streams to parent and everything else
// to child (a two-agent fake).
type routingProvider struct {
	parent     *fakeProvider
	child      *fakeProvider
	childModel string
}

func (r *routingProvider) Stream(ctx context.Context, req ai.StreamRequest) (<-chan ai.Event, error) {
	if req.Model == r.childModel {
		return r.child.Stream(ctx, req)
	}
	return r.parent.Stream(ctx, req)
}

func (r *routingProvider) Name() string { return "router" }
func (r *routingProvider) API() string  { return "router" }
