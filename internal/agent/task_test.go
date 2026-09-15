package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
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

// A child that ends without calling yield used to look identical to a real
// handoff (status "completed", no signal). It is now nudged, and the parent
// is told when the nudges run out (parity finding T3 #6).
func TestTaskToolNoYieldChildWarnsParent(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: doneEvents("I did the thing, here are my thoughts")}, // no yield
		{events: doneEvents("still no yield")},                        // nudge 1
		{events: doneEvents("final prose")},                           // nudge 2
	}}
	res, err := taskTool(p).Execute(context.Background(), json.RawMessage(`{"prompt":"do it"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "never called yield") {
		t.Fatalf("parent must be warned about the unstructured handoff:\n%s", res.Text)
	}
	p.mu.Lock()
	n := len(p.gotReqs)
	p.mu.Unlock()
	if n != 1+yieldNudges {
		t.Fatalf("child was nudged %d times, want %d", n-1, yieldNudges)
	}
}

// A child that yields after the first nudge needs no warning.
func TestTaskToolNoYieldChildRecoversOnNudge(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: doneEvents("prose first")},
		{events: yieldEvents(`{"result":"RECOVERED"}`)},
	}}
	res, err := taskTool(p).Execute(context.Background(), json.RawMessage(`{"prompt":"do it"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text, "never called yield") {
		t.Fatalf("a recovered yield must not warn:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "RECOVERED") {
		t.Fatalf("the yielded result is missing:\n%s", res.Text)
	}
}

// omp's batch wire shape ({context, tasks[]}) must spawn, not be rejected
// with "prompt is required" (parity finding T3 #1).
func TestTaskToolBatchShape(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: yieldEvents(`{"result":"ALPHA-DONE"}`)},
		{events: yieldEvents(`{"result":"BETA-DONE"}`)},
	}}
	res, err := taskTool(p).Execute(context.Background(), json.RawMessage(
		`{"context":"SHARED FRAMING","tasks":[{"name":"alpha","task":"do A"},{"name":"beta","task":"do B"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("batch rejected: %+v", res)
	}
	for _, want := range []string{"ALPHA-DONE", "BETA-DONE", "batch: 2 task(s), 0 failed", "· alpha", "· beta"} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("report missing %q:\n%s", want, res.Text)
		}
	}
	// Both children saw the shared context — that is what `context` is for.
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.gotReqs) != 2 {
		t.Fatalf("requests = %d, want one per task", len(p.gotReqs))
	}
	for i, req := range p.gotReqs {
		found := false
		for _, m := range req.Messages {
			for _, b := range m.Content {
				if tb, ok := b.(ai.TextBlock); ok && strings.Contains(tb.Text, "SHARED FRAMING") {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("child %d never saw the batch context", i)
		}
	}
}

// A batch item missing its assignment is reported per item; the others still
// run (one typo in a 5-item batch must not lose the other four).
func TestTaskToolBatchItemWithoutTask(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: yieldEvents(`{"result":"OK-ONE"}`)},
	}}
	res, err := taskTool(p).Execute(context.Background(), json.RawMessage(
		`{"tasks":[{"name":"good","task":"do it"},{"name":"empty","task":"  "}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("a partially bad batch must still report: %+v", res)
	}
	if !strings.Contains(res.Text, "OK-ONE") {
		t.Fatalf("the good item was lost: %s", res.Text)
	}
	if !strings.Contains(res.Text, "1 failed") || !strings.Contains(res.Text, "empty") {
		t.Fatalf("the bad item must be named as a failure: %s", res.Text)
	}
}

// A bare {prompt:""} stays the loud error it always was — with the batch
// grammar named, so a model following the docs can self-correct.
func TestTaskToolSinglePromptErrorNamesBatch(t *testing.T) {
	res, _ := taskTool(&fakeProvider{}).Execute(context.Background(), json.RawMessage(`{}`))
	if !res.IsError || !strings.Contains(res.Text, "tasks[]") {
		t.Fatalf("error must name both shapes: %+v", res)
	}
}

// --- #272: the frontmatter a definition declares must reach the child ----

func effortResolver(level string) *ai.ThinkingBudget {
	tokens, ok := config.EffortBudget(level)
	if !ok {
		return nil
	}
	return &ai.ThinkingBudget{Tokens: tokens}
}

func TestTaskToolAppliesDeclaredModelAndEffort(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: yieldEvents(`{"result":"ok"}`)},
	}}
	tt := &TaskTool{
		Provider: p, Model: "parent-model", ChildTools: []tool.Tool{echoTool{}},
		Agents: []AgentDefinition{{
			Name: "thinker", Description: "d",
			Model: "@slow", ThinkingLevel: "high",
		}},
		ExpandModel:  func(ref string) (string, bool) { return "resolved-model", ref == "@slow" },
		ExpandEffort: effortResolver,
	}
	res, err := tt.Execute(context.Background(), json.RawMessage(`{"prompt":"go","agent":"thinker"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("spawn failed: %q", res.Text)
	}
	if len(p.gotReqs) != 1 {
		t.Fatalf("child requests = %d", len(p.gotReqs))
	}
	req := p.gotReqs[0]
	if req.Model != "resolved-model" {
		t.Fatalf("frontmatter model not expanded on the child request: %q", req.Model)
	}
	if req.Thinking == nil || req.Thinking.Tokens != config.EffortTokens["high"] {
		t.Fatalf("thinkingLevel not applied: %+v", req.Thinking)
	}
	if strings.Contains(res.Text, "warnings:") {
		t.Fatalf("clean definition must warn about nothing: %q", res.Text)
	}
}

// An unresolvable role never reaches the wire (the raw "@role" 404s one turn
// later); the parent's model is kept and the parent is told.
func TestTaskToolUnresolvableModelKeepsParentAndSaysSo(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{{events: yieldEvents(`{"result":"ok"}`)}}}
	tt := &TaskTool{
		Provider: p, Model: "parent-model", ChildTools: []tool.Tool{echoTool{}},
		Agents:      []AgentDefinition{{Name: "a", Description: "d", Model: "@nosuch"}},
		ExpandModel: func(string) (string, bool) { return "", false },
	}
	res, _ := tt.Execute(context.Background(), json.RawMessage(`{"prompt":"go","agent":"a"}`))
	if p.gotReqs[0].Model != "parent-model" {
		t.Fatalf("model = %q", p.gotReqs[0].Model)
	}
	if !strings.Contains(res.Text, "@nosuch") {
		t.Fatalf("handoff must report the unexpanded role: %q", res.Text)
	}
}

// A `tools:` name the child cannot have (typo, or a tool with no child
// version) used to narrow the child silently — the file looked accepted.
func TestTaskToolReportsDroppedTools(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{{events: yieldEvents(`{"result":"ok"}`)}}}
	tt := &TaskTool{
		Provider: p, Model: "m", ChildTools: []tool.Tool{echoTool{}},
		Agents: []AgentDefinition{{Name: "a", Description: "d", Tools: stringList{"echo", "web_search"}}},
	}
	res, _ := tt.Execute(context.Background(), json.RawMessage(`{"prompt":"go","agent":"a"}`))
	if !strings.Contains(res.Text, "web_search") {
		t.Fatalf("dropped tool must be named to the parent: %q", res.Text)
	}
	if !strings.Contains(res.Text, "warnings:") {
		t.Fatalf("notes need a heading: %q", res.Text)
	}
}

// The advertised list is what makes a legal name guessable at all: before
// #272 nothing named the definitions anywhere the model could read.
func TestTaskToolDescriptionAdvertisesAgents(t *testing.T) {
	tt := &TaskTool{Agents: []AgentDefinition{{Name: "scout", Description: "read-only recon"}}}
	if d := tt.Description(); !strings.Contains(d, "scout") || !strings.Contains(d, "read-only recon") {
		t.Fatalf("description omits the agent list: %q", d)
	}
	// No named agents = no vestigial heading.
	if d := (&TaskTool{}).Description(); strings.Contains(d, "named agent types") {
		t.Fatalf("empty set must advertise nothing: %q", d)
	}
}

// Newly written files are spawnable without a restart: the frozen
// startup snapshot was half of the "available: none" report.
func TestTaskToolResolvesAgentsPerSpawn(t *testing.T) {
	dir := t.TempDir()
	p := &fakeProvider{calls: []fakeScript{
		{events: yieldEvents(`{"result":"ok"}`)},
		{events: yieldEvents(`{"result":"ok"}`)},
	}}
	tt := &TaskTool{Provider: p, Model: "m", AgentRoots: dir, ChildTools: []tool.Tool{echoTool{}}}
	res, _ := tt.Execute(context.Background(), json.RawMessage(`{"prompt":"go","agent":"fresh"}`))
	// The bundled names are available; the authored one is not yet.
	if !res.IsError || !strings.Contains(res.Text, "unknown agent") {
		t.Fatalf("unwritten agent must fail: %q", res.Text)
	}
	if strings.Contains(res.Text, "available: none") {
		t.Fatalf("a stock install must never report an empty set: %q", res.Text)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".xdev", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".xdev", "agents", "fresh.md"),
		[]byte("---\nname: fresh\ndescription: newly written\n---\ngo"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ = tt.Execute(context.Background(), json.RawMessage(`{"prompt":"go","agent":"fresh"}`))
	if res.IsError {
		t.Fatalf("newly written agent needs a restart: %q", res.Text)
	}
	// And the model-facing listing picked it up too.
	if !strings.Contains(tt.Description(), "newly written") {
		t.Fatalf("description cached past the file change")
	}
}
