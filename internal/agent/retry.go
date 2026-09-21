package agent

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"
)

// RetryPolicy bounds the pre-content retry ladder (omp TurnRecovery:
// base 500ms → capped 8s exponential with 25% downward jitter).
type RetryPolicy struct {
	MaxRetries int           // attempts after the first failure
	BaseDelay  time.Duration // first backoff
	MaxDelay   time.Duration // backoff cap
	// RetryAllErrors makes every error class — including
	// empty turns (ErrEmptyTurn, "the model produced no answer")
	// — retryable: Run rebuilds context from history and
	// re-runs the ladder instead of ending the session, so a
	// transient upstream stall never looks like a silent death
	// (#331 follow-up: keep going until the goal is done).
	// Off by default — an empty turn is usually the model being
	// done, and retrying it forever burns tokens on a hard stop.
	// RetryPolicy.Infinite already lifts the transient and
	// overflow bounds; this controls the empty-turn bound
	// independently because a model that can only ever answer
	// nothing would otherwise loop with no out.
	RetryAllErrors bool
	// Infinite is retry.infinite / -retry-forever: once the failover chain
	// has drained, keep re-running the ladder instead of surfacing the
	// error, so an outage of any length is survived. Off by default — see
	// maxEscalationRounds.
	Infinite bool
}

// maxEscalationRounds bounds what "always retry" means once the whole
// failover chain has drained (loop.go's M5 ladder): the turn re-runs the
// retry ladder on the current target this many extra times, each behind a
// full-cap backoff, then the error surfaces. An outage that outlasts one
// pass through the chain is survived; a hard failure misclassified as
// transient still terminates. Infinite lifts the bound.
const maxEscalationRounds = 2

// DefaultRetryPolicy is the omp-shaped default ladder.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxRetries: 4, BaseDelay: 500 * time.Millisecond, MaxDelay: 8 * time.Second}
}

// withDefaults fills a zero-ish policy from the shipped default. Handoff
// side requests compare MaxRetries there the same way, so one caller
// reading a half-filled policy cannot change what the ladder means.
func (p RetryPolicy) withDefaults() RetryPolicy {
	if p.MaxRetries == 0 && p.BaseDelay == 0 {
		d := DefaultRetryPolicy()
		// No production build site sets the ladder timings, so the copy
		// must carry Infinite across — otherwise wireAgentMode's one-line
		// settings copy is silently dropped on the floor.
		d.Infinite = p.Infinite
		p = d
	}
	return p
}

// AllTargetsDownError is the voice of an unbounded wait (retry.infinite /
// -retry-forever): once every failover target has drained, the ladder
// raises it through TurnHooks.OnEvent once per round so a run that keeps
// trying reads as waiting, not as hung — and so a script parsing the
// stream can tell the rounds apart. It Unwraps to the provider failure
// that drained the ladder, which keeps ai.Classify saying "transient".
type AllTargetsDownError struct {
	Round   int           // 1-based escalation round just started
	Delay   time.Duration // backoff before the round's first attempt
	LastErr error         // the failure that drained the ladder
}

func (e *AllTargetsDownError) Error() string {
	return fmt.Sprintf("all targets down — retrying in %s (round %d: %v)",
		e.Delay.Round(time.Second), e.Round, e.LastErr)
}

func (e *AllTargetsDownError) Unwrap() error { return e.LastErr }

// delay computes the backoff for attempt n (1-based): base·2^(n−1) capped,
// with 25% downward jitter so simultaneous failures don't retry in lockstep.
func (p RetryPolicy) delay(n int) time.Duration {
	base := p.BaseDelay
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	d := base * time.Duration(1<<uint(n-1))
	if max := p.MaxDelay; max > 0 && d > max {
		d = max
	}
	return d - time.Duration(rand.Int64N(int64(d)/4+1))
}

// sleepBackoff waits for the attempt's backoff, honoring cancellation.
func sleepBackoff(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
