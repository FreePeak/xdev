package tui

import (
	"strings"
	"testing"
)

// The HUD segment vocabulary is keyed camelCase (`debugMouse`, like every
// settings key) but SetStatusSegments folds a configured name to lower case
// before testing it, so the camelCase entry was reported "unknown" the line
// after the map accepted it — and hudSegment's own `case "debugMouse"` could
// never be reached from a folded name either. The one segment a pointer probe
// needs was impossible to enable, so a drive could not tell a click the app
// ignored from one it never received.
//
// Pinned here because the failure is invisible: no error, no crash, a settings
// key that quietly does nothing.
func TestStatusSegmentsAcceptTheirOwnCamelCaseNames(t *testing.T) {
	app, _ := newTestApp(t, 110, 34)
	app.SetDebugMouse(true)
	app.SetStatusSegments([]string{"debugMouse"})

	app.mu.Lock()
	segs := append([]string(nil), app.statusSegs...)
	app.mu.Unlock()
	if len(segs) != 1 || segs[0] != "debugmouse" {
		t.Fatalf("statusSegs = %v, want the folded debugMouse segment", segs)
	}

	// And it must RENDER: the folded name has to reach the switch arm, and the
	// segment hides only while it has nothing to say.
	app.mu.Lock()
	text, _ := app.hudSegment(segs[0])
	if text != "" {
		t.Fatalf("hudSegment(%q) = %q, want empty until a mouse event lands", segs[0], text)
	}
	app.debugMouseLine = "btn1 press @9,6"
	parts := app.hudParts()
	app.mu.Unlock()
	if len(parts) != 1 || parts[0].name != "debugmouse" || parts[0].text != "btn1 press @9,6" {
		t.Fatalf("hudParts = %+v, want the debugMouse segment carrying the last event", parts)
	}
}

// Every name the vocabulary advertises must survive its own filter, at either
// case. A key nobody can select is a key that does not exist, and this is the
// one place that fact is cheap to test.
func TestEveryStatusSegmentNameIsSelectable(t *testing.T) {
	for _, name := range statusSegmentNames() {
		if !isStatusSegment(strings.ToLower(name)) {
			t.Errorf("%q is in the vocabulary but its own lower-case form is rejected", name)
		}
		if !isStatusSegment(name) {
			t.Errorf("%q is in the vocabulary but rejected in its own spelling", name)
		}
	}
}
