package tui

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// TestBeatDoneNamesASlowIteration pins the reason this exists: recovery from a
// long iteration must leave a line. A stall dump cannot — the loop came back,
// so the watchdog never fires, and the iteration that froze the screen for 45s
// (a synchronous terminal flush under a held paste, #20) leaves no trace.
func TestBeatDoneNamesASlowIteration(t *testing.T) {
	app, _ := loopApp(80, 24)
	old := slowIteration
	slowIteration = 10 * time.Millisecond
	t.Cleanup(func() { slowIteration = old })

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	app.beatDone("event", time.Now().Add(-50*time.Millisecond)) // slow: reported
	app.beatDone("tick", time.Now())                            // fast: silent
	os.Stderr = orig
	w.Close()

	out, _ := io.ReadAll(r)
	if got := strings.Count(string(out), "UI loop iteration took"); got != 1 {
		t.Fatalf("slow iteration reported %d times, want exactly once:\n%s", got, out)
	}
	if !strings.Contains(string(out), "(event)") {
		t.Fatalf("report does not name the phase:\n%s", out)
	}
	if app.loopBeat.Load() == 0 {
		t.Fatal("beatDone did not beat")
	}
}
