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
