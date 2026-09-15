package ai

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestWatchdogFirstProgressAbort(t *testing.T) {
	// stop is the CALLER's ctx (never canceled here); sctx is the stream's
	// own ctx, which the watchdog cancels on expiry — mirroring adapters.
	sctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := make(chan Event) // never emits
	out := withWatchdog(context.Background(), cancel, src, 40*time.Millisecond, time.Hour)

	select {
	case ev, ok := <-out:
		if !ok {
			t.Fatal("channel closed without an abort event")
		}
		if ev.Type != EventError || !errors.Is(ev.Err, ErrWatchdogAborted) {
			t.Fatalf("ev = %+v err=%v", ev, ev.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("no abort within first-progress timeout")
	}
	// The request context was canceled so the HTTP body read dies too.
	select {
	case <-sctx.Done():
	case <-time.After(time.Second):
		t.Fatal("stream context not canceled on abort")
	}
}

func TestWatchdogIdleAbortAfterProgress(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := make(chan Event, 4)
	out := withWatchdog(context.Background(), cancel, src, time.Hour, 60*time.Millisecond)

	src <- Event{Type: EventTextDelta, Delta: "hi"}
	select {
	case ev := <-out:
		if ev.Type != EventTextDelta || ev.Delta != "hi" {
			t.Fatalf("first event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("progress event not relayed")
	}
	// Silence now: the idle timer must fire.
	select {
	case ev := <-out:
		if ev.Type != EventError || !errors.Is(ev.Err, ErrWatchdogAborted) {
			t.Fatalf("ev = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no idle abort")
	}
}

func TestWatchdogPassesThroughCleanStream(t *testing.T) {
	sctx, cancel := context.WithCancel(context.Background())
	src := make(chan Event, 3)
	src <- Event{Type: EventTextDelta, Delta: "a"}
	src <- Donef(StopReasonStop, nil, nil)
	close(src)
	out := withWatchdog(sctx, cancel, src, time.Hour, time.Hour)

	var got []Event
	for ev := range out { // must terminate by source close, never time out
		got = append(got, ev)
	}
	if len(got) != 2 || got[1].Type != EventDone {
		t.Fatalf("events = %+v", got)
	}
	if sctx.Err() != nil {
		t.Fatalf("clean stream canceled the context: %v", sctx.Err())
	}
}

// TestWatchdogAbortClassifiesTransient pins the M5 integration: a
// watchdog expiry must engage the retry ladder, not fall through as an
// unrecoverable error.
func TestWatchdogAbortClassifiesTransient(t *testing.T) {
	err := fmt.Errorf("%w (first=1s idle=1s)", ErrWatchdogAborted)
	if got := Classify(err); got != ClassTransient {
		t.Fatalf("Classify(watchdog) = %v, want ClassTransient", got)
	}
	// The raw sentinel too (wrapped by nothing).
	if got := Classify(ErrWatchdogAborted); got != ClassTransient {
		t.Fatalf("Classify(raw watchdog) = %v, want ClassTransient", got)
	}
}

// TestWatchdogForwardsTerminalEventDespiteStreamTeardown pins a real bug:
// the adapters cancel the stream's derived context as soon as the source
// goroutine returns, so a relay keyed on THAT context can drop the
// terminal done/error event on the floor. Only the caller's ctx may end
// the relay early.
func TestWatchdogForwardsTerminalEventDespiteStreamTeardown(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	src := make(chan Event, 2)
	src <- Event{Type: EventTextDelta, Delta: "x"}
	src <- Donef(StopReasonStop, nil, nil)
	close(src)
	out := withWatchdog(context.Background(), cancel, src, time.Hour, time.Hour)
	cancel() // adapter teardown races the relay, as in the real adapters

	var got []Event
	for ev := range out {
		got = append(got, ev)
	}
	if len(got) != 2 || got[1].Type != EventDone {
		t.Fatalf("terminal event dropped: %+v", got)
	}
}

func TestWatchdogBackPressureDoesNotAbort(t *testing.T) {
	// #283 RCA §1: a frozen UI stopped the consumer, the relay jammed, the
	// idle window expired on UNDELIVERED source events, and the stale
	// expiry (Go's select may hand it back before the receive branch once
	// delivery resumes) aborted a stream that was healthy at the socket —
	// each episode costing a backoff, a retry and one injected fake-user
	// continuation turn. Delivery starvation must never become a verdict.
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := make(chan Event, 64) // same capacity as the relay's own buffer
	out := withWatchdog(context.Background(), cancel, src, 20*time.Millisecond, 60*time.Millisecond)

	const n = 300 // > src cap + out cap: delivery cannot keep up without a reader
	go func() {
		for i := range n {
			src <- Event{Type: EventTextDelta, Delta: fmt.Sprint(i)}
		}
		close(src)
	}()

	// Three jam rounds: fill the buffers, let the idle window expire well
	// past, then release exactly one event. Pre-fix, every release is a
	// coin flip between the receive branch and the stale expiry.
	got := 0
	for range 3 {
		time.Sleep(200 * time.Millisecond)
		select {
		case ev := <-out:
			if ev.Type == EventError {
				t.Fatalf("back-pressure stall aborted a live stream: %v", ev.Err)
			}
			got++
		default:
			t.Fatal("relay delivered nothing after the stall")
		}
	}

	// Drain: every event must arrive and no error event may appear.
	for ev := range out {
		if ev.Type == EventError {
			t.Fatalf("relay emitted %v on a completed stream", ev.Err)
		}
		got++
	}
	if got != n {
		t.Fatalf("delivered %d of %d events", got, n)
	}
}
