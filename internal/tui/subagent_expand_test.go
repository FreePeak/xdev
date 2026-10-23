package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

func subRowScreenY(t *testing.T, app *App, label string) int {
	t.Helper()
	app.mu.Lock()
	defer app.mu.Unlock()
	_, vp := app.selViewport()
	hdr := app.transcriptTop()
	for y := hdr; y < hdr+vp; y++ {
		if id, got := app.subChildAt(y); id != "" && got == label {
			return y
		}
	}
	t.Fatalf("no child row named %q is on screen", label)
	return -1
}

// subTranscriptApp is a session with one running `task` call whose child the
// host tracked, drawn so the child's row has a screen row to be clicked on.
func subAgentOps(ids ...string) *HubOps {
	return &HubOps{
		Roster: func() []HubAgent { return rosterRows() },
		Transcript: func(id string, fromSeq int) ([]HubTranscriptLine, int, bool) {
			for _, want := range ids {
				if id == want {
					return []HubTranscriptLine{
						{Role: "user", Text: "map the app shell"},
						{Role: "assistant", Text: "reading src/boot.tsx"},
						{Role: "toolResult", Text: "src/hooks/useChat.ts"},
					}, 3, true
				}
			}
			return nil, 0, false
		},
	}
}

// subRowScreenY is the screen row the child row with the given label painted
// on, found by asking the app for every transcript row's child.

func TestSubagentHitIsTheChildRowNotTheCallRow(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.AddToolBlock("c1", "task", `{"name":"scout","prompt":"map the app shell"}`)
	app.AddTaskChild("c1", "scout", "", "free")
	app.SetTaskChildTranscript("c1", "scout", "fg-7")
	app.draw()

	app.mu.Lock()
	hits := app.rowIdx.rend[0].subs
	app.mu.Unlock()
	if len(hits) != 1 {
		t.Fatalf("subs = %+v, want one child hit", hits)
	}
	if hits[0].offset != 1 {
		t.Fatalf("child hit offset = %d, want 1 (the row after the call row)", hits[0].offset)
	}
	y := subRowScreenY(t, app, "scout")
	app.mu.Lock()
	id, _ := app.subChildAt(y - 1)
	app.mu.Unlock()
	if id != "" {
		t.Fatalf("screen row %d (the call row) resolved to child %q", y-1, id)
	}
}

func TestUntrackedSubagentRowIsNotOpenable(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.AddToolBlock("c1", "task", `{"prompt":"work"}`)
	app.AddTaskChild("c1", "reader", "", "free")
	app.SetHubOps(subAgentOps("fg-7"))
	app.draw()

	app.mu.Lock()
	_, vp := app.selViewport()
	hdr := app.transcriptTop()
	openable := false
	for y := hdr; y < hdr+vp; y++ {
		if id, _ := app.subChildAt(y); id != "" {
			openable = true
		}
	}
	app.mu.Unlock()
	if openable {
		t.Fatal("an untracked child row is openable")
	}
	if app.openNewestSubChildTranscript() {
		t.Fatal("the chord opened a transcript for a session with no tracked child")
	}
}

func TestSubagentRowDragStillSelects(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.AddToolBlock("c1", "task", `{"name":"scout","prompt":"map the app shell"}`)
	app.AddTaskChild("c1", "scout", "", "free")
	app.SetTaskChildTranscript("c1", "scout", "fg-7")
	app.draw()
	app.SetHubOps(subAgentOps("fg-7"))

	y := subRowScreenY(t, app, "scout")
	app.mu.Lock()
	press(app, 6, y)
	app.mu.Unlock()
	if got := hubUI(app); got.open {
		t.Fatal("press alone opened the panel")
	}
	app.mu.Lock()
	press(app, 6, y)
	release(app, 40, y)
	opened := app.HubRosterOpen()
	app.mu.Unlock()
	if opened {
		t.Fatal("a drag across a child row opened the transcript")
	}
}

