package agent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// A "background" subagent must outlive the turn that dispatched it. Start
// inherited the tool-call context, so the job was canceled the moment the
// parent turn ended — the roster then showed `[killed: signal]` for a child
// that had been asked to keep working.

func TestBackgroundJobSurvivesTurnCancel(t *testing.T) {
	turnCtx, endTurn := context.WithCancel(context.Background())

	started := make(chan struct{})
	var once sync.Once
	prov := &slowChildProvider{started: started, once: &once}

	hub := NewHub()
	tools := []tool.Tool{sleepyTool{}}
	spec := SubagentSpec{
		Name: "keep-going", Prompt: "count", Provider: prov, Model: "m",
		Tools: tools, MaxTurns: 3,
	}
	// Start the job through the tool-shaped path: hub.Start with the TURN ctx.
	id, err := hub.Start(turnCtx, spec)
	if err != nil {
		t.Fatal(err)
	}
	<-started // the child is genuinely mid-run

	endTurn() // the parent turn ends while the child is still working
	time.Sleep(80 * time.Millisecond)

	info, ok := hub.Status(id)
	if !ok {
		t.Fatalf("job %s vanished", id)
	}
	if info.Status != "running" {
		t.Fatalf("job status after the parent turn ended = %q, want running", info.Status)
	}

	// Close is the session boundary that does stop it.
	hub.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if i, _ := hub.Status(id); i.Status != "running" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if i, _ := hub.Status(id); i.Status == "running" {
		t.Fatal("Hub.Close did not end the running job")
	}
}

// slowChildProvider streams a tool call and then blocks in the tool loop, so
// the child is mid-run for the duration of the test.
type slowChildProvider struct {
	started chan struct{}
	once    *sync.Once
	mu      sync.Mutex
	n       int
}

func (p *slowChildProvider) Stream(ctx context.Context, req ai.StreamRequest) (<-chan ai.Event, error) {
	p.once.Do(func() { close(p.started) })
	p.mu.Lock()
	p.n++
	turn := p.n
	p.mu.Unlock()
	ch := make(chan ai.Event, 8)
	if turn == 1 {
		ch <- ai.Event{Type: ai.EventStart, Provider: "slow", Model: "m"}
		ch <- ai.Event{Type: ai.EventToolcallStart, ToolCallID: "b1", ToolName: "sleepy", StreamIndex: 0}
		ch <- ai.Event{Type: ai.EventToolcallEnd, StreamIndex: 0, PartialJSON: `{}`}
		ch <- ai.Donef(ai.StopReasonStop, nil, &ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{
			ai.ToolCallBlock{ID: "b1", Name: "sleepy", Arguments: json.RawMessage(`{}`), StreamIndex: 0},
		}, StopReason: ai.StopReasonStop})
		close(ch)
		return ch, nil
	}
	// Later turns block until the context is canceled (the child "still working").
	<-ctx.Done()
	return nil, ctx.Err()
}
func (p *slowChildProvider) Name() string { return "slow" }
func (p *slowChildProvider) API() string  { return "slow" }

// sleepyTool blocks until its context ends, standing in for a child that is
// still working when the parent turn closes.
type sleepyTool struct{}

func (sleepyTool) Name() string        { return "sleepy" }
func (sleepyTool) Description() string { return "blocks until canceled" }
func (sleepyTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (sleepyTool) Tier() tool.Tier { return tool.TierExec }
func (sleepyTool) Execute(ctx context.Context, _ json.RawMessage) (tool.Result, error) {
	<-ctx.Done()
	return tool.Result{Text: "canceled", IsError: true}, ctx.Err()
}
