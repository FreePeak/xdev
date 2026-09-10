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

// ErrWatchdogAborted wraps every watchdog expiry so callers can tell a
// stalled stream from other transport errors.
var ErrWatchdogAborted = errors.New("stream watchdog: no progress")

// withWatchdog wraps a stream channel with the first-progress and idle
// timers. Expiry cancels the stream's request context (killing the HTTP
// body), emits one error{ErrWatchdogAborted} event, and drains the source
// channel until close so the upstream goroutine exits. Events may still
// be relayed while the abort races a fresh event — whichever the select
// sees first wins, matching omp's turn-recovery semantics.
func withWatchdog(sctx context.Context, cancel context.CancelFunc, ch <-chan Event, first, idle time.Duration) <-chan Event {
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
					continue
				}
				aborted = true
				cancel()
				out <- Event{Type: EventError, Err: fmt.Errorf("%w (first=%s idle=%s)", ErrWatchdogAborted, first, idle)}
				go func() {
					for range ch { // keep the upstream goroutine draining
					}
				}()
			}
		}
	}()
	return out
}
