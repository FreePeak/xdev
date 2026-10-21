package session

import (
	"encoding/json"
	"strings"

	"github.com/FreePeak/xdev/internal/ai"
)

// SessionStatus is the lifecycle badge of a session, derived from the tail of
// its file (#107): a session the user killed mid-turn must not look like one
// that ran to the end.
//
// The vocabulary is omp's six verdicts (rules 36, 37) — error, aborted,
// interrupted, complete, pending, unknown — plus xdev's own "empty". "empty"
// is not one of omp's six because it answers a question omp never asks: not
// "how did the last turn end" but "is there a conversation here at all". An
// empty session is skipped by every resume path (see resolveResumeID), so it
// must not be a flavour of "interrupted" — the two mean opposite things.
type SessionStatus string

const (
	// StatusError — the last assistant turn died on a provider failure:
	// the request was refused, the stream broke, the ladder gave up.
	StatusError SessionStatus = "error"
	// StatusAborted — the turn was cut short before it produced an answer:
	// the user stopped it (or the caller cancelled), or the provider
	// refused the content on policy grounds.
	StatusAborted SessionStatus = "aborted"
	// StatusInterrupted — the session stopped mid-turn: the provider hit
	// the output limit, a tool call never came back, or the transcript
	// still ends on a tool result whose turn never finished.
	StatusInterrupted SessionStatus = "interrupted"
	// StatusComplete — the last assistant turn ended normally.
	StatusComplete SessionStatus = "complete"
	// StatusPending — the transcript ends on a user prompt that nothing
	// has answered yet: the turn is in flight, not finished.
	StatusPending SessionStatus = "pending"
	// StatusUnknown — the tail holds a message whose role this build does
	// not know. One unreadable line must not cost the session its badge.
	StatusUnknown SessionStatus = "unknown"
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

// statusMessageWire is the minimal decode of a persisted line. It is
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
	// ShortSummary is the one-line rendering a compaction record carries
	// (omp rule 27). It is the preview fallback for a session that has no
	// title (rule 45).
	ShortSummary string `json:"shortSummary"`
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
//
// The scan runs BACKWARDS and stops at the first message it finds: the
// listing calls this for every session file, on the picker's per-keystroke
// path, so the window is not parsed to its end.
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
			if !isHeaderLine(w.Type) {
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
func isHeaderLine(entryType string) bool {
	return entryType == "title" || entryType == "session"
}

// TailCompactionSummary returns the one-line summary of the LAST compaction
// record in the window — the preview fallback for a session that carries no
// title (omp rule 45).
//
// ponytail: the summary is only found when the compaction record falls inside
// the 32 KiB window, so a session compacted long ago and busy since falls
// back to its first message instead. Upgrade path if that ever shows up:
// record the summary in the session header when the compaction lands.
func TailCompactionSummary(tail []byte) string {
	lines := strings.Split(string(tail), "\n")
	summary := ""
	for _, line := range lines {
		// Cheap prefilter: only a compaction record can carry the summary,
		// and the listing reads this window for every file.
		if !strings.Contains(line, TypeCompaction) {
			continue
		}
		var w struct {
			Type         string `json:"type"`
			ShortSummary string `json:"shortSummary"`
		}
		if err := json.Unmarshal([]byte(line), &w); err != nil {
			continue
		}
		if w.Type == TypeCompaction && w.ShortSummary != "" {
			summary = w.ShortSummary
		}
	}
	return summary
}

// classifyTerminal maps the last message onto one of omp's six verdicts
// (rules 36, 37). The role decides the family and the stop reason decides
// the failure: an errored turn is "error", an aborted one is "aborted", and
// everything short of a clean finish inside an assistant turn is
// "interrupted" (the output limit, a tool call that never came back). A
// transcript that ends on the user prompt is "pending" — the turn is in
// flight, not finished — and a tool result nobody consumed is
// "interrupted": the call it answers is still open.
func classifyTerminal(role ai.Role, stop ai.StopReason, content []struct {
	Type string `json:"type"`
}) SessionStatus {
	switch role {
	case ai.RoleAssistant:
		switch stop {
		case ai.StopReasonError:
			return StatusError
		case ai.StopReasonAborted:
			return StatusAborted
		case ai.StopReasonLength:
			return StatusInterrupted
		}
		for _, b := range content {
			if b.Type == "toolCall" {
				return StatusInterrupted // the call never came back
			}
		}
		return StatusComplete
	case ai.RoleUser:
		return StatusPending // nothing has answered this prompt yet
	case ai.RoleToolResult:
		return StatusInterrupted // the call it answers is still open
	default:
		return StatusUnknown // a role this build does not know
	}
}
