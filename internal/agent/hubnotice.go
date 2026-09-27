package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// A background subagent used to end silently: the job settled, the dock
// repainted, and the parent model found out only if it happened to think to
// poll `hub result`. A model cannot poll what it does not know finished, so
// the result was frequently never read at all.
//
// The fix cannot be an in-turn injection. `Run` takes `history` by value and
// keeps no reference to it, and when the parent is idle there is no Agent and
// no turn goroutine to drain a queue — so a notice has to reach the parent the
// way a scheduled reminder does (see cmd/xdev/tui.go): gate on idle, claim the
// turn, persist into the store the next run rebuilds from, then start the turn.
//
// This file is that delivery, kept out of the TUI so it is testable without a
// terminal. The host supplies three callbacks and gets the same shape
// StartScheduleDelivery uses.

// ErrHubNoticeDelivery means the host could not claim the turn or persist the
// completion notice. The result is not lost — the job stays on the roster and
// `hub result` still answers — so a host should report this and move on rather
// than retry, which is why StartHubNoticeDelivery does not re-queue.
var ErrHubNoticeDelivery = errors.New("hub: completion notice delivery failed")

// HubNoticeAttribution marks the notice as harness text, so it is not counted
// as something the user typed (stats.go) and does not render as a user turn
// (harnessUserAttribution in cmd/xdev/tui.go).
const HubNoticeAttribution = "hub-notice"

// maxHubNoticeChars bounds the quoted result. The point of the notice is that
// the model knows the work landed and roughly what it said; re-sending a large
// artifact through a channel the model did not ask for would re-create the
// context cost this whole seam exists to avoid.
const maxHubNoticeChars = 2000

// HubNoticePrompt renders one settled job as a turn's worth of text. The
// wording has to tell the model two things it cannot otherwise know: that the
// job finished, and that the result is now in front of it — so that it reads
// the result instead of re-running the work.
func HubNoticePrompt(info JobInfo, res *SubagentResult) string {
	label := info.Label
	if label == "" {
		label = info.ID
	}
	var b strings.Builder
	switch {
	case res == nil:
		// No handoff to quote. Return here: the body below dereferences res.
		fmt.Fprintf(&b, "background subagent %s (%s) finished, but returned no result.", label, info.ID)
		return b.String()
	case res.Status == "failed":
		fmt.Fprintf(&b, "background subagent %s (%s) failed: %s", label, info.ID, res.Err)
	default:
		fmt.Fprintf(&b, "background subagent %s (%s) finished (%s). Its result follows.", label, info.ID, res.Status)
	}
	body := res.Text
	if body == "" && len(res.Yield) > 0 {
		body = string(res.Yield)
	}
	if body == "" {
		return b.String()
	}
	b.WriteString("\n\n")
	if len(body) > maxHubNoticeChars {
		body = body[:maxHubNoticeChars] + "\n[truncated — read the subagent transcript or call hub result for the full output]"
	}
	b.WriteString(body)
	return b.String()
}

// StartHubNoticeDelivery wires h so a settled background job reaches an idle
// parent. It returns immediately; delivery happens on the settling job's
// goroutine.
//
// The three callbacks mirror StartScheduleDelivery and carry the same contract:
//
//   - admit is a non-mutating gate, called with no lock held. Return false to
//     skip this settle (the common case: a turn is already running).
//   - persist writes the notice. It must be safe to call from another
//     goroutine and should claim the turn before appending, so a notice can
//     never be written for a turn that then fails to start.
//   - after runs once per delivered notice, after persist succeeded.
//
// Nothing is retried: a settle that finds the session busy is not re-queued.
// The model sees the result on the next turn regardless, because the job stays
// on the roster and `hub result` still answers. Re-queuing would risk a notice
// storm when several jobs settle during one long turn.
func StartHubNoticeDelivery(ctx context.Context, h *Hub, admit func() bool, persist func(notice string) error, after func(err error)) {
	if h == nil {
		return
	}
	var seenMu sync.Mutex
	seen := map[string]bool{}
	h.AddSettleListener(func(info JobInfo, res *SubagentResult) {
		if ctx.Err() != nil {
			return
		}
		// Revive and Send-to-parked re-enter launchLocked, so the same id can
		// settle twice. A parked-then-revived job is a new result the model
		// has not seen, so allow one notice per (id, status) pair rather than
		// one per id.
		seenMu.Lock()
		key := info.ID + "\x00" + info.Status
		if seen[key] {
			seenMu.Unlock()
			return
		}
		seen[key] = true
		seenMu.Unlock()

		if admit != nil && !admit() {
			return
		}
		notice := HubNoticePrompt(info, res)
		var err error
		if persist != nil {
			err = persist(notice)
		}
		if after != nil {
			after(err)
		}
	})
}
