package tui

import (
	"strings"
	"testing"
)

// Goal implements CommandAPI for the fake used by the dispatch tests.
func (f *fakeAPI) Goal(args string) error {
	f.blocks = append(f.blocks, "goal "+args)
	return nil
}

// TestGoalCommandView pins /goal: the GoalOps seam renders the live goal and
// budget into the transcript; a nil seam degrades to a notice, never a panic.
func TestGoalCommandView(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)

	// Nil seam: the command is consumed and reports the missing wiring.
	if err := app.Goal(""); err == nil {
		t.Fatal("nil goalOps must error, not silently no-op")
	}
	if !dispatch(app, "/goal") {
		t.Fatal("/goal not consumed")
	}
	blocks := app.Blocks()
	last := blocks[len(blocks)-1]
	// The wording is the seam's own; the contract is that an unwired goal
	// surfaces as an error block rather than a silent no-op.
	if last.Kind != KindSystem || !strings.Contains(last.Text, "not wired") {
		t.Fatalf("nil-seam block = %+v", last)
	}

	// Wired seam: the view text lands as a system block.
	app.SetGoalOps(&GoalOps{View: func() string {
		return "goal: active\nobjective: ship goal mode\nbudget: 120/5000 tokens used (4880 remaining)"
	}})
	if !dispatch(app, "/goal") {
		t.Fatal("wired /goal not consumed")
	}
	blocks = app.Blocks()
	last = blocks[len(blocks)-1]
	if last.Kind != KindSystem || !strings.Contains(last.Text, "ship goal mode") || !strings.Contains(last.Text, "4880 remaining") {
		t.Fatalf("wired block = %+v", last)
	}
}
