package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/config"
)

// TestComputerToolWiredThroughRegistry is the cmd-seam smoke test for M15
// #65: newToolRegistry registers the computer tool, the shipped default
// refuses every op, and settings.computer.enabled flips that gate (the
// refusal becomes a platform-level outcome instead).
func TestComputerToolWiredThroughRegistry(t *testing.T) {
	ctx := context.Background()

	// Default: settings nil (pre-main callers) and an unconfigured
	// Settings both leave desktop control off.
	for _, settings := range []*config.Settings{nil, {}} {
		reg := newToolRegistry(t.TempDir(), nil, "p", "m", settings, nil, nil)
		ct, ok := reg.Get("computer")
		if !ok {
			t.Fatal("computer tool not registered")
		}
		if ct.Name() != "computer" {
			t.Fatalf("registered tool name = %q", ct.Name())
		}
		res, err := ct.Execute(ctx, json.RawMessage(`{"op":"window"}`))
		if err != nil {
			t.Fatalf("window: harness error: %v", err)
		}
		if !res.IsError || !strings.Contains(res.Text, "computer.enabled") {
			t.Fatalf("default must refuse with the enable hint: %+v", res)
		}
	}

	// Enabled through the settings layer the registry is handed.
	on := true
	settings := &config.Settings{Computer: config.ComputerSettings{Enabled: &on}}
	reg := newToolRegistry(t.TempDir(), nil, "p", "m", settings, nil, nil)
	ct, ok := reg.Get("computer")
	if !ok {
		t.Fatal("computer tool not registered")
	}
	res, err := ct.Execute(ctx, json.RawMessage(`{"op":"window"}`))
	if err != nil {
		t.Fatalf("window: harness error: %v", err)
	}
	if strings.Contains(res.Text, "computer is disabled") {
		t.Fatalf("computer.enabled: true did not open the gate: %+v", res)
	}
}

// TestComputerToolIsParentOnly pins the deliberate scope: desktop control
// reaches the parent, not a subagent's tool list (a scoped child must not
// drive the developer's machine).
func TestComputerToolIsParentOnly(t *testing.T) {
	reg := newToolRegistry(t.TempDir(), nil, "p", "m", nil, nil, nil)
	tt, ok := reg.Get("task")
	if !ok {
		t.Fatal("task tool not registered")
	}
	task, ok := tt.(*agent.TaskTool)
	if !ok {
		t.Fatalf("task tool is %T", tt)
	}
	for _, child := range task.ChildTools {
		if child.Name() == "computer" {
			t.Fatal("a scoped subagent must not drive the developer's desktop")
		}
	}
}
