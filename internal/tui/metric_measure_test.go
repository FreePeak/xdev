package tui

import (
	"fmt"
	"testing"
	"time"
)

// The HUD's ⚡ t/s and the ↑ ↓ counters are measurements, so each of these
// pins one way they used to report a number that was simply not what it
// claimed: a rate divided by a dead turn's elapsed time, tokens the window
// never timed, a previous session's totals, a stale reading standing in for
// an unmeasured one.

// deltas streams n deltas spaced gap apart, so the decode window
// (first delta → last delta) is (n-1)*gap.
func deltas(app *App, n int, gap time.Duration, s string) {
	for i := 0; i < n; i++ {
		if i > 0 {
			time.Sleep(gap)
		}
		app.AppendAssistant(s)
	}
}

func measuredRate(t *testing.T, app *App) float64 {
	t.Helper()
	app.mu.Lock()
	defer app.mu.Unlock()
	return app.st.Rate
}

func rateSegment(t *testing.T, app *App) string {
	t.Helper()
	app.mu.Lock()
	defer app.mu.Unlock()
	text, _ := app.hudSegment("rate")
	return text
}

// TestRateWindowIsPerMessage: a turn that dies into the retry ladder never
// emits EventDone, so its decode window stays open. The next message's
// first EventStart must discard it — otherwise that message's own output
// tokens are divided by the dead turn's elapsed time and the rate reads
// several times too low for the rest of the session.
func TestRateWindowIsPerMessage(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)

	// Turn 1: streams, dies, never reaches AddUsage.
	deltas(app, 3, 50*time.Millisecond, "partial")
	app.FinishRun()

	// Turn 2 opens a message, then streams 5 deltas at 100ms: its own
	// window is 400ms and its true rate is 1000/0.4 = 2500 t/s.
	app.BeginMessage()
	deltas(app, 5, 100*time.Millisecond, "answer ")
	app.AddUsage(10, 1000, 1010)

	got := measuredRate(t, app)
	if got < 1500 || got > 4000 {
		t.Fatalf("rate = %.1f t/s, want ~2500 (the message's own 400ms window, not the dead turn's)", got)
	}
}

// TestRateWindowSpansToolDeltas: output_tokens counts tool-call argument
// JSON, which the transcript never renders. If the window stops at the last
// text delta, a big tool call divides 30k tokens by a 200ms text window and
// reports an impossible ~150k t/s.
func TestRateWindowSpansToolDeltas(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	app.BeginMessage()
	deltas(app, 3, 100*time.Millisecond, "calling the tool")
	for i := 0; i < 10; i++ {
		time.Sleep(20 * time.Millisecond) // ~20ms of argument streaming
		app.NoteToolDelta()
	}
	app.AddUsage(20, 1000, 1020)

	got := measuredRate(t, app)
	// 1000 tokens over ~400ms is 2500 t/s. Without tool deltas in the window
	// the same turn reads 5000; the number must land between, and never near
	// the ~150k an untimed argument stream produces.
	if got < 2000 || got > 3200 {
		t.Fatalf("rate = %.1f t/s, want ~2500 (window must span the tool deltas)", got)
	}
}

// TestRateSegmentHidesWhenUnmeasured: a message too short to measure must
// not leave the previous turn's reading on screen as if it were this one's.
func TestRateSegmentHidesWhenUnmeasured(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	app.BeginMessage()
	deltas(app, 3, 100*time.Millisecond, "ok")
	app.AddUsage(1, 100, 101)
	if rateSegment(t, app) == "" {
		t.Fatal("a measured rate must render")
	}

	// A 2-token burst inside 100ms: nothing to measure.
	app.BeginMessage()
	app.AppendAssistant("x")
	app.AddUsage(1, 2, 3)
	if show := rateSegment(t, app); show != "" {
		t.Fatalf("unmeasured message left %q on the status row, want the segment hidden", show)
	}
	if got := measuredRate(t, app); got != 0 {
		t.Fatalf("rate = %.1f after an unmeasurable message, want 0", got)
	}
}

// TestRateIsOneMeasuredNumber: the live rune estimate read up to 5x off the
// provider-measured value and the segment swapped between them mid-turn. The
// segment now shows the measured number and nothing else.
func TestRateIsOneMeasuredNumber(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	app.SetRunning(true)
	app.BeginMessage()
	deltas(app, 3, 100*time.Millisecond, `func main() { fmt.Println("hi") }`)
	if show := rateSegment(t, app); show != "" {
		t.Fatalf("mid-stream the segment drew %q from a rune estimate; want it hidden until usage lands", show)
	}

	app.AddUsage(5, 120, 125)
	settled := measuredRate(t, app)
	show := rateSegment(t, app)
	if want := fmt.Sprintf("⚡ %.1f t/s", settled); show != want {
		t.Fatalf("rate segment = %q, want %q (the measured value, not a rune estimate)", show, want)
	}
	// Ending the run does not change the number on the row.
	app.SetRunning(false)
	if after := rateSegment(t, app); after != show {
		t.Fatalf("the rendered rate changed when the run ended: %q then %q", show, after)
	}
}

// TestResetClearsSessionCounters: ↑ ↓ ⚡ ⌚ belong to one session. /resume,
// /fork, /new and /clear all go through Reset, so anything it leaves behind
// is the previous session's number drawn next to the new transcript.
func TestResetClearsSessionCounters(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	app.AddUsage(50_000, 50_000, 100_000)
	app.AddCost(1.23)
	app.SetTTFT(450)
	app.SetWork(2 * time.Hour)
	app.BeginMessage()
	deltas(app, 3, 100*time.Millisecond, "hi")
	app.AddUsage(1, 100, 101)
	if rateSegment(t, app) == "" {
		t.Fatal("setup: the rate must be measured first")
	}

	app.Reset()

	app.mu.Lock()
	in, out, rate, ttft, cost, work, ctx := app.st.TokensIn, app.st.TokensOut, app.st.Rate, app.st.TTFT, app.st.Cost, app.st.Work, app.st.CtxUsed
	app.mu.Unlock()
	if in != 0 || out != 0 {
		t.Fatalf("↑%d ↓%d survived Reset — the HUD would show the previous session's token counters", in, out)
	}
	if rate != 0 {
		t.Fatalf("rate %.1f survived Reset — a new session opens showing a ⚡ it never measured", rate)
	}
	if ttft != 0 {
		t.Fatalf("ttft %dms survived Reset", ttft)
	}
	if cost != 0 {
		t.Fatalf("cost %.2f survived Reset", cost)
	}
	if work != 0 || ctx != 0 {
		t.Fatalf("work=%v ctx=%d should be cleared too", work, ctx)
	}
	tokens, _ := app.hudSegment("tokens")
	if tokens != "" {
		t.Fatalf("the token segment still draws %q after Reset", tokens)
	}
	if show := rateSegment(t, app); show != "" {
		t.Fatalf("the rate segment still draws %q after Reset", show)
	}
}
