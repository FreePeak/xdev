package tui

import (
	"strings"
	"testing"
	"time"
)

// TestTabStripPolicyHidesAndNumbers: tui.tabs.mode off hides the strip and
// frees its row (the chords and /tabs keep working — the strip is a view of
// the tabset, not the tabset), and tui.tabs.indicators numbers swaps the
// status glyph for the tab's index, which is the legend for the C-1..9 chords.
// New() defaults tabsStrip on, so a host that never calls SetTabPolicy still
// paints the shipped strip.
func TestTabStripPolicyHidesAndNumbers(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	app.SetTabs([]TabInfo{
		{ID: "aaaa1111", Title: "first", Running: true, Current: true},
		{ID: "bbbb2222", Title: "second"},
	})
	app.draw()
	if !strings.Contains(screenText(scr), "first") {
		t.Fatal("the default policy does not paint the strip")
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
