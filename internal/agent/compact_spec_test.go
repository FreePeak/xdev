package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// The speculative compaction arm (omp's speculative compaction): the
// background summarize is armed BEFORE the threshold is due — inside the
// lead window — and its result is consumed at the boundary where the
// compaction becomes due.

// specConfig is a compaction config with a real window: 100_000 tokens, a
// threshold of 80% of it (80_000) and a lead window of 10_000 tokens (omp's
// oF: min(32000, max(8192, floor(80000 * 0.125)))).
func specConfig(methods ...string) CompactionConfig {
	return CompactionConfig{
		ContextWindow:    100_000,
		KeepRecentTokens: 5,
		ThresholdPercent: 80,
		Methods:          methods,
	}
}

// bigText appends one old user message of about `tokens` estimated tokens —
// the history a compaction summarizes.
func bigText(t *testing.T, s *session.Store, tokens int64) {
	t.Helper()
	appendMsg(t, s, userText(strings.Repeat("x", int(tokens*4))))
}

// specFixture persists one big old message of about `tokens` tokens and the
// tail question that survives the compaction, and returns the history.
func specFixture(t *testing.T, s *session.Store, tokens int64) []ai.Message {
	t.Helper()
	bigText(t, s, tokens)
	appendMsg(t, s, userText("keep this tail question"))
	return ladderHistory(t, s)
}

