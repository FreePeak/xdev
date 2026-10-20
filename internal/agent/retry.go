package agent

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
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

// outageMayNotEndATurn reports whether the ladder may keep retrying a
// connectivity outage without a round budget. It rides retry.infinite, which
// is the shipped default, and it is the whole point of that setting: an
// outage is not a failure of the request, and the only thing that ends it is
// the network coming back. Cancelling the turn still ends it (Esc, Ctrl+C,
// --max-time, SIGINT) — the wait is a sleepBackoff, not a bare loop.
//
// What stays bounded: every other transient — a dead provider answering
// `connection refused`, a wedged host that accepts and never answers, a
// stream that keeps dying mid-frame. Those are failures OF THE REQUEST, and
// a misclassified one must still terminate (maxSilentRecoveryRounds). Only
// ai.IsNetworkOutage, which reads the kernel's own routing verdict, lifts
// the bound.
func (p RetryPolicy) outageMayNotEndATurn(err error) bool {
	return p.Infinite && ai.IsNetworkOutage(err)
}

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
		// must carry Infinite and RetryAllErrors across — otherwise
		// wireAgentMode's one-line settings copy is silently dropped.
		d.Infinite = p.Infinite
		d.RetryAllErrors = p.RetryAllErrors
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

// SilentProviderError is the voice of a request the provider has not
// answered yet: the turn is waiting on the response headers, and the
// screen says so instead of showing an unexplained spinner.
//
// It is not a failure — the wait has a ceiling (the transport's header
// budget) and the turn continues either way. It is the announcement that
// the wait is a wait, so the person watching knows the turn is alive and
// what it is waiting for.
type SilentProviderError struct {
	Waited   time.Duration
	Messages int
}

func (e *SilentProviderError) Error() string {
	return fmt.Sprintf("the provider has not answered yet (%s, %d messages in the request) — still waiting",
		e.Waited.Truncate(time.Second), e.Messages)
}

// VerdictRetryError is the voice of a verdict the ladder is riding out
// because the session is not finished: the provider refused the request
// (a credential denial, a client-posture gate, a rejected body) and the
// turn waits for the next attempt instead of ending on it. Raised through
// TurnHooks.OnEvent once per round, so a turn that waits on a verdict reads
// as waiting and not as hung. It Unwraps to the provider's own error, so a
// reader can tell which verdict it was.
type VerdictRetryError struct {
	Round   int           // 1-based retry round just started
	Delay   time.Duration // backoff before the next attempt
	Reason  string        // what the provider said about this client
	LastErr error         // the provider's verdict
}

func (e *VerdictRetryError) Error() string {
	return fmt.Sprintf("%s — retrying in %s (round %d: %v)",
		e.Reason, e.Delay.Round(time.Second), e.Round, e.LastErr)
}

func (e *VerdictRetryError) Unwrap() error { return e.LastErr }

// EmptyTurnRetryError is the voice of an unbounded empty-turn wait
// (retry.retryAllErrors): once the nudge budget is spent, the loop keeps
// rebuilding context and re-running. Raised through TurnHooks.OnEvent so
// a run that keeps trying reads as waiting, not as hung.
type EmptyTurnRetryError struct {
	Round int           // 1-based empty-turn recovery round
	Delay time.Duration // backoff before the next attempt
}

func (e *EmptyTurnRetryError) Error() string {
	return fmt.Sprintf("empty turn — retrying in %s (round %d)",
		e.Delay.Round(time.Second), e.Round)
}

// ContinuationRetryError is the voice of a retain-and-continue round
// (oneTurnWithRecovery's post-content path): the partial was kept, a
// continuation turn was injected, and the ladder is about to wait Delay
// before re-asking. Same contract as AllTargetsDownError and
// EmptyTurnRetryError — a console that collapses the wire error into one
// notice still has the wait to print. Unlike those two it carries no
// underlying failure: the recovery already happened.
type ContinuationRetryError struct {
	Delay time.Duration // backoff before the continuation turn is re-sent
}

func (e *ContinuationRetryError) Error() string {
	return fmt.Sprintf("provider cut off mid-message — continuation injected, retrying in %s",
		e.Delay.Round(time.Second))
}

