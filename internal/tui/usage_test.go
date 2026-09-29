package tui

import (
	"strings"
	"testing"
	"time"
)

// TestUsageReportBlocks: /usage is the report the status row has no width
// for, so each line is here because a real number drives it. The two that
// had no home on the row at all are the cache HIT RATE (a ratio the split
// only means as) and the tool-call count — the request behind both is that
// they were unwritable anywhere.
func TestUsageReportBlocks(t *testing.T) {
	app, scr := drawnApp(t, 100, 40)
	// prompt 65054, cached 64575, completion 1770, reasoning 1264.
	app.AddUsage(65054-64575, 1770, 64575, 1264, 66824)
	app.AddCost(0.0123)
	app.SetContextWindow(200000)
	app.SetContextReplay(66824)
	app.AddLLMTime(103*time.Second, 1800)
	app.AddToolBlock("1", "bash", "")
	app.FinishTool("1", "bash", false, "ok", ToolOutcome{Dur: "60s", Elapsed: 60 * time.Second})
	app.AddToolBlock("2", "edit", "")
	app.FinishTool("2", "edit", true, "boom", ToolOutcome{Dur: "1s", Elapsed: time.Second})
	app.SetWork(2 * time.Minute)
	app.draw()

	report := app.UsageReport()
	for _, want := range []string{
		"Token usage",
		"66,824 tok",    // the provider's own total for the request
		"cache hit 99%", // 64575 of 65054 prompt tokens
		"uncached input 479 tok",
		"cached input 64,575 tok",
		"output 1,770 tok",
		"reasoning 1,264 tok", // the reasoning is inside output, said so
		"$0.0123",
		"Session statistics",
		"LLM time 1m43s",
		"tool time 1m01s",
		"avg time to first token 1.8s",
		"tool calls 2 (1 failed)",
		"context 66.8k/200k (33%)",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}
	_ = scr
}

// TestUsageReportHidesWhatItCannotKnow: a zero line is a claim about a
// measurement, not a fact. A session with no cost, no finished turn and no
// tool call says nothing about any of them — an unwired cost drawn as $0.00
// is how a free provider starts reading as a billing bug.
func TestUsageReportHidesWhatItCannotKnow(t *testing.T) {
	app, _ := drawnApp(t, 100, 24)
	app.draw()

	report := app.UsageReport()
	for _, unwanted := range []string{"cost", "avg time to first token", "tool calls", "LLM time 0s"} {
		if strings.Contains(report, unwanted) {
			t.Errorf("empty session drew %q:\n%s", unwanted, report)
		}
	}
	// A session that only streamed tokens still reports the token block —
	// the hit rate hides with its denominator, not the totals.
	app.AddUsage(100, 0, 0, 0, 100)
	report = app.UsageReport()
	if !strings.Contains(report, "100 tok") || strings.Contains(report, "cache hit") {
		t.Errorf("no-cache session:\n%s", report)
	}
}

// TestUsageReportClearedByReset: /usage reports the session, so a
// /resume or /fork that left the previous session's totals behind would
// draw them next to the freshly replayed transcript — the exact defect
// the HUD's Reset clear was raised for.
func TestUsageReportClearedByReset(t *testing.T) {
	app, _ := drawnApp(t, 100, 24)
	app.AddUsage(5000, 500, 4000, 0, 9500)
	app.AddLLMTime(30*time.Second, 900)
	app.AddToolBlock("1", "bash", "")
	app.FinishTool("1", "bash", true, "boom", ToolOutcome{Dur: "5s", Elapsed: 5 * time.Second})
	app.SetWork(time.Minute)

	app.Reset()

	report := app.UsageReport()
	if strings.Contains(report, "9,500") || strings.Contains(report, "tool calls 1") ||
		strings.Contains(report, "30s") {
		t.Fatalf("the previous session's totals survived Reset:\n%s", report)
	}
}

