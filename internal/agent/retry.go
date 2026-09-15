package agent

import (
	"context"
	"math/rand/v2"
	"time"
)

// RetryPolicy bounds the pre-content retry ladder (omp TurnRecovery:
// base 500ms → capped 8s exponential with 25% downward jitter).
type RetryPolicy struct {
	MaxRetries int           // attempts after the first failure
	BaseDelay  time.Duration // first backoff
	MaxDelay   time.Duration // backoff cap
}

// maxEscalationRounds bounds what "always retry" means once the whole
// failover chain has drained (loop.go's M5 ladder): the turn re-runs the
// retry ladder on the current target this many extra times, each behind a
// full-cap backoff, then the error surfaces. An outage that outlasts one
// pass through the chain is survived; a hard failure misclassified as
// transient still terminates.
const maxEscalationRounds = 2

// DefaultRetryPolicy is the omp-shaped default ladder.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxRetries: 4, BaseDelay: 500 * time.Millisecond, MaxDelay: 8 * time.Second}
}

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
