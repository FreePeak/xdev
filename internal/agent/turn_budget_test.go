package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
)

// A turn may not outlive its budget. Under the shipped defaults
// (retry.infinite and retry.retryAllErrors are both default-ON, and
// internal/config's settings merge is one-way so a later layer can never
// switch them off) three recovery loops had no ceiling at all: the
// empty-turn retry, the retain-and-continue retry, and the two
// all-targets-down escalations. Any of them pins the TUI's single turn
// claim (cmd/xdev/tui.go's `running`) and every later submit is refused
// with "a turn is already running" — the wedged session a7e17741
// (2026-09-28), whose file holds a toolResult at 22:49:13 and a user
// "continue" at 23:42:21 with nothing in between.

// The empty-turn recovery is the loop a7e17741 was almost certainly in: a
// model that answers nothing past the two nudges was re-asked every backoff
// for the life of the process, writing nothing (every announcement is a
// display-only AddSystemBlock).
func TestEmptyTurnRecoveryIsBoundedUnderRetryAllErrors(t *testing.T) {
	blank := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.ThinkingBlock{Thinking: "(context elided)"}}}
	blankScript := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, blank)}}
	// Enough blanks to prove the loop stops rather than draining the script:
	// a run that loops past maxEmptyTurnRecoveries keeps calling and ends on
	// "script exhausted" instead of ErrEmptyTurn.
	calls := make([]fakeScript, maxEmptyTurnNudges+maxEmptyTurnRecoveries+4)
	for i := range calls {
		calls[i] = blankScript
	}
	p := &fakeProvider{calls: calls}
	a, s, _ := storeAgent(t, p, CompactionConfig{})
	a.Retry = RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, RetryAllErrors: true}
	_, err := a.Run(context.Background(), "sys", submitHistory(t, s, "hi"))
	if !errors.Is(err, ErrEmptyTurn) {
		t.Fatalf("an unanswering model must end the turn with ErrEmptyTurn, got %v", err)
	}
	// 1 initial + maxEmptyTurnNudges nudges + maxEmptyTurnRecoveries
	// recoveries: the cap bounds the retry itself, so the last call is the
	// one that reports the failure rather than another retry.
	want := maxEmptyTurnNudges + maxEmptyTurnRecoveries + 1
	if n := len(p.gotReqs); n != want {
		t.Fatalf("stream requests = %d, want %d: the empty-turn recovery must be bounded", n, want)
	}
}

// The retain-and-continue budget: a stream that cuts off after visible text
// cannot be replayed, so recovery resumes from the partial. That is the one
// recovery with a hard bound — and the bound has to apply on the infinite
// ladder too, or a gateway that always cuts off mid-message pins the turn
// forever.
func TestPostContentContinuationIsBoundedUnderInfiniteRetry(t *testing.T) {
	// Every script: some text, then a stream error. Nothing ever answers.
	cut := fakeScript{events: []ai.Event{
		{Type: ai.EventTextStart}, textEvent("partial "),
		ai.Errorf(errors.New("connection reset by peer")),
	}}
	calls := make([]fakeScript, maxPostContentContinuations+4)
	for i := range calls {
		calls[i] = cut
	}
	p := &fakeProvider{calls: calls}
	a, s, _ := storeAgent(t, p, CompactionConfig{})
	a.Retry = RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Infinite: true}
	_, err := a.Run(context.Background(), "sys", submitHistory(t, s, "hi"))
	if err == nil {
		t.Fatal("a stream that never completes must end the turn with an error")
	}
	// One call per retain-and-continue round, plus the final failing call
	// that surfaces the error.
	if n := len(p.gotReqs); n > maxPostContentContinuations+1 {
		t.Fatalf("stream requests = %d, want at most %d: retain-and-continue must be bounded on the infinite ladder",
			n, maxPostContentContinuations+1)
	}
}

// The all-targets-down escalation, same contract: retry.infinite means "an
// outage of any length is survived", not "no turn ever ends". A host that
// never comes back must surface the error rather than hold the turn claim.
func TestSilentRecoveryEscalationIsBoundedUnderInfiniteRetry(t *testing.T) {
	down := fakeScript{err: errors.New("dial tcp 127.0.0.1:8080: connect: connection refused")}
	calls := make([]fakeScript, maxSilentRecoveryRounds*(oneShotRetry().MaxRetries+1)+4)
	for i := range calls {
		calls[i] = down
	}
	p := &fakeProvider{calls: calls}
	a, s, _ := storeAgent(t, p, CompactionConfig{})
	a.Retry = RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Infinite: true}
	_, err := a.Run(context.Background(), "sys", submitHistory(t, s, "hi"))
	if err == nil {
		t.Fatal("a host that never answers must end the turn with an error")
	}
	// Each escalation round spends the whole bounded ladder first
	// (MaxRetries+1 calls) after one opening call, and the last round is the
	// one that surfaces the error rather than starting another.
	wantMax := 1 + maxSilentRecoveryRounds*(oneShotRetry().MaxRetries+1) + 1
	if n := len(p.gotReqs); n > wantMax {
		t.Fatalf("stream requests = %d, want at most %d: the escalation ladder must be bounded under retry.infinite", n, wantMax)
	}
}

// The health-check probe is the same loop one layer up, and the one most
// likely to see a host that is simply gone. It gets the same ceiling.
func TestHealthCheckEscalationIsBoundedUnderInfiniteRetry(t *testing.T) {
	health := make([]error, maxSilentRecoveryRounds+8)
	for i := range health {
		health[i] = errors.New("connection refused")
	}
	p := &scriptedHealthProvider{
		fakeProvider: &fakeProvider{calls: []fakeScript{{events: []ai.Event{doneEvent("never reached")}}}},
		health:       health,
	}
	a, s, _ := storeAgent(t, p.fakeProvider, CompactionConfig{})
	a.Provider = p
	a.Retry = RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Infinite: true}
	_, err := a.Run(context.Background(), "sys", submitHistory(t, s, "hi"))
	if err == nil {
		t.Fatal("a health check that never recovers must end the turn with an error")
		// One opening probe, then at most maxSilentRecoveryRounds retries, then
		// the failing probe that reports it.
		if n := p.healthCalls(); n > maxSilentRecoveryRounds+1 {
			t.Fatalf("health probes = %d, want at most %d: the probe escalation must be bounded under retry.infinite",
				n, maxSilentRecoveryRounds+1)
		}
	}
}

// The bound must not shorten the outage Infinite exists to survive: a host
// that is down for longer than the bounded ladder but still inside
// maxSilentRecoveryRounds must still be recovered from, with no user
// involvement. (The pre-existing TestInfiniteSurvivesWhatBoundedRoundsCannot
// covers the short side of the same contract.)
func TestBoundedSilentRecoveryStillSurvivesALongOutage(t *testing.T) {
	p := &fakeProvider{calls: downThenOK(maxEscalationRounds*3+2, "back up")}
	a, s, _ := storeAgent(t, p, CompactionConfig{})
	a.Retry = RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Infinite: true}
	final, err := a.Run(context.Background(), "sys", submitHistory(t, s, "hi"))
	if err != nil {
		t.Fatalf("an outage inside maxSilentRecoveryRounds must still be survived: %v", err)
	}
	if final.Text() != "back up" {
		t.Fatalf("final = %q, want %q", final.Text(), "back up")
	}
}
