package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// E2E: a prewalk agent runs on a scripted provider. Turn 1: model calls
// write. Turn 2: the StreamRequest.Model must be the prewalk target.
func TestPrewalkSwitchesAfterFirstEdit(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: toolCallEvents("write", `{"path":"a.txt","content":"x"}`)},
	}}
	reg := tool.NewRegistry()
	reg.Register(tool.NewWriteTool())
	tgt := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{
			{Type: ai.EventStart, Provider: "fake", API: "fake-api", Model: "smol-model"},
			{Type: ai.EventTextDelta, Delta: "all done"},
			ai.Donef(ai.StopReasonStop, nil, &ai.Message{Role: ai.RoleAssistant,
				Content: []ai.Block{ai.TextBlock{Text: "all done"}}, StopReason: ai.StopReasonStop}),
		}},
	}}
	ag := &Agent{
		Provider: p, Tools: reg, Model: "big-model", MaxTurns: 3,
		Prewalk: &Prewalk{Target: FailoverTarget{Provider: tgt, Model: "smol-model"}},
	}
	_, err := ag.Run(context.Background(), "sys", []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "write the file"}}},
	})
	if err != nil && !strings.Contains(err.Error(), "script exhausted") {
		t.Fatal(err)
	}
	if !ag.prewalk.done {
		t.Fatal("prewalk did not switch after the write")
	}
	if ag.Model != "smol-model" {
		t.Fatalf("Model = %q, want smol-model", ag.Model)
	}
	// The first request goes to the primary on big-model; after the switch
	// the second goes to the target provider on smol-model.
	if len(p.gotReqs) != 1 {
		t.Fatalf("primary saw %d requests, want 1", len(p.gotReqs))
	}
	if p.gotReqs[0].Model != "big-model" {
		t.Fatalf("primary model = %q", p.gotReqs[0].Model)
	}
	if len(tgt.gotReqs) != 1 {
		t.Fatalf("target saw %d requests, want 1", len(tgt.gotReqs))
	}
	if tgt.gotReqs[0].Model != "smol-model" {
		t.Fatalf("target model = %q, want smol-model", tgt.gotReqs[0].Model)
	}
}

// Without a successful edit/write, prewalk must never fire — a read-only
// or failed-write run stays on the primary model.
func TestPrewalkStaysUnarmedWithoutMutation(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: toolCallEvents("read", `{}`)},
		{events: []ai.Event{
			{Type: ai.EventStart},
			ai.Donef(ai.StopReasonStop, nil, &ai.Message{Role: ai.RoleAssistant,
				Content: []ai.Block{ai.TextBlock{Text: "all read"}}, StopReason: ai.StopReasonStop}),
		}},
	}}
	reg := tool.NewRegistry()
	reg.Register(tool.NewReadTool())
	tgt := &fakeProvider{}
	ag := &Agent{
		Provider: p, Tools: reg, Model: "big-model", MaxTurns: 3,
		Prewalk: &Prewalk{Target: FailoverTarget{Provider: tgt, Model: "smol-model"}},
	}
	if _, err := ag.Run(context.Background(), "sys", []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "read a file"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if ag.prewalk.done || len(tgt.gotReqs) != 0 {
		t.Fatalf("prewalk fired without a mutation (done=%v target reqs=%d)", ag.prewalk.done, len(tgt.gotReqs))
	}
}
