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

// childScripts scripts a child that reads a file on one turn and yields on
// the next — the real shape, since a child dispatches its tool calls a turn
// at a time.
func childScripts(t *testing.T, yieldArgs string) []fakeScript {
	t.Helper()
	read := []ai.Event{
		{Type: ai.EventStart},
		{Type: ai.EventToolcallStart, ToolCallID: "c1", ToolName: "spy", StreamIndex: 0},
		{Type: ai.EventToolcallDelta, StreamIndex: 0, PartialJSON: `{"path":"parser.go"}`},
		{Type: ai.EventToolcallEnd, StreamIndex: 0, PartialJSON: `{"path":"parser.go"}`},
		ai.Donef(ai.StopReasonStop, nil, &ai.Message{
			Role: ai.RoleAssistant,
			Content: []ai.Block{
				ai.ToolCallBlock{ID: "c1", Name: "spy", Arguments: json.RawMessage(`{"path":"parser.go"}`), StreamIndex: 0},
			},
			StopReason: ai.StopReasonStop,
		}),
	}
	return []fakeScript{{events: read}, {events: yieldEvents(yieldArgs)}}
}

// TestSpawnChildEmitsProgress pins the user-facing event seam: a child run
// reports start → one event per tool call → end, in order. The point is the
// HUMAN's view of the child (the TUI renders these), which is a different
// channel from the parent's context — the return value must be untouched.
func TestSpawnChildEmitsProgress(t *testing.T) {
	var mu sync.Mutex
	var got []SubagentEvent
	spy := &progressSpy{}
	p := &fakeProvider{calls: childScripts(t, `{"result":"done"}`)}
	res, err := SpawnChild(context.Background(), SubagentSpec{
		Name: "reader", Prompt: "read the parser", Provider: p, Model: "m",
		Tools: []tool.Tool{spy},
		OnEvent: func(ev SubagentEvent) {
			mu.Lock()
			got = append(got, ev)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "yielded" || res.Text != "done" {
		t.Fatalf("the parent-visible result changed: %+v", res)
	}
	spy.mu.Lock()
	ran := spy.ran
	spy.mu.Unlock()
	if len(ran) != 1 {
		t.Fatalf("child tool calls = %d, want 1", len(ran))
	}

	mu.Lock()
	defer mu.Unlock()
	kinds := make([]SubagentEventKind, 0, len(got))
	for _, ev := range got {
		kinds = append(kinds, ev.Kind)
	}
	want := []SubagentEventKind{SubagentStart, SubagentTool, SubagentEnd}
	if len(kinds) != len(want) {
		t.Fatalf("events = %v, want %v", kinds, want)
	}
	for i, k := range want {
		if kinds[i] != k {
			t.Fatalf("events = %v, want %v", kinds, want)
		}
	}
	for _, ev := range got {
		if ev.Label != "reader" {
			t.Fatalf("event %+v carries no child label", ev)
		}
	}
	if got[0].Model != "m" {
		t.Fatalf("start event = %+v, want the child model", got[0])
	}
	// `spy` is a child tool the user has never heard of: the row has to say
	// what it is. `yield` is the child's own handoff machinery, not work.
	if got[1].Tool != "spy" {
		t.Fatalf("tool event = %+v, want the child's tool call", got[1])
	}
	// The row says what the child is doing, and this package does not decide
	// that: it hands over the child's own arguments and the display side
	// runs the naming-argument precedence it already owns.
	if !strings.Contains(string(got[1].Args), "parser.go") {
		t.Fatalf("tool event args = %s, want the child's own arguments", got[1].Args)
	}
	if got[1].Status != "ok" {
		t.Fatalf("tool event status = %q, want ok", got[1].Status)
	}
	if got[2].Status != "yielded" {
		t.Fatalf("end event status = %q, want the child's handoff status", got[2].Status)
	}
}

// A batch is the shape a user watches with the least patience — up to 8
// children at once — so every one of them has to be nameable on the row the
// user is looking at. A batch item that carries neither `name` nor `agent`
// falls back to its position, the way its own report section already does.
func TestTaskToolBatchNamesEveryChild(t *testing.T) {
	// A provider that answers every child the same way: three children,
	// each named by a different fallback, and one `name` for the model
	// that passes all three.
	p := &fakeProvider{calls: []fakeScript{
		{events: yieldEvents(`{"result":"done"}`)},
		{events: yieldEvents(`{"result":"done"}`)},
		{events: yieldEvents(`{"result":"done"}`)},
	}}
	var mu sync.Mutex
	labels := map[string]int{}
	tt := &TaskTool{
		Provider: p, Model: "m", ChildTools: []tool.Tool{echoTool{}},
		OnEvent: func(ev SubagentEvent) {
			if ev.Kind != SubagentStart {
				return
			}
			mu.Lock()
			labels[ev.Label]++
			mu.Unlock()
		},
	}
	args := `{"context":"shared","tasks":[` +
		`{"name":"reader","task":"read a.go"},` +
		`{"task":"count callers of B"},` +
		`{"task":"find the TODO in C"}]}`
	res, err := tt.Execute(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("batch failed: %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(labels) != 3 || labels["reader"] != 1 {
		t.Fatalf("started labels = %v, want one named child per batch item", labels)
	}
	if labels["task #2"] != 1 || labels["task #3"] != 1 {
		t.Fatalf("an unnamed item fell back to nothing: %v", labels)
	}
}

// progressSpy is a child tool that answers anything and remembers the naming
// argument it was given, so the emitted event can be checked against a real
// call rather than a scripted expectation.
type progressSpy struct {
	mu  sync.Mutex
	ran []string
}

func (s *progressSpy) Name() string        { return "spy" }
func (s *progressSpy) Description() string { return "spy tool" }
func (s *progressSpy) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)
}
func (s *progressSpy) Execute(_ context.Context, args json.RawMessage) (tool.Result, error) {
	s.mu.Lock()
	s.ran = append(s.ran, string(args))
	s.mu.Unlock()
	return tool.Result{Text: "ok"}, nil
}
