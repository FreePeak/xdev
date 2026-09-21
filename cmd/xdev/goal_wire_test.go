package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// registerGoal is the seam every run mode uses to put the goal tool on a
// registry: the tool carries the session-scoped state, so it is registered
// where the session is.
func registerGoal(reg *tool.Registry) *agent.GoalState {
	gs := agent.NewGoalState(nil)
	reg.Register(&agent.GoalTool{Goals: gs})
	return gs
}

// TestGoalToolWiredThroughRegistry is the cmd-seam smoke test for M11 #40:
// a run mode registers the goal tool next to its session, wireTaskParent binds
// it to that session, and tool calls persist goal_updated entries — and a
// session switch rebinds to the new session's (empty) goal.
func TestGoalToolWiredThroughRegistry(t *testing.T) {
	reg := newToolRegistry(t.TempDir(), nil, "p", "m", nil, nil, nil)
	// The shared builder deliberately carries no goal: the tool's state must
	// be the one bound to the live session, so it is registered per run mode.
	if _, ok := reg.Get(agent.GoalToolName); ok {
		t.Fatal("newToolRegistry must not register the goal tool")
	}
	gs := registerGoal(reg)
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

// /goal <objective> must work in a TUI session: the command reads the goal
// state through the registry (agent.GoalStateOf), and the user-reported defect
// was the TUI having no goal tool at all, so every /goal answered "goal not
// wired". This pins the registration+bind contract the TUI relies on.
func TestTUIGoalToolIsRegisteredAndBound(t *testing.T) {
	reg := newToolRegistry(t.TempDir(), nil, "p", "m", nil, nil, nil)
	gs := registerGoal(reg)
	store := session.OpenMem("/proj", "goal seam")
	t.Cleanup(func() { _ = store.Close() })
	wireTaskParent(reg, store)

	if got := agent.GoalStateOf(reg); got != gs {
		t.Fatal("the TUI's /goal cannot reach the tool's state")
	}
	// A TUI session opens with no goal — unlike print mode, whose placeholder
	// keeps a long run going (#387): an active goal is what /vibe refuses to
	// enter over, and the objective is the user's to name.
	if v, ok := gs.View(); ok {
		t.Fatalf("a fresh session must have no goal, got %+v", v)
	}
	if _, err := gs.Create("land the exporter", 0); err != nil {
		t.Fatalf("/goal <objective> on a fresh session: %v", err)
	}
	if v, _ := gs.View(); v.Objective != "land the exporter" || v.Status != agent.GoalActive {
		t.Fatalf("goal = %+v", v)
	}
}
