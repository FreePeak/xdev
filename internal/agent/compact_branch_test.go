package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// branchStore holds a history long enough to be worth summarizing: the seam is
// only asked for branches carrying more than BranchSummaryMinTokens.
func branchStore(t *testing.T, long bool) (*Agent, *session.Store) {
	t.Helper()
	p := &fakeProvider{}
	a, s, _ := storeAgent(t, p, ladderConfig("handoff"))
	appendMsg(t, s, userText("first host message"))
	appendMsg(t, s, ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "acknowledged"}}})
	if long {
		appendMsg(t, s, userText(strings.Repeat("context ", 1500)))
	}
	return a, s
}

func branchSummaryEntry(t *testing.T, s *session.Store) *session.BranchSummaryEntry {
	t.Helper()
	for _, e := range s.Entries() {
		if b, ok := e.(*session.BranchSummaryEntry); ok {
			return b
		}
	}
	t.Fatal("no branch_summary entry was appended")
	return nil
}

// TestSummarizeBranchUsesTheSeamAndMovesTheLeaf pins the generated path: the
// note comes from the smol-role seam, the entry hangs off the branch being
// left, and the leaf ends up on the branch target.
func TestSummarizeBranchUsesTheSeamAndMovesTheLeaf(t *testing.T) {
	a, s := branchStore(t, true)
	abandoned := s.LeafID()
	target := s.Entries()[0].Envelope().ID

	var gotPrompt string
	err := a.SummarizeBranch(context.Background(), target, func(_ context.Context, prompt string) (string, error) {
		gotPrompt = prompt
		return "  the abandoned branch proved the parser was fine  ", nil
	})
	if err != nil {
		t.Fatalf("SummarizeBranch: %v", err)
	}
	if !strings.Contains(gotPrompt, branchSummaryPrompt) {
		t.Fatalf("prompt = %q, want the branch summary instruction", gotPrompt)
	}
	if !strings.Contains(gotPrompt, "first host message") {
		t.Fatalf("prompt must carry the abandoned transcript:\n%s", gotPrompt)
	}
	entry := branchSummaryEntry(t, s)
	if entry.Summary.Text() != "the abandoned branch proved the parser was fine" {
		t.Fatalf("summary = %q, want the seam's note, trimmed", entry.Summary.Text())
	}
	if entry.Env.ParentID != abandoned {
		t.Fatalf("branch_summary parent = %q, want the abandoned leaf %q", entry.Env.ParentID, abandoned)
	}
	if got := s.LeafID(); got != target {
		t.Fatalf("leaf = %q, want the branch target %q", got, target)
	}
}

// TestSummarizeBranchKeepsMarkerForShortBranchesAndFailures pins the two
// fallbacks: a branch too small to be worth a round trip is not summarized,
// and a failing (or absent) seam records the fixed marker instead.
func TestSummarizeBranchKeepsMarkerForShortBranchesAndFailures(t *testing.T) {
	t.Run("short branch", func(t *testing.T) {
		a, s := branchStore(t, false)
		calls := 0
		if err := a.SummarizeBranch(context.Background(), s.Entries()[0].Envelope().ID,
			func(context.Context, string) (string, error) {
				calls++
				return "unused", nil
			}); err != nil {
			t.Fatalf("SummarizeBranch: %v", err)
		}
		if calls != 0 {
			t.Fatalf("seam calls = %d, want none for a short branch", calls)
		}
		if got := branchSummaryEntry(t, s).Summary.Text(); got != branchSummaryMarker {
			t.Fatalf("summary = %q, want the marker", got)
		}
	})
	t.Run("failing seam", func(t *testing.T) {
		logs := captureLogs(t)
		a, s := branchStore(t, true)
		target := s.Entries()[0].Envelope().ID
		err := a.SummarizeBranch(context.Background(), target, func(context.Context, string) (string, error) {
			return "", errors.New("smol role unavailable")
		})
		if err != nil {
			t.Fatalf("a failing seam must not fail the switch: %v", err)
		}
		if got := branchSummaryEntry(t, s).Summary.Text(); got != branchSummaryMarker {
			t.Fatalf("summary = %q, want the marker", got)
		}
		if s.LeafID() != target {
			t.Fatalf("leaf = %q, want the branch target", s.LeafID())
		}
		if !strings.Contains(logs.String(), "smol role unavailable") {
			t.Errorf("the failure must be logged, logs:\n%s", logs.String())
		}
	})
	t.Run("nil seam", func(t *testing.T) {
		a, s := branchStore(t, true)
		if err := a.SummarizeBranch(context.Background(), s.Entries()[0].Envelope().ID, nil); err != nil {
			t.Fatalf("SummarizeBranch: %v", err)
		}
		if got := branchSummaryEntry(t, s).Summary.Text(); got != branchSummaryMarker {
			t.Fatalf("summary = %q, want the marker", got)
		}
	})
}

// TestSummarizeBranchRejectsUnknownTarget pins the guard the tree selector
// relies on: branching to an entry the store does not have is an error, and
// nothing is appended.
func TestSummarizeBranchRejectsUnknownTarget(t *testing.T) {
	a, s := branchStore(t, true)
	before := len(s.Entries())
	err := a.SummarizeBranch(context.Background(), "nope-nope", nil)
	if err == nil || !strings.Contains(err.Error(), "no entry matching") {
		t.Fatalf("err = %v, want an unknown-entry error", err)
	}
	if got := len(s.Entries()); got != before {
		t.Fatalf("store grew by %d entries, want none", got-before)
	}
}

// TestSummarizeBranchStoreNeedsNoAgent pins the spelling the tree selector
// uses: it holds a store, not the run's agent, and must still get the note,
// the entry placement and the leaf move.
func TestSummarizeBranchStoreNeedsNoAgent(t *testing.T) {
	_, s := branchStore(t, true)
	abandoned := s.LeafID()
	target := s.Entries()[0].Envelope().ID
	if err := SummarizeBranchStore(context.Background(), s, target, func(context.Context, string) (string, error) {
		return "branch note without an agent", nil
	}); err != nil {
		t.Fatalf("SummarizeBranchStore: %v", err)
	}
	entry := branchSummaryEntry(t, s)
	if entry.Summary.Text() != "branch note without an agent" {
		t.Fatalf("summary = %q", entry.Summary.Text())
	}
	if entry.Env.ParentID != abandoned {
		t.Fatalf("parent = %q, want the abandoned leaf %q", entry.Env.ParentID, abandoned)
	}
	if s.LeafID() != target {
		t.Fatalf("leaf = %q, want the target %q", s.LeafID(), target)
	}
	// A nil store is a programming error, not a silent no-op.
	if err := SummarizeBranchStore(context.Background(), nil, target, nil); err == nil {
		t.Fatal("a nil store must be reported")
	}
}
