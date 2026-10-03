package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// borderInk is the reasoning box's frame colour on screen: the foreground of
// the first cell of the rendered top border row.
func borderInk(app *App, i int) tcell.Color {
	app.mu.Lock()
	defer app.mu.Unlock()
	lines := app.blockLines(i, app.blocks[i], 80)
	if len(lines) == 0 || len(lines[0].runs) == 0 {
		return tcell.ColorDefault
	}
	fg, _, _ := lines[0].runs[0].style.Decompose()
	return fg
}

// setFade places the focus tween at one progress value without driving the
// tick loop, so a test can read the colour the renderer would paint.
func setFade(app *App, v float64) {
	app.mu.Lock()
	app.focusFade = v
	app.mu.Unlock()
}

// TestFocusFadeEasesBorderBetweenTwoInks is the "did it actually paint" check
// for the focus tween: the border must pass through a colour NEITHER of its
// endpoints, and it must do so on the real render path (blockLines) rather
// than on the style the test constructed itself.
//
// The cache is the trap this pins. The tween changes only the border's
// colour — never a row's text or length — so a render stamp that ignored it
// would serve one cached border for the whole fade and the box would snap at
// the end. driveFade asserts the mid-fade colour differs from the settled
// one, which is exactly what a missing stamp would hide.
func TestFocusFadeEasesBorderBetweenTwoInks(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.BeginThinking()
	app.AppendThinking(thinkLines(40))
	app.EndThinking()
	focusBox(app, 0)

	// Settled, unfocused: the plain AccentThinking ink, the style every
	// existing chrome test already asserts.
	setFade(app, -1)
	resting := borderInk(app, 0)

	// Half-faded: an eased blend toward the dim ink, so it must differ from
	// BOTH the resting border and the settled focused border.
	setFade(app, 0.5)
	mid := borderInk(app, 0)
	if mid == resting {
		t.Errorf("mid-fade border ink = %v, same as the resting border: the tween is not painting (render cache?)", mid)
	}
	setFade(app, 1)
	focusedInk := borderInk(app, 0)
	if focusedInk == mid {
		t.Errorf("settled focused border ink = %v, same as mid-fade: the tween does not land", focusedInk)
	}

	// The blend is between the two theme inks, not an arbitrary colour.
	dim := app.th.Get(theme.GrayDim)
	accent := app.th.Get(theme.AccentThinking)
	if want := tcell.NewRGBColor(
		int32(theme.Lerp(dim, accent, 0.5).R),
		int32(theme.Lerp(dim, accent, 0.5).G),
		int32(theme.Lerp(dim, accent, 0.5).B),
	); mid != want {
		t.Errorf("mid-fade border ink = %v, want the Lerp(GrayDim, AccentThinking, 0.5) = %v", mid, want)
	}

	// Landing back on the sentinel must restore the resting style exactly:
	// the sentinel is "no tween", not "tween at zero".
	setFade(app, -1)
	if got := borderInk(app, 0); got != resting {
		t.Errorf("after the tween ends, border ink = %v, want the resting %v", got, resting)
	}
}

// TestFocusFadeLeavesIdleTranscriptUntouched pins the cost discipline: -1 is
// the shipped default, so a session that never clicks a reasoning box never
// enters the tween and never repaints because of it.
func TestFocusFadeLeavesIdleTranscriptUntouched(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.mu.Lock()
	rest := app.focusFade
	app.mu.Unlock()
	if rest >= 0 {
		t.Errorf("fresh app focusFade = %v, want the -1 sentinel (no tween in flight)", rest)
	}
	if got := fadeBucket(rest); got != -1 {
		t.Errorf("fadeBucket(%v) = %d, want -1", rest, got)
	}
}

// TestFadeBucketSeparatesEveryTickStep pins the render-cache stamp: two
// consecutive ticks of the tween must land in different buckets, or one frame
// of the ease is coalesced away and the border visibly stutters.
func TestFadeBucketSeparatesEveryTickStep(t *testing.T) {
	if fadeBucket(0) != 0 {
		t.Fatalf("fadeBucket(0) = %d, want 0", fadeBucket(0))
	}
	prev := int8(-2)
	for step := 0.0; step <= 1.0; step += focusFadeStep {
		got := fadeBucket(step)
		if got == prev {
			t.Fatalf("fadeBucket(%.3f) = %d, same as the previous tick's bucket: a frame of the ease is lost",
				step, got)
		}
		prev = got
	}
	if got := fadeBucket(1); got != 8 {
		t.Errorf("fadeBucket(1) = %d, want 8", got)
	}
}

// firstThinkBoxRow paints the transcript and returns the screen row the first
// reasoning box's top border lands on, so a test can press the real mouse at
// the real coordinate instead of poking thinkFocus directly — the tween is
// armed in selection.go, and only a real press exercises that wiring.
func firstThinkBoxRow(t *testing.T, app *App, scr tcell.SimulationScreen) int {
	t.Helper()
	app.width, app.height = scr.Size() // paint reads the App's size, not the screen's
	app.paint()
	scr.Show() // Show flushes the back buffer into the simulation's cells
	cells, w, h := scr.GetContents()
	if cells == nil {
		t.Fatal("simulation screen has no cells")
	}
	for y := 0; y < h; y++ {
		var b strings.Builder
		for x := 0; x < w; x++ {
			if r := cells[y*w+x].Runes; len(r) > 0 {
				b.WriteString(string(r))
			} else {
				b.WriteByte(' ')
			}
		}
		// Match the state's own word, not the glyph, so the probe survives a
		// symbol preset that redraws the frame.
		if strings.Contains(b.String(), "Thought") {
			return y
		}
	}
	return -1
}

// TestMousePressArmsTheFocusTween drives the real press path: a click that
// names a reasoning box must reset focusFade to 0, which is the only wiring
// that starts the ease. A unit test that assigns thinkFocus itself would stay
// green with selection.go never arming anything.
func TestMousePressArmsTheFocusTween(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.SetHandlers(func(string) {}, func() {}, func() {})
	app.BeginThinking()
	app.AppendThinking(thinkLines(40))
	app.EndThinking()

	y := firstThinkBoxRow(t, app, scr)
	if y < 0 {
		t.Skip("no painted reasoning box in this geometry")
	}

	app.mu.Lock()
	app.focusFade = 0.5 // mid-fade from some earlier focus
	app.mu.Unlock()

	app.handleMouse(tcell.NewEventMouse(5, y, tcell.Button1, tcell.ModNone), true)

	app.mu.Lock()
	focus, fade := app.thinkFocus, app.focusFade
	app.mu.Unlock()
	if focus != 0 {
		t.Fatalf("press on the box's top border (row %d): thinkFocus = %d, want 0", y, focus)
	}
	if fade != 0 {
		t.Errorf("focus change left focusFade = %v, want 0 — the ease must start from the dim end, not resume", fade)
	}
}

// TestMousePressOffBoxLeavesTheTweenAlone is the cost guard on the other side:
// a press that lands on plain transcript is not a focus change, so it must not
// arm a border ease nobody asked for.
func TestMousePressOffBoxLeavesTheTweenAlone(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.SetHandlers(func(string) {}, func() {}, func() {})
	app.width, app.height = scr.Size()
	app.AddSystemBlock("ready")
	app.paint()

	app.handleMouse(tcell.NewEventMouse(5, 2, tcell.Button1, tcell.ModNone), true)

	app.mu.Lock()
	fade := app.focusFade
	app.mu.Unlock()
	if fade >= 0 {
		t.Errorf("press on plain transcript armed focusFade = %v, want the -1 sentinel", fade)
	}
}
