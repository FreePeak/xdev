package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// The internet going away must not end a turn.
//
// Reported 2026-10-12: the link dropped mid-session, the ladder spent its
// twelve escalation rounds (~2 minutes at the shipped 500ms/8s ladder) and
// the turn ended with the transport error — while the only thing that could
// ever fix it, the network coming back, was minutes away. The session was
// left holding an answer nobody got, and the user's only recovery was to
// resend the same message by hand.
//
// The fix is not a bigger round count (12 rounds is ~90 minutes when each
// round is a full ladder against a host that accepts and goes silent — the
// bound is a ROUND count, and a round count cannot express a wall clock).
// It is to stop counting rounds at all for the ONE failure where retrying
// is unambiguously right: a connectivity outage, where every target and
// every retry fails identically until the machine is back on the network.

// outageErr is the shape a dropped link actually produces: a *net.OpError
// wrapping the kernel's own routing verdict, wrapped again by the adapter's
// "stream start" prefix. IsNetworkOutage must see through all three layers,
// which is what pinning the real types here checks.
func outageErr() error {
	return fmt.Errorf("agent: stream start: %w", &net.OpError{Op: "dial", Err: syscall.ENETUNREACH})
}

// downThenOutage scripts `down` connection failures, then `outage` link
// failures, then success. It is downThenOK's shape with the two failure
// kinds kept apart, because the whole change is that they are NOT the same
// failure any more.
func downThenOutage(down, outage int, final string) []fakeScript {
	var calls []fakeScript
	for range down {
		calls = append(calls, fakeScript{err: errors.New("dial tcp 127.0.0.1:8080: connect: connection refused")})
	}
	for range outage {
		calls = append(calls, fakeScript{err: outageErr()})
	}
	return append(calls, fakeScript{events: []ai.Event{textEvent(final), doneEvent(final)}})
}

// TestOutageDoesNotEndTheTurn is the report: the link is down for far longer
// than the bounded ladder and the escalation rounds can cover, and the turn
// still finishes with the model's answer once it returns.
func TestOutageDoesNotEndTheTurn(t *testing.T) {
	// Well past what a bounded ladder survives: 1 ladder + 12 escalation
	// rounds of 2 calls is 25, so 40 outage failures is comfortably beyond
	// any round budget the shipped constants could express.
	bounded := 1 + maxSilentRecoveryRounds*(oneShotRetry().MaxRetries+1)
	p := &fakeProvider{calls: downThenOutage(0, bounded*2, "back online")}
	a, s, _ := storeAgent(t, p, CompactionConfig{})
	a.Retry = RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Infinite: true}

	final, err := a.Run(context.Background(), "sys", submitHistory(t, s, "hi"))
	if err != nil {
		t.Fatalf("a connectivity outage must not end the turn: %v", err)
	}
	if final.Text() != "back online" {
		t.Fatalf("final = %q, want the answer that arrived after the network returned", final.Text())
	}
}

// TestOutageWaitIsAnnouncedAndReadable pins the other half: a turn that
// spends an hour waiting must read as waiting, on the screen and in the
// session file. The record must NOT claim a remaining budget it does not
// have — "remainingRounds: 0" for a loop that is still climbing is exactly
// the kind of wrong the record exists to prevent.
func TestOutageWaitIsAnnouncedAndReadable(t *testing.T) {
	p := &fakeProvider{calls: downThenOutage(0, 4, "ok")}
	a, s, _ := storeAgent(t, p, CompactionConfig{})
	a.Retry = RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Infinite: true}
	var announced []error
	a.Hooks = TurnHooksFunc{
		OnEventF: func(ev ai.Event) {
			if ev.Type == ai.EventError {
				announced = append(announced, ev.Err)
			}
		},
		OnMessageEndF:    func(m *ai.Message) { _ = s.Append(&session.MessageEntry{Message: *m}) },
		OnToolResultMsgF: func(m *ai.Message) { _ = s.Append(&session.MessageEntry{Message: *m}) },
	}
	if _, err := a.Run(context.Background(), "sys", submitHistory(t, s, "hi")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(announced) == 0 {
		t.Fatal("an unbounded outage wait must be announced, or the turn reads as hung")
	}
	// The raw wire error is on the stream too (the adapter emits it), so the
	// wait notice is found by shape, not by position.
	var wait error
	for _, ev := range announced {
		var down *AllTargetsDownError
		if errors.As(ev, &down) {
			wait = ev
			break
		}
	}
	if wait == nil {
		t.Fatalf("announcements = %v, want an AllTargetsDownError among them", announced)
	}
	if !strings.Contains(wait.Error(), "retrying in") {
		t.Fatalf("announcement = %q, want it to name the wait", wait)
	}
	notices := customNotices(t, s, "recovery_all_targets_down")
	if len(notices) == 0 {
		t.Fatal("an unbounded outage wait must be written to the session file")
	}
	if got := notices[0]["unbounded"]; got != true {
		t.Fatalf("first notice unbounded = %v, want true (no ceiling to report)", got)
	}
	if _, ok := notices[0]["remainingRounds"]; ok {
		t.Fatalf("an unbounded notice must not report a remaining budget: %v", notices[0])
	}
}

