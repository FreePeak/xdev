package notify

import (
	"sync"
	"time"
)

// JobSettle is one job reaching a terminal status, as the hub reports it.
//
// It is a plain struct rather than the hub's own JobInfo, so internal/notify
// never depends on internal/agent: cmd/xdev translates at the wiring point and
// this package stays testable without a hub.
type JobSettle struct {
	ID     string
	Label  string
	Status string
	// Detail is the failure text (a result's error or note) for the body.
	Detail string
}

// Job statuses, mirroring the hub's vocabulary.
const (
	StatusRunning        = "running"
	StatusCompleted      = "completed"
	StatusYielded        = "yielded"
	StatusSchemaMismatch = "schema-mismatch"
	StatusFailed         = "failed"
	StatusCanceled       = "canceled"
)

// IsFailure reports whether a result with this status is an error, using the
// same predicate the hub tool already applies (agent/hubtool.go). A second
// spelling of "failed" would drift from the first.
func IsFailure(status string) bool {
	return status == StatusFailed || status == StatusSchemaMismatch
}

// feedState is what a Feed remembers about one job.
type feedState struct {
	// gone is the last status already reported, so a re-settle of the same
	// (id, status) — which the hub allows, since Revive and send-to-parked
	// re-enter a job — does not notify twice.
	gone string
	// errored records that this id has already failed. A later success for it
	// must stay silent: the human has been told, and a "completed" arriving
	// after a failure reads as a recovery that never happened.
	errored bool
}

// Feed decides which settles deserve a notification and hands those to a
// dispatcher.
//
// The decision lives here rather than in the hub's settle listener because that
// listener runs on the settling job's own goroutine with no lock held, and a
// panic in it takes the process down: the listener must stay trivial, so the
// filter, the dedupe and the focus gate sit behind this mutex instead.
type Feed struct {
	dispatch *Dispatcher
	// on is the effective value of the `notifications.subagents` axis, read
	// once at wiring time. The hub's jobs ARE subagents, so that axis governs
	// this feed; `notifications.errors` governs a failed turn, elsewhere.
	on bool

	mu      sync.Mutex
	jobs    map[string]*feedState
	focused bool
	// focusKnown is false until the terminal reports focus at all. Unknown
	// focus notifies rather than suppresses: "cannot tell" is not "the human is
	// looking", and for a failure the safe error is the noisy one.
	focusKnown bool
}

// NewFeed builds a feed over dispatch, enabled by the subagents axis.
func NewFeed(dispatch *Dispatcher, on bool) *Feed {
	return &Feed{dispatch: dispatch, on: on, jobs: make(map[string]*feedState)}
}

// Focus records the terminal's focus state. Until this is called at least once
// the feed treats focus as unknown and suppresses nothing.
func (f *Feed) Focus(focused bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.focused, f.focusKnown = focused, true
}

// JobSettled records one settle and queues a notification if it warrants one.
//
// It never blocks and never panics: the queue send is non-blocking by design,
// and every string it hands over is treated as hostile text by the sanitizer.
func (f *Feed) JobSettled(j JobSettle) {
	if !f.on || !f.worthTelling(j) {
		return
	}
	if f.watching() {
		return
	}
	f.dispatch.Send(Event{
		Kind:  "job_failed",
		Title: settleTitle(j),
		Body:  settleBody(j),
		JobID: j.ID,
		At:    time.Now(),
	})
}

// worthTelling applies the failure filter and the dedupe, and records the
// settle. It is the whole of F5 and F3.
func (f *Feed) worthTelling(j JobSettle) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	st := f.jobs[j.ID]
	if st == nil {
		st = &feedState{}
		f.jobs[j.ID] = st
	}
	if st.gone == j.Status {
		return false // same (id, status) settling again is not news
	}
	st.gone = j.Status
	if IsFailure(j.Status) {
		st.errored = true
		return true
	}
	// A success that follows a reported failure is a re-settle in disguise.
	return false
}

// watching reports whether the human is already looking at this window, which
// is the only case that suppresses at all.
func (f *Feed) watching() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.focusKnown && f.focused
}

// settleTitle names the job and its outcome. The label is the human's own word
// for the work so it leads, and the two failure modes stay distinguishable: a
// schema mismatch is xdev's bug, a plain failure is the model's.
func settleTitle(j JobSettle) string {
	name := j.Label
	if name == "" {
		name = j.ID
	}
	if j.Status == StatusSchemaMismatch {
		return name + " returned the wrong shape"
	}
	return name + " failed"
}

// settleBody carries what the human needs to decide whether to go look.
func settleBody(j JobSettle) string {
	body := j.Detail
	if body == "" {
		body = "no error detail reported"
	}
	if j.ID != "" {
		body += " (job " + j.ID + ")"
	}
	return body
}
