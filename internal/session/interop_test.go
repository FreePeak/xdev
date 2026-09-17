package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

// TestInteropOMPSample is the M2 exit criterion (PRD §3.2): xdev must parse
// an omp-generated session file and produce an equivalent LLM context.
// The fixture internal/session/testdata/omp-sample.jsonl is a real omp
// session (first turns, byte-identical prefix), checked in read-only.
func TestInteropOMPSample(t *testing.T) {
	fixture := "testdata/omp-sample.jsonl"
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	// Fixture sanity: real omp shape — 256-byte title slot + header + entries.
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("fixture too small: %d lines", len(lines))
	}
	if len(lines[0]) != TitleSlotWidth-1 {
		t.Fatalf("fixture title slot = %d bytes, want %d", len(lines[0]), TitleSlotWidth-1)
	}
	if !strings.Contains(string(raw), `"type":"model_change"`) {
		t.Fatal("fixture lost its model_change entry")
	}

	s, err := Open(fixture)
	if err != nil {
		t.Fatalf("Open real omp file: %v", err)
	}
	defer s.Close()

	// Fixture composition (verified against the real file):
	// 1 model_change, 6 messages, 3 custom, 1 title_change (unknown→opaque).
	entries := s.Entries()
	if len(entries) != 11 {
		t.Fatalf("entries = %d, want 11", len(entries))
	}
	var messages, customs, modelChanges, unknowns int
	for _, e := range entries {
		switch te := e.(type) {
		case *MessageEntry:
			messages++
		case *CustomEntry:
			customs++
		case *ModelChangeEntry:
			modelChanges++
		case *UnknownEntry:
			if te.EntryType != "title_change" {
				t.Fatalf("unexpected unknown type %q", te.EntryType)
			}
			unknowns++
		}
	}
	if messages != 6 || customs != 3 || modelChanges != 1 || unknowns != 1 {
		t.Fatalf("counts: messages=%d customs=%d modelChanges=%d unknowns=%d",
			messages, customs, modelChanges, unknowns)
	}

	// Leaf: last entry id from the file.
	if s.LeafID() != "fdaab2a0" {
		t.Fatalf("leaf = %q, want fdaab2a0", s.LeafID())
	}

	// Title from the slot, cwd from the header — both from the real file.
	if s.Title() != "Configure header to hide sidebar" {
		t.Fatalf("title = %q", s.Title())
	}
	if s.CWD() != "/home/dev/.config/herdr" {
		t.Fatalf("cwd = %q", s.CWD())
	}

	// Context reconstruction.
	r, err := buildContext(entries, s.LeafID(), SystemPrompt{Text: "sys"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Model != "router/free" {
		t.Fatalf("model = %q, want router/free", r.Model)
	}

	// Expected model-visible conversation, in order:
	// user → assistant(thinking+toolCall answered) → toolResult →
	// assistant(thinking+toolCall answered) → toolResult →
	// assistant(thinking+toolCall DANGLING → call neutralized, thinking kept).
	if len(r.Messages) != 6 {
		var got []string
		for _, m := range r.Messages {
			got = append(got, string(m.Role))
		}
		t.Fatalf("messages = %d (%v), want 6", len(r.Messages), got)
	}
	wantRoles := []ai.Role{
		ai.RoleUser, ai.RoleAssistant, ai.RoleToolResult,
		ai.RoleAssistant, ai.RoleToolResult, ai.RoleAssistant,
	}
	for i, want := range wantRoles {
		if r.Messages[i].Role != want {
			t.Fatalf("messages[%d].Role = %q, want %q", i, r.Messages[i].Role, want)
		}
	}

	// First user message content survived the round-trip.
	if got := r.Messages[0].Text(); got != "config my herdr hide the sidbar" {
		t.Fatalf("user text = %q", got)
	}

	// The last assistant message had a dangling tool call (call_184c...)
	// with no following toolResult in the fixture: its toolCall block must
	// be neutralized while the thinking block survives.
	last := r.Messages[5]
	for _, b := range last.Content {
		if tc, ok := b.(ai.ToolCallBlock); ok {
			t.Fatalf("dangling toolCall %s leaked into context", tc.ID)
		}
	}
	hasThinking := false
	for _, b := range last.Content {
		if _, ok := b.(ai.ThinkingBlock); ok {
			hasThinking = true
		}
	}
	if !hasThinking {
		t.Fatal("neutralized assistant message lost its thinking block")
	}

	// No dangling tool calls anywhere in the context: every toolCall in an
	// assistant message is answered by a later toolResult message.
	seen := map[string]bool{}
	for _, m := range r.Messages {
		switch m.Role {
		case ai.RoleAssistant:
			for _, tc := range m.ToolCalls() {
				if seen[tc.ID] {
					t.Fatalf("toolCall %s repeated", tc.ID)
				}
				seen[tc.ID] = false
			}
		case ai.RoleToolResult:
			done, known := seen[m.ToolCallID]
			if !known {
				t.Fatalf("toolResult %s without prior call", m.ToolCallID)
			}
			if done {
				t.Fatalf("toolResult %s duplicated", m.ToolCallID)
			}
			seen[m.ToolCallID] = true
		}
	}
	for id, answered := range seen {
		if !answered {
			t.Fatalf("dangling tool call %s in final context", id)
		}
	}
}

// TestInteropOMPSampleUnmodified verifies Open+Close never rewrite the
// omp file (unknown/foreign lines stay byte-exact on disk).
func TestInteropOMPSampleUnmodified(t *testing.T) {
	fixture := "testdata/omp-sample.jsonl"
	before, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("Open/Close modified the session file")
	}
}

// TestInteropOMPSampleBranch branches mid-history on the real omp fixture
// and verifies the in-memory leaf move + context narrowing. It works on a
// copy so the pristine (read-only) fixture is never touched.
func TestInteropOMPSampleBranch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "omp-copy.jsonl")
	raw, err := os.ReadFile("testdata/omp-sample.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Branch to the first assistant message (id from the real file).
	if err := s.Branch("7106ed9e"); err != nil {
		t.Fatal(err)
	}
	if s.LeafID() != "7106ed9e" {
		t.Fatalf("leaf = %s", s.LeafID())
	}
	r, err := buildContext(s.Entries(), s.LeafID(), SystemPrompt{})
	if err != nil {
		t.Fatal(err)
	}
	// Path: model_change → user → title_change(opaque) → custom → assistant.
	// The assistant's toolCall is dangling on this branch → neutralized.
	if len(r.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(r.Messages))
	}
	if r.Messages[1].Role != ai.RoleAssistant {
		t.Fatalf("role = %s", r.Messages[1].Role)
	}
	for _, b := range r.Messages[1].Content {
		if tc, ok := b.(ai.ToolCallBlock); ok {
			t.Fatalf("dangling toolCall %s leaked", tc.ID)
		}
	}

	// The branch marker must survive a reopen and restore the leaf.
	if err := s.Append(userMsg("", "", "on branch")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var foundMarker bool
	for _, e := range s2.Entries() {
		if c, ok := e.(*CustomEntry); ok && c.CustomType == TypeBranch {
			foundMarker = true
			if c.Data["to"] != "7106ed9e" {
				t.Fatalf("marker to = %v", c.Data["to"])
			}
		}
	}
	if !foundMarker {
		t.Fatal("branch marker missing after reopen")
	}
}