// TestCancellingAnOutageWaitStillEndsTheTurn is the ceiling on the fix. The
// wait is unbounded only in ROUNDS: Esc, Ctrl+C, --max-time and SIGINT all
// cancel the context, and the turn must return immediately rather than
// holding the session's single turn claim for the life of the process
// (a7e17741, 2026-09-28 — the wedge this bound was written for).
func TestCancellingAnOutageWaitStillEndsTheTurn(t *testing.T) {
	var calls []fakeScript
	for range 1 << 12 {
		calls = append(calls, fakeScript{err: outageErr()})
	}
	p := &fakeProvider{calls: calls}
	a, s, _ := storeAgent(t, p, CompactionConfig{})
	a.Retry = RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Infinite: true}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.Run(ctx, "sys", submitHistory(t, s, "hi"))
		done <- err
	}()
	// Let the ladder enter the outage wait, then cancel like Esc does.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled outage wait must return the turn, not hold it forever")
	}
}

// TestBoundedPolicyStillSurfacesAnOutage is the boundary on the other side:
// retry.infinite off means the ladder is bounded, and an outage is not
// special. Without this, "the network is back" would silently become an
// excuse to ignore an explicit opt-out.
func TestBoundedPolicyStillSurfacesAnOutage(t *testing.T) {
	var calls []fakeScript
	for range 64 {
		calls = append(calls, fakeScript{err: outageErr()})
	}
	p := &fakeProvider{calls: calls}
	a, s, _ := storeAgent(t, p, CompactionConfig{})
	a.Retry = oneShotRetry() // Infinite false

	if _, err := a.Run(context.Background(), "sys", submitHistory(t, s, "hi")); err == nil {
		t.Fatal("a bounded policy must still surface an outage")
	}
}

// A dead provider is NOT a connectivity outage, however similar the strings
// look. `connection refused` is a host that answered "nothing is listening",
// and the ladder must keep bounding it — a gateway on loopback that is not
// running must not hold a turn forever.
func TestConnectionRefusedIsNotAnOutage(t *testing.T) {
	if ai.IsNetworkOutage(errors.New("dial tcp 127.0.0.1:8080: connect: connection refused")) {
		t.Fatal("a refused connection is a dead provider, not a local outage")
	}
	var calls []fakeScript
	for range 64 {
		calls = append(calls, fakeScript{err: errors.New("dial tcp 127.0.0.1:8080: connect: connection refused")})
	}
	p := &fakeProvider{calls: calls}
	a, s, _ := storeAgent(t, p, CompactionConfig{})
	a.Retry = RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Infinite: true}
	if _, err := a.Run(context.Background(), "sys", submitHistory(t, s, "hi")); err == nil {
		t.Fatal("a refused connection must still end the turn after the bounded rounds")
	}
	// The bounded escalation is what stopped it: the script is long enough
	// that only a bound could have ended this.
	want := 1 + maxSilentRecoveryRounds*(oneShotRetry().MaxRetries+1) + 1
	if n := len(p.gotReqs); n > want {
		t.Fatalf("stream calls = %d, want at most %d (bounded escalation)", n, want)
	}
}

// A health-check probe that fails because the link is down is the same
// outage one layer up, and it must be unbounded for the same reason: the
// probe cannot succeed until the network returns.
func TestOutageHealthCheckIsUnbounded(t *testing.T) {
	health := make([]error, maxSilentRecoveryRounds+8)
	for i := range health {
		health[i] = outageErr()
	}
	p := &scriptedHealthProvider{
		fakeProvider: &fakeProvider{calls: []fakeScript{{events: []ai.Event{doneEvent("never reached")}}}},
		health:       health,
	}
	a, s, _ := storeAgent(t, p.fakeProvider, CompactionConfig{})
	a.Provider = p
	a.Retry = RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Infinite: true}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.Run(ctx, "sys", submitHistory(t, s, "hi"))
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if got := p.healthCalls(); got <= maxSilentRecoveryRounds {
		t.Fatalf("health probes = %d, want more than %d: an outage must outlast the bounded escalation", got, maxSilentRecoveryRounds)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled outage health check must return the turn")
	}
}
