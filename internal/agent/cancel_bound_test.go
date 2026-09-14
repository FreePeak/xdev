package agent

// #126: a cancelled turn must actually stop, including for a tool that never
// looks at the context. The loop refuses to *start* a tool once cancelled, which
// is the only pre-execution gate grep and glob have (neither checks at entry)
// and the only one ext_*/mcp_* tools have (third-party code we cannot assume
// checks anything). A gate with no bound underneath it is a hang: the user
// pressed cancel and the harness is still blocked in someone else's loop.
//
// These tests are deliberately built on a stub that ignores ctx entirely and
// whose side effect is released by the test *after* the cancellation — the shape
// the existing write-based cancel test cannot express, because write self-checks
// the same window and passes whether or not the bound exists.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// stubbornTool is the non-cooperating tool: no entry check, no select on
// ctx.Done(), side effect committed only when the test releases it.
type stubbornTool struct {
	entered chan struct{}
	release chan struct{}
	ran     chan struct{}
	once    sync.Once
	text    string
}

func (t *stubbornTool) Name() string        { return "stubborn" }
func (t *stubbornTool) Description() string { return "blocks until the test lets go" }
func (t *stubbornTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (t *stubbornTool) Execute(_ context.Context, _ json.RawMessage) (tool.Result, error) {
	t.once.Do(func() { close(t.entered) })
	<-t.release // deliberately NOT ctx.Done(): this is the tool that will not stop
	close(t.ran)
	text := t.text
	if text == "" {
		text = "committed after the turn was cancelled"
	}
	return tool.Result{Text: text}, nil
}

func stubbornScript(id string) fakeScript {
	return fakeScript{events: []ai.Event{
		{Type: ai.EventStart},
		{Type: ai.EventToolcallStart, ToolCallID: id, ToolName: "stubborn", StreamIndex: 0},
		{Type: ai.EventToolcallEnd, StreamIndex: 0, PartialJSON: `{}`},
		ai.Donef(ai.StopReasonStop, nil, &ai.Message{
			Role:       ai.RoleAssistant,
			Content:    []ai.Block{ai.ToolCallBlock{ID: id, Name: "stubborn", Arguments: json.RawMessage(`{}`), StreamIndex: 0}},
			StopReason: ai.StopReasonStop,
		}),
	}}
}

func quietScript(text string) fakeScript {
	return fakeScript{events: []ai.Event{
		{Type: ai.EventTextStart},
		{Type: ai.EventTextDelta, Delta: text},
		{Type: ai.EventDone, StopReason: ai.StopReasonStop,
			Message: &ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: text}}, StopReason: ai.StopReasonStop}},
	}}
}

func stubbornAgent(stub tool.Tool, p *fakeProvider, grace time.Duration, results *[]*ai.Message) *Agent {
	reg := tool.NewRegistry()
	reg.Register(stub)
	return &Agent{
		Provider:    p,
		Tools:       reg,
		CancelGrace: grace,
		Hooks: TurnHooksFunc{
			OnToolResultMsgF: func(m *ai.Message) { *results = append(*results, m) },
		},
	}
}

func userTurn() []ai.Message {
	return []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}}
}

func TestCancelledTurnDoesNotWaitOnAStubbornTool(t *testing.T) {
	stub := &stubbornTool{entered: make(chan struct{}), release: make(chan struct{}), ran: make(chan struct{})}
	var results []*ai.Message
	a := stubbornAgent(stub, &fakeProvider{calls: []fakeScript{stubbornScript("c1"), quietScript("never reached")}}, 100*time.Millisecond, &results)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.Run(ctx, "sys", userTurn())
		done <- err
	}()
	waitChan(t, "the stubborn tool to start", stub.entered)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled turn is still waiting on a tool that ignores cancellation")
	}
	if len(results) != 1 {
		t.Fatalf("tool result messages = %d, want one", len(results))
	}
	text := results[0].Text()
	if !strings.Contains(text, "abandoned") || !strings.Contains(text, "100ms") {
		t.Fatalf("the abandoned call must be reported as abandoned with its bound, got %q", text)
	}
	if !strings.Contains(text, "stubborn") {
		t.Errorf("the report must name the tool: %q", text)
	}
	// The late result belongs to a turn that is already over: releasing the tool
	// must not put anything new on the transcript.
	close(stub.release)
	waitChan(t, "the abandoned tool to finish", stub.ran)
	if len(results) != 1 {
		t.Fatalf("an abandoned tool's late result leaked into the turn: %d messages", len(results))
	}
}

func TestCancelledTurnKeepsAToolResultThatArrivedInTime(t *testing.T) {
	// The bound must not discard work that finished: a tool that returns inside
	// the grace period reports its real result, not an abandonment.
	stub := &stubbornTool{
		entered: make(chan struct{}), release: make(chan struct{}), ran: make(chan struct{}),
		text: "finished quickly, honestly",
	}
	var results []*ai.Message
	a := stubbornAgent(stub, &fakeProvider{calls: []fakeScript{stubbornScript("c1"), quietScript("never reached")}}, 3*time.Second, &results)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.Run(ctx, "sys", userTurn())
		done <- err
	}()
	waitChan(t, "the tool to start", stub.entered)
	cancel()
	close(stub.release) // it lands within the grace, just after the cancel

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run never returned")
	}
	if len(results) != 1 {
		t.Fatalf("tool result messages = %d", len(results))
	}
	if got := results[0].Text(); !strings.Contains(got, "finished quickly, honestly") {
		t.Fatalf("a result that arrived within the grace must be kept, got %q", got)
	}
}
