package notify

import (
	"strings"
	"testing"
	"time"
)

// collect returns a dispatcher whose only sink records events, plus the feed
// wired to it.
func newTestFeed(t *testing.T, on bool) (*Feed, *recordingSink) {
	t.Helper()
	sink := &recordingSink{}
	d := New(16, time.Second, sink)
	t.Cleanup(d.Close)
	return NewFeed(d, on), sink
}

// F5: only `failed` and `schema-mismatch` alert. A yielded job is mid-flight,
// a completed one is the expected outcome, and a canceled one is the human's
// own keystroke — alerting on any of them trains them to ignore the channel.
func TestFeedAlertsOnlyOnFailureStatuses(t *testing.T) {
	cases := []struct {
		status string
		alert  bool
	}{
		{StatusFailed, true},
		{StatusSchemaMismatch, true},
		{StatusCompleted, false},
		{StatusYielded, false},
		{StatusCanceled, false},
		{StatusRunning, false},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			f, sink := newTestFeed(t, true)
			f.JobSettled(JobSettle{ID: "j1", Label: "audit", Status: tc.status})
			got := waitEvents(t, sink, 1, 200*time.Millisecond)
			if tc.alert && len(got) != 1 {
				t.Fatalf("status %q should alert, got %d events", tc.status, len(got))
			}
			if !tc.alert && len(got) != 0 {
				t.Fatalf("status %q must stay silent, got %+v", tc.status, got)
			}
		})
	}
}

// The predicate must agree with the hub tool's, or a job the tool calls an
// error would be silent here.
func TestIsFailureMatchesTheHubToolPredicate(t *testing.T) {
	for _, s := range []string{StatusFailed, StatusSchemaMismatch} {
		if !IsFailure(s) {
			t.Errorf("%q is an error to the hub tool and must be an error here", s)
		}
	}
	for _, s := range []string{StatusCompleted, StatusYielded, StatusCanceled, StatusRunning} {
		if IsFailure(s) {
			t.Errorf("%q is not an error to the hub tool", s)
		}
	}
}

// F3: the same (id, status) settling twice notifies once. The hub allows a
// re-settle (Revive, send-to-parked), so a listener that just forwards would
// tell the human the same thing twice.
func TestFeedDeduplicatesRepeatedSettles(t *testing.T) {
	f, sink := newTestFeed(t, true)
	for i := 0; i < 3; i++ {
		f.JobSettled(JobSettle{ID: "j1", Label: "audit", Status: StatusFailed})
	}
	if got := waitEvents(t, sink, 1, 200*time.Millisecond); len(got) != 1 {
		t.Fatalf("three identical settles produced %d notifications, want 1", len(got))
	}
}

// A different job failing is news, even when the status repeats.
func TestFeedDoesNotConfuseJobs(t *testing.T) {
	f, sink := newTestFeed(t, true)
	f.JobSettled(JobSettle{ID: "j1", Status: StatusFailed})
	f.JobSettled(JobSettle{ID: "j2", Status: StatusFailed})
	if got := waitEvents(t, sink, 2, time.Second); len(got) != 2 {
		t.Fatalf("two jobs failing produced %d notifications, want 2", len(got))
	}
}

// A success after a reported failure stays silent: the human already knows the
// job broke, and a "completed" arriving late reads as a recovery that did not
// happen. A third settle of that success must stay silent too.
func TestFeedStaysSilentOnSuccessAfterFailure(t *testing.T) {
	f, sink := newTestFeed(t, true)
	f.JobSettled(JobSettle{ID: "j1", Label: "audit", Status: StatusFailed})
	f.JobSettled(JobSettle{ID: "j1", Label: "audit", Status: StatusCompleted})
	f.JobSettled(JobSettle{ID: "j1", Label: "audit", Status: StatusCompleted})

	got := waitEvents(t, sink, 1, 250*time.Millisecond)
	if len(got) != 1 {
		t.Fatalf("failure-then-success produced %d notifications, want 1: %+v", len(got), got)
	}
	if got[0].Kind != "job_failed" {
		t.Fatalf("the one notification should be the failure, got %q", got[0].Kind)
	}
}