func TestSubagentSummaryLineIsClaudeCodes(t *testing.T) {
	d := 4200 * time.Millisecond
	app, scr := newTestApp(t, 100, 30)
	app.AddToolBlock("c1", "task", `{"name":"scout","prompt":"x"}`)
	app.AddTaskChild("c1", "scout", "", "m")
	app.SetTaskChildTranscript("c1", "scout", "fg-1")
	app.SetHubOps(&HubOps{
		Roster: func() []HubAgent { return []HubAgent{{ID: "fg-1", Tools: 3, Tokens: 1500, Elapsed: d}} },
		Transcript: func(id string, fromSeq int) ([]HubTranscriptLine, int, bool) { return nil, 0, false },
	})
	app.RefreshTaskChildStats("c1", "scout", "fg-1")
	app.FinishTaskChild("c1", "scout", "ok", d)
	app.draw()
	out := screenText(scr)
	t.Logf("dur=%s", humanDur(d))
	if !strings.Contains(out, "Done (3 tool uses · 1.5k tokens · ") {
		t.Fatalf("missing summary: %s", out)
	}
	if !strings.Contains(out, "·") {
		t.Fatal("no middot")
	}
	// second child singular
	app2, scr2 := newTestApp(t, 100, 30)
	app2.AddToolBlock("c2", "task", `{"name":"scout2","prompt":"x"}`)
	app2.AddTaskChild("c2", "scout2", "", "m")
	app2.SetTaskChildTranscript("c2", "scout2", "fg-2")
	app2.SetHubOps(&HubOps{
		Roster: func() []HubAgent { return []HubAgent{{ID: "fg-2", Tools: 1, Tokens: 0, Elapsed: d}} },
		Transcript: func(id string, fromSeq int) ([]HubTranscriptLine, int, bool) { return nil, 0, false },
	})
	app2.RefreshTaskChildStats("c2", "scout2", "fg-2")
	app2.FinishTaskChild("c2", "scout2", "ok", d)
	app2.draw()
	out2 := screenText(scr2)
	if !strings.Contains(out2, "1 tool use") {
		t.Fatalf("singular missing: %s", out2)
	}
}

func TestSubagentSummaryLineLiveWhileRunning(t *testing.T) {
	d := time.Second
	app, scr := newTestApp(t, 100, 30)
	app.AddToolBlock("c1", "task", `{"name":"scout","prompt":"x"}`)
	app.AddTaskChild("c1", "scout", "", "m")
	app.SetTaskChildTranscript("c1", "scout", "fg-1")
	app.SetHubOps(&HubOps{
		Roster: func() []HubAgent { return []HubAgent{{ID: "fg-1", Tools: 2, Tokens: 100, Elapsed: d}} },
		Transcript: func(id string, fromSeq int) ([]HubTranscriptLine, int, bool) { return nil, 0, false },
	})
	app.RefreshTaskChildStats("c1", "scout", "fg-1")
	app.draw()
	out := screenText(scr)
	if !strings.Contains(out, "Running...") {
		t.Fatalf("not running: %s", out)
	}
	app.SetHubOps(&HubOps{
		Roster: func() []HubAgent { return []HubAgent{{ID: "fg-1", Tools: 5, Tokens: 200, Elapsed: d}} },
		Transcript: func(id string, fromSeq int) ([]HubTranscriptLine, int, bool) { return nil, 0, false },
	})
	app.UpdateTaskChild("c1", "scout", "read", `{}`, "ok")
	app.RefreshTaskChildStats("c1", "scout", "fg-1")
	app.draw()
	out2 := screenText(scr)
	if !strings.Contains(out2, "5") {
		t.Fatalf("count not updated: %s", out2)
	}
}

