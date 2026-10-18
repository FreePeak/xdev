package notify

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// recordingSink records what it was handed.
type recordingSink struct {
	mu     sync.Mutex
	events []Event
	fn     func(Event) error
}

func (s *recordingSink) Emit(ev Event) error {
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
	if s.fn != nil {
		return s.fn(ev)
	}
	return nil
}

func (s *recordingSink) seen() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events...)
}

// waitFor polls until cond holds or the deadline passes. Notifications are
// delivered asynchronously by design, so every assertion on delivery is a poll.
func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// TestPanickingSinkDoesNotStopLaterEvents is the core F1 case: a sink that
// panics must not take the process down, and the next event must still be
// delivered.
func TestPanickingSinkDoesNotStopLaterEvents(t *testing.T) {
	bad := SinkFunc(func(Event) error { panic("transport exploded") })
	good := &recordingSink{}

	d := New(4, 200*time.Millisecond, bad, good)
	defer d.Close()

	d.Send(Event{Kind: "first", Title: "boom"})
	d.Send(Event{Kind: "second", Title: "still works"})

	if !waitFor(t, time.Second, func() bool { return len(good.seen()) == 2 }) {
		t.Fatalf("later events were lost after a panic: got %d, want 2", len(good.seen()))
	}
}

// TestBlockingSinkIsDroppedAndDoesNotStallDelivery: a transport that ignores its
// deadline costs the dispatcher one timeout, and the queue keeps moving.
func TestBlockingSinkIsDroppedAndDoesNotStallDelivery(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	blocker := SinkFunc(func(Event) error { <-release; return nil })
	good := &recordingSink{}

	d := New(4, 50*time.Millisecond, blocker, good)
	defer d.Close()

	d.Send(Event{Kind: "slow"})
	d.Send(Event{Kind: "next"})

	// The second event arrives only after the first sink times out, which is
	// the bounded cost the plan allows.
	if !waitFor(t, 2*time.Second, func() bool {
		for _, ev := range good.seen() {
			if ev.Kind == "next" {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("a blocking sink stalled later delivery: %+v", good.seen())
	}
}

// TestSendNeverBlocksTheCaller: Send is called from the settling job's own
// goroutine, so filling the queue must drop rather than wait.
func TestSendNeverBlocksTheCaller(t *testing.T) {
	// hold keeps the sink busy so the queue stays full; the cleanup releases it
	// before Close, which would otherwise wait out its own drain deadline.
	hold := make(chan struct{})
	d := New(2, time.Second, SinkFunc(func(Event) error { <-hold; return nil }))
	defer func() {
		close(hold)
		d.Close()
	}()

	start := time.Now()
	for i := 0; i < 200; i++ {
		d.Send(Event{Kind: "burst"})
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("Send blocked for %s with a full queue", elapsed)
	}
}

// TestSinkErrorsAreContained: a sink returning an error is logged and ignored,
// and the remaining sinks still run.
func TestSinkErrorsAreContained(t *testing.T) {
	failing := SinkFunc(func(Event) error { return errors.New("no tty") })
	good := &recordingSink{}

	d := New(4, 200*time.Millisecond, failing, good)
	defer d.Close()
	d.Send(Event{Kind: "job_failed"})

	if !waitFor(t, time.Second, func() bool { return len(good.seen()) == 1 }) {
		t.Fatal("a failing sink prevented a later sink from running")
	}
}

// TestSendStampsTimeAndCloseIsIdempotent covers the two small promises the API
// makes to its callers.
func TestSendStampsTimeAndCloseIsIdempotent(t *testing.T) {
	got := &recordingSink{}
	d := New(4, 200*time.Millisecond, got)
	d.Send(Event{Kind: "job_failed"})
	if !waitFor(t, time.Second, func() bool { return len(got.seen()) == 1 }) {
		t.Fatal("event never delivered")
	}
	if got.seen()[0].At.IsZero() {
		t.Fatal("Send did not stamp At")
	}
	d.Close()
	d.Close() // must not panic on a closed channel
}

// TestCloseDrainsQueuedEvents: closing is a shutdown courtesy, not a discard.
func TestCloseDrainsQueuedEvents(t *testing.T) {
	got := &recordingSink{}
	d := New(8, 200*time.Millisecond, got)
	for i := 0; i < 5; i++ {
		d.Send(Event{Kind: "job_failed"})
	}
	d.Close()
	if n := len(got.seen()); n != 5 {
		t.Fatalf("Close delivered %d of 5 queued events", n)
	}
}
