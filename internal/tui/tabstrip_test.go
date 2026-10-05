package tui

import (
	"strings"
	"testing"
	"time"
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
	if strings.Contains(screenText(scr), "second") {
		t.Fatalf("the opt-in default must not paint the strip:\n%s", screenText(scr))
	}
	if app.tabStripVisible() {
		t.Fatal("the opt-in default must not claim the transcript's first row")
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
