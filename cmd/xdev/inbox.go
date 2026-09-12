package main

import (
	"strings"
	"sync"

	"github.com/FreePeak/xdev/internal/agent"
)

// Mailbox push delivery (#90). The poller shipped complete — unread sweep,
// dedupe, MarkRead, synchronous Stop — but no mode ever called Start, so a
// message from another session only surfaced when this session happened to run
// the `inbox` tool. Starting it here (one call site, in the registry every mode
// builds) makes arrivals land in the live run as a follow-up, which is the
// semantics the omp teammate channel has.

var (
	inboxMu sync.Mutex
	// inboxSink is the run mode's delivery target. It reports whether the
	// message was taken: a sink that declines (no turn running) leaves the
	// message UNREAD rather than consuming it.
	inboxSink func(agent.Message) bool
)

// setInboxSink installs the run mode's delivery target (its live agent). The
// previous sink is returned so the caller can restore it on exit.
func setInboxSink(fn func(agent.Message) bool) func(agent.Message) bool {
	inboxMu.Lock()
	prev := inboxSink
	inboxSink = fn
	inboxMu.Unlock()
	return prev
}

// deliverInbox routes one arrived message to the installed sink. With no sink
// it reports not-handled, and the poller leaves the message UNREAD so the next
// session (or the `inbox` tool) still sees it.
func deliverInbox(msg agent.Message) bool {
	inboxMu.Lock()
	sink := inboxSink
	inboxMu.Unlock()
	if sink == nil {
		return false
	}
	return sink(msg)
}

// startInboxPoller wires the mailbox's push path: called once per process from
// newToolRegistry, it polls and hands each arrival to deliverInbox. Returns
// the stop func the run mode defers.
func startInboxPoller(mb *agent.Mailbox) func() {
	if mb == nil {
		return func() {}
	}
	p := agent.NewInboxPoller(mb)
	p.OnMessage = deliverInbox
	p.Start()
	return p.Stop
}

// inboxFollowUp renders one mailbox message as the text the model sees. It is
// attributed like a user turn so the run can tell a peer message apart from its
// own prompt, and it names the sender because the model cannot read headers.
func inboxFollowUp(msg agent.Message) string {
	var b strings.Builder
	b.WriteString("mailbox message from ")
	from := msg.From
	if from == "" {
		from = "unknown"
	}
	b.WriteString(from)
	if msg.Subject != "" {
		b.WriteString(" — " + msg.Subject)
	}
	if msg.Body != "" {
		b.WriteString("\n" + msg.Body)
	}
	return b.String()
}
