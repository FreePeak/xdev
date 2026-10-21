package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

// Live 2026-10-21 (session 8efefc9e): a "tell me where we are" question ran
// 113 turns and 182 tool calls over 17 minutes and produced no answer. The
// cause was this tier: it cleared every tool result older than the newest
// five on EVERY step boundary, so the model watched its own findings turn
// into placeholders and re-ran the commands to get them back. 183 of 183
// tool calls in that run were issued at a point where an earlier result from
// the same tool had already been cleared.
//
// The tier is a preparation for the compaction ladder. It runs under
// pressure, not on every boundary.
func TestMicrocompactDoesNotRunWhenTheWindowHasRoom(t *testing.T) {
	stubPressure(t, 0)
	p := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{textEvent("done"), doneEvent("done")}},
	}}
	// A 1M window: threshold 800k, micro-compaction bar 400k. The history is
	// a handful of kilobytes — nowhere near either bar.
	cfg := CompactionConfig{ContextWindow: 1_000_000, KeepRecentTokens: 20_000}
	a, _, _ := storeAgent(t, p, cfg)
	a.Retry = fastRetry()

	history := []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hello"}}},
	}
	for i := 0; i < 12; i++ {
		history = append(history, ai.Message{
			Role:     ai.RoleToolResult,
			ToolName: "bash",
			Content:  []ai.Block{ai.TextBlock{Text: strings.Repeat("out", 2000)}},
		})
	}
	out := a.maybeCompact(context.Background(), history)
	for i, m := range out {
		if strings.Contains(m.Text(), "cleared from the context") {
			t.Fatalf("result %d was cleared with room in the window: %q", i, m.Text())
		}
	}
}

// Under pressure the tier still runs: the request has to be bounded.
func TestMicrocompactRunsUnderPressure(t *testing.T) {
	stubPressure(t, 0)
	p := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{textEvent("done"), doneEvent("done")}},
	}}
	// A 200k window: threshold 160k, micro-compaction bar 80k. The history
	// below is well past the bar.
	cfg := CompactionConfig{ContextWindow: 200_000, KeepRecentTokens: 20_000}
	a, _, _ := storeAgent(t, p, cfg)
	a.Retry = fastRetry()

	history := []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hello"}}},
	}
	for i := 0; i < 20; i++ {
		history = append(history, ai.Message{
			Role:     ai.RoleToolResult,
			ToolName: "bash",
			Content:  []ai.Block{ai.TextBlock{Text: strings.Repeat("out", 20000)}},
		})
	}
	out := a.maybeCompact(context.Background(), history)
	cleared := 0
	for _, m := range out {
		if strings.Contains(m.Text(), "cleared from the context") {
			cleared++
		}
	}
	if cleared == 0 {
		t.Fatal("expected the tier to clear stale results under pressure")
	}
}

// The trigger must be read on the history as it is. It used to be read AFTER
// the tier had cleared the stale results, so a run whose real content was far
// past the threshold saw a small history and the ladder never ran — the tier
// silently suppressed the very compaction it exists to prepare for.
func TestCompactionTriggerIsReadBeforeTheTierClears(t *testing.T) {
	stubPressure(t, 0)
	p := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{textEvent("summary"), doneEvent("summary")}},
	}}
	// A window small enough that the real content is past the threshold.
	cfg := CompactionConfig{ContextWindow: 20_000, KeepRecentTokens: 1_000}
	a, s, _ := storeAgent(t, p, cfg)
	a.Retry = fastRetry()

	appendMsg(t, s, userText("investigate"))
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("c%d", i)
		appendMsg(t, s, callMsg(id, "bash", `{"command":"true"}`))
		appendMsg(t, s, resultMsg(id, "bash", strings.Repeat("out", 1000)))
	}
	history := ladderHistory(t, s)
	if !a.compactionDue(history) {
		t.Fatalf("precondition: the real content must be past the threshold (tokens=%d threshold=%d)",
			contextTokens(history), a.compaction().threshold())
	}
	_ = a.maybeCompact(context.Background(), history)
	// The tier's clearing must not have made the ladder look unnecessary:
	// the summary call has to have run.
	if len(p.gotReqs) < 1 {
		t.Fatalf("provider calls = %d, want the summary call to have run", len(p.gotReqs))
	}
	if !strings.Contains(p.gotReqs[0].Messages[0].Text(), "Conversation transcript") {
		t.Fatalf("the call is not the summary: %q", p.gotReqs[0].Messages[0].Text()[:120])
	}
}
