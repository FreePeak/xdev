package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// retry.infinite: once the ladder and the (empty) failover chain drain, the
// turn keeps re-running the ladder instead of surfacing — so an outage of any
// length, e.g. a gateway answering `connection refused`, is survived.

// downN scripts n transient failures, then success. `connection refused` is
// the real-world string the whole feature exists for; the classifier already
// calls it transient (ai connectionResetRe), so this test also pins that.
func downThenOK(down int, final string) []fakeScript {
	var calls []fakeScript
	for range down {
		calls = append(calls, fakeScript{err: errors.New("dial tcp 127.0.0.1:8080: connect: connection refused")})
	}
	return append(calls, fakeScript{events: []ai.Event{textEvent(final), doneEvent(final)}})
}

func TestInfiniteSurvivesWhatBoundedRoundsCannot(t *testing.T) {
	// The bounded ladder spends (MaxRetries+1) calls per round ×
	// (1 + maxEscalationRounds) rounds. Failing one call past that total,
	// only an infinite policy ever reaches the success.
	bounded := (oneShotRetry().MaxRetries + 1) * (maxEscalationRounds + 1)
	p := &fakeProvider{calls: downThenOK(bounded+1, "back up")}
	a, _, p := storeAgent(t, p, CompactionConfig{})
	a.Retry = RetryPolicy{MaxRetries: oneShotRetry().MaxRetries, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Infinite: true}
	final, err := a.Run(context.Background(), "sys", submitHistory(t, session.OpenMem("test", "t"), "hi"))
	if err != nil {
		t.Fatalf("an infinite ladder must outlast the bounded rounds: %v", err)
	}
	if final.Text() != "back up" {
		t.Fatalf("final = %q", final.Text())
	}
	if len(p.gotReqs) != bounded+2 {
		t.Fatalf("stream calls = %d, want %d", len(p.gotReqs), bounded+2)
	}
}

// The wait must be visible: each unbounded round raises an
// AllTargetsDownError through OnEvent, classifying as transient so the
// stream-error displays keep it as a notice rather than a failure.
func TestInfiniteRoundIsAnnouncedOnTheStream(t *testing.T) {
	var mu sync.Mutex
	var announced []error
	p := &fakeProvider{calls: downThenOK(maxEscalationRounds*3+2, "ok")}
	a, s, _ := storeAgent(t, p, CompactionConfig{})
	a.Hooks = TurnHooksFunc{
		OnEventF: func(ev ai.Event) {
			if ev.Type == ai.EventError {
				var down *AllTargetsDownError
				if errors.As(ev.Err, &down) {
					mu.Lock()
					announced = append(announced, ev.Err)
					mu.Unlock()
				}
			}
		},
		OnMessageEndF:    func(m *ai.Message) { _ = s.Append(&session.MessageEntry{Message: *m}) },
		OnToolResultMsgF: func(m *ai.Message) { _ = s.Append(&session.MessageEntry{Message: *m}) },
	}
	a.Retry = RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Infinite: true}
	if _, err := a.Run(context.Background(), "sys", submitHistory(t, s, "hi")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(announced) < maxEscalationRounds+1 {
		t.Fatalf("announcements = %d, want at least the %d rounds past the bounded ones", len(announced), maxEscalationRounds+1)
	}
	first := announced[0]
	var down *AllTargetsDownError
	if !errors.As(first, &down) || down.Round < 1 || down.Delay <= 0 || down.LastErr == nil {
		t.Fatalf("announcement = %v, want round/delay/last error", first)
	}
	if !strings.Contains(first.Error(), "all targets down") {
		t.Fatalf("announcement text = %q", first)
	}
	if ai.Classify(first) != ai.ClassTransient {
		t.Fatalf("an announcement must unwrap to its transient cause: %v", first)
	}
}

// A zero-ish RetryPolicy handed to the loop keeps its Infinite flag through
// the default fill-in — otherwise the settings copy in wireAgentMode (which
// never sets the timings) is dropped and the feature is unreachable.
func TestInfiniteRidesWithDefaults(t *testing.T) {
	p := (RetryPolicy{Infinite: true}).withDefaults()
	if !p.Infinite || p.MaxRetries != DefaultRetryPolicy().MaxRetries {
		t.Fatalf("withDefaults = %+v", p)
	}
}
