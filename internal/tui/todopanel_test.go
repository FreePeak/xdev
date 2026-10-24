package tui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/tool"
)

// todoStripFixture builds a three-phase list through the real tool, so the
// strip's rows come from state the model itself produced.
func todoStripFixture(t *testing.T, phases []map[string]any) []tool.TodoPhase {
	t.Helper()
	tt := tool.NewTodoTool()
	args, err := json.Marshal(map[string]any{"op": "init", "list": phases})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := tt.Execute(context.Background(), args); err != nil || res.IsError {
		t.Fatalf("init: %v %v", err, res.Text)
	}
	return tt.Snapshot()
}

// The policy, which is the whole feature: the ACTIVE phase is expanded with its
// open tasks, the phases after it are one-line rows with their progress, and a
// closed plan paints nothing at all. This is omp's sticky-HUD shape without its
// spine, and the reason the strip exists — a six-phase plan reads in one screen.
func TestTodoStripExpandsTheActivePhaseOnly(t *testing.T) {
	phases := todoStripFixture(t, []map[string]any{
		{"phase": "Foundation", "items": []string{"wire the registry", "write the tests"}},
		{"phase": "Render", "items": []string{"paint the strip", "cover it with a test"}},
		{"phase": "Polish", "items": []string{"document the policy"}},
	})
	// init auto-promotes the first pending task; move the work into phase two
	// so the expand decision is about the in-progress task, not list order.
	phases[0].Tasks[0].Status = tool.TodoCompleted
	phases[0].Tasks[1].Status = tool.TodoCompleted
	phases[1].Tasks[0].Status = tool.TodoInProgress

	rows := todoStripLines(phases, todoStripRowMax)
	if len(rows) == 0 {
		t.Fatal("an open plan must paint a strip")
	}
	joined := todoStripText(rows)
	if !strings.HasPrefix(rows[0].text, "TODO · 2/5") {
		t.Fatalf("header = %q, want the closed/total counts", rows[0].text)
	}
	// The active phase is expanded: its header plus its open task rows.
	if !strings.Contains(joined, "II. Render · 0/2") {
		t.Fatalf("the active phase is missing from the strip:\n%s", joined)
	}
	var taskRows []string
	for _, r := range rows {
		if r.kind == todoStripTask {
			taskRows = append(taskRows, r.text)
		}
	}
	// "paint the strip" is in progress, "cover it with a test" is pending: both
	// are open work in the active phase and both must show. The finished phase
	// one tasks must NOT: the strip is what is left to do.
	if len(taskRows) != 2 {
		t.Fatalf("active phase preview = %v, want its two open tasks", taskRows)
	}
	if !strings.Contains(strings.Join(taskRows, "\n"), "[/] paint the strip") {
		t.Fatalf("the in-progress task must lead the preview: %v", taskRows)
	}
	if strings.Contains(joined, "wire the registry") {
		t.Fatalf("a closed phase ahead of the active one must not spend the strip:\n%s", joined)
	}
	// The phase AFTER the active one is one line with its progress.
	if !strings.Contains(joined, "III. Polish · 0/1") {
		t.Fatalf("a following phase must be a one-line row:\n%s", joined)
	}
}

// Lifecycle: no open task, no strip. The one rule the surface has to keep on its
// own — nothing sets a timer, so the strip retires by construction.
func TestTodoStripRetiresWhenThePlanIsClosed(t *testing.T) {
	phases := todoStripFixture(t, []map[string]any{
		{"phase": "Foundation", "items": []string{"wire the registry"}},
	})
	if len(todoStripLines(nil, todoStripRowMax)) != 0 {
		t.Fatal("no list must paint nothing")
	}
	phases[0].Tasks[0].Status = tool.TodoCompleted
	if rows := todoStripLines(phases, todoStripRowMax); rows != nil {
		t.Fatalf("a closed plan must paint nothing, got %v", todoStripText(rows))
	}
	// Dropped counts as closed for the same reason: it is settled work.
	phases[0].Tasks[0].Status = tool.TodoDropped
	if rows := todoStripLines(phases, todoStripRowMax); rows != nil {
		t.Fatalf("a dropped plan must paint nothing, got %v", todoStripText(rows))
	}
}

// The height budget cuts whole rows and says so, so a short terminal never
// paints half a phase.
func TestTodoStripFitsTheHeightBudget(t *testing.T) {
	phases := todoStripFixture(t, []map[string]any{
		{"phase": "Foundation", "items": []string{"one", "two", "three", "four", "five", "six", "seven"}},
	})
	full := todoStripLines(phases, todoStripRowMax)
	if len(full) > todoStripRowMax {
		t.Fatalf("the strip overran its budget: %d rows", len(full))
	}
	if !strings.Contains(todoStripText(full), "more") {
		t.Fatalf("a capped preview must say how much it hid:\n%s", todoStripText(full))
	}
	short := todoStripLines(phases, 2)
	if len(short) != 2 {
		t.Fatalf("a 2-row budget painted %d rows:\n%s", len(short), todoStripText(short))
	}
	if !strings.Contains(todoStripText(short), "more below") {
		t.Fatalf("a cut strip must say so:\n%s", todoStripText(short))
	}
}

// On screen: the strip paints above the composer, and its marker wears the same
// ink the transcript's tool box gives it.
func TestTodoStripPaintsAboveTheComposer(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	app.AddSystemBlock("ready") // the transcript path, not the welcome screen
	tt := tool.NewTodoTool()
	if res, err := tt.Execute(context.Background(), json.RawMessage(
		`{"op":"init","items":["paint the strip","cover it with a test"]}`)); err != nil || res.IsError {
		t.Fatalf("init: %v %v", err, res.Text)
	}
	app.mu.Lock()
	app.ensureDock().ops.Todo = tt.Snapshot
	app.mu.Unlock()
	app.draw()

	out := screenText(scr)
	if !strings.Contains(out, "TODO · 0/2") {
		t.Fatalf("the strip is not on screen:\n%s", out)
	}
	if !strings.Contains(out, "[/] paint the strip") {
		t.Fatalf("the strip does not show the current task:\n%s", out)
	}
	// It is CHROME: above the composer's top border, and above the queue.
	app.mu.Lock()
	composerTop := app.height - 1 - app.composerRows()
	want := app.todoPanelTop(app.queueTop(composerTop))
	rows := app.todoPanelRows()
	app.mu.Unlock()
	if rows == 0 {
		t.Fatal("todoPanelRows reports no strip while the screen paints one")
	}
	lines := strings.Split(out, "\n")
	if !strings.Contains(lines[want], "TODO · 0/2") {
		t.Fatalf("the strip's first row is at %d, not %d:\n%s", indexOfLine(lines, "TODO · 0/2"), want, out)
	}

	// Closing the plan retires the strip on the next frame.
	if res, err := tt.Execute(context.Background(), json.RawMessage(
		`{"op":"done"}`)); err != nil || res.IsError {
		t.Fatalf("done: %v %v", err, res.Text)
	}
	app.draw()
	if s := screenText(scr); strings.Contains(s, "TODO ·") {
		t.Fatalf("a closed plan must retire the strip:\n%s", s)
	}
}

func todoStripText(rows []todoStripRow) string {
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(r.text)
		b.WriteByte('\n')
	}
	return b.String()
}

func indexOfLine(lines []string, sub string) int {
	for i, l := range lines {
		if strings.Contains(l, sub) {
			return i
		}
	}
	return -1
}
