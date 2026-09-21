package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
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
	// Tagged as harness text: the transcript and the stats counter both
	// branch on the attribution, so an untagged wrap-up would replay as a ❯
	// block and count as something the user typed (#283).
	if last.Attribution != TurnBudgetAttribution {
		t.Fatalf("wrap-up attribution = %q, want %q", last.Attribution, TurnBudgetAttribution)
	}
	if len(*ends) != 4 {
		t.Fatalf("message_end hooks = %d, want 4", len(*ends))
	}
}

// TestRunWrapsUpAtPerTurnTokenBudget pins the per-turn cap (RCA #1):
// a single turn crossing the per-turn token budget wraps up inline and
// the session keeps going — the run does not end asking the user to
// say "continue" (the session is not a turn).
func TestRunWrapsUpAtPerTurnTokenBudget(t *testing.T) {
	// First turn blows past the per-turn token cap (5M).
	toolMsg := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.ToolCallBlock{ID: "c", Name: "echo", Arguments: json.RawMessage(`{"text":"x"}`)}}}
	toolScript := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop,
		&ai.Usage{TotalTokens: 6_000_000}, toolMsg)}}
	// The inline wrap-up turn reports the status and the run keeps going.
	wrapped := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.TextBlock{Text: "wrapped up"}}}
	wrapScript := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, wrapped)}}
	// A normal next turn returns normally.
	doneScript := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil,
		&ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
			Content: []ai.Block{ai.TextBlock{Text: "done"}}})}}
	p := &fakeProvider{calls: []fakeScript{toolScript, wrapScript, doneScript}}
	a, ends, _ := runAgent(t, p)
	a.TurnTokenBudget = 5_000_000

	final, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "go"}}}})
	if err != nil {
		t.Fatalf("per-turn budget must not fail the run, got %v", err)
	}
	if final.Text() != "done" {
		t.Fatalf("final = %q, want \"done\" — per-turn budget wrapped up inline and run kept going", final.Text())
	}
	if n := len(p.gotReqs); n != 3 {
		t.Fatalf("stream requests = %d, want 3 (turn + inline wrap-up + next turn)", n)
	}
	if len(*ends) != 2 {
		t.Fatalf("message_end hooks = %d, want 2 (wrap-up + done; the turn that crossed the cap is dropped before OnMessageEnd, same as the old session-budget break)", len(*ends))
	}
	// The turn that crossed the cap got a wrap-up user message injected
	// (the TurnBudgetPrompt), not a run-ending stop.
	req := p.gotReqs[1]
	last := req.Messages[len(req.Messages)-1]
	if last.Role != ai.RoleUser || last.Text() != TurnBudgetPrompt {
		t.Fatalf("wrap-up prompt missing from second request: %+v", last)
	}
}

// TestEmptyTurnNudgeIsBounded pins #331: a turn with no text and no tool
// call — the reasoning-only shape a thinking-mode upstream leaves behind —
// must not end the run, and the nudge that keeps it alive is bounded per run
// so a model that only ever stalls cannot loop on it.
func TestEmptyTurnNudgeIsBounded(t *testing.T) {
	blank := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.ThinkingBlock{Thinking: "(context elided)"}}}
	answered := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.TextBlock{Text: "here is the answer"}}}
	blankScript := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, blank)}}
	answerScript := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, answered)}}
	tests := []struct {
		name       string
		calls      []fakeScript
		wantCalls  int
		wantFinal  string
		wantNudges int
	}{
		{
			name:       "nudge recovers the run",
			calls:      []fakeScript{blankScript, answerScript},
			wantCalls:  2,
			wantFinal:  "here is the answer",
			wantNudges: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &fakeProvider{calls: tc.calls}
			a, ends, _ := runAgent(t, p)
			var nudges []string
			a.Hooks = TurnHooksFunc{
				OnMessageEndF: func(m *ai.Message) { *ends = append(*ends, m) },
				OnEmptyTurnF:  func(text string) { nudges = append(nudges, text) },
			}
			final, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if final.Text() != tc.wantFinal {
				t.Fatalf("final = %q, want %q", final.Text(), tc.wantFinal)
			}
			if n := len(p.gotReqs); n != tc.wantCalls {
				t.Fatalf("stream requests = %d, want %d", n, tc.wantCalls)
			}
			if len(nudges) != tc.wantNudges {
				t.Fatalf("OnEmptyTurn fired %d times, want %d", len(nudges), tc.wantNudges)
			}
			// The nudge rides as harness text, never as something the user
			// typed: the transcript and the stats counter both branch on the
			// attribution (same contract as TestRunWrapsUpAtTurnLimit).
			last := p.gotReqs[tc.wantCalls-1]
			var found bool
			for _, m := range last.Messages {
				if m.Text() != EmptyTurnNudgePrompt {
					continue
				}
				found = true
				if m.Role != ai.RoleUser || m.Attribution != EmptyTurnAttribution {
					t.Fatalf("nudge message = %+v, want hidden user turn with attribution %q", m, EmptyTurnAttribution)
				}
			}
			if !found {
				t.Fatalf("nudge prompt missing from the recovery request: %+v", last.Messages)
			}
		})
	}
}

