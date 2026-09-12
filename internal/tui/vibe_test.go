package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// /vibe is a real command: bare toggles, on/off set the mode, and a
// directive re-enters with that text as the first prompt. A host refusal
// (plan/goal conflict) surfaces as an error block, never a silent no-op.
func TestDispatchVibe(t *testing.T) {
	f := &fakeAPI{}
	if !dispatch(f, "/vibe") {
		t.Fatal("/vibe must be consumed as a command")
	}
	if f.vibe != "" {
		t.Fatalf("bare /vibe args = %q, want empty", f.vibe)
	}
	if !dispatch(f, "/vibe fix the flaky test in packages/tui") {
		t.Fatal("/vibe <prompt> must be consumed")
	}
	if f.vibe != "fix the flaky test in packages/tui" {
		t.Fatalf("directive args = %q", f.vibe)
	}
	if !dispatch(f, "/vibe off") || f.vibe != "off" {
		t.Fatalf("/vibe off args = %q", f.vibe)
	}
	// A refused entry is reported to the user.
	bad := &fakeAPI{fail: "vibe"}
	dispatch(bad, "/vibe")
	if len(bad.blocks) != 1 || !strings.Contains(bad.blocks[0], "error: boom") {
		t.Fatalf("refusal blocks = %q", bad.blocks)
	}
	// Discoverable.
	h := &fakeAPI{}
	dispatch(h, "/help")
	if len(h.blocks) != 1 || !strings.Contains(h.blocks[0], "vibe") {
		t.Fatalf("/help output = %q, must list /vibe", h.blocks)
	}

	// Nothing reaches the model: the director's first prompt is submitted by
	// the App, not by the dispatcher.
	if len(f.sent) != 0 {
		t.Fatalf("/vibe must not send model text: %q", f.sent)
	}
}

// vibeStub stands in for the cmd-side director state.
type vibeStub struct {
	on       bool
	conflict bool
	entered  int
	exited   int
}

func (s *vibeStub) ops() *VibeOps {
	return &VibeOps{
		Active: func() bool { return s.on },
		Set: func(on bool) error {
			if on && s.conflict {
				return fmt.Errorf("vibe: exit plan mode first — vibe is mutually exclusive with plan and goal modes")
			}
			if on {
				s.on, s.entered = true, s.entered+1
				return nil
			}
			s.on, s.exited = false, s.exited+1
			return nil
		},
		Status: func() string { return "vibe: 1 worker(s)\nalpha  fast  done  turn 1  fake/dev" },
	}
}

// Through the real key path: enter → announce + indicator, directive →
// submitted prompt, status → registry, off → announce + indicator gone.
func TestAppVibeTransitionsAndIndicator(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	var sent []string
	app.SetHandlers(func(text string) { sent = append(sent, text) }, func() {}, func() {})
	st := &vibeStub{}
	app.SetVibeOps(st.ops())

	run := func(input string) {
		t.Helper()
		typeRunes(app, input)
		app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	}
	last := func() string {
		t.Helper()
		blocks := app.Blocks()
		if len(blocks) == 0 {
			t.Fatal("no transcript blocks")
		}
		return blocks[len(blocks)-1].Text
	}
	has := func(text string) bool {
		t.Helper()
		for _, b := range app.Blocks() {
			if strings.Contains(b.Text, text) {
				return true
			}
		}
		return false
	}

	run("/vibe")
	if !st.on || st.entered != 1 {
		t.Fatalf("bare /vibe must enter: %+v", st)
	}
	if !has("vibe mode ON") {
		t.Fatalf("entry must announce itself: %q", last())
	}
	if !screenHasVibe(scr, app) {
		t.Fatal("the status line must show the Vibe indicator while the mode is on")
	}

	run("/vibe fix the flaky test")
	if len(sent) != 1 || sent[0] != "fix the flaky test" {
		t.Fatalf("directive submission = %q", sent)
	}
	if !st.on || st.entered != 2 {
		t.Fatalf("a directive enters the mode: %+v", st)
	}
	run("/vibe status")
	if !strings.Contains(last(), "alpha") {
		t.Fatalf("status = %q", last())
	}

	run("/vibe off")
	if st.on || st.exited != 1 {
		t.Fatalf("off must exit: %+v", st)
	}
	if !has("vibe mode OFF") {
		t.Fatalf("exit must announce the restored toolset: %q", last())
	}
	if screenHasVibe(scr, app) {
		t.Fatal("the indicator must leave with the mode")
	}

	// A conflict (plan/goal active) refuses entering and says why.
	st.conflict = true
	run("/vibe on")
	if st.on {
		t.Fatal("a conflicting entry must not activate the mode")
	}
	if !has("mutually exclusive") {
		t.Fatalf("conflict must reach the user: %q", last())
	}

	// An unwired seam degrades to a notice instead of panicking.
	bare, _ := newTestApp(t, 80, 24)
	typeRunes(bare, "/vibe")
	bare.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	blocks := bare.Blocks()
	if len(blocks) != 1 || !strings.Contains(blocks[0].Text, "not wired") {
		t.Fatalf("unwired /vibe blocks = %+v", blocks)
	}
}

// screenHasVibe renders a frame and looks for the indicator text.
func screenHasVibe(scr tcell.SimulationScreen, app *App) bool {
	app.draw()
	prim, w, h := scr.GetContents()
	for y := 0; y < h; y++ {
		var row strings.Builder
		for x := 0; x < w; x++ {
			cell := prim[y*w+x]
			if len(cell.Runes) > 0 {
				row.WriteRune(cell.Runes[0])
			} else {
				row.WriteByte(' ')
			}
		}
		if strings.Contains(row.String(), "Vibe") {
			return true
		}
	}
	return false
}
