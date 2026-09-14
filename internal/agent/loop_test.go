package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// fakeProvider streams a scripted sequence of events per Stream call.
type fakeProvider struct {
	mu      sync.Mutex // a batch spawn drives it from several goroutines
	calls   []fakeScript
	i       int
	gotReqs []ai.StreamRequest
}

type fakeScript struct {
	events []ai.Event
	err    error
}

func (f *fakeProvider) Stream(_ context.Context, req ai.StreamRequest) (<-chan ai.Event, error) {
	f.mu.Lock()
	f.gotReqs = append(f.gotReqs, req)
	if f.i >= len(f.calls) {
		f.mu.Unlock()
		return nil, errors.New("script exhausted")
	}
	sc := f.calls[f.i]
	f.i++
	f.mu.Unlock()
	ch := make(chan ai.Event, 32)
	go func() {
		defer close(ch)
		if sc.err != nil {
			ch <- ai.Errorf(sc.err)
			return
		}
		for _, ev := range sc.events {
			ch <- ev
		}
	}()
	return ch, nil
}

func (f *fakeProvider) Name() string { return "fake" }
func (f *fakeProvider) API() string  { return "fake-api" }

// echoTool returns its arguments as text.
type echoTool struct{}

func (echoTool) Name() string        { return "echo" }
func (echoTool) Description() string { return "echo the arguments" }
func (echoTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`)
}
func (echoTool) Execute(_ context.Context, args json.RawMessage) (tool.Result, error) {
	var a struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return tool.Result{}, err
	}
	return tool.Result{Text: a.Text, Details: map[string]any{"echoed": a.Text}}, nil
}

func runAgent(t *testing.T, p *fakeProvider) (*Agent, *[]*ai.Message, *[]*ai.Message) {
	t.Helper()
	reg := tool.NewRegistry()
	reg.Register(echoTool{})
	var ends, results []*ai.Message
	hooks := TurnHooksFunc{
		OnMessageEndF:    func(m *ai.Message) { ends = append(ends, m) },
		OnToolResultMsgF: func(m *ai.Message) { results = append(results, m) },
	}
	a := &Agent{Provider: p, Tools: reg, Hooks: hooks}
	return a, &ends, &results
}

func TestRunTextOnly(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{{
		events: []ai.Event{
			{Type: ai.EventStart, Provider: "fake", Model: "m"},
			{Type: ai.EventTextStart},
			{Type: ai.EventTextDelta, Delta: "Hello "},
			{Type: ai.EventTextDelta, Delta: "world"},
			{Type: ai.EventDone, StopReason: ai.StopReasonStop,
				Message: &ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "Hello world"}}, StopReason: ai.StopReasonStop}},
		},
	}}}
	a, ends, _ := runAgent(t, p)
	final, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Text() != "Hello world" {
		t.Fatalf("final = %q", final.Text())
	}
	if len(*ends) != 1 {
		t.Fatalf("message_end hooks = %d", len(*ends))
	}
}

func TestRunWithToolCall(t *testing.T) {
	args := json.RawMessage(`{"text":"pong"}`)
	toolDone := ai.Donef(ai.StopReasonStop, nil, &ai.Message{
		Role: ai.RoleAssistant,
		Content: []ai.Block{
			ai.TextBlock{Text: "calling"},
			ai.ToolCallBlock{ID: "c1", Name: "echo", Arguments: args, StreamIndex: 0},
		},
		StopReason: ai.StopReasonStop,
	})
	p := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{
			{Type: ai.EventStart},
			{Type: ai.EventToolcallStart, ToolCallID: "c1", ToolName: "echo", StreamIndex: 0},
			{Type: ai.EventToolcallDelta, StreamIndex: 0, PartialJSON: `{"text":"po`},
			{Type: ai.EventToolcallDelta, StreamIndex: 0, PartialJSON: `{"text":"pong"}`},
			{Type: ai.EventToolcallEnd, StreamIndex: 0},
			toolDone,
		}},
		{events: []ai.Event{
			{Type: ai.EventTextStart},
			{Type: ai.EventTextDelta, Delta: "done"},
			{Type: ai.EventDone, StopReason: ai.StopReasonStop,
				Message: &ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "done"}}, StopReason: ai.StopReasonStop}},
		}},
	}}
	a, ends, results := runAgent(t, p)
	final, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Text() != "done" {
		t.Fatalf("final = %q", final.Text())
	}
	if len(*ends) != 2 || len(*results) != 1 {
		t.Fatalf("hooks: ends=%d results=%d", len(*ends), len(*results))
	}
	rm := (*results)[0]
	if rm.Role != ai.RoleToolResult || rm.ToolCallID != "c1" || rm.ToolName != "echo" {
		t.Fatalf("toolResult msg = %+v", rm)
	}
	if rm.Text() != "pong" {
		t.Fatalf("toolResult text = %q", rm.Text())
	}
	// Second turn's request must contain the toolResult message.
	req2 := p.gotReqs[1]
	if n := len(req2.Messages); n != 3 {
		t.Fatalf("turn-2 messages = %d", n)
	}
	if req2.Messages[2].Role != ai.RoleToolResult {
		t.Fatalf("turn-2 msg[2] role = %v", req2.Messages[2].Role)
	}
	// The streamed request must carry the tool definition.
	if len(req2.Tools) != 1 || req2.Tools[0].Name != "echo" {
		t.Fatalf("tools = %+v", req2.Tools)
	}
}

