package session

import (
	"encoding/json"
	"strings"

	"github.com/FreePeak/xdev/internal/ai"
)

// SessionStatus is the lifecycle badge of a session, derived from the tail of
// its file (#107): a session the user killed mid-turn must not look like one
// that ran to the end.
type SessionStatus string

const (
	// StatusDone — the last assistant turn ended normally.
	StatusDone SessionStatus = "done"
	// StatusInterrupted — the session stopped mid-turn: the user aborted it,
	// the provider errored or hit the output limit, a tool call never came
	// back, or the transcript still ends on the user prompt that was being
	// answered when it died.
	StatusInterrupted SessionStatus = "interrupted"
)

// statusTailMax bounds the tail read that finds the terminal entry. The store
// appends bookkeeping after the last message (model_change, session_exit,
// branch markers); the largest such distance across the on-disk store is
// 20 KB.
//
// ponytail: the window is a fixed 32 KiB, so bookkeeping longer than that
// would hide the terminal entry and the session would read as "interrupted"
// — the safe direction (never a false "done"). Upgrade path if that ever
// shows up: grow the window on a retry, or record the terminal entry's
// offset as a custom entry the way session_exit is recorded.
const statusTailMax = 32 * 1024

// statusMessageWire is the minimal decode of a persisted message. It is
// deliberately not ai.Message: ai.Message's block union is strict, so a line
// carrying a block type this build does not know would fail the whole decode
// and cost the status of the file. Only the fields the verdict needs are read.
type statusMessageWire struct {
	Type    string `json:"type"`
	Message *struct {
		Role       ai.Role       `json:"role"`
		StopReason ai.StopReason `json:"stopReason"`
		Content    []struct {
			Type string `json:"type"`
		} `json:"content"`
	} `json:"message"`
}

// StatusEmpty marks a session whose file carries no message at all: a title
// slot, a session header, and nothing else. It is its own badge rather than a
// flavour of "interrupted" because the two mean opposite things to a resume —
// an interrupted session has a conversation to continue, an empty one has
// nothing to show and nothing for the model to read back.
const StatusEmpty SessionStatus = "empty"

// ClassifyStatus derives the badge from a slice of the file's tail: the last
// message-bearing entry decides. The window may begin mid-line or on a torn
// write; a fragment fails the decode and is skipped, so no line-boundary
// search is needed. A window holding no message but at least one bookkeeping
// line (a goal entry, a model change, a session_exit) means the transcript
// sits behind the window — that is interrupted, not empty. Only a file with
// nothing but its slot and header is StatusEmpty.
func ClassifyStatus(tail []byte) SessionStatus {
	lines := strings.Split(string(tail), "\n")
	bookkeeping := false
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if line == "" || line[0] != '{' {
			continue
		}
		var w statusMessageWire
		if err := json.Unmarshal([]byte(line), &w); err != nil {
			continue
		}
		if w.Type != TypeMessage || w.Message == nil {
			if !isHeaderLine(line) {
				bookkeeping = true
			}
			continue
		}
		return classifyTerminal(w.Message.Role, w.Message.StopReason, w.Message.Content)
	}
	if bookkeeping {
		return StatusInterrupted
	}
	return StatusEmpty
}

// isHeaderLine reports whether a non-message line is one of the two every
// session file opens with — the title slot and the session header. Those are
// not entries, so a file holding only them has no conversation and is empty;
// every other non-message line is bookkeeping and implies the transcript is
// behind this window.
func isHeaderLine(line string) bool {
	var w struct {
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(line), &w) != nil {
		return false
	}
	return w.Type == "title" || w.Type == "session"
}

// classifyTerminal maps the last message onto the row's two words. Anything
// short of a cleanly finished assistant turn counts as interrupted — an
// aborted turn persists nothing past the prompt it was answering, so a
// user-last transcript IS the signature of a user interruption.
func classifyTerminal(role ai.Role, stop ai.StopReason, content []struct {
	Type string `json:"type"`
}) SessionStatus {
	switch role {
	case ai.RoleAssistant:
		switch stop {
		case ai.StopReasonError, ai.StopReasonAborted, ai.StopReasonLength:
			return StatusInterrupted
		}
		for _, b := range content {
			if b.Type == "toolCall" {
				return StatusInterrupted // the call never came back
			}
		}
		return StatusDone
	default: // user (nothing answered it) or toolResult (call left open)
		return StatusInterrupted
	}
}
