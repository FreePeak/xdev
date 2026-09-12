package main

import (
	"os"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/session"
)

// TestSummarizeAndBranchAppendsAndSwitches pins the /tree Shift+Enter
// contract: a branch_summary entry is recorded on the abandoned leaf, then
// the leaf moves to the target.
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
	abandoned := st.LeafID()

	if err := summarizeAndBranch(st, first); err != nil {
		t.Fatal(err)
	}
	if got := st.LeafID(); got != first {
		t.Fatalf("leaf = %q, want %q", got, first)
	}

	summaries := 0
	for _, e := range st.Entries() {
		bs, ok := e.(*session.BranchSummaryEntry)
		if !ok {
			continue
		}
		summaries++
		if bs.Env.ParentID != abandoned {
			t.Errorf("branch_summary parent = %q, want the abandoned leaf %q", bs.Env.ParentID, abandoned)
		}
	}
	if summaries != 1 {
		t.Fatalf("branch_summary entries = %d, want 1", summaries)
	}

	if err := summarizeAndBranch(st, "deadbeef"); err == nil {
		t.Fatal("unknown entry id must error")
	}
}

// TestTreeEntriesSnapshot pins the selector row data built from the store:
// depth from the parent chain, active = current leaf, role/summary filled.
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
	if rows[0].Depth != 0 || rows[1].Depth != 1 {
		t.Fatalf("depths = %d,%d; want 0,1", rows[0].Depth, rows[1].Depth)
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