// TestGroupTokens pins the grouping: a report number is read, not parsed.
func TestGroupTokens(t *testing.T) {
	for in, want := range map[int64]string{
		0: "0", 999: "999", 1000: "1,000", 1016717: "1,016,717",
		200000: "200,000", -1234567: "-1,234,567",
	} {
		if got := groupTokens(in); got != want {
			t.Errorf("groupTokens(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestStatusRowCarriesTheUsageMetrics pins the two dsh readings on the row
// itself, not only in /usage: the hit rate and the call count are the two
// figures the one-glance row exists for, and a report the user has to type
// to read is not where a glance goes.
func TestStatusRowCarriesTheUsageMetrics(t *testing.T) {
	app, scr := drawnApp(t, 200, 24)
	// 479 fresh + 64575 cached + 1770 out, two calls of which one failed.
	app.AddUsage(479, 1770, 64575, 0, 66824)
	app.AddToolBlock("1", "bash", "")
	app.FinishTool("1", "bash", false, "ok", ToolOutcome{Elapsed: 2 * time.Second})
	app.AddToolBlock("2", "edit", "")
	app.FinishTool("2", "edit", true, "boom", ToolOutcome{Elapsed: time.Second})
	app.SetContextWindow(200000)
	app.draw()

	row := lastRow(screenText(scr))
	for _, want := range []string{
		"cache 99%", // dsh's hit RATE — the total is the tokens segment's job
		"ctx 66.8k/200k",
		"✳2 calls (1 failed)",
	} {
		if !strings.Contains(row, want) {
			t.Errorf("status row missing %q: %q", want, row)
		}
	}
}

// TestStatusRowHidesUnmeasuredUsageMetrics: a zero line is a claim about a
// measurement. A provider that bills no cache, and a session that has
// called no tool, must keep the plain row — 0% cache and "0 calls" are
// readings nobody made.
func TestStatusRowHidesUnmeasuredUsageMetrics(t *testing.T) {
	app, scr := drawnApp(t, 200, 24)
	app.AddUsage(1200, 340, 0, 0, 1540)
	app.draw()

	row := lastRow(screenText(scr))
	if strings.Contains(row, "cache") || strings.Contains(row, "calls") {
		t.Fatalf("an unmeasured metric drew a glyph: %q", row)
	}
	if !strings.Contains(row, "▤↑1.2k │ ↓340") {
		t.Fatalf("the token split changed: %q", row)
	}
}

// TestUsageReportOmitsAnImpossibleShare: tool time and active time are two
// differently-measured spans (a bang-mode call is tool time with no run span
// to bank it into), so their ratio can exceed 100%. Drawing "113% of active
// time" is a claim about a denominator that is wrong; the line is omitted
// instead of clamped, because a clamped 100% is the same lie in nicer clothes.
func TestUsageReportOmitsAnImpossibleShare(t *testing.T) {
	app, _ := drawnApp(t, 200, 24)
	app.AddUsage(10, 10, 0, 0, 20)
	app.AddToolBlock("1", "bash", "")
	app.FinishTool("1", "bash", false, "ok", ToolOutcome{Elapsed: 2 * time.Second})
	// No SetWork: an out-of-run call leaves the active total at zero, and even
	// a tiny banked span leaves the share over 100%.
	app.SetWork(time.Second)

	report := app.UsageReport()
	if strings.Contains(report, "of active time") {
		t.Fatalf("an impossible share was drawn:\n%s", report)
	}
	if !strings.Contains(report, "tool calls 1") {
		t.Fatalf("the call count must survive the omitted share:\n%s", report)
	}
	// A sane ratio is still shown: the guard is on the ratio, not on zero.
	app.Reset()
	app.AddToolBlock("1", "bash", "")
	app.FinishTool("1", "bash", false, "ok", ToolOutcome{Elapsed: time.Second})
	app.SetWork(time.Minute)
	if !strings.Contains(app.UsageReport(), "of active time") {
		t.Fatalf("a sane share must still be drawn:\n%s", app.UsageReport())
	}
}

// TestCacheSegmentCarriesNoSecondTotal: the row drew the session total
// twice — the tokens segment's ↑⇢↓ split, then "▤66.3k · cache 100%" beside
// it, a number the split already sums to. dsh can afford that (its pill is
// its ONLY token reading); a row that already shows the split cannot. The
// cache segment now reports the RATE alone, with no icon either: the ▤ is
// already on the segment this one refines. The exact total stays where a
// grouped figure belongs — /usage.
func TestCacheSegmentCarriesNoSecondTotal(t *testing.T) {
	app, scr := drawnApp(t, 200, 24)
	app.AddUsage(479, 1770, 64575, 0, 66824)
	app.draw()

	if got, _ := app.hudSegment("cache"); got != "cache 99%" {
		t.Fatalf("the cache segment = %q, want the rate alone (no total, no second ▤)", got)
	}
	row := lastRow(screenText(scr))
	if strings.Contains(row, "66.8k") {
		t.Fatalf("the cache segment re-printed the total the split already sums: %q", row)
	}
	// One ▤ on the row, not two: the database icon belongs to the split.
	if n := strings.Count(row, "▤"); n != 1 {
		t.Fatalf("the database icon is drawn %d times: %q", n, row)
	}
	if !strings.Contains(row, "▤↑479 ⇢64.6k │ ↓1.8k") || !strings.Contains(row, "cache 99%") {
		t.Fatalf("the row lost a reading: %q", row)
	}
}
