package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/theme"
	"github.com/FreePeak/xdev/internal/tui"
)

// The hook layer is where the HUD's numbers come from, so the wiring is half
// the measurement. These drive tuiHooks.OnEvent with the event sequence a
// provider really emits, through the app's own paint loop, and read the
// status row a user would read. A unit test of App alone passes with the
// hook still unwired — that is how the tool-delta case shipped unwired.

// metricTestApp builds a TUI running its real paint loop, and stops it with
// the test. The loop is what puts the HUD on screen: a test that mutates App
// and reads its fields proves the arithmetic, not that the row is right.
func metricTestApp(t *testing.T, w, h int) (*tui.App, tcell.SimulationScreen) {
	t.Helper()
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(scr.Fini)
	scr.SetSize(w, h)
	app := tui.New(scr, theme.Load("groknight"), "test/free", "sess")
	app.AddSystemBlock("ready")
	go app.Run()
	t.Cleanup(app.Quit)
	return app, scr
}

// hudRow is the bottom painted line: the status row, right-aligned.
func hudRow(scr tcell.SimulationScreen) string {
	prim, w, _ := scr.GetContents()
	rows := len(prim) / w
	if rows == 0 {
		return ""
	}
	var b strings.Builder
	last := w * (rows - 1)
	for x := 0; x < w && last+x < len(prim); x++ {
		if r := prim[last+x].Runes; len(r) > 0 {
			b.WriteString(string(r))
		} else {
			b.WriteByte(' ')
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// awaitHUD waits for the status row to contain want, and returns it. The
// paint loop runs at ~30fps, so a scripted stream needs a frame or two to
// land; a missing segment is what the negative cases assert on instead.
func awaitHUD(t *testing.T, scr tcell.SimulationScreen, want string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var row string
	for time.Now().Before(deadline) {
		row = hudRow(scr)
		if want == "" || strings.Contains(row, want) {
			return row
		}
		time.Sleep(20 * time.Millisecond)
	}
	return row
}

// settle gives the paint loop a couple of frames to catch up with the last
// event, so a negative assertion cannot pass just because nothing repainted.
func settle() { time.Sleep(120 * time.Millisecond) }

// rateOf pulls the decode-rate figure off the row, or 0 when the segment is
// hidden. It matches on the "t/s" unit rather than a glyph: the leading icon
// is theme-owned (dsh's gauge, theme.HUDIcon) and a test that pinned a glyph
// would break the moment a theme overrode it.
func rateOf(t *testing.T, row string) float64 {
	t.Helper()
	i := strings.Index(row, "t/s")
	if i < 0 {
		return 0
	}
	// The number sits in the last space-separated field before the unit.
	fields := strings.Fields(row[:i])
	if len(fields) == 0 {
		return 0
	}
	var v float64
	if _, err := fmt.Sscanf(fields[len(fields)-1], "%f", &v); err != nil {
		t.Fatalf("rate is not a number: %q", row)
	}
	return v
}

// streamScript feeds a provider's event sequence to the hook layer, with the
// real gaps a stream has: deltas arrive over time, not pre-baked.
func streamScript(h *tuiHooks, gap time.Duration, kinds ...ai.EventType) {
	h.OnEvent(ai.Event{Type: ai.EventStart, Provider: "stub"})
	if len(kinds) > 0 && kinds[0] == ai.EventTextStart {
		h.OnEvent(ai.Event{Type: ai.EventTextStart})
	}
	for i, k := range kinds {
		if i > 0 {
			time.Sleep(gap)
		}
		switch k {
		case ai.EventTextDelta:
			h.OnEvent(ai.Event{Type: k, Delta: "tok "})
		case ai.EventToolcallDelta:
			h.OnEvent(ai.Event{Type: k, ToolCallID: "c1", PartialJSON: `{"path":"/tmp/x"}`})
		default:
			h.OnEvent(ai.Event{Type: k})
		}
	}
}

// TestHUDHooksMeasureTheMessage: a normal message — EventStart, deltas over
// time, EventDone with usage — must leave the token pill and a measured rate
// on the row. The token reading is the pill (total, dsh's UsagePill), so the
// assertion is the pill's own sum: 1,000 fresh + 300 out = 1.3k.
func TestHUDHooksMeasureTheMessage(t *testing.T) {
	app, scr := metricTestApp(t, 120, 24)
	h := &tuiHooks{ts: &tuiSession{app: app}}

	streamScript(h, 100*time.Millisecond,
		ai.EventTextStart, ai.EventTextDelta, ai.EventTextDelta, ai.EventTextDelta, ai.EventTextEnd)
	h.OnEvent(ai.Event{Type: ai.EventDone, StopReason: ai.StopReasonStop,
		Usage: &ai.Usage{Input: 1000, Output: 300, TotalTokens: 1300}})

	row := awaitHUD(t, scr, "t/s")
	if !strings.Contains(row, "1.3k") {
		t.Fatalf("the token pill did not render the message's total: %q", row)
	}
	// 300 output tokens over a ~200ms window is ~1500 t/s. Assert the order
	// of magnitude: the exact figure depends on scheduler jitter.
	if got := rateOf(t, row); got < 500 || got > 5000 {
		t.Fatalf("rate = %.1f t/s, want the message's own window (~1500): %q", got, row)
	}
}

// TestHUDHooksResetWindowOnNewMessage is the field report's defect. A turn
// that dies mid-stream never emits EventDone, so its window stays open; the
// next turn's EventStart must close it. Without that, this turn's tokens are
// divided by the dead one's elapsed time PLUS the retry backoff — measured
// under 1000 t/s where the real figure is over 5000, and wrong for the rest
// of the session.
func TestHUDHooksResetWindowOnNewMessage(t *testing.T) {
	app, scr := metricTestApp(t, 120, 24)
	h := &tuiHooks{ts: &tuiSession{app: app}}

	// Turn 1: streams, then dies. No EventDone.
	streamScript(h, 100*time.Millisecond,
		ai.EventTextStart, ai.EventTextDelta, ai.EventTextDelta, ai.EventTextDelta)
	h.OnEvent(ai.Event{Type: ai.EventError, Err: fmt.Errorf("stream reset")})

	// The recovery ladder waits, then turn 2 runs: 5 deltas at 50ms, a 200ms
	// window of its own — well over the 100ms floor, so it IS measurable.
	time.Sleep(400 * time.Millisecond)
	streamScript(h, 50*time.Millisecond,
		ai.EventTextStart, ai.EventTextDelta, ai.EventTextDelta, ai.EventTextDelta, ai.EventTextDelta, ai.EventTextDelta, ai.EventTextEnd)
	h.OnEvent(ai.Event{Type: ai.EventDone, StopReason: ai.StopReasonStop,
		Usage: &ai.Usage{Input: 10, Output: 300, TotalTokens: 310}})

	row := awaitHUD(t, scr, "t/s")
	// 300 tokens over 200ms is ~1500 t/s. A leaked window would add the dead
	// turn's ~200ms plus the 400ms retry gap: ~370 t/s.
	if got := rateOf(t, row); got < 700 {
		t.Fatalf("rate = %.1f t/s after a retried turn — the decode window leaked from the dead message, so 300 tokens were divided by the dead turn and the retry gap instead of this message's 200ms: %q", got, row)
	}
}

// TestHUDHooksCountToolDeltas: output_tokens includes tool-argument JSON. The
// window must span those deltas or every token the provider billed is divided
// by a window that stopped at the last text delta.
func TestHUDHooksCountToolDeltas(t *testing.T) {
	app, scr := metricTestApp(t, 120, 24)
	h := &tuiHooks{ts: &tuiSession{app: app}}

	streamScript(h, 50*time.Millisecond,
		ai.EventTextStart, ai.EventTextDelta,
		ai.EventToolcallDelta, ai.EventToolcallDelta, ai.EventToolcallDelta, ai.EventToolcallDelta)
	h.OnEvent(ai.Event{Type: ai.EventDone, StopReason: ai.StopReasonStop,
		Usage: &ai.Usage{Input: 20, Output: 300, TotalTokens: 320}})

	row := awaitHUD(t, scr, "t/s")
	// 300 tokens over ~200ms (text + argument deltas together) is ~1500 t/s.
	// Without the tool deltas in the window it would read ~6000.
	if got := rateOf(t, row); got < 500 || got > 3000 {
		t.Fatalf("rate = %.1f t/s, want the window to span the tool deltas too (~1500): %q", got, row)
	}
}

// TestHUDHooksUnmeasuredTurnHides: a turn with nothing to measure must not
// leave the previous turn's number on the row. A 1-token message has no
// window to divide by; showing the last turn's figure claims it measured
// this one.
func TestHUDHooksUnmeasuredTurnHides(t *testing.T) {
	app, scr := metricTestApp(t, 120, 24)
	h := &tuiHooks{ts: &tuiSession{app: app}}

	streamScript(h, 100*time.Millisecond,
		ai.EventTextStart, ai.EventTextDelta, ai.EventTextDelta, ai.EventTextEnd)
	h.OnEvent(ai.Event{Type: ai.EventDone, StopReason: ai.StopReasonStop,
		Usage: &ai.Usage{Input: 10, Output: 200, TotalTokens: 210}})
	if row := awaitHUD(t, scr, "t/s"); rateOf(t, row) <= 0 {
		t.Fatalf("setup: the measured turn drew no rate: %q", row)
	}

	// A single-delta, 1-token message: nothing to measure.
	streamScript(h, 0, ai.EventTextStart, ai.EventTextDelta, ai.EventTextEnd)
	h.OnEvent(ai.Event{Type: ai.EventDone, StopReason: ai.StopReasonStop,
		Usage: &ai.Usage{Input: 1, Output: 1, TotalTokens: 2}})
	settle()
	if row := hudRow(scr); strings.Contains(row, "t/s") {
		t.Fatalf("an unmeasurable turn left %q on the row; the segment must hide", row)
	}
}

// TestHUDHooksEmptyMessageDoesNotPanic: a provider that errors before its
// first delta still opens a message. The hook must survive EventStart
// followed immediately by EventDone with no usage at all.
func TestHUDHooksEmptyMessageDoesNotPanic(t *testing.T) {
	app, scr := metricTestApp(t, 120, 24)
	h := &tuiHooks{ts: &tuiSession{app: app}}
	h.OnEvent(ai.Event{Type: ai.EventStart, Provider: "stub"})
	h.OnEvent(ai.Event{Type: ai.EventDone, StopReason: ai.StopReasonStop})
	settle()
	if row := hudRow(scr); strings.Contains(row, "t/s") {
		t.Fatalf("an empty message drew %q", row)
	}
}

// TestResumedSessionKeepsStatusMetrics is the field report's defect: every
// metric on the status bar was wiped by Reset() at resume and only WORK TIME
// was re-banked from the replayed history, so a resumed session showed a
// fresh 0s, no tokens, and no spend for work it had already done and been
// charged for. Reset is the right boundary — the counters are per-session —
// but the replay has to put them back, and the row is where that is read.
func TestResumedSessionKeepsStatusMetrics(t *testing.T) {
	app, scr := metricTestApp(t, 120, 24)

	// What the live session banked before it closed, then what the resume
	// path replays: the same persisted usage, off the assistant messages.
	msgs := []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "first"}}},
		{Role: ai.RoleAssistant, DurationMS: 4000, Usage: &ai.Usage{
			Input: 479, Output: 1770, CacheRead: 64575, CacheWrite: 1100,
			TotalTokens: 66824, ReasoningTokens: 1264,
			Cost: &ai.UsageCost{Total: 0.0123},
		}},
		{Role: ai.RoleAssistant, DurationMS: 2500, Usage: &ai.Usage{
			Input: 500, Output: 100, TotalTokens: 600,
			Cost: &ai.UsageCost{Total: 0.001},
		}},
		// A tool call's span belongs to the work timer too: the live one
		// banks the whole run, so a resumed timer that counted only the
		// provider requests read at roughly half.
		{Role: ai.RoleToolResult, ToolCallID: "c1", ToolName: "bash", DurationMS: 9000,
			Content: []ai.Block{ai.TextBlock{Text: "ok"}}},
	}
	// Reset is the adoption boundary (/resume, a tab switch, a tree rewind):
	// everything goes, and the one replay path has to put it all back.
	app.Reset()
	replaySession(app, msgs)

	row := awaitHUD(t, scr, "1t·2g")
	// 979 in + 1870 out + 64575 cache + 1100 cache writes = 68524 tokens.
	if !strings.Contains(row, "68.5k") {
		t.Fatalf("the token pill lost the resumed session's total: %q", row)
	}
	// The cache-hit rate beside it: 64575 of the (979 + 64575) prompt side
	// the replayed history actually billed.
	if !strings.Contains(row, "98%") {
		t.Fatalf("the resumed cache-hit rate is gone: %q", row)
	}
	// 4000 + 2500 assistant spans + the 9000ms tool span = 15.5s of work.
	if !strings.Contains(row, "15s") {
		t.Fatalf("the work timer lost the resumed history's 15.5s: %q", row)
	}
	report := app.UsageReport()
	if !strings.Contains(report, "$0.0133") {
		t.Fatalf("the report lost the resumed session's spend:\n%s", report)
	}
}

