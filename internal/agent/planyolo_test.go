package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// --plan-yolo pre-approves the FIRST proposal: the reviewer still fires,
// but its reject cannot block the acceptance; later proposals take the
// reviewer path again.
func TestPlanYoloAutoApprovesFirstPropose(t *testing.T) {
	reviewed := 0
	pm := &PlanMode{Active: true, Yolo: true}
	pm.Propose = NewProposeTool(pm, func(context.Context, string) (bool, string) {
		reviewed++
		return false, "the user must review this"
	})
	pt := pm.Propose.(*proposeTool)

	res, err := pt.Execute(context.Background(), json.RawMessage(`{"plan":"1. patch planmode.go"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(res.Text, "--plan-yolo") {
		t.Fatalf("first proposal = %q (IsError=%v), want the yolo acceptance", res.Text, res.IsError)
	}
	if pm.Active {
		t.Fatal("yolo acceptance left plan mode active")
	}
	if reviewed != 1 {
		t.Fatalf("reviewer called %d times, want 1", reviewed)
	}
	if pm.Pending != "" {
		t.Fatalf("accepted proposal not consumed: %q", pm.Pending)
	}

	// The auto-approval is one-shot: a later proposal goes through the
	// reviewer and its reject is honored.
	pm.Active = true
	res, err = pt.Execute(context.Background(), json.RawMessage(`{"plan":"second draft"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "rejected") || !pm.Active {
		t.Fatalf("second proposal = %q (active=%v), want the reviewer's reject", res.Text, pm.Active)
	}
}

// E2E: plan-yolo accepts the first proposal in a live run and hands the
// run to the execution model, reusing the failover switch machinery.
func TestPlanYoloSwitchesToExecutionModel(t *testing.T) {
	plan := &fakeProvider{calls: []fakeScript{
		{events: toolCallEvents(ProposeToolName, `{"plan":"1. write x.txt"}`)},
	}}
	reg := tool.NewRegistry()
	reg.Register(tool.NewReadTool())
	exec := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{
			{Type: ai.EventStart, Provider: "fake", API: "fake-api", Model: "exec-model"},
			{Type: ai.EventTextDelta, Delta: "implementing"},
			ai.Donef(ai.StopReasonStop, nil, &ai.Message{Role: ai.RoleAssistant,
				Content: []ai.Block{ai.TextBlock{Text: "implementing"}}, StopReason: ai.StopReasonStop}),
		}},
	}}
	pm := &PlanMode{Active: true, Yolo: true}
	// A TUI-like reviewer holds every proposal; plan-yolo must override it
	// on the first one only.
	pm.Propose = NewProposeTool(pm, func(context.Context, string) (bool, string) {
		return false, "awaiting user review"
	})
	ag := &Agent{Provider: plan, Tools: reg, Model: "plan-model", MaxTurns: 3, PlanMode: pm}
	pm.OnAccept = func() { ag.SwitchToModel(FailoverTarget{Provider: exec, Model: "exec-model"}, "plan-yolo") }

	_, err := ag.Run(context.Background(), "sys", []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "plan the change"}}},
	})
	if err != nil && !strings.Contains(err.Error(), "script exhausted") {
		t.Fatal(err)
	}
	if pm.Active {
		t.Fatal("plan mode still active after the yolo acceptance")
	}
	if ag.Model != "exec-model" {
		t.Fatalf("Model = %q, want exec-model", ag.Model)
	}
	// The switch is a failover-chain swap: the execution provider serves
	// the turn after the proposal, the planning provider never sees it.
	if len(plan.gotReqs) != 1 {
		t.Fatalf("planning provider saw %d requests, want 1", len(plan.gotReqs))
	}
	if len(exec.gotReqs) != 1 || exec.gotReqs[0].Model != "exec-model" {
		t.Fatalf("execution provider requests = %d (model %q), want 1 on exec-model", len(exec.gotReqs), exec.gotReqs[0].Model)
	}
}

// xd://propose serves the pending plan and refuses unknown read devices.
func TestXDProposeDeviceReadsPendingPlan(t *testing.T) {
	pm := &PlanMode{}
	if got, err := pm.DeviceRead("xd://propose"); err != nil || !strings.Contains(got, "no pending") {
		t.Fatalf("empty device read = %q, %v", got, err)
	}
	pm.Pending = "1. touch planmode.go\n2. run the agent tests"
	got, err := pm.DeviceRead("xd://propose")
	if err != nil {
		t.Fatal(err)
	}
	if got != pm.Pending {
		t.Fatalf("device read = %q, want the pending plan", got)
	}
	if _, err := pm.DeviceRead("xd://bogus"); err == nil {
		t.Fatal("unknown xd:// read device must error")
	}
}

// xd://resolve accepts and xd://reject revises; both consume the pending
// proposal, and resolve fires the one-shot acceptance hook.
func TestXDResolveAndRejectDevicesFinalize(t *testing.T) {
	pm := &PlanMode{Active: true}
	if _, err := pm.ResolveDevice("xd://resolve", "go"); err == nil {
		t.Fatal("resolve without a pending proposal must error")
	}

	pm.Pending = "plan A"
	out, err := pm.RejectDevice("xd://reject", "too risky\nand out of scope")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "too risky") || strings.Contains(out, "out of scope") {
		t.Fatalf("reject note = %q, want the first line only", out)
	}
	if !pm.Active {
		t.Fatal("reject must leave plan mode on so the model revises")
	}
	if pm.Pending != "" {
		t.Fatal("reject did not consume the pending proposal")
	}

	accepts := 0
	pm.OnAccept = func() { accepts++ }
	pm.Pending = "plan A revised"
	out, err = pm.ResolveDevice("xd://resolve", "ship it")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ship it") {
		t.Fatalf("resolve note = %q, want the reason echoed", out)
	}
	if pm.Active || accepts != 1 {
		t.Fatalf("resolve active=%v accepts=%d, want false/1", pm.Active, accepts)
	}
	if _, err := pm.ResolveDevice("xd://resolve", "again"); err == nil {
		t.Fatal("second resolve must find nothing pending")
	}
}