func TestSubagentNoTokensOmitsTheSegment(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	app.AddToolBlock("c1", "task", `{"name":"scout","prompt":"x"}`)
	app.AddTaskChild("c1", "scout", "", "m")
	app.SetTaskChildTranscript("c1", "scout", "fg-1")
	app.SetHubOps(&HubOps{
		Roster: func() []HubAgent { return []HubAgent{} },
		Transcript: func(id string, fromSeq int) ([]HubTranscriptLine, int, bool) { return nil, 0, false },
	})
	app.draw()
	out := screenText(scr)
	if strings.Contains(out, "tokens") {
		t.Fatalf("tokens present: %s", out)
	}
	if strings.Contains(out, "0 tokens") {
		t.Fatalf("zero tokens present: %s", out)
	}
}

func TestSubagentFailedChildSaysFailed(t *testing.T) {
	d := time.Second
	app, scr := newTestApp(t, 100, 30)
	app.AddToolBlock("c1", "task", `{"name":"scout","prompt":"x"}`)
	app.AddTaskChild("c1", "scout", "", "m")
	app.SetTaskChildTranscript("c1", "scout", "fg-1")
	app.SetHubOps(&HubOps{
		Roster: func() []HubAgent { return []HubAgent{{ID: "fg-1", Tools: 1, Tokens: 10, Elapsed: d}} },
		Transcript: func(id string, fromSeq int) ([]HubTranscriptLine, int, bool) { return nil, 0, false },
	})
	app.RefreshTaskChildStats("c1", "scout", "fg-1")
	app.FinishTaskChild("c1", "scout", "error", d)
	app.draw()
	out := screenText(scr)
	if !strings.Contains(out, "Failed (") {
		t.Fatalf("no Failed: %s", out)
	}
	if strings.Contains(out, "Done (") {
		t.Fatalf("Done present: %s", out)
	}
}

func TestSubagentLineExpandsInPlace(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	app.AddToolBlock("c1", "task", `{"name":"scout","prompt":"x"}`)
	app.AddTaskChild("c1", "scout", "", "m")
	app.SetTaskChildTranscript("c1", "scout", "fg-1")
	app.SetHubOps(subAgentOps("fg-1"))
	app.draw()
	app.handleKey(tcell.NewEventKey(tcell.KeyCtrlO, 0, tcell.ModNone))
	// expanded state
	app.mu.Lock()
	b := runningTask(t, app, "c1")
	expanded := false
	if len(b.Sub) > 0 {
		expanded = b.Sub[0].Expanded
	}
	app.mu.Unlock()
	if !expanded {
		t.Fatal("not expanded")
	}
	out := screenText(scr)
	if !strings.Contains(out, "map the app shell") {
		t.Fatalf("transcript not painted: %s", out)
	}
	app.handleKey(tcell.NewEventKey(tcell.KeyCtrlO, 0, tcell.ModNone))
	app.mu.Lock()
	b = runningTask(t, app, "c1")
	expanded = true
	if len(b.Sub) > 0 {
		expanded = b.Sub[0].Expanded
	}
	app.mu.Unlock()
	if expanded {
		t.Fatal("still expanded")
	}
}

func TestSubagentClickOnLineTogglesThatChild(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.AddToolBlock("c1", "task", `{"context":"shared","tasks":[{"prompt":"a"},{"prompt":"b"}]}`)
	app.AddTaskChild("c1", "first", "", "free")
	app.SetTaskChildTranscript("c1", "first", "fg-1")
	app.AddTaskChild("c1", "second", "", "free")
	app.SetTaskChildTranscript("c1", "second", "fg-2")
	app.draw()
	app.SetHubOps(subAgentOps("fg-1", "fg-2"))

	// find y for first
	y := subRowScreenY(t, app, "first")
	app.mu.Lock()
	press(app, 6, y)
	release(app, 6, y)
	app.mu.Unlock()

	app.mu.Lock()
	b := runningTask(t, app, "c1")
	var aExp, bExp bool
	for _, s := range b.Sub {
		if s.Label == "first" {
			aExp = s.Expanded
		}
		if s.Label == "second" {
			bExp = s.Expanded
		}
	}
	app.mu.Unlock()
	if !aExp {
		t.Fatal("first not expanded")
	}
	if bExp {
		t.Fatal("second expanded")
	}
}
