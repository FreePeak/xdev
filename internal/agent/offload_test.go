package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

// fakeOffloader records every seam call; fail=true models a storage error.
type fakeOffloader struct {
	calls int
	fail  bool
}

func (o *fakeOffloader) Offload(toolName, callID, text string) (string, bool, error) {
	o.calls++
	if o.fail {
		return "", false, errors.New("disk full")
	}
	return fmt.Sprintf("[offloaded %d B of %s/%s]", len(text), toolName, callID), true, nil
}

// offloadAgent runs one scripted echo tool-call turn through an agent
// wired with the given offloader and returns the tool-result message.
func offloadAgent(t *testing.T, o ArtifactOffloader, payload string) *ai.Message {
	t.Helper()
	args, err := json.Marshal(map[string]string{"text": payload})
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{
			{Type: ai.EventStart},
			{Type: ai.EventToolcallStart, ToolCallID: "c1", ToolName: "echo", StreamIndex: 0},
			{Type: ai.EventToolcallDelta, StreamIndex: 0, PartialJSON: string(args)},
			{Type: ai.EventToolcallEnd, StreamIndex: 0},
			ai.Donef(ai.StopReasonStop, nil, &ai.Message{Role: ai.RoleAssistant,
				Content:    []ai.Block{ai.ToolCallBlock{ID: "c1", Name: "echo", Arguments: args, StreamIndex: 0}},
				StopReason: ai.StopReasonStop}),
		}},
		{events: []ai.Event{{Type: ai.EventTextStart}, textEvent("done"), doneEvent("done")}},
	}}
	a, _, results := runAgent(t, p)
	a.Offload = o
	if _, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(*results) != 1 {
		t.Fatalf("tool results = %d", len(*results))
	}
	return (*results)[0]
}

func TestOffloadSeamSpillsOverThresholdAndKeepsVerbatimOnError(t *testing.T) {
	big := strings.Repeat("x", OffloadThresholdBytes+1)

	// Over the gate: the stub replaces the bytes in the conversation.
	o := &fakeOffloader{}
	rm := offloadAgent(t, o, big)
	if o.calls != 1 {
		t.Fatalf("offloader calls = %d, want 1", o.calls)
	}
	if want := fmt.Sprintf("[offloaded %d B of echo/c1]", len(big)); !strings.HasPrefix(rm.Text(), want) {
		t.Fatalf("toolResult not replaced by the stub: %q", rm.Text()[:min(len(rm.Text()), 80)])
	}

	// A failed durable write keeps the bytes verbatim: a lost spill must
	// never silently cost the model its output (#115's data-loss rule).
	f := &fakeOffloader{fail: true}
	rm = offloadAgent(t, f, big)
	if f.calls != 1 {
		t.Fatalf("offloader calls = %d, want 1", f.calls)
	}
	if rm.Text() != big {
		t.Fatal("failed offload must retain the result verbatim")
	}

	// Under the gate: the seam is never consulted.
	s := &fakeOffloader{}
	rm = offloadAgent(t, s, "pong")
	if s.calls != 0 {
		t.Fatalf("offloader consulted for a %d-byte result", len("pong"))
	}
	if rm.Text() != "pong" {
		t.Fatalf("small result altered: %q", rm.Text())
	}
}
