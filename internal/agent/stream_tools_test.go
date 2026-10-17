package agent

import (
	"context"
	"encoding/json"
	"os"
	"sync/atomic"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

type streamReadTool struct {
	calls atomic.Int32
}

func (*streamReadTool) Name() string        { return "read" }
func (*streamReadTool) Description() string { return "read test double" }
func (*streamReadTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)
}
func (s *streamReadTool) Execute(_ context.Context, args json.RawMessage) (tool.Result, error) {
	s.calls.Add(1)
	return tool.Result{Text: "read result"}, nil
}

func TestStreamingReadOnlyToolReused(t *testing.T) {
	os.Setenv("XDEV_STREAM_TOOLS", "1")
	defer os.Unsetenv("XDEV_STREAM_TOOLS")

	rt := &streamReadTool{}
	reg := tool.NewRegistry()
	reg.Register(rt)

	p := &fakeProvider{calls: []fakeScript{
		{events: toolCallEvents("read", `{"path":"x.txt"}`)},
		{events: doneEvents("done")},
	}}
	ag := &Agent{Provider: p, Tools: reg, Model: "m", MaxTurns: 3, StreamingToolExecution: true}
	_, err := ag.Run(context.Background(), "sys", []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "read x"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rt.calls.Load() != 1 {
		t.Fatalf("read tool executed %d times, want 1", rt.calls.Load())
	}
}
