package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

func TestPlanModeDeniesMutationEndToEnd(t *testing.T) {
	// Turn 1: the model tries write → denied with the plan-mode message.
	// Turn 2: the model proposes → accepted (no reviewer wired).
	p := &fakeProvider{calls: []fakeScript{
		{events: toolCallEvents("write", `{"path":"x.txt","content":"y"}`)},
		{events: toolCallEvents(ProposeToolName, `{"plan":"1. write x.txt"}`)},
		{events: doneEvents("starting implementation")},
	}}
	reg := tool.NewRegistry()
	reg.Register(tool.NewWriteTool())
	pm := &PlanMode{Active: true}
	pm.Propose = &proposeTool{pm: pm}
	ag := &Agent{Provider: p, Tools: reg, Model: "m", MaxTurns: 5, PlanMode: pm}

	_, err := ag.Run(context.Background(), "sys", []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "plan writing x.txt"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The write never happened (fake write tool would have created it);
	// its denial text reached the model as toolResult.
	req2 := p.gotReqs[1]
	denied := false
	for _, m := range req2.Messages {
		if m.Role == ai.RoleToolResult && m.IsError {
			for _, b := range m.Content {
				if tb, ok := b.(ai.TextBlock); ok && strings.Contains(tb.Text, "plan mode") {
					denied = true
				}
			}
		}
	}
	if !denied {
		t.Fatal("plan-mode denial did not reach the model")
	}
	// After the accepted propose, plan mode is off and the next turn's
	// system prompt has no plan-mode reminder; more importantly the
	// sub-state cleared.
	if pm.Active {
		t.Fatal("propose did not clear plan mode")
	}
}

func TestPlanModeReviewerRejectsThenAccepts(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: toolCallEvents(ProposeToolName, `{"plan":"first draft"}`)},
		{events: toolCallEvents(ProposeToolName, `{"plan":"revised draft"}`)},
		{events: doneEvents("implementing")},
	}}
	reg := tool.NewRegistry()
	pm := &PlanMode{Active: true}
	calls := 0
	pm.Propose = &proposeTool{pm: pm, OnPropose: func(_ context.Context, plan string) (bool, string) {
		calls++
		if plan == "revised draft" {
			return true, "approved with note"
		}
		return false, "needs more detail on verification"
	}}
	ag := &Agent{Provider: p, Tools: reg, Model: "m", MaxTurns: 5, PlanMode: pm}
	_, err := ag.Run(context.Background(), "sys", []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "make a plan"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("reviewer saw %d proposals, want 2", calls)
	}
	if pm.Active {
		t.Fatal("accepted proposal must clear plan mode")
	}
	// The rejection note reached the model.
	found := false
	for _, req := range p.gotReqs {
		for _, m := range req.Messages {
			if m.Role == ai.RoleToolResult {
				for _, b := range m.Content {
					if tb, ok := b.(ai.TextBlock); ok && strings.Contains(tb.Text, "needs more detail") {
						found = true
					}
				}
			}
		}
	}
	if !found {
		t.Fatal("rejection note did not reach the model")
	}
}

func TestPlanModeReadOnlyToolsPass(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: toolCallEvents("read", `{"path":"planmode.go"}`)},
		{events: doneEvents("researched")},
	}}
	reg := tool.NewRegistry()
	reg.Register(tool.NewReadTool())
	pm := &PlanMode{Active: true}
	ag := &Agent{Provider: p, Tools: reg, Model: "m", MaxTurns: 3, PlanMode: pm}
	_, err := ag.Run(context.Background(), "sys", []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "read the file"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The read executed (not denied): its result carries the file content
	// or a read error, but never a plan-mode denial.
	req2 := p.gotReqs[1]
	for _, m := range req2.Messages {
		if m.Role == ai.RoleToolResult {
			for _, b := range m.Content {
				if tb, ok := b.(ai.TextBlock); ok && strings.Contains(tb.Text, "plan mode: read is") {
					t.Fatal("read-only tool was denied in plan mode")
				}
			}
		}
	}
}

func TestProposeToolParameters(t *testing.T) {
	pt := &proposeTool{pm: &PlanMode{}}
	params := json.RawMessage(pt.Parameters())
	var doc struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal(params, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Required) != 1 || doc.Required[0] != "plan" {
		t.Fatalf("required = %v", doc.Required)
	}
}

// The propose def is exposed exactly while plan mode is live.
func TestToolDefsExposeProposeOnlyWhileActive(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(tool.NewReadTool())
	pm := &PlanMode{Active: true, Propose: &proposeTool{pm: &PlanMode{}}}
	ag := &Agent{Tools: reg, PlanMode: pm}
	has := func(defs []ai.ToolDef) bool {
		for _, d := range defs {
			if d.Name == ProposeToolName {
				return true
			}
		}
		return false
	}
	if !has(ag.toolDefs()) {
		t.Fatal("active plan mode must expose propose")
	}
	pm.Active = false
	if has(ag.toolDefs()) {
		t.Fatal("normal mode must not expose propose")
	}
}
