package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/session"
)

// TestGoalToolWiredThroughRegistry is the cmd-seam smoke test for M11 #40:
// newToolRegistry registers the goal tool, wireTaskParent binds it to the
// active session, and tool calls persist goal_updated entries — and a session
// switch rebinds to the new session's (empty) goal.
func TestGoalToolWiredThroughRegistry(t *testing.T) {
	reg := newToolRegistry(t.TempDir(), nil, "p", "m", nil, nil, nil)
	gt, ok := reg.Get(agent.GoalToolName)
	if !ok {
		t.Fatal("goal tool not registered")
	}
	store := session.OpenMem("/proj", "cmd goal smoke")
	t.Cleanup(func() { _ = store.Close() })
	wireTaskParent(reg, store)

	exec := func(args string) string {
		t.Helper()
		res, err := gt.Execute(context.Background(), json.RawMessage(args))
		if err != nil {
			t.Fatalf("goal %s: %v", args, err)
		}
		if res.IsError {
			t.Fatalf("goal %s: %s", args, res.Text)
		}
		return res.Text
	}
	if out := exec(`{"op":"create","objective":"wired through cmd","token_budget":1000}`); !strings.Contains(out, "active") {
		t.Fatalf("create = %q", out)
	}
	exec(`{"op":"evidence","note":"registry smoke"}`)
	if out := exec(`{"op":"complete"}`); !strings.Contains(out, "completed") {
		t.Fatalf("complete = %q", out)
	}

	var entries int
	for _, e := range store.Entries() {
		if _, ok := e.(*session.GoalUpdatedEntry); ok {
			entries++
		}
	}
	if entries != 3 {
		t.Fatalf("goal_updated entries = %d, want 3", entries)
	}

	// Session switch: the goal follows the active session, not the process.
	other := session.OpenMem("/proj", "other")
	t.Cleanup(func() { _ = other.Close() })
	wireTaskParent(reg, other)
	gs := agent.GoalStateOf(reg)
	if gs == nil {
		t.Fatal("goal state not reachable from the registry")
	}
	if v, ok := gs.View(); ok {
		t.Fatalf("session switch leaked the goal: %+v", v)
	}
	if got := gs.Describe(); !strings.Contains(got, "none") {
		t.Fatalf("Describe = %q", got)
	}
}
