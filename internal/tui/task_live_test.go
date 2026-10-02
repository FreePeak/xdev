package tui

import (
	"testing"
	"time"
)

// taskRow is a session with one running `task` call — what the host has
// between OnToolStart and OnToolEnd for a spawn.
func taskRow(t *testing.T) *App {
	t.Helper()
	app, _ := newTestApp(t, 100, 30)
	app.AddToolBlock("c1", "task", `{"name":"reader","prompt":"read parser.go"}`)
	return app
}

func TestTaskChildBindsToRunningTaskRow(t *testing.T) {
	app := taskRow(t)
	app.AddTaskChild("c1", "reader", "", "m")
	b := runningTask(t, app, "c1")
	if len(b.Sub) != 1 || b.Sub[0].Label != "reader" {
		t.Fatalf("children = %+v, want the reader on the running task row", b.Sub)
	}
	if b.Sub[0].Status != "running" {
		t.Fatalf("child status = %q, want running", b.Sub[0].Status)
	}
}

// Two `task` calls can be in flight in the same turn (MaxToolWorkers), so a
// child must land on ITS call's row and nowhere else — the same rule
// FinishTool follows with the call id.
func TestTaskChildRowsFollowTheirOwnCall(t *testing.T) {
	app := taskRow(t)
	app.AddTaskChild("c1", "reader", "", "m")
	app.AddToolBlock("c2", "task", `{"name":"writer","prompt":"write it"}`)
	app.AddTaskChild("c2", "writer", "", "m")

	first := runningTask(t, app, "c1")
	second := runningTask(t, app, "c2")
	if len(first.Sub) != 1 || first.Sub[0].Label != "reader" {
		t.Fatalf("first call got the wrong child: %+v", first.Sub)
	}
	if len(second.Sub) != 1 || second.Sub[0].Label != "writer" {
		t.Fatalf("second call got the wrong child: %+v", second.Sub)
	}
}

func TestTaskChildUpdateLandsOnItsOwnLabel(t *testing.T) {
	app := taskRow(t)
	app.AddTaskChild("c1", "reader", "", "m")
	app.AddTaskChild("c1", "writer", "", "m")
	app.UpdateTaskChild("c1", "writer", "read", `{"path":"b.go"}`, "ok")

	b := runningTask(t, app, "c1")
	if len(b.Sub) != 2 {
		t.Fatalf("children = %+v, want both", b.Sub)
	}
	if b.Sub[0].Tool != "" {
		t.Fatalf("an update leaked onto the wrong child: %+v", b.Sub[0])
	}
	got := b.Sub[1]
	if got.Tool != "read" || got.Status != "ok" || got.Args != `{"path":"b.go"}` {
		t.Fatalf("child = %+v, want the read that just landed", got)
	}
	if got.Calls != 1 {
		t.Fatalf("child calls = %d, want 1", got.Calls)
	}
}

func TestTaskChildSettles(t *testing.T) {
	app := taskRow(t)
	app.AddTaskChild("c1", "reader", "", "m")
	app.FinishTaskChild("c1", "reader", "yielded", 90*time.Second)
	c := runningTask(t, app, "c1").Sub[0]
	if c.Status != "yielded" || c.Dur != 90*time.Second {
		t.Fatalf("child = %+v, want the terminal status and wall time", c)
	}
}

// A batch runs up to 8 children at once. Painting every row would push the
// parent row itself off a normal terminal, so the PAINT keeps the first
// three (start order, so the rows do not jump while you read them) and says
// how many it dropped — while the block still knows every child, so a late
// settle lands on the right one.
func TestTaskBatchKeepsFirstThreeAndCountsTheRest(t *testing.T) {
	app := taskRow(t)
	labels := []string{"a", "b", "c", "d", "e"}
	for _, label := range labels {
		app.AddTaskChild("c1", label, "", "m")
	}
	b := runningTask(t, app, "c1")
	rows, more := b.subVisible()
	if len(rows) != subRowsMax || more != 2 {
		t.Fatalf("visible = %d rows + %d more, want %d + 2", len(rows), more, subRowsMax)
	}
	for i, want := range []string{"a", "b", "c"} {
		if rows[i].Label != want {
			t.Fatalf("row %d = %q, want %q (start order)", i, rows[i].Label, want)
		}
	}
	if len(b.Sub) != len(labels) {
		t.Fatalf("block kept %d children, want all %d: a settle has to find its own", len(b.Sub), len(labels))
	}
	// A dropped child is still real: its settle must find it.
	app.FinishTaskChild("c1", "e", "yielded", time.Second)
	if got := findSubLocked(b, "e"); got == nil || got.Status != "yielded" {
		t.Fatalf("a dropped child could not settle: %+v", b.Sub[len(b.Sub)-1])
	}
}

// A late event (a child settling after its call row closed) must not land on
// whatever call came next, and must not invent a row of its own.
func TestTaskChildAfterSettleIsDropped(t *testing.T) {
	app := taskRow(t)
	app.FinishTool("c1", "task", false, "yielded", ToolOutcome{Dur: "1s"})
	app.AddTaskChild("c1", "late", "", "m")
	app.AddToolBlock("c2", "bash", `{"command":"ls"}`)

	if b := runningTask(t, app, "c1"); b != nil {
		t.Fatalf("a settled call took a late child: %+v", b.Sub)
	}
	for _, b := range app.blocks {
		if len(b.Sub) > 0 {
			t.Fatalf("a late child landed on %q: %+v", b.ToolName, b.Sub)
		}
	}
}

func TestNonTaskToolIsUntouched(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.AddToolBlock("c9", "read", `{"path":"a.go"}`)
	app.AddTaskChild("c9", "reader", "", "m")
	app.UpdateTaskChild("c9", "reader", "read", `{}`, "ok")
	app.FinishTaskChild("c9", "reader", "yielded", time.Second)
	for _, b := range app.blocks {
		if len(b.Sub) > 0 {
			t.Fatalf("a read call grew subagent state: %+v", b.Sub)
		}
	}
}

// runningTask is the running `task` row for this call id, or nil.
func runningTask(t *testing.T, app *App, callID string) *Block {
	t.Helper()
	app.mu.Lock()
	defer app.mu.Unlock()
	for i := len(app.blocks) - 1; i >= 0; i-- {
		b := app.blocks[i]
		if b.Kind == KindTool && b.ToolName == taskToolName && b.Status == "running" && b.CallID == callID {
			return b
		}
	}
	return nil
}
