package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// #92: auto_retry_start/end and session_shutdown had no emission point. A
// hook that logs provider health needs the attempt, the classified reason and
// the delay — the debug log is invisible to it.

type eventCollector struct {
	mu     sync.Mutex
	events map[string][]map[string]any
}

func (c *eventCollector) ToolCall(_ context.Context, call ai.ToolCallBlock) (json.RawMessage, error) {
	args := call.Arguments
	return args, nil
}
func (c *eventCollector) ToolResult(_ context.Context, _ ai.ToolCallBlock, result json.RawMessage, _ bool) json.RawMessage {
	return result
}
func (c *eventCollector) Emit(_ context.Context, event string, payload any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, _ := payload.(map[string]any)
	c.events[event] = append(c.events[event], m)
}

func (c *eventCollector) names() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.events))
	for k := range c.events {
		out = append(out, k)
	}
	return out
}

func retryProvider(err error) *retryFailProvider { return &retryFailProvider{err: err} }

type retryFailProvider struct {
	err   error
	calls int
	mu    sync.Mutex
}

func (p *retryFailProvider) Stream(context.Context, ai.StreamRequest) (<-chan ai.Event, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return nil, p.err
}
func (p *retryFailProvider) Name() string { return "fail" }
func (p *retryFailProvider) API() string  { return "fail" }

func TestAutoRetryEventsFire(t *testing.T) {
	col := &eventCollector{events: map[string][]map[string]any{}}
	// A 429 is retriable under the ladder; the budget is one retry so the
	// test stays fast.
	ag := &Agent{
		Provider: retryProvider(&ai.HTTPError{API: "fail", Status: 429, Body: "slow down"}),
		Tools:    tool.NewRegistry(), Model: "m",
		Intercept: col,
		Retry:     fastRetry(),
	}
	if _, err := ag.Run(context.Background(), "sys", []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "go"}}},
	}); err == nil {
		t.Fatal("an always-429 provider must end in an error")
	}
	starts, ends := col.events["auto_retry_start"], col.events["auto_retry_end"]
	if len(starts) == 0 || len(ends) == 0 {
		t.Fatalf("no auto_retry events emitted (got %v)", col.names())
	}
	if len(starts) != len(ends) {
		t.Fatalf("%d starts vs %d ends: a started retry must be closed", len(starts), len(ends))
	}
	first := starts[0]
	if first["attempt"] == nil || first["kind"] == nil {
		t.Fatalf("payload missing attempt/kind: %+v", first)
	}
	if !strings.Contains(anyString(first["error"]), "429") {
		t.Fatalf("the reason must be in the payload: %+v", first)
	}
	if first["maxAttempts"] == nil || first["delayMs"] == nil {
		t.Fatalf("payload missing maxAttempts/delayMs: %+v", first)
	}
}

func TestSessionShutdownEvent(t *testing.T) {
	col := &eventCollector{events: map[string][]map[string]any{}}
	ag := &Agent{Provider: retryProvider(nil), Model: "m", Intercept: col}
	ag.EmitSessionShutdown()
	got := col.events["session_shutdown"]
	if len(got) != 1 || got[0]["model"] != "m" {
		t.Fatalf("session_shutdown = %v", got)
	}
	// No bus: a no-op, not a panic.
	(&Agent{Model: "m"}).EmitSessionShutdown()
}

func anyString(v any) string {
	s, _ := v.(string)
	return s
}