// delay computes the backoff for attempt n (1-based): base·2^(n−1) capped,
// with 25% downward jitter so simultaneous failures don't retry in lockstep.
// n < 1 is treated as 1: callers that reset attempt to 0 before sleeping
// (escalation / auth rebuild) must still wait, not busy-spin.
//
// The exponential is saturated before it is multiplied. retry.infinite and
// retry.retryAllErrors are both default-on, so the round counters that feed n
// (escalation, emptyRetries) have no ceiling, and measured against the old
// multiply with the default 500ms base / 8s cap: n = 1…35 land at or below
// the cap, then every n from 36 to 56 except 39, 43, 44, 47, 49, 52, 53 and
// 55 panics with "invalid argument to Int64N" (the int64 overflow leaves a
// negative duration that the MaxDelay clamp cannot catch, because d > max is
// false for a negative), and n = 57+ wraps to exactly zero — which
// sleepBackoff reads as "nothing to wait for", the busy-spin half of the bug.
// The panic is the crash the TUI dies with; the zero is the freeze.
func (p RetryPolicy) delay(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	base := p.BaseDelay
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	max := p.MaxDelay
	if max <= 0 {
		max = base
	}
	d := base
	for i := 1; i < n && d < max; i++ {
		if d > max/2 {
			// The next doubling would pass max, and max is the cap anyway:
			// stopping here is where the old multiply-and-clamp landed too.
			d = max
			break
		}
		d *= 2
	}
	if d > max {
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

// sessionUnfinished reports whether the session still has work that has not
// been completed — i.e. the last persisted message is a user message with no
// assistant answer yet, or an active goal (not completed/dropped/budget-
// exhausted) remains. While this is true, retry bounds are lifted so the
// agent keeps retrying forever instead of giving up; cancellation (Esc/
// Ctrl+C) and an unrecoverable context overflow still stop.
func (a *Agent) sessionUnfinished() bool {
	if a.Goals != nil {
		if v, ok := a.Goals.View(); ok && v.Goal.Status == GoalActive {
			return true
		}
	}
	if a.Store == nil {
		return false
	}
	entries := a.Store.Entries()
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if me, ok := e.(*session.MessageEntry); ok {
			if me.Message.Role == "user" {
				return true
			}
			if me.Message.Role == "assistant" {
				return false
			}
		}
	}
	return false
}

// retryUnfinished re-runs the ladder after a verdict that re-sending the
// same request cannot repair — a credential denial, a client-posture gate, a
// provider-rejected body — while the session still has work that has not
// been answered. The turn is not done, so the ladder keeps waiting: the user
// can fix the key, switch client or start a paid tier while the turn waits,
// and the next attempt picks it up. Cancellation (Esc/Ctrl+C) still ends it.
//
// The round is announced on the event stream and in the session file, so a
// turn that waits on a verdict reads as waiting and not as hung. Returns
// the rebuilt history and the error that ends the turn, if any.
func (a *Agent) retryUnfinished(ctx context.Context, policy RetryPolicy, history []ai.Message, round int, reason string, err error) ([]ai.Message, error) {
	d := policy.delay(round)
	if a.Store != nil {
		if res, buildErr := session.BuildContext(a.Store.Entries(), a.Store.LeafID(), session.SystemPrompt{}); buildErr == nil {
			history = res.Messages
		}
	}
	a.persistRecoveryNotice("recovery_unfinished_verdict", round, d, err)
	a.noticeVerdictRetry(round, d, reason, err)
	if serr := sleepBackoff(ctx, d); serr != nil {
		return history, serr
	}
	return history, nil
}

// noticeVerdictRetry raises one verdict round on the event stream: the
// provider refused the request and the turn is waiting for the next
// attempt. Same contract as noticeAllTargetsDown and noticeEmptyTurnRetry —
// nil-safe, so a mode that installs no hooks stays quiet.
func (a *Agent) noticeVerdictRetry(round int, d time.Duration, reason string, err error) {
	if a == nil || a.Hooks == nil {
		return
	}
	a.Hooks.OnEvent(ai.Errorf(&VerdictRetryError{Round: round, Delay: d, Reason: reason, LastErr: err}))
}
