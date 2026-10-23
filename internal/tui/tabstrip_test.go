package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// TestTabStripPolicyHidesAndNumbers: the strip is OPT-IN (a default install
// paints no tab row), tui.tabs.indicators numbers swaps the status glyph for
// the tab's index — the legend for the C-1..9 chords — and turning the policy
// back off hides the strip and frees its row. The chords and /tabs keep
// working throughout: the strip is a view of the tabset, not the tabset.
func TestTabStripPolicyHidesAndNumbers(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	app.SetTabs([]TabInfo{
		{ID: "aaaa1111", Title: "first", Running: true, Current: true},
		{ID: "bbbb2222", Title: "second"},
	})
	app.draw()
	if !strings.Contains(screenText(scr), "second") {
		t.Fatalf("the default must paint the strip:\n%s", screenText(scr))
	}
	if !app.tabStripVisible() {
		t.Fatal("the default must claim the transcript's first row")
	}

	app.SetTabPolicy(false, false)
	app.draw()
	if strings.Contains(screenText(scr), "second") {
		t.Fatalf("mode=off must not paint the strip:\n%s", screenText(scr))
	}
	if app.tabStripVisible() {
		t.Fatal("mode=off must not claim the transcript's first row")
	}

	app.SetTabPolicy(true, false)
	app.draw()
	if !strings.Contains(screenText(scr), "first") {
		t.Fatalf("tui.tabs.mode on did not paint the strip:\n%s", screenText(scr))
	}

	app.SetTabPolicy(true, true)
	app.draw()
	got := screenText(scr)
	if !strings.Contains(got, "1 first") || !strings.Contains(got, "2 second") {
		t.Fatalf("indicators=numbers did not number the tabs:\n%s", got)
	}

	app.SetTabPolicy(false, true)
	app.draw()
	got = screenText(scr)
	if strings.Contains(got, "second") {
		t.Fatalf("mode=off still painted the strip:\n%s", got)
	}
	if app.tabStripVisible() {
		t.Fatal("mode=off still claims the transcript's first row")
		// Hiding the strip must not stop the chords reaching it: a C-2 jump on a
		// host whose strip is hidden still calls the host's pick callback. It runs
		// on its own goroutine (the UI thread must not host the rebuild), so wait
		// for it rather than reading the variable straight after the call.
		picked := make(chan string, 1)
		app.SetTabPick(func(id string) error { picked <- id; return nil })
		app.selectTab(2)
		select {
		case got := <-picked:
			if got != "bbbb2222" {
				t.Fatalf("selectTab with the strip hidden picked %q, want bbbb2222", got)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("selectTab never reached the host: the strip policy disabled the chords")
		}
	}
}

// TestTabStripDimmedBands: every tab chip paints on a dimmed band, and the
// current session's chip is one step brighter than the rest — so the strip
// still reads as a row when the terminal mutes its colours, and the eye finds
// the current session without leaning on the underline alone.
func TestTabStripDimmedBands(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	app.SetTabs([]TabInfo{
		{ID: "aaaa1111", Title: "alpha", Current: true},
		{ID: "bbbb2222", Title: "beta"},
	})
	app.draw()
	dimBg, ok := app.th.Slot(theme.BgHighlight)
	if !ok {
		t.Fatal("the test theme must set BgHighlight; without it the strip keeps terminal-default cells")
	}
	// The band under a chip's first label rune: 'a' opens the current chip,
	// 'b' the inactive one. Two labels with no shared letters, so the probe
	// cannot land on the other chip by accident.
	chipBg := func(label rune) tcell.Color {
		for x := 0; x < 100; x++ {
			r, _, st, _ := scr.GetContent(x, tabStripRow)
			if r == label {
				_, bg, _ := st.Decompose()
				return bg
			}
		}
		t.Fatalf("chip %q is not painted on the strip row", string(label))
		return tcell.ColorDefault
	}
	wantDim := app.cellColor(dimBg)
	wantBright := app.cellColor(blendTowardWhite(dimBg, 0.2))
	if got := chipBg('a'); got != wantBright {
		t.Fatalf("current chip bg = %v, want the blended brighter band %v", got, wantBright)
	}
	if got := chipBg('b'); got != wantDim {
		t.Fatalf("inactive chip bg = %v, want the BgHighlight dim band %v", got, wantDim)
	}
}