// waitPending runs boundaries until the armed result lands and returns the
// history. The boundaries it runs are not due, so nothing is applied.
func waitPending(t *testing.T, a *Agent, hist []ai.Message) []ai.Message {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		hist = a.maybeCompact(context.Background(), hist)
		if job := a.compactAsync; job != nil && job.pending != nil {
			return hist
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the armed result never landed")
	return hist
}

// waitCompaction runs boundaries until `want` compactions are recorded.
func waitCompaction(t *testing.T, a *Agent, s *session.Store, hist []ai.Message, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(compactionEntries(s)) < want && time.Now().Before(deadline) {
		_ = a.maybeCompact(context.Background(), hist)
		time.Sleep(time.Millisecond)
	}
	if n := len(compactionEntries(s)); n < want {
		t.Fatalf("compaction entries = %d, want %d", n, want)
	}
}

// TestLeadWindowMatchesTheOmpFormula pins the lead window: omp's
// oF(n) = min(32000, max(8192, floor(n * 0.125))).
func TestLeadWindowMatchesTheOmpFormula(t *testing.T) {
	tests := []struct{ threshold, want int64 }{
		{0, 8192},
		{1000, 8192},
		{65536, 8192},
		{108800, 13600},
		{1000000, 32000},
	}
	for _, tc := range tests {
		if got := leadWindow(tc.threshold); got != tc.want {
			t.Fatalf("leadWindow(%d) = %d, want %d", tc.threshold, got, tc.want)
		}
	}
}

// TestTheArmStartsInsideTheLeadWindow pins the arm's timing: a boundary
// that is NOT due but sits inside the lead window below the threshold arms
// the background summarize — and compacts nothing.
func TestTheArmStartsInsideTheLeadWindow(t *testing.T) {
	stubPressure(t, 0)
	p := newLadderProvider()
	cfg := specConfig("handoff")
	cfg.Async = true
	a, s := asyncAgent(t, p, cfg)
	hist := specFixture(t, s, 75000)
	used := contextTokens(hist)
	threshold := a.compaction().threshold()
	if used >= threshold || threshold-used > leadWindow(threshold) {
		t.Fatalf("the fixture is not inside the lead window: used=%d threshold=%d", used, threshold)
	}

	_ = a.maybeCompact(context.Background(), hist)
	if a.compactAsync == nil {
		t.Fatal("the boundary did not arm inside the lead window")
	}
	waitChan(t, "the background summarize", p.started)
	if n := len(compactionEntries(s)); n != 0 {
		t.Fatalf("the arm must not compact, entries = %d", n)
	}
}

// TestTheArmDoesNotStartOutsideTheLeadWindow is the other side of the same
// rule: a context far below the threshold arms nothing, so no provider call
// is spent on a session that is not about to compact.
func TestTheArmDoesNotStartOutsideTheLeadWindow(t *testing.T) {
	stubPressure(t, 0)
	p := newLadderProvider()
	cfg := specConfig("handoff")
	cfg.Async = true
	a, s := asyncAgent(t, p, cfg)
	hist := specFixture(t, s, 60000)
	used := contextTokens(hist)
	threshold := a.compaction().threshold()
	if threshold-used <= leadWindow(threshold) {
		t.Fatalf("the fixture is inside the lead window: used=%d threshold=%d", used, threshold)
	}

	_ = a.maybeCompact(context.Background(), hist)
	if a.compactAsync != nil {
		t.Fatal("the boundary armed outside the lead window")
	}
	if _, summaries, _ := p.counts(); summaries != 0 {
		t.Fatalf("summarize calls = %d, want none", summaries)
	}
}

// TestTheArmedResultAppliesWhenTheCompactionBecomesDue is the point of the
// whole change: the summary is produced BEFORE the threshold is crossed, so
// the boundary that crosses it applies it without a provider round-trip.
func TestTheArmedResultAppliesWhenTheCompactionBecomesDue(t *testing.T) {
	stubPressure(t, 0)
	p := newLadderProvider()
	cfg := specConfig("handoff")
	cfg.Async = true
	a, s := asyncAgent(t, p, cfg)
	hist := specFixture(t, s, 75000)
	ctx := context.Background()

	_ = a.maybeCompact(ctx, hist)
	if a.compactAsync == nil {
		t.Fatal("the boundary did not arm")
	}
	waitChan(t, "the background summarize", p.started)
	close(p.released)

	// The context crosses the threshold.
	bigText(t, s, 10000)
	hist = ladderHistory(t, s)
	waitCompaction(t, a, s, hist, 1)

	entries := compactionEntries(s)
	if len(entries) != 1 {
		t.Fatalf("compaction entries = %d, want exactly one", len(entries))
	}
	if !strings.Contains(entries[0].Summary.Text(), "SUMMARY-FROM-ASYNC") {
		t.Fatalf("summary = %q, want the armed answer", entries[0].Summary.Text())
	}
	if entries[0].Method != MethodHandoff {
		t.Fatalf("method = %q, want %q", entries[0].Method, MethodHandoff)
	}
	if _, summaries, _ := p.counts(); summaries != 1 {
		t.Fatalf("summarize calls = %d, want exactly one (the arm)", summaries)
	}
}

// TestTheArmedResultIsNotAppliedBelowTheThreshold is the behaviour change
// against the old async trigger: an armed result waits for the boundary
// where the compaction is DUE — it is not applied at the next boundary
// regardless.
func TestTheArmedResultIsNotAppliedBelowTheThreshold(t *testing.T) {
	stubPressure(t, 0)
	p := newLadderProvider()
	cfg := specConfig("handoff")
	cfg.Async = true
	a, s := asyncAgent(t, p, cfg)
	hist := specFixture(t, s, 75000)
	ctx := context.Background()

	_ = a.maybeCompact(ctx, hist)
	waitChan(t, "the background summarize", p.started)
	close(p.released)
	hist = waitPending(t, a, hist)

	if n := len(compactionEntries(s)); n != 0 {
		t.Fatalf("the armed result applied below the threshold, entries = %d", n)
	}
	if _, summaries, _ := p.counts(); summaries != 1 {
		t.Fatalf("summarize calls = %d, want exactly one (the arm)", summaries)
	}
}

// TestTheArmedResultIsDroppedWhenTheSessionBranchesAway: a session that
// branches away from the snapshot leaves the summary describing history the
// model can no longer see — the arm is dropped and the boundary compacts
// synchronously.
//
// The branch lands ON the anchor, so only the snapshot rule can catch it:
// the anchor is still on the active path, the leaf the arm snapshotted is
// not.
func TestTheArmedResultIsDroppedWhenTheSessionBranchesAway(t *testing.T) {
	stubPressure(t, 0)
	p := newLadderProvider()
	cfg := specConfig("handoff")
	cfg.Async = true
	cfg.KeepRecentTokens = 20
	a, s := asyncAgent(t, p, cfg)
	bigText(t, s, 5000)
	bigText(t, s, 70000)
	appendMsg(t, s, userText(strings.Repeat("m", 60))) // the anchor
	appendMsg(t, s, userText("keep this tail question"))
	hist := ladderHistory(t, s)
	ctx := context.Background()

	_ = a.maybeCompact(ctx, hist)
	waitChan(t, "the background summarize", p.started)
	close(p.released)
	hist = waitPending(t, a, hist)

	// The session branches away from the snapshot leaf, onto the anchor.
	branchTo := s.Entries()[2].Envelope().ID
	if err := s.Branch(branchTo); err != nil {
		t.Fatal(err)
	}
	// The context grows past the threshold again.
	bigText(t, s, 10000)
	hist = ladderHistory(t, s)
	waitCompaction(t, a, s, hist, 1)

	if _, summaries, _ := p.counts(); summaries != 2 {
		t.Fatalf("summarize calls = %d, want two (the dropped arm and the fallback)", summaries)
	}
}

// TestTheArmedResultIsDroppedWhenTheContextIsRebuiltAfterTheArm: a
// compaction written after the arm rebuilds the context, so the armed
// summary describes the old one.
func TestTheArmedResultIsDroppedWhenTheContextIsRebuiltAfterTheArm(t *testing.T) {
	stubPressure(t, 0)
	p := newLadderProvider()
	cfg := specConfig("handoff")
	cfg.Async = true
	a, s := asyncAgent(t, p, cfg)
	hist := specFixture(t, s, 75000)
	ctx := context.Background()

	_ = a.maybeCompact(ctx, hist)
	waitChan(t, "the background summarize", p.started)
	close(p.released)
	hist = waitPending(t, a, hist)

	// Another compaction rewrites the context after the arm.
	kept := s.Entries()[0].Envelope().ID
	if err := s.Append(&session.CompactionEntry{
		Summary:          textSummary("an earlier compaction"),
		FirstKeptEntryID: &kept,
	}); err != nil {
		t.Fatal(err)
	}
	bigText(t, s, 10000)
	hist = ladderHistory(t, s)
	waitCompaction(t, a, s, hist, 2)

	if _, summaries, _ := p.counts(); summaries != 2 {
		t.Fatalf("summarize calls = %d, want two (the dropped arm and the fallback)", summaries)
	}
}

// TestTheArmedResultIsDroppedWhenItsAnchorLeavesTheActivePath: the kept
// window must still resolve, or the compaction would keep nothing.
func TestTheArmedResultIsDroppedWhenItsAnchorLeavesTheActivePath(t *testing.T) {
	stubPressure(t, 0)
	p := newLadderProvider()
	cfg := specConfig("handoff")
	cfg.Async = true
	a, s := asyncAgent(t, p, cfg)
	hist := specFixture(t, s, 75000)
	ctx := context.Background()

	_ = a.maybeCompact(ctx, hist)
	waitChan(t, "the background summarize", p.started)
	close(p.released)
	hist = waitPending(t, a, hist)

	// The anchor leaves the active path: the compaction that follows it
	// keeps a different window, so the armed summary anchors nowhere.
	job := a.compactAsync
	if job == nil {
		t.Fatal("the arm was dropped before its anchor could break")
	}
	job.anchor = "an-entry-that-is-not-on-the-path"
	bigText(t, s, 10000)
	hist = ladderHistory(t, s)
	waitCompaction(t, a, s, hist, 1)

	if _, summaries, _ := p.counts(); summaries != 2 {
		t.Fatalf("summarize calls = %d, want two (the dropped arm and the fallback)", summaries)
	}
}

// TestTheArmedResultIsDroppedWhenTheCompactedContextWouldNotFit: the
// compacted context (plus what is pending) must fit the head-room the
// threshold leaves — floor(threshold * 0.8). The fixture keeps 70_000
// tokens, so the compaction would leave the context over the limit.
func TestTheArmedResultIsDroppedWhenTheCompactedContextWouldNotFit(t *testing.T) {
	stubPressure(t, 0)
	logs := captureLogs(t)
	p := newLadderProvider()
	cfg := specConfig("handoff")
	cfg.Async = true
	cfg.KeepRecentTokens = 70000
	a, s := asyncAgent(t, p, cfg)
	bigText(t, s, 5000)
	bigText(t, s, 70000)
	appendMsg(t, s, userText("keep this tail question"))
	hist := ladderHistory(t, s)
	ctx := context.Background()

	_ = a.maybeCompact(ctx, hist)
	waitChan(t, "the background summarize", p.started)
	close(p.released)
	hist = waitPending(t, a, hist)

	bigText(t, s, 10000)
	hist = ladderHistory(t, s)
	waitCompaction(t, a, s, hist, 1)

	if !strings.Contains(logs.String(), "would not fit") {
		t.Fatalf("the arm was not dropped for head-room: %s", logs.String())
	}
	if _, summaries, _ := p.counts(); summaries != 2 {
		t.Fatalf("summarize calls = %d, want two (the dropped arm and the fallback)", summaries)
	}
}

// TestTheArmedResultIsDroppedWhenItWouldNotShrinkTheContext: a compaction
// that does not shrink the context is pointless. The summary is bigger than
// the history it replaces, so the compaction would grow the context.
func TestTheArmedResultIsDroppedWhenItWouldNotShrinkTheContext(t *testing.T) {
	pressure := 0.0
	stubPressureValue(t, &pressure)
	logs := captureLogs(t)
	p := newLadderProvider()
	p.summary = strings.Repeat("S", 24000) // ~6000 tokens
	cfg := CompactionConfig{ContextWindow: 20000, KeepRecentTokens: 5, ThresholdTokens: 10000, Methods: []string{"handoff"}, Async: true}
	a, s := asyncAgent(t, p, cfg)
	hist := specFixture(t, s, 5000)
	ctx := context.Background()

	_ = a.maybeCompact(ctx, hist)
	waitChan(t, "the background summarize", p.started)
	close(p.released)
	hist = waitPending(t, a, hist)

	// The memory pressure makes the boundary due without the token
	// threshold: the context is under 80% of the threshold, so the
	// head-room rule passes and the no-shrink rule is the one that bites.
	pressure = 1.0
	hist = ladderHistory(t, s)
	waitCompaction(t, a, s, hist, 1)

	if !strings.Contains(logs.String(), "would not shrink") {
		t.Fatalf("the arm was not dropped for the no-shrink rule: %s", logs.String())
	}
	if _, summaries, _ := p.counts(); summaries != 2 {
		t.Fatalf("summarize calls = %d, want two (the dropped arm and the fallback)", summaries)
	}
}

// TestAFailedArmFallsBackToTheSynchronousCompaction: a summarize that
// fails leaves the boundary with nothing to apply, so it compacts
// synchronously and the session still holds exactly one compaction.
func TestAFailedArmFallsBackToTheSynchronousCompaction(t *testing.T) {
	stubPressure(t, 0)
	p := newLadderProvider()
	p.failFirst = true
	cfg := specConfig("handoff")
	cfg.Async = true
	a, s := asyncAgent(t, p, cfg)
	hist := specFixture(t, s, 75000)
	ctx := context.Background()

	_ = a.maybeCompact(ctx, hist)
	waitChan(t, "the background summarize", p.started)
	close(p.released)

	// The context crosses the threshold: the boundary that finds the failed
	// arm has nothing to apply and compacts synchronously.
	bigText(t, s, 10000)
	hist = ladderHistory(t, s)
	waitCompaction(t, a, s, hist, 1)

	if n := len(compactionEntries(s)); n != 1 {
		t.Fatalf("compaction entries = %d, want exactly one", n)
	}
	if _, summaries, _ := p.counts(); summaries != 2 {
		t.Fatalf("summarize calls = %d, want two (the failed arm and the fallback)", summaries)
	}
}

// TestTheDefaultInstallNeverArms is the shipped default: compaction.async
// is off, so nothing is armed, no provider call is spent in the
// background, and the boundary that crosses the threshold compacts
// synchronously — exactly as before this change.
func TestTheDefaultInstallNeverArms(t *testing.T) {
	stubPressure(t, 0)
	p := newLadderProvider()
	cfg := specConfig("handoff")
	a, s := asyncAgent(t, p, cfg)
	hist := specFixture(t, s, 75000)
	ctx := context.Background()
	// The synchronous compaction answers at once.
	close(p.released)

	_ = a.maybeCompact(ctx, hist)
	if a.compactAsync != nil {
		t.Fatal("the default install armed a background summarize")
	}
	if _, summaries, _ := p.counts(); summaries != 0 {
		t.Fatalf("summarize calls = %d, want none", summaries)
	}

	// The boundary that crosses the threshold compacts synchronously.
	bigText(t, s, 10000)
	hist = ladderHistory(t, s)
	waitCompaction(t, a, s, hist, 1)
	if _, summaries, _ := p.counts(); summaries != 1 {
		t.Fatalf("summarize calls = %d, want exactly one (the synchronous compaction)", summaries)
	}
}

// stubPressureValue pins the memory pressure to a value the test moves.
func stubPressureValue(t *testing.T, p *float64) {
	t.Helper()
	old := memPressure
	memPressure = func() float64 { return *p }
	t.Cleanup(func() { memPressure = old })
}