// TestCompactionUsageReachesTheStatusRow is the TUI half of the
// compaction-billing fix. The summarize call is a provider request the user
// was billed for; before the fix its tokens reached no counter, so the token
// pill, the cache-hit rate and the spend segment all read as if compaction
// were free. The row is the surface a user actually reads, so it is what this
// drives — a unit test of App alone would pass with the hook unwired, which is
// exactly how the tool-delta case shipped.
func TestCompactionUsageReachesTheStatusRow(t *testing.T) {
	app, scr := metricTestApp(t, 120, 24)
	h := &tuiHooks{ts: &tuiSession{app: app}}
	h.OnCompaction(9_000)
	h.OnCompactionUsage(&ai.Usage{
		Input: 900, Output: 120, CacheRead: 40_000, TotalTokens: 41_020,
		Cost: &ai.UsageCost{Total: 0.0187},
	})

	row := awaitHUD(t, scr, "41k")
	if !strings.Contains(row, "41k") {
		t.Fatalf("the summarize call's tokens are not on the row: %q", row)
	}
	// 40_000 of the 40_900 prompt tokens came from cache.
	if !strings.Contains(row, "97%") {
		t.Fatalf("the cache-hit rate ignored the summarize: %q", row)
	}
	// The long form is where a human reconciles a bill, and its cache-hit
	// rate is the same ratio the row shows: 40_000 of 40_900 prompt tokens.
	report := app.UsageReport()
	if !strings.Contains(report, "$0.0187") {
		t.Fatalf("the report lost the compaction's spend:\n%s", report)
	}
	if !strings.Contains(report, "cache hit 97%") {
		t.Fatalf("the report's cache-hit rate ignored the summarize:\n%s", report)
	}
}
