package agent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

// recInterceptor records every Emit for the lifecycle-event tests.
type recInterceptor struct {
	mu     sync.Mutex
	events []string
}

func (r *recInterceptor) ToolCall(_ context.Context, _ ai.ToolCallBlock) (json.RawMessage, error) {
	return nil, nil
}
func (r *recInterceptor) ToolResult(_ context.Context, _ ai.ToolCallBlock, res json.RawMessage, _ bool) json.RawMessage {
	return res
}
func (r *recInterceptor) Emit(_ context.Context, event string, _ any) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
}
func (r *recInterceptor) has(ev string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e == ev {
			return true
		}
	}
	return false
}
func (r *recInterceptor) count(ev string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e == ev {
			n++
		}
	}
	return n
}

func plainTextMsg(text string) fakeScript {
	return fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil,
		&ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: text}}, StopReason: ai.StopReasonStop})}}
}

// TestRunEmitsTurnLifecycle: one Run must emit the full agent lifecycle:
// session_start, before_agent_start, agent_start up front; one turn_start
// and one turn_end per executed turn (two turns here: a tool turn, then the
// closing text turn); agent_end at exit.
func TestRunEmitsTurnLifecycle(t *testing.T) {
	callMsg := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.ToolCallBlock{ID: "c", Name: "echo", Arguments: json.RawMessage(`{"text":"x"}`)}}}
	p := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, callMsg)}},
		plainTextMsg("bye"),
	}}
	rec := &recInterceptor{}
	a, _, _ := runAgent(t, p)
	a.Intercept = rec

	if _, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "go"}}}}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"session_start", "before_agent_start", "agent_start", "agent_end"} {
		if !rec.has(want) {
			t.Fatalf("missing %q in %v", want, rec.events)
		}
	}
	if n := rec.count("turn_start"); n != 2 {
		t.Fatalf("turn_start fired %d times, want one per turn: %v", n, rec.events)
	}
	if n := rec.count("turn_end"); n != 2 {
		t.Fatalf("turn_end fired %d times, want one per turn: %v", n, rec.events)
	}
}

// TestRunTurnEndAfterTools: a turn that executes tool calls still closes
// with turn_end before the next turn starts.
func TestRunTurnEndAfterTools(t *testing.T) {
	rec := &recInterceptor{}
	callMsg := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.ToolCallBlock{ID: "c", Name: "echo", Arguments: json.RawMessage(`{"text":"x"}`)}}}
	p := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, callMsg)}},
		plainTextMsg("done"),
	}}
	a, _, _ := runAgent(t, p)
	a.Intercept = rec
	if _, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "go"}}}}); err != nil {
		t.Fatal(err)
	}
	if n := rec.count("turn_end"); n != 2 {
		t.Fatalf("turn_end = %d, want 2: %v", n, rec.events)
	}
}

// TestWithCompactionEvent: the seam fires session_compact on the bus when
// compaction runs, while the inner TurnHooks.OnCompaction still fires.
func TestWithCompactionEvent(t *testing.T) {
	rec := &recInterceptor{}
	var sawTokens int64 = -1
	inner := TurnHooksFunc{OnCompactionF: func(b int64) { sawTokens = b }}
	wrapped := WithCompactionEvent(inner, rec)
	if WithCompactionEvent(nil, rec) != nil {
		t.Fatal("nil hooks must pass through")
	}
	if WithCompactionEvent(inner, nil) == nil {
		t.Fatal("nil interceptor must return the same hook value, not nil")
	}

	wrapped.OnCompaction(4321)
	if sawTokens != 4321 {
		t.Fatalf("inner OnCompaction lost: %d", sawTokens)
	}
	if !rec.has("session_compact") {
		t.Fatalf("session_compact missing: %v", rec.events)
	}
}
