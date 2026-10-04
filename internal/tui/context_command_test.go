package tui

import (
	"strings"
	"testing"
)

// /context must reach its seam, refuse a value outside the vocabulary BEFORE
// the ops see it (so nothing is written for a typo), and report the pin in
// force on the bare form. nil ops degrade to a notice like every other
// request-side seam.
func TestDispatchContextWindow(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	var set []string
	pin := "auto"
	app.SetContextOps(&ContextOps{
		Current: func() string { return pin },
		Set: func(next string) error {
			set = append(set, next)
			pin = next
			return nil
		},
	})

	if !dispatch(app, "/context") {
		t.Fatal("/context not consumed")
	}
	if len(set) != 0 {
		t.Fatalf("a bare report must not write: %v", set)
	}
	report := app.blocks[len(app.blocks)-1].Text
	if !strings.Contains(report, "auto") {
		t.Fatalf("bare report names the pin: %q", report)
	}
	// The bare form is the discovery surface: it must list every choice.
	for _, choice := range []string{"200k", "300k", "500k", "1m"} {
		if !strings.Contains(report, choice) {
			t.Fatalf("bare report is missing %q: %q", choice, report)
		}
	}

	app.blocks = nil
	if !dispatch(app, "/context 500k") {
		t.Fatal("/context 500k not consumed")
	}
	if len(set) != 1 || set[0] != "500k" {
		t.Fatalf("set = %v, want [500k]", set)
	}
	if got := app.blocks[len(app.blocks)-1].Text; !strings.Contains(got, "500k") {
		t.Fatalf("confirm names the window: %q", got)
	}

	// A size outside the vocabulary must not reach the ops at all.
	if !dispatch(app, "/context 700k") {
		t.Fatal("/context 700k not consumed")
	}
	if len(set) != 1 {
		t.Fatalf("an unknown window must not reach the ops: %v", set)
	}
	if got := app.blocks[len(app.blocks)-1].Text; !strings.Contains(got, "auto|200k|300k|500k|1m") {
		t.Fatalf("usage notice = %q", got)
	}

	// auto is a real rung: it hands every model back its catalog window.
	if !dispatch(app, "/context auto") {
		t.Fatal("/context auto not consumed")
	}
	if len(set) != 2 || set[1] != "auto" {
		t.Fatalf("set = %v, want [500k auto]", set)
	}

	app.SetContextOps(nil)
	app.blocks = nil
	if !dispatch(app, "/context 1m") {
		t.Fatal("/context 1m not consumed")
	}
	if got := app.blocks[len(app.blocks)-1].Text; !strings.Contains(got, "not wired") {
		t.Fatalf("unwired notice = %q", got)
	}
}
