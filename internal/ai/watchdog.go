package ai

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Stream watchdog defaults (PRD §3.3): a stream must make progress or it
// dies into error{aborted} instead of hanging a turn forever. The gateway
// in front of slow models can legally sit for minutes, so these are
// generous; they bound infinite stalls, not slowness.
var (
	// FirstProgressTimeout bounds the wait for the first event after
	// response headers arrive.
	FirstProgressTimeout = 90 * time.Second
	// IdleTimeout bounds the gap between two events (reset on every event).
	IdleTimeout = 90 * time.Second
)

// abortGrace bounds how long an aborted relay keeps waiting for its source
// to close. A well-behaved adapter closes immediately after cancel, so this
// only catches a wedged producer.
const abortGrace = 2 * time.Second

// ErrWatchdogAborted wraps every watchdog expiry so callers can tell a
// stalled stream from other transport errors. It classifies as transient
// (Classify), so the M5 ladder owns recovery.
var ErrWatchdogAborted = errors.New("stream watchdog: no progress")

// withWatchdog wraps a stream channel with the first-progress and idle
// timers. Expiry cancels the stream's request context (killing the HTTP
// body read), emits one error{ErrWatchdogAborted}, and keeps draining the
// source until close so its goroutine exits.
//
// The relay NEVER ends early: the Provider contract promises exactly one
// terminal done/error event then close, and consumers (agent loop, print,
// TUI) read to close — so a caller-cancel that raced the source teardown
// must not swallow the terminal event. Every source event is forwarded
// verbatim and the channel closes exactly when the source closes.
func withWatchdog(_ context.Context, cancel context.CancelFunc, ch <-chan Event, first, idle time.Duration) <-chan Event {
	out := make(chan Event, 64)
	go func() {
		defer close(out)
		timer := time.NewTimer(first)
		defer timer.Stop()
		aborted := false
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					return
				}
				if aborted {
					continue // drain until the upstream closes
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(idle)
				out <- ev
			case <-timer.C:
				if aborted {
					// Second expiry after the abort: the source never
					// closed, so stop waiting. The relay's lifetime must
					// never outlive the stream by more than one grace
					// window, or every aborted stream leaks a goroutine
					// (caught by the CI leak test on Linux).
					return
				}
				aborted = true
				cancel()
				out <- Event{Type: EventError, Err: fmt.Errorf("%w (first=%s idle=%s)", ErrWatchdogAborted, first, idle)}
				// Keep reading the source in THIS goroutine (no second
				// drain goroutine) and arm the grace window: if the source
				// never closes, the next timer hit ends the relay.
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(abortGrace)
			}
		}
	}()
	return out
}