// F4: a focused window suppresses, an unfocused one notifies — and focus the
// terminal never reported is treated as unfocused, because "cannot tell" is not
// "the human is looking".
func TestFeedFocusGate(t *testing.T) {
	t.Run("unknown focus notifies", func(t *testing.T) {
		f, sink := newTestFeed(t, true)
		f.JobSettled(JobSettle{ID: "j1", Status: StatusFailed})
		if got := waitEvents(t, sink, 1, 250*time.Millisecond); len(got) != 1 {
			t.Fatal("focus unknown must fail toward notifying")
		}
	})
	t.Run("focused suppresses", func(t *testing.T) {
		f, sink := newTestFeed(t, true)
		f.Focus(true)
		f.JobSettled(JobSettle{ID: "j1", Status: StatusFailed})
		if got := waitEvents(t, sink, 1, 200*time.Millisecond); len(got) != 0 {
			t.Fatalf("a focused terminal is already being watched, got %+v", got)
		}
	})
	t.Run("blurred notifies", func(t *testing.T) {
		f, sink := newTestFeed(t, true)
		f.Focus(false)
		f.JobSettled(JobSettle{ID: "j1", Status: StatusFailed})
		if got := waitEvents(t, sink, 1, 250*time.Millisecond); len(got) != 1 {
			t.Fatal("a blurred terminal is the case the feature exists for")
		}
	})
}

// The subagents axis off means no sink work at all, and the feed must not even
// record the settle.
func TestFeedDisabledByAxisDoesNothing(t *testing.T) {
	f, sink := newTestFeed(t, false)
	f.JobSettled(JobSettle{ID: "j1", Status: StatusFailed})
	if got := waitEvents(t, sink, 1, 150*time.Millisecond); len(got) != 0 {
		t.Fatalf("a disabled feed notified: %+v", got)
	}
}

// The body is the failure text plus the job id, and an escaped payload in the
// detail cannot reach the terminal: the feed hands it to the same sanitizer.
func TestFeedBodyCarriesDetailAndSurvivesInjection(t *testing.T) {
	f, sink := newTestFeed(t, true)
	f.JobSettled(JobSettle{
		ID:     "j1",
		Label:  "audit",
		Status: StatusFailed,
		Detail: "\x1b]52;c;cGF3bg==\x07\nexit 1",
	})
	got := waitEvents(t, sink, 1, time.Second)
	if len(got) != 1 {
		t.Fatal("the failure never arrived")
	}
	body := got[0].Sanitized().Body
	if strings.ContainsAny(body, "\x1b\x07") {
		t.Fatalf("detail carried a control byte through: %q", body)
	}
	if !strings.Contains(body, "j1") {
		t.Fatalf("body should name the job so `xdev hub` can find it: %q", body)
	}
}

// A label-less job still produces something a human can act on.
func TestFeedTitleFallsBackToIDAndNamesTheMode(t *testing.T) {
	cases := []struct {
		in   JobSettle
		want string
	}{
		{JobSettle{ID: "j1", Label: "audit", Status: StatusFailed}, "audit failed"},
		{JobSettle{ID: "j1", Status: StatusFailed}, "j1 failed"},
		{JobSettle{ID: "j1", Label: "audit", Status: StatusSchemaMismatch}, "audit returned the wrong shape"},
	}
	for _, tc := range cases {
		if got := settleTitle(tc.in); got != tc.want {
			t.Errorf("settleTitle(%+v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// waitEvents polls until n events have arrived, then returns everything seen.
func waitEvents(t *testing.T, sink *recordingSink, n int, d time.Duration) []Event {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) && len(sink.seen()) < n {
		time.Sleep(2 * time.Millisecond)
	}
	// One more beat, so an unwanted extra notification has a chance to land.
	time.Sleep(20 * time.Millisecond)
	return sink.seen()
}