// TestEmptyTurnStallSurfacesAsAnError is the other end of #331: once the
// nudges are spent and the model still answers nothing, the run ends with
// ErrEmptyTurn instead of a nil-error stop that the TUI paints as a plain
// halt with nothing on screen (field report, session 1883e928).
func TestEmptyTurnStallSurfacesAsAnError(t *testing.T) {
	blank := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.ThinkingBlock{Thinking: "(context elided)"}}}
	blankScript := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, blank)}}
	// Three blank scripts: the two nudges are spent, the third answer ends
	// the run with the error and no fourth request is made.
	p := &fakeProvider{calls: []fakeScript{blankScript, blankScript, blankScript, blankScript}}
	a, _, _ := runAgent(t, p)
	var nudges int
	a.Hooks = TurnHooksFunc{OnEmptyTurnF: func(string) { nudges++ }}
	_, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}})
	if !errors.Is(err, ErrEmptyTurn) {
		t.Fatalf("a stalled run must surface ErrEmptyTurn, got %v", err)
	}
	if nudges != maxEmptyTurnNudges {
		t.Fatalf("OnEmptyTurn fired %d times, want %d", nudges, maxEmptyTurnNudges)
	}
	if n := len(p.gotReqs); n != maxEmptyTurnNudges+1 {
		t.Fatalf("stream requests = %d, want %d", n, maxEmptyTurnNudges+1)
	}
}
// TestEmptyTurnRetryAllErrors pins the retry-all-errors path: with
// RetryAllErrors on, a model that answers nothing after the nudges
// are spent keeps going — the loop rebuilds context, waits a
// backoff, and re-runs until the model answers or the budget fires,
// instead of surfacing ErrEmptyTurn.
func TestEmptyTurnRetryAllErrors(t *testing.T) {
	blank := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.ThinkingBlock{Thinking: "(context elided)"}}}
	blankScript := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, blank)}}
	answered := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.TextBlock{Text: "here is the answer"}}}
	answerScript := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, answered)}}
	// Two blank turns (the two nudges) + one recovery attempt that
	// answers — the flag lifts the empty-turn bound.
	p := &fakeProvider{calls: []fakeScript{blankScript, blankScript, answerScript}}
	a, _, _ := runAgent(t, p)
	a.Retry = RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, RetryAllErrors: true}
	final, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Text() != "here is the answer" {
		t.Fatalf("final = %q, want %q", final.Text(), "here is the answer")
	}
	// Two blank turns (nudges) + one answer = 3 stream requests.
	if n := len(p.gotReqs); n != 3 {
		t.Fatalf("stream requests = %d, want 3", n)
	}
}
// TestEmptyTurnRetryAllErrorsStillStalls pins the other side: with
// RetryAllErrors on but no store to rebuild from, the flag has no
// recovery path and the run still ends with an error — it burns
// through the scripted blanks and surfaces "script exhausted"
// rather than looping forever.
func TestEmptyTurnRetryAllErrorsStillStalls(t *testing.T) {
	blank := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.ThinkingBlock{Thinking: "(context elided)"}}}
	blankScript := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, blank)}}
	// 1 initial + 2 nudges = 3 blanks to drain the nudge budget,
	// then the 4th call exhausts the script. With RetryAllErrors
	// on and no store, the loop keeps going past the nudges but
	// still ends — it does not loop forever.
	p := &fakeProvider{calls: []fakeScript{blankScript, blankScript, blankScript, blankScript}}
	a, _, _ := runAgent(t, p)
	a.Retry = RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, RetryAllErrors: true}
	_, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}})
	if err == nil {
		t.Fatal("a stalled run with no store must surface an error, got nil")
	}
	if errors.Is(err, ErrEmptyTurn) {
		t.Fatal("with RetryAllErrors on and no store, the run must not surface ErrEmptyTurn — it keeps going until the script exhausts")
	}
}

