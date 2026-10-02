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
	pm := &PlanMode{active: true}
	pm.propose = &proposeTool{pm: pm}
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
	if pm.active {
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
	pm := &PlanMode{active: true}
	calls := 0
	pm.propose = &proposeTool{pm: pm, OnPropose: func(_ context.Context, plan string) (bool, string) {
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
	if pm.active {
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
	pm := &PlanMode{active: true}
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
	pm := &PlanMode{active: true}
	pm.propose = &proposeTool{pm: &PlanMode{}}
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
	pm.SetActive(false)
	if has(ag.toolDefs()) {
		t.Fatal("normal mode must not expose propose")
	}
}

// The dock is only free because it is not polled: it rebuilds when a source says
// it moved. Publish, accept and reject are the three moves a proposal makes, and
// each must say so once — a missed one leaves a stale document on screen, an
// extra one is a repaint at frame rate by the back door.
func TestPlanModeInvalidateFiresOnEveryTransition(t *testing.T) {
	var fires int
	pm := &PlanMode{active: true}
	pm.SetInvalidate(func() { fires++ })
	pm.Publish("1. do the thing")
	if fires != 1 {
		t.Fatalf("publish: %d fires, want 1", fires)
	}
	if got := pm.Pending(); got != "1. do the thing" {
		t.Fatalf("pending %q", got)
	}
	if _, err := pm.Resolve(false, "revise it"); err != nil {
		t.Fatal(err)
	}
	if fires != 2 {
		t.Fatalf("reject: %d fires, want 2", fires)
	}
	if pm.Pending() != "" {
		t.Fatal("a rejected plan must leave nothing pending")
	}
	if !pm.Active() {
		t.Fatal("rejecting leaves plan mode on")
	}
	pm.Publish("v2")
	pm.SetActive(true)
	if _, err := pm.Resolve(true, ""); err != nil {
		t.Fatal(err)
	}
	if fires != 4 {
		t.Fatalf("accept: %d fires, want 4", fires)
	}
	if pm.Active() || pm.Pending() != "" {
		t.Fatal("accept resolves the proposal and leaves plan mode")
	}
	// The proposal path bumps too, including the one that holds for review.
	pm2 := &PlanMode{active: true}
	hits := 0
	pm2.SetInvalidate(func() { hits++ })
	_, _ = NewProposeTool(pm2, func(context.Context, string) (bool, string) {
		// Mid-call the human can already read the document.
		if hits == 0 {
			t.Error("propose must publish before the reviewer answers")
		}
		return false, "keep going"
	}).Execute(context.Background(), json.RawMessage(`{"plan":"1. ship it"}`))
	if hits != 1 {
		t.Fatalf("a held proposal fired %d bumps, want 1", hits)
	}
}

// The task list rides along with the plan it was written against: one read, the
// same snapshot the model sees, and no panic when the host has no todo tool.
func TestPlanModeTodoIsDisplayOnly(t *testing.T) {
	pm := &PlanMode{}
	if pm.Todo() != "" {
		t.Fatal("a mode with no list has nothing to show")
	}
	tt := tool.NewTodoTool()
	if _, err := tt.Execute(context.Background(), json.RawMessage(`{"op":"init","items":["ship it"]}`)); err != nil {
		t.Fatal(err)
	}
	pm.SetTodo(tt)
	if got := pm.Todo(); !strings.Contains(got, "TASKS · 0/1 done") || !strings.Contains(got, "ship it") {
		t.Fatalf("todo view %q", got)
	}
	var nilPM *PlanMode
	if nilPM.Todo() != "" {
		t.Fatal("a nil PlanMode must be unreadable, not fatal")
	}
}

// #420: undeclared tools fail closed in plan mode; ConcurrentOK is false.
func TestPlanModeUndeclaredToolDenied(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(undeclaredPlanTool{})
	pm := &PlanMode{active: true}
	call := ai.ToolCallBlock{ID: "1", Name: "synth_undeclared", Arguments: json.RawMessage(`{}`)}
	res, blocked := applyPlanMode(pm, call, reg)
	if !blocked || !res.IsError {
		t.Fatalf("undeclared tool must be denied: blocked=%v res=%+v", blocked, res)
	}
	if !strings.Contains(res.Text, "plan mode:") {
		t.Fatalf("denial text = %q", res.Text)
	}
	ttool, _ := reg.Get("synth_undeclared")
	if tool.ConcurrentOK(ttool) {
		t.Fatal("undeclared tool must not be ConcurrentOK")
	}
}

// #420: former planReadOnlyTools still pass via Caps, not the deleted map.
func TestPlanModeCapsAllowGrep(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(&tool.GrepTool{CWD: t.TempDir()})
	pm := &PlanMode{active: true}
	call := ai.ToolCallBlock{ID: "1", Name: "grep", Arguments: json.RawMessage(`{"pattern":"x"}`)}
	if _, blocked := applyPlanMode(pm, call, reg); blocked {
		t.Fatal("grep must pass plan mode via Caps")
	}
}

type undeclaredPlanTool struct{}

func (undeclaredPlanTool) Name() string                { return "synth_undeclared" }
func (undeclaredPlanTool) Description() string         { return "no caps" }
func (undeclaredPlanTool) Parameters() json.RawMessage { return json.RawMessage(`{}`) }
func (undeclaredPlanTool) Execute(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{Text: "ran"}, nil
}
