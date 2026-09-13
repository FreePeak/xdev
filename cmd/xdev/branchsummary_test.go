package main

import (
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/session"
)

// #83: SummarizeBranchStore's summarizer seam had no production caller, so
// every branch summary was the fixed marker. The cmd side now resolves the
// cheap role and passes a real writer, keeping the marker as the fallback.

func TestBranchSummarizerNilWithoutRole(t *testing.T) {
	// No models.yml, no roles: the feature degrades to marker-only instead of
	// erroring the tree switch.
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	loadedSettings = &config.Settings{}
	t.Cleanup(func() { loadedSettings = nil })
	if got := branchSummarizer(); got != nil {
		t.Fatal("a host with no resolvable role must yield nil (marker fallback)")
	}
}

func TestBranchSummarizerRespectsDisabledSetting(t *testing.T) {
	off := false
	loadedSettings = &config.Settings{}
	loadedSettings.BranchSummary.Enabled = &off
	t.Cleanup(func() { loadedSettings = nil })
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	if got := branchSummarizer(); got != nil {
		t.Fatal("branchSummary.enabled false must disable the model call")
	}
}

// The engine contract, exercised through the cmd seam's fallback path: with a
// nil summarizer a branch switch still records a marker and moves the leaf.
func TestSummarizeAndBranchFallsBackToMarker(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	loadedSettings = &config.Settings{}
	t.Cleanup(func() { loadedSettings = nil })

	st := session.OpenMem("/proj", "marker fallback")
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Append(&session.MessageEntry{Message: ai.Message{
		Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "one"}},
	}}); err != nil {
		t.Fatal(err)
	}
	leaf := st.LeafID()
	if err := st.Append(&session.MessageEntry{Message: ai.Message{
		Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "two"}},
	}}); err != nil {
		t.Fatal(err)
	}
	target := st.LeafID()
	if target == "" || target == leaf {
		t.Fatalf("test needs two nodes (leaf=%q target=%q)", leaf, target)
	}
	if err := summarizeAndBranch(st, leaf); err != nil {
		t.Fatalf("summarizeAndBranch: %v", err)
	}
	var found bool
	for _, e := range st.Entries() {
		if bs, ok := e.(*session.BranchSummaryEntry); ok {
			found = true
			if !strings.Contains(bs.Summary.Text(), "branch") && !strings.Contains(bs.Summary.Text(), "abandoned") {
				t.Fatalf("unexpected marker text: %q", bs.Summary.Text())
			}
		}
	}
	if !found {
		t.Fatal("no branch_summary entry recorded")
	}
	if st.LeafID() != leaf {
		t.Fatalf("leaf = %q, want the switched branch point %q", st.LeafID(), leaf)
	}
}
