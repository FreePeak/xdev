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
