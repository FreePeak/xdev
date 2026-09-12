package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/session"
)

// Branch summaries (M5 #24): when the session branches away from a long
// branch, the abandoned stretch is condensed into a branch_summary entry so
// the model on the new branch knows what was tried. The text comes from the
// smol role through a seam the call site wires — the same bounded-transcript,
// one-provider-call shape the memory pipeline already builds (cmd/xdev's
// complete("@smol"), and the handoff document helper #23 exposes) — and a
// missing or failing seam records the fixed marker instead, because switching
// branches must never depend on a model round-trip.
const (
	// BranchSummaryMinTokens gates the model call: a short branch has nothing
	// worth condensing, so that switch stays instant.
	BranchSummaryMinTokens = 2000
	// BranchSummaryInputChars caps the transcript handed to the seam.
	BranchSummaryInputChars = 16000
	// BranchSummaryMaxChars caps the note that gets stored (a branch note must
	// not become the next prompt's problem).
	BranchSummaryMaxChars = 1200
	// branchSummaryMarker is the note a branch gets without a summarizer; it
	// is the pre-#24 fixed text, so a nil seam is not a behavior change.
	branchSummaryMarker = "(branch summary) the previous branch was abandoned for a tree-selector switch"
)

// branchSummaryPrompt is what the seam is asked; the transcript follows it.
const branchSummaryPrompt = `The session is about to switch to a different branch of its history.
Summarize the branch being left: what it tried, what it established, and what
should not be repeated, for a reader who will never see it. Reply with the
note only, no preamble.`

// BranchSummarizer condenses a transcript into the note stored on a
// branch_summary entry. nil (or a failure) keeps the marker text.
type BranchSummarizer func(ctx context.Context, prompt string) (string, error)

// SummarizeBranch is the Agent spelling of SummarizeBranchStore, against the
// agent's own store.
func (a *Agent) SummarizeBranch(ctx context.Context, entryID string, summarize BranchSummarizer) error {
	if a.Store == nil {
		return errors.New("branch summary: no session store")
	}
	return SummarizeBranchStore(ctx, a.Store, entryID, summarize)
}

// SummarizeBranchStore appends the branch_summary entry recording the branch
// being left and then moves the leaf to entryID — the order the tree selector
// needs: the note hangs off the abandoned leaf, the new leaf is the branch
// target. It takes the store rather than the agent because the caller that
// branches (the tree selector) holds a store, not the run.
//
// summarize may be nil. When set, it is called only for a branch carrying more
// than BranchSummaryMinTokens of context, is fed a bounded transcript, and its
// answer is bounded too; any failure is logged and falls back to the marker
// rather than failing the switch. An unresolvable smol role must be passed as
// nil for the same reason.
func SummarizeBranchStore(ctx context.Context, store *session.Store, entryID string, summarize BranchSummarizer) error {
	if store == nil {
		return errors.New("branch summary: no session store")
	}
	if store.Entry(entryID) == nil {
		return fmt.Errorf("branch: no entry matching %q", entryID)
	}
	text := branchSummaryMarker
	transcript, tokens := branchTranscript(store)
	if summarize != nil && transcript != "" && tokens >= BranchSummaryMinTokens {
		note, err := summarize(ctx, branchSummaryPrompt+"\n\n"+transcript)
		switch {
		case err != nil:
			logx.Errorf("branch summary: %v — recording the marker instead", err)
		case strings.TrimSpace(note) != "":
			note = strings.TrimSpace(note)
			if len(note) > BranchSummaryMaxChars {
				note = capText(note, BranchSummaryMaxChars) + "…"
			}
			text = note
		}
	}
	if err := store.Append(&session.BranchSummaryEntry{Summary: ai.Message{
		Role:    ai.RoleUser,
		Content: []ai.Block{ai.TextBlock{Text: text}},
	}}); err != nil {
		return fmt.Errorf("branch summary: %w", err)
	}
	return store.Branch(entryID)
}

// branchTranscript renders the branch being left (bounded) together with its
// estimated size, so the caller decides whether a model call is warranted.
func branchTranscript(store *session.Store) (string, int64) {
	res, err := session.BuildContext(store.Entries(), store.LeafID(), session.SystemPrompt{})
	if err != nil || len(res.Messages) == 0 {
		return "", 0
	}
	return renderTranscript(res.Messages, elideResultChars, BranchSummaryInputChars),
		contextTokens(res.Messages)
}
