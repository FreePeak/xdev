package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/tool"
)

// The xd:// proposal devices are registered in the real registry: the
// read tool serves the pending plan, and the write seam finalizes it
// (write xd://resolve / xd://reject — omp naming parity, #36).
func TestXDProposeDevicesInRegistry(t *testing.T) {
	pm := &agent.PlanMode{}
	pm.SetActive(true)
	pm.Publish("1. patch planmode.go\n2. run the tests")
	reg := newToolRegistry(t.TempDir(), nil, "p", "m", nil, nil, pm)

	rt, ok := reg.Get("read")
	if !ok {
		t.Fatal("read tool missing from the registry")
	}
	res, err := rt.Execute(context.Background(), json.RawMessage(`{"path":"xd://propose"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(res.Text, "patch planmode.go") {
		t.Fatalf("read xd://propose = %q (IsError=%v)", res.Text, res.IsError)
	}

	text, handled, err := tool.WriteURI("xd://resolve", "go ahead")
	if err != nil || !handled {
		t.Fatalf("write xd://resolve: handled=%v err=%v", handled, err)
	}
	if !strings.Contains(text, "plan approved") {
		t.Fatalf("resolve text = %q", text)
	}
	if pm.Active() || pm.Pending() != "" {
		t.Fatalf("resolve left active=%v pending=%q", pm.Active(), pm.Pending())
	}

	pm.SetActive(true)
	pm.Publish("plan B")
	if _, handled, err := tool.WriteURI("xd://reject", "revise step 2"); err != nil || !handled {
		t.Fatalf("write xd://reject: handled=%v err=%v", handled, err)
	}
	if !pm.Active() {
		t.Fatal("reject must leave plan mode on so the model revises")
	}
}

// The ask tool rides the shipped registry with the headless default sink.
func TestAskToolRegistered(t *testing.T) {
	reg := newToolRegistry(t.TempDir(), nil, "p", "m", nil, nil, &agent.PlanMode{})
	got, ok := reg.Get(tool.AskToolName)
	if !ok {
		t.Fatal("ask tool missing from the registry")
	}
	at, isAsk := got.(*tool.AskTool)
	if !isAsk {
		t.Fatalf("ask registry entry is %T", got)
	}
	if at.Sink != nil {
		t.Fatal("print-mode registry must leave the sink unset (headless policy)")
	}
	if at.Timeout <= 0 {
		t.Fatalf("ask timeout = %v, want the settings default", at.Timeout)
	}
}
