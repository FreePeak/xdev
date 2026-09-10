package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// toolCallEvents scripts one assistant turn that calls a tool.
func toolCallEvents(name, args string) []ai.Event {
	start := ai.Event{Type: ai.EventToolcallStart, ToolCallID: "c1", ToolName: name, StreamIndex: 0}
	return []ai.Event{
		{Type: ai.EventStart},
		start,
		{Type: ai.EventToolcallDelta, StreamIndex: 0, PartialJSON: args},
		{Type: ai.EventToolcallEnd, StreamIndex: 0, PartialJSON: args},
		ai.Donef(ai.StopReasonStop, nil, &ai.Message{
			Role: ai.RoleAssistant,
			Content: []ai.Block{
				ai.ToolCallBlock{ID: "c1", Name: name, Arguments: json.RawMessage(args), StreamIndex: 0},
			},
			StopReason: ai.StopReasonStop,
		}),
	}
}

// spyTool records that it ran, and answers anything.
type spyTool struct{ ran *[]string }

func (s spyTool) Name() string        { return "spy" }
func (s spyTool) Description() string { return "spy tool" }
func (s spyTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)
}
func (s spyTool) Execute(_ context.Context, args json.RawMessage) (tool.Result, error) {
	*s.ran = append(*s.ran, string(args))
	return tool.Result{Text: "spy ran"}, nil
}

// bashSpy is a bash-named tool so Classify sees TierExec.
type bashSpy struct{ ran *[]string }

func (b bashSpy) Name() string        { return "bash" }
func (b bashSpy) Description() string { return "run a command" }
func (b bashSpy) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)
}
func (b bashSpy) Execute(_ context.Context, args json.RawMessage) (tool.Result, error) {
	*b.ran = append(*b.ran, string(args))
	return tool.Result{Text: "executed"}, nil
}

func policyAgent(t *testing.T, scripts []fakeScript, pol tool.ApprovalPolicy, approve ApprovalFunc) (*Agent, *[]string, *hookLog) {
	t.Helper()
	var ran []string
	log := &hookLog{}
	reg := tool.NewRegistry()
	reg.Register(bashSpy{ran: &ran})
	reg.Register(spyTool{ran: &ran})
	p := &fakeProvider{calls: scripts}
	a := &Agent{Provider: p, Tools: reg, Hooks: log, Model: "m", Retry: fastRetry(), Policy: pol, Approve: approve}
	return a, &ran, log
}

// hookLog captures what the model was told.
type hookLog struct{ results []string }

func (h *hookLog) OnStart(ai.StreamRequest)     {}
func (h *hookLog) OnEvent(ai.Event)             {}
func (h *hookLog) OnToolStart(ai.ToolCallBlock) {}
func (h *hookLog) OnToolEnd(ai.ToolCallBlock, tool.Result, time.Duration) {
}
func (h *hookLog) OnMessageEnd(m *ai.Message) {}
func (h *hookLog) OnToolResultMessage(m *ai.Message) {
	h.results = append(h.results, m.Text())
}
func (h *hookLog) OnCompaction(int64)             {}
func (h *hookLog) OnTurnEnd(ai.StopReason, error) {}

func TestAgentDeniesBeforeExecuting(t *testing.T) {
	var ran []string
	pol := tool.ApprovalPolicy{
		Mode:         tool.Yolo,
		BashPatterns: []tool.PolicyRule{{Pattern: "rm -rf *", Action: tool.ActionDeny}},
	}
	args := `{"command":"rm -rf /tmp/oops"}`
	p := &fakeProvider{calls: []fakeScript{
		{events: toolCallEvents("bash", args)},
		{events: []ai.Event{{Type: ai.EventStart}, textEvent("noted"), doneEvent("noted")}},
	}}
	log := &hookLog{}
	reg := tool.NewRegistry()
	reg.Register(bashSpy{ran: &ran})
	a := &Agent{Provider: p, Tools: reg, Hooks: log, Model: "m", Retry: fastRetry(), Policy: pol}
	hist := []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "go"}}}}
	if _, err := a.Run(context.Background(), "sys", hist); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 0 {
		t.Fatalf("denied command reached the tool: %v", ran)
	}
	if len(log.results) != 1 || !strings.Contains(log.results[0], "denied") {
		t.Fatalf("model was not told why: %v", log.results)
	}
}

func TestAgentPromptWithoutApproverRefuses(t *testing.T) {
	// Unattended runs (print mode, children) have no user to ask: the safe
	// answer is refusal, not an implicit allow.
	a, ran, log := policyAgent(t, []fakeScript{
		{events: toolCallEvents("bash", `{"command":"anything"}`)},
		{events: []ai.Event{{Type: ai.EventStart}, textEvent("ok"), doneEvent("ok")}},
	}, tool.ApprovalPolicy{Mode: tool.AlwaysAsk}, nil)
	hist := []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "go"}}}}
	if _, err := a.Run(context.Background(), "sys", hist); err != nil {
		t.Fatal(err)
	}
	if len(*ran) != 0 {
		t.Fatal("a prompt with nobody to answer executed the tool")
	}
	if len(log.results) == 0 || !strings.Contains(log.results[0], "refused") {
		t.Fatalf("model was not told: %v", log.results)
	}
}

