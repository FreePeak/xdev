package tui

import (
	"strings"
	"testing"
	"time"
)

// subScreen runs the transcript through a simulation screen and returns the
// painted text, so a row assertion is about what a person would see.
func subScreen(t *testing.T) (*App, func() string) {
	t.Helper()
	app, scr := newTestApp(t, 100, 30)
	return app, func() string { app.draw(); return screenText(scr) }
}

func TestSubagentRowShowsEachChild(t *testing.T) {
	app, text := subScreen(t)
	app.AddToolBlock("c1", "task", `{"name":"reader","prompt":"read parser.go"}`)
	app.AddTaskChild("c1", "reader", "", "m")
	app.UpdateTaskChild("c1", "reader", "read", `{"path":"internal/tui/app.go"}`, "ok")
	app.AddTaskChild("c1", "writer", "", "m")

	out := text()
	// The parent row keeps saying what the model called.
	if !strings.Contains(out, "task") {
		t.Fatalf("no task row painted:\n%s", out)
	}
	// Each child is a named row, and the child's own last action is what the
	// row says — the naming argument is toolDetail's job, so "read · path".
	for _, want := range []string{"reader", "read", "internal/tui/app.go", "writer"} {
		if !strings.Contains(out, want) {
			t.Fatalf("child row missing %q:\n%s", want, out)
		}
	}
}

// A running child takes the parent's spinning frame; a settled one keeps the
// dim `⎿` tick. That is what makes "which of these is still going" a glance
// down the rows instead of a read of the elapsed clocks — and the child row
// still hangs under the call row rather than reading as a sibling call.
func TestSubagentRowUsesTheSameGlyphAsTheCallRow(t *testing.T) {
	app, text := subScreen(t)
	app.AddToolBlock("c1", "task", `{"prompt":"x"}`)
	app.AddTaskChild("c1", "reader", "", "m")
	out := text()
	// The call row is the anchor: the child row hangs below it.
	call := strings.Index(out, "task")
	child := strings.Index(out, "reader")
	if call < 0 || child < call {
		t.Fatalf("the child row is not under the call row:\n%s", out)
	}
	frames := app.th.SpinnerFrames()
	running := false
	for _, f := range frames {
		if strings.Contains(out, "  "+f+" reader") {
			running = true
		}
	}
	if !running {
		t.Fatalf("a running child painted no spinner frame:\n%s", out)
	}
	if !strings.Contains(out, "  ◈ task") && !strings.Contains(out, "  "+frames[0]+" task") {
		t.Fatalf("the call row lost its state bullet:\n%s", out)
	}

	// Settled: the tick comes back and the row stops spinning.
	app.FinishTaskChild("c1", "reader", "yielded", 2*time.Minute)
	if out = text(); !strings.Contains(out, "⎿ reader") {
		t.Fatalf("a settled child lost its tick:\n%s", out)
	}
}

func TestSubagentBatchShowsTheOverflowCount(t *testing.T) {
	app, text := subScreen(t)
	app.AddToolBlock("c1", "task", `{"context":"shared","tasks":[]}`)
	for _, label := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		app.AddTaskChild("c1", label, "", "m")
	}
	out := text()
	if !strings.Contains(out, "+5 more running") {
		t.Fatalf("an 8-child batch hid the overflow silently:\n%s", out)
	}
	for _, label := range []string{"a", "b", "c"} {
		if !strings.Contains(out, label) {
			t.Fatalf("the first three children should be visible, %q missing:\n%s", label, out)
		}
	}
	// Only 3 of 8 painted: a batch must not take over the screen.
	for _, label := range []string{"d", "e", "f", "g", "h"} {
		if strings.Contains(out, " "+label+" ·") {
			t.Fatalf("child %q painted past the cap:\n%s", label, out)
		}
	}
}

// A settled child stops saying "running" — the row is the only place a user
// sees that one child is done while its siblings keep going.
func TestSubagentSettledChildIsNotRunning(t *testing.T) {
	app, _ := subScreen(t)
	app.AddToolBlock("c1", "task", `{"prompt":"x"}`)
	app.AddTaskChild("c1", "reader", "", "m")
	app.AddTaskChild("c1", "writer", "", "m")
	app.FinishTaskChild("c1", "reader", "yielded", 2*time.Minute)
	app.UpdateTaskChild("c1", "writer", "bash", `{"command":"go test ./..."}`, "ok")

	rows, _ := runningTask(t, app, "c1").subVisible()
	if rows[0].Status != "yielded" || rows[1].Status != "ok" {
		t.Fatalf("statuses = %q/%q, want yielded/ok", rows[0].Status, rows[1].Status)
	}
}

func TestSubagentRowSurvivesAnEmptyCall(t *testing.T) {
	app, text := subScreen(t)
	app.AddToolBlock("c1", "read", `{"path":"a.go"}`)
	out := text()
	if strings.Contains(out, "⎿") {
		t.Fatalf("a read call grew a child row:\n%s", out)
	}
}
