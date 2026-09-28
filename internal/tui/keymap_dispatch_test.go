package tui

import (
	"testing"

	"github.com/gdamore/tcell/v2"
)

// /hotkeys advertises C-r as history-prev, so the chord must recall. The key
// event shape is the one a live terminal delivers (verified against a pty in the
// 2026-09-28 audit: Key=KeyCtrlR, Rune set, ModCtrl set).
func TestHistoryPrevChordRecalls(t *testing.T) {
	app, _ := newTestApp(t, 120, 40)
	app.SetHandlers(func(string) {}, func() {}, func() {})
	for _, s := range []string{"first", "second", "third"} {
		for _, r := range s {
			app.handleKey(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
		}
		app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	}
	if !app.ed.HasHistory() {
		t.Fatal("no history after three sends")
	}
	ctrlR := tcell.NewEventKey(tcell.KeyCtrlR, 'r', tcell.ModCtrl)
	app.handleKey(ctrlR)
	if got := app.ed.Text(); got != "third" {
		t.Fatalf("C-r recalled %q, want %q", got, "third")
	}
	app.handleKey(ctrlR)
	if got := app.ed.Text(); got != "second" {
		t.Fatalf("second C-r recalled %q, want %q", got, "second")
	}
}

// Up and C-r must not disagree: the chord is the same recall, one table entry
// apart.
func TestHistoryPrevChordMatchesUpArrow(t *testing.T) {
	app, _ := newTestApp(t, 120, 40)
	app.SetHandlers(func(string) {}, func() {}, func() {})
	for _, s := range []string{"alpha", "beta"} {
		for _, r := range s {
			app.handleKey(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
		}
		app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	}
	app.handleKey(tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone))
	byUp := app.ed.Text()
	app.ed.Reset()
	app.handleKey(tcell.NewEventKey(tcell.KeyCtrlR, 'r', tcell.ModCtrl))
	if got := app.ed.Text(); got != byUp {
		t.Errorf("C-r recalled %q, Up recalled %q — the two paths disagree", got, byUp)
	}
}
