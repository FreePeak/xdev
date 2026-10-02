package tui

import (
	"strings"
	"testing"
)

// A live box is spliced into the transcript, so the row index — an
// index-aligned table of cached render, row offsets and timestamp — must not
// keep serving stale rows after the splice: every block AFTER the box would
// paint the block one place above it.
func TestLiveBoxSpliceInvalidatesTheRowIndex(t *testing.T) {
	// Tall enough that the whole transcript stays in view: this test is about
	// which text sits under which row, not about scrolling.
	app, scr := drawnApp(t, 60, 40)
	// Two calls in one batch, announced together, so the later one is already
	// on screen when the earlier one opens its box between the two rows.
	app.AddToolBlock("call-a", "bash", `{"command":"aaa"}`)
	app.AddToolBlock("call-b", "bash", `{"command":"bbb"}`)
	app.draw()
	app.draw()

	// call-a's box splices in ABOVE call-b's row: b's row, its running clock
	// and everything the row index cached for it shift down by one.
	app.AppendToolOutput("call-a", "bash", "aaa-output\n")
	app.draw()
	after := screenText(scr)

	lines := strings.Split(after, "\n")
	index := func(want string) int {
		for i, l := range lines {
			if strings.Contains(l, want) {
				return i
			}
		}
		return -1
	}
	aOut, bRow := index("aaa-output"), index("bbb")
	if aOut < 0 {
		t.Fatalf("the live box did not paint:\n%s", after)
	}
	if bRow < 0 {
		t.Fatalf("the second call row vanished:\n%s", after)
	}
	// The nearest tool call row above the box is the one producing it, and
	// the nearest one below it is still the other call: a stale index would
	// serve the FOLLOWING call's cached render for the box, so the box would
	// read as bbb's output — or aaa's row would vanish.
	callRow := func(s string) bool { return strings.Contains(s, "bash ·") }
	owner := -1
	for i := aOut - 1; i >= 0; i-- {
		if callRow(lines[i]) {
			owner = i
			break
		}
	}
	if owner < 0 || !strings.Contains(lines[owner], "aaa") {
		t.Fatalf("the box is not under the call producing it:\n%s", after)
	}
	next := -1
	for i := aOut + 1; i < len(lines); i++ {
		if callRow(lines[i]) {
			next = i
			break
		}
	}
	if next != bRow || !strings.Contains(lines[next], "bbb") {
		t.Fatalf("the second call is not the row after the box (row %d, want %d):\n%s", next, bRow, after)
	}
}