// TestEmptyTurnRetryAllErrorsAnnouncesOnStream pins the freeze fix: with
// RetryAllErrors on, each empty-turn recovery after the nudge budget is
// spent raises EmptyTurnRetryError through OnEvent so the TUI/print
// surfaces "still waiting" instead of looking hung.
func TestEmptyTurnRetryAllErrorsAnnouncesOnStream(t *testing.T) {
	blank := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.ThinkingBlock{Thinking: "(context elided)"}}}
	blankScript := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, blank)}}
	answered := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.TextBlock{Text: "recovered"}}}
	answerScript := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, answered)}}
	// 1 initial + 2 nudges drain the budget; 3rd blank triggers the
	// RetryAllErrors recovery (and its announcement); 4th answers.
	p := &fakeProvider{calls: []fakeScript{blankScript, blankScript, blankScript, answerScript}}
	var announced []error
	a, s, _ := storeAgent(t, p, CompactionConfig{})
	a.Hooks = TurnHooksFunc{
		OnEventF: func(ev ai.Event) {
			if ev.Type != ai.EventError {
				return
			}
			var empty *EmptyTurnRetryError
			if errors.As(ev.Err, &empty) {
				announced = append(announced, ev.Err)
			}
		},
		OnMessageEndF:    func(m *ai.Message) { _ = s.Append(&session.MessageEntry{Message: *m}) },
		OnToolResultMsgF: func(m *ai.Message) { _ = s.Append(&session.MessageEntry{Message: *m}) },
	}
	a.Retry = RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, RetryAllErrors: true}
	final, err := a.Run(context.Background(), "sys", submitHistory(t, s, "hi"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Text() != "recovered" {
		t.Fatalf("final = %q", final.Text())
	}
	if len(announced) < 1 {
		t.Fatal("empty-turn recovery must announce on the event stream")
	}
	var empty *EmptyTurnRetryError
	if !errors.As(announced[0], &empty) || empty.Round < 1 || empty.Delay <= 0 {
		t.Fatalf("announcement = %v, want round/delay", announced[0])
	}
	if !strings.Contains(announced[0].Error(), "empty turn") {
		t.Fatalf("announcement text = %q", announced[0])
	}
}

// TestEmptyTurnNudgeDoesNotFireOnToolCalls guards the other side of #331:
// a tool-call turn still runs the tools, and a turn that answers in prose
// still ends the run — the nudge is for the blank turn alone.
func TestEmptyTurnNudgeDoesNotFireOnToolCalls(t *testing.T) {
	callMsg := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.ToolCallBlock{ID: "c", Name: "echo", Arguments: json.RawMessage(`{"text":"x"}`)}}}
	p := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, callMsg)}},
		{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil,
			&ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
				Content: []ai.Block{ai.TextBlock{Text: "done"}}})}},
	}}
	a, _, _ := runAgent(t, p)
	var nudges int
	a.Hooks = TurnHooksFunc{OnEmptyTurnF: func(string) { nudges++ }}
	final, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "go"}}}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Text() != "done" {
		t.Fatalf("final = %q", final.Text())
	}
	if nudges != 0 {
		t.Fatalf("OnEmptyTurn fired %d times on a tool-call turn", nudges)
	}
}

func TestEffectiveMaxTurns(t *testing.T) {
	tests := []struct {
		name string
		set  int
		want int
	}{
		{"unbounded by default", 0, 0},
		{"explicit wins", 5, 5},
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
	// Unknown stream-start errors are retried from the current context
	// (bounded by escalation). Script enough failures to drain the bound
	// so the original provider error still surfaces.
	p := &fakeProvider{calls: []fakeScript{
		{err: errors.New("boom")},
		{err: errors.New("boom")},
		{err: errors.New("boom")},
	}}
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