func TestRunWrapsUpAtTurnLimit(t *testing.T) {
	callMsg := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.ToolCallBlock{ID: "c", Name: "echo", Arguments: json.RawMessage(`{"text":"x"}`)}}}
	toolScript := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, callMsg)}}
	textScript := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil,
		&ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
			Content: []ai.Block{ai.TextBlock{Text: "wrapped up"}}})}}
	p := &fakeProvider{calls: []fakeScript{toolScript, toolScript, toolScript, textScript}}
	a, ends, _ := runAgent(t, p)
	a.MaxTurns = 3

	final, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "go"}}}})
	if err != nil {
		t.Fatalf("turn budget must not fail the run, got %v", err)
	}
	if final.Text() != "wrapped up" {
		t.Fatalf("final = %q", final.Text())
	}
	if n := len(p.gotReqs); n != 4 {
		t.Fatalf("stream requests = %d, want 3 budgeted + 1 wrap-up", n)
	}
	req := p.gotReqs[3]
	last := req.Messages[len(req.Messages)-1]
	if last.Role != ai.RoleUser || last.Text() != TurnBudgetPrompt {
		t.Fatalf("wrap-up prompt missing from last request: %+v", last)
	}
	if len(*ends) != 4 {
		t.Fatalf("message_end hooks = %d, want 4", len(*ends))
	}
}

func TestEffectiveMaxTurns(t *testing.T) {
	if DefaultMaxTurns < 100 {
		t.Fatalf("DefaultMaxTurns = %d, long tasks would be killed", DefaultMaxTurns)
	}
	tests := []struct {
		name string
		set  int
		want int
	}{
		{"zero means default", 0, DefaultMaxTurns},
		{"explicit wins", 5, 5},
		{"negative means default", -1, DefaultMaxTurns},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &Agent{MaxTurns: tt.set}
			if got := a.effectiveMaxTurns(); got != tt.want {
				t.Fatalf("effectiveMaxTurns() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestRunRunawayToolLoopTerminates(t *testing.T) {
	// More tool-call scripts than the budget: only the cap can end the run.
	callMsg := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.ToolCallBlock{ID: "c", Name: "echo", Arguments: json.RawMessage(`{"text":"x"}`)}}}
	loop := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, callMsg)}}
	// Wrap-up also tries a tool call — budget is spent, it must not run.
	wrap := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, callMsg)}}
	p := &fakeProvider{calls: []fakeScript{loop, loop, loop, loop, wrap}}
	a, _, results := runAgent(t, p)
	a.MaxTurns = 3

	final, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "go"}}}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := len(p.gotReqs); n != 4 {
		t.Fatalf("stream requests = %d, want 4 (wrap-up is the last)", n)
	}
	if n := len(*results); n != 3 {
		t.Fatalf("executed tool calls = %d, want 3 (wrap-up calls must not run)", n)
	}
	if final == nil || len(final.ToolCalls()) == 0 {
		t.Fatalf("wrap-up message with tool calls not returned: %+v", final)
	}
}

func TestRunStreamError(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{{err: errors.New("boom")}}}
	a, _, _ := runAgent(t, p)
	_, err := a.Run(context.Background(), "sys", nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want provider error, got %v", err)
	}
}

func TestSteeringInjection(t *testing.T) {
	textMsg := func(s string) *ai.Message {
		return &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
			Content: []ai.Block{ai.TextBlock{Text: s}}}
	}
	p := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, textMsg("first"))}},
		{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, textMsg("second"))}},
	}}

	var a *Agent
	var ends int
	hooks := TurnHooksFunc{OnMessageEndF: func(m *ai.Message) {
		ends++
		if ends == 1 {
			a.Steer("correction") // user types while the agent works
		}
	}}
	reg := tool.NewRegistry()
	reg.Register(echoTool{})
	a = &Agent{Provider: p, Tools: reg, Hooks: hooks}

	final, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "go"}}}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Text() != "second" {
		t.Fatalf("final = %q", final.Text())
	}
	// Turn 2 must contain the steered user message between turns.
	req2 := p.gotReqs[1]
	found := false
	for _, m := range req2.Messages {
		if m.Role == ai.RoleUser && m.Text() == "correction" {
			found = true
		}
	}
	if !found {
		t.Fatalf("steer message missing from turn 2: %+v", req2.Messages)
	}
}

// badSchemaTool carries a trailing-comma parameters blob — the exact shape
// (an invalid json.RawMessage) that once made every provider call die at
// stream start, killing all tools with it.
type badSchemaTool struct{}

func (badSchemaTool) Name() string        { return "bad_schema" }
func (badSchemaTool) Description() string { return "malformed on purpose" }
func (badSchemaTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"},},`)
}
func (badSchemaTool) Execute(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{}, nil
}

// TestToolDefsGuardsMalformedSchema: one bad schema degrades to {} and is
// named in the log; it must not poison the rest of the request.
func TestToolDefsGuardsMalformedSchema(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(tool.NewReadTool())
	reg.Register(badSchemaTool{})
	a := &Agent{Tools: reg}
	defs := a.toolDefs()
	if len(defs) != 2 {
		t.Fatalf("defs = %d, want 2", len(defs))
	}
	var bad, read ai.ToolDef
	for _, d := range defs {
		if d.Name == "bad_schema" {
			bad = d
		} else {
			read = d
		}
	}
	if string(bad.Parameters) != "{}" {
		t.Fatalf("bad schema went through unguarded: %s", bad.Parameters)
	}
	if !json.Valid(read.Parameters) || !strings.Contains(string(read.Parameters), "path") {
		t.Fatalf("healthy tool schema altered: %s", read.Parameters)
	}
}
