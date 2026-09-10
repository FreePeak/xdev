package ai

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWatchdogFirstProgressAbort(t *testing.T) {
	sctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := make(chan Event) // never emits
	out := withWatchdog(sctx, cancel, src, 40*time.Millisecond, time.Hour)

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
	sctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := make(chan Event, 4)
	out := withWatchdog(sctx, cancel, src, time.Hour, 60*time.Millisecond)

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
	defer cancel()
	src := make(chan Event, 3)
	src <- Event{Type: EventTextDelta, Delta: "a"}
	src <- Donef(StopReasonStop, nil, nil)
	close(src)
	out := withWatchdog(sctx, cancel, src, 30*time.Millisecond, 30*time.Millisecond)

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
