package main

import (
	"os"
	"testing"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/session"
)

// TestSummarizeAndBranchAppendsAndSwitches pins the /tree Shift+Enter
// contract (omp branchWithSummary): the leaf moves to the target, then the
// branch_summary is appended AS A CHILD OF THE TARGET — the note must ride
// the new branch's context, not dangle off the abandoned leaf where no
// future prompt would ever read it.
func TestSummarizeAndBranchAppendsAndSwitches(t *testing.T) {
	st := session.OpenMem("/tmp/tree-selector", "t")
	if err := st.Append(&session.MessageEntry{Message: ai.Message{
		Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "first"}},
	}}); err != nil {
		t.Fatal(err)
	}
	first := st.LeafID()
	if err := st.Append(&session.MessageEntry{Message: ai.Message{
		Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "second"}},
	}}); err != nil {
		t.Fatal(err)
	}

	if err := summarizeAndBranch(st, first); err != nil {
		t.Fatal(err)
	}

	var summary *session.BranchSummaryEntry
	for _, e := range st.Entries() {
		if bs, ok := e.(*session.BranchSummaryEntry); ok {
			summary = bs
		}
	}
	if summary == nil {
		t.Fatal("no branch_summary entry was appended")
	}
	if summary.Env.ParentID != first {
		t.Errorf("branch_summary parent = %q, want the target %q", summary.Env.ParentID, first)
	}
	if got := st.LeafID(); got != summary.Env.ID {
		t.Errorf("leaf = %q, want the summary (omp makes it the new tip)", got)
	}
	// The point of the placement: the note reaches the model's context.
	res, err := session.BuildContext(st.Entries(), st.LeafID(), session.SystemPrompt{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 2 || res.Messages[1].Text() == "" {
		t.Fatalf("context = %+v, want [first, summary]", res.Messages)
	}

	before := len(st.Entries())
	if err := summarizeAndBranch(st, "deadbeef"); err == nil {
		t.Fatal("unknown entry id must error")
	}
	if len(st.Entries()) != before {
		t.Fatal("failed summarize-and-branch must append nothing")
	}
}

// TestTreeRewindTarget pins omp's navigateTree target rule: a user message
// rewinds to its PARENT and returns its prompt as the composer draft (the
// first message rewinds to the root, ""), every other entry lands on
// itself with no draft.
func TestTreeRewindTarget(t *testing.T) {
	user := &session.MessageEntry{Message: ai.Message{
		Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "do the thing"}},
	}}
	user.Env = session.Envelope{ID: "aaaaaaaa", Type: session.TypeMessage}
	assistant := &session.MessageEntry{Message: ai.Message{
		Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "done"}},
	}}
	assistant.Env = session.Envelope{ID: "bbbbbbbb", ParentID: "aaaaaaaa", Type: session.TypeMessage}

	if target, draft := treeRewindTarget(user); target != "" || draft != "do the thing" {
		t.Fatalf("first user row: target=%q draft=%q, want root rewind + the prompt", target, draft)
	}
	if target, draft := treeRewindTarget(assistant); target != "bbbbbbbb" || draft != "" {
		t.Fatalf("assistant row: target=%q draft=%q, want itself, no draft", target, draft)
	}

	// A user row the harness wrote (provider cut-off, turn-budget wrap-up, goal
	// continuation) rewinds the same way but offers no draft: resending text
	// the user never typed would put it in the composer as their own words.
	harness := &session.MessageEntry{Message: ai.Message{
		Role: ai.RoleUser, Attribution: agent.TurnBudgetAttribution,
		Content: []ai.Block{ai.TextBlock{Text: agent.TurnBudgetPrompt}},
	}}
	harness.Env = session.Envelope{ID: "cccccccc", ParentID: "bbbbbbbb", Type: session.TypeMessage}
	if target, draft := treeRewindTarget(harness); target != "bbbbbbbb" || draft != "" {
		t.Fatalf("harness row: target=%q draft=%q, want the parent rewind and no draft", target, draft)
	}
}

// TestTreeEntriesSnapshot pins the selector row data built from the store:
// file order, active = current leaf, role/summary filled. No depth — rows are
// flush left and tagged with their author, so the snapshot stopped computing
// an indentation level.
func TestTreeEntriesSnapshot(t *testing.T) {
	st := session.OpenMem("/tmp/tree-selector", "t")
	if err := st.Append(&session.MessageEntry{Message: ai.Message{
		Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "start\ntask"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Append(&session.MessageEntry{Message: ai.Message{
		Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "plan"}},
	}}); err != nil {
		t.Fatal(err)
	}

	rows := treeEntries(st)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].Role != "user" || rows[1].Role != "assistant" {
		t.Fatalf("roles = %q,%q", rows[0].Role, rows[1].Role)
	}
	if rows[0].Summary != "start task" {
		t.Fatalf("summary = %q, want the whitespace-collapsed text", rows[0].Summary)
	}
	if rows[0].Active || !rows[1].Active {
		t.Fatalf("active flags = %v,%v; want only the leaf", rows[0].Active, rows[1].Active)
	}
}

// TestTreeEntriesLabelEveryRow is the regression for the blank tree rows. A
// real agent run is mostly tool-call-only assistant turns and tool results, and
// those carry no text block at all (45% of a 714-message session), so a summary
// built on Message.Text() alone left nearly half the selector's rows empty: an
// id over a blank line, which is what reads as "the last of my history shows
// nothing". Every row has to say something.
func TestTreeEntriesLabelEveryRow(t *testing.T) {
	st := session.OpenMem("/tmp/tree-rows", "t")
	for _, m := range []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "run the tests"}}},
		{Role: ai.RoleAssistant, Content: []ai.Block{ai.ToolCallBlock{
			ID: "c1", Name: "bash", Arguments: []byte(`{"command":"go test ./..."}`),
		}}},
		{Role: ai.RoleToolResult, ToolName: "bash"}, // a result with no output text
		{Role: ai.RoleAssistant, Content: []ai.Block{ai.ThinkingBlock{Thinking: "checking"}}},
		{Role: ai.RoleAssistant, StopReason: ai.StopReasonAborted}, // turn that produced nothing
	} {
		if err := st.Append(&session.MessageEntry{Message: m}); err != nil {
			t.Fatal(err)
		}
	}
	rows := treeEntries(st)
	if len(rows) != 5 {
		t.Fatalf("rows = %d, want 5", len(rows))
	}
	want := []string{"run the tests", "bash · go test ./...", "↩ bash", "… checking", "(aborted)"}
	for i, r := range rows {
		if r.Summary != want[i] {
			t.Errorf("row %d (%s): summary = %q, want %q", i, r.Role, r.Summary, want[i])
		}
	}
}

// TestSessionLabelSidecarRoundTrip pins label persistence: set, overwrite,
// and clear on the dataDir sidecar JSON.
func TestSessionLabelSidecarRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(config.DataDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := loadSessionLabels(); len(got) != 0 {
		t.Fatalf("missing sidecar = %v, want empty", got)
	}

	if err := saveSessionLabel("aaaa1111", "milestone"); err != nil {
		t.Fatal(err)
	}
	if err := saveSessionLabel("bbbb2222", "other"); err != nil {
		t.Fatal(err)
	}
	if err := saveSessionLabel("aaaa1111", ""); err != nil {
		t.Fatal(err)
	}

	m := loadSessionLabels()
	if len(m) != 1 || m["bbbb2222"] != "other" {
		t.Fatalf("labels = %v, want only bbbb2222", m)
	}
}
