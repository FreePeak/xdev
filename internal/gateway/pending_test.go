package gateway

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestRestartReportsTheLostTurn: a turn that was running when the daemon
// died must be reported, not swallowed. The answer was produced and then
// dropped on the floor, and the user was left waiting for an answer that
// had already been thrown away.
func TestRestartReportsTheLostTurn(t *testing.T) {
	b := newFakeBot(t)
	d := newTestDaemon(t, b, []int64{7})
	if err := WritePending(d.dataDir, map[int64]PendingTurn{
		7: {Chat: 7, Text: "the question", Started: time.Now().Add(-time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = d.Run(ctx) }()

	if !waitFor(t, 5*time.Second, func() bool {
		for _, m := range b.sentTo(7) {
			if strings.Contains(m, "restarted") {
				return true
			}
		}
		return false
	}) {
		t.Fatal("the chat was never told that its turn was lost")
	}
	// The record is cleared, so the next start does not repeat the notice.
	if !waitFor(t, 5*time.Second, func() bool { return len(ReadPending(d.dataDir)) == 0 }) {
		t.Fatalf("pending records = %v, want none after the notice", ReadPending(d.dataDir))
	}
}

// TestPendingSurvivesACancelledDaemon: a turn that ended because the daemon
// is going down keeps its record. Its answer never reached the chat, and
// the next start has to be able to say so.
func TestPendingSurvivesACancelledDaemon(t *testing.T) {
	b := newFakeBot(t)
	d := newTestDaemon(t, b, []int64{7})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.ctx = ctx

	d.markPending(7, "sess-1", "the question")
	d.finishPending(7)

	got := ReadPending(d.dataDir)
	if len(got) != 1 {
		t.Fatalf("pending records = %v, want the lost turn to survive a cancelled daemon", got)
	}
	if got[7].Text != "the question" {
		t.Errorf("pending text = %q, want the question that was lost", got[7].Text)
	}
}

// TestPendingIsClearedAfterAGoodTurn: a turn that ended normally must not
// leave a record behind, or every restart would report a turn that was
// answered.
func TestPendingIsClearedAfterAGoodTurn(t *testing.T) {
	b := newFakeBot(t)
	d := newTestDaemon(t, b, []int64{7})
	d.ctx = context.Background()

	d.markPending(7, "sess-1", "the question")
	d.finishPending(7)

	if got := ReadPending(d.dataDir); len(got) != 0 {
		t.Fatalf("pending records = %v, want none after a turn that ended", got)
	}
}