func TestAgentApproverAllowsAndSeesReason(t *testing.T) {
	var asked []string
	var reasons []string
	a, ran, _ := policyAgent(t, []fakeScript{
		{events: toolCallEvents("bash", `{"command":"git push"}`)},
		{events: []ai.Event{{Type: ai.EventStart}, textEvent("pushed"), doneEvent("pushed")}},
	}, tool.ApprovalPolicy{
		Mode:         tool.Yolo,
		BashPatterns: []tool.PolicyRule{{Pattern: "git push *", Action: tool.ActionPrompt}},
	}, func(call ai.ToolCallBlock, reason string) bool {
		asked = append(asked, call.Name)
		reasons = append(reasons, reason)
		return true
	})
	hist := []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "go"}}}}
	if _, err := a.Run(context.Background(), "sys", hist); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0] != "bash" {
		t.Fatalf("approver calls = %v", asked)
	}
	if len(reasons) == 0 || !strings.Contains(reasons[0], "git push *") {
		t.Fatalf("the approver must be told which rule fired: %v", reasons)
	}
	if len(*ran) != 1 {
		t.Fatalf("approved tool did not run: %v", *ran)
	}
}

func TestAgentYoloNeverPrompts(t *testing.T) {
	// The shipped default must stay untouched: zero approver calls.
	calls := 0
	a, ran, _ := policyAgent(t, []fakeScript{
		{events: toolCallEvents("bash", `{"command":"ls"}`)},
		{events: []ai.Event{{Type: ai.EventStart}, textEvent("ok"), doneEvent("ok")}},
	}, tool.ApprovalPolicy{Mode: tool.Yolo}, func(ai.ToolCallBlock, string) bool {
		calls++
		return false
	})
	hist := []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "go"}}}}
	if _, err := a.Run(context.Background(), "sys", hist); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("yolo prompted %d times", calls)
	}
	if len(*ran) != 1 {
		t.Fatalf("yolo blocked the tool: %v", *ran)
	}
}

// TestThinkingReachesProvider pins the last link: the role effort set on
// the agent must arrive on the provider request, not just sit in a field.
func TestThinkingReachesProvider(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{{Type: ai.EventStart}, textEvent("ok"), doneEvent("ok")}},
	}}
	a := &Agent{Provider: p, Tools: tool.NewRegistry(), Hooks: &hookLog{}, Model: "m",
		Thinking: &ai.ThinkingBudget{Tokens: 1234}}
	hist := []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "go"}}}}
	if _, err := a.Run(context.Background(), "sys", hist); err != nil {
		t.Fatal(err)
	}
	if len(p.gotReqs) != 1 || p.gotReqs[0].Thinking == nil || p.gotReqs[0].Thinking.Tokens != 1234 {
		t.Fatalf("thinking budget did not reach the request: %+v", p.gotReqs)
	}
}

// TestSubagentInheritsParentPolicy pins that delegation cannot sideload
// around approval: a child runs under the parent's policy, with yield
// exempt (a child that cannot hand back its result is a hang, not a
// security win).
func TestSubagentInheritsParentPolicy(t *testing.T) {
	parent := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{{Type: ai.EventStart}, textEvent("parent done"), doneEvent("parent done")}},
	}}
	child := &fakeProvider{calls: []fakeScript{
		{events: toolCallEvents("echo", `{"text":"hi"}`)},
		{events: []ai.Event{{Type: ai.EventStart}, textEvent("child done"), doneEvent("child done")}},
	}}
	var echoRan []string
	res, err := SpawnChild(context.Background(), SubagentSpec{
		Name: "guarded", Prompt: "go", Provider: child,
		Tools:    []tool.Tool{spyRan{ran: &echoRan}},
		Policy:   tool.ApprovalPolicy{Mode: tool.AlwaysAsk},
		Thinking: &ai.ThinkingBudget{Tokens: 99},
	})
	_ = parent
	_ = res
	if err != nil {
		t.Fatal(err)
	}
	// The refused tool never executed: delegation is not a way around the
	// parent's approval posture.
	if len(echoRan) != 0 {
		t.Fatalf("child executed a tool the parent policy must gate: %v", echoRan)
	}
	// A refusal is a tool result, not a crash: the child model must be told
	// why, and may then finish normally.
	if len(child.gotReqs) < 2 {
		t.Fatalf("child never saw the refusal: %d requests", len(child.gotReqs))
	}
	joined := ""
	for _, m := range child.gotReqs[1].Messages {
		joined += m.Text() + "\n"
	}
	if !strings.Contains(joined, "refused") {
		t.Fatalf("refusal not surfaced to the child model:\n%s", joined)
	}
	// The parent's resolved effort forwarded into the child's requests.
	if child.gotReqs[0].Thinking == nil || child.gotReqs[0].Thinking.Tokens != 99 {
		t.Fatalf("child lost the parent's thinking budget: %+v", child.gotReqs[0].Thinking)
	}
}

// spyRan is an echo-named tool that records execution.
type spyRan struct{ ran *[]string }

func (s spyRan) Name() string        { return "echo" }
func (s spyRan) Description() string { return "echo" }
func (s spyRan) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`)
}
func (s spyRan) Execute(_ context.Context, args json.RawMessage) (tool.Result, error) {
	*s.ran = append(*s.ran, string(args))
	return tool.Result{Text: "echoed"}, nil
}
