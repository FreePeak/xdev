package session

import (
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

func ctx(t *testing.T, entries []Entry, leaf string) *ContextResult {
	t.Helper()
	r, err := buildContext(entries, leaf, SystemPrompt{Text: "sys"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func roles(r *ContextResult) string {
	out := ""
	for _, m := range r.Messages {
		out += string(m.Role) + ","
	}
	return out
}

func TestBuildContextBasicChain(t *testing.T) {
	entries := []Entry{
		userMsg("11111111", "", "hi"),
		asstMsg("22222222", "11111111", "hello"),
		userMsg("33333333", "22222222", "again"),
	}
	r := ctx(t, entries, "33333333")
	if roles(r) != "user,assistant,user," {
		t.Fatalf("roles = %s", roles(r))
	}
	if r.Model != "" {
		t.Fatalf("model = %q", r.Model)
	}
	if r.Messages[0].Text() != "hi" {
		t.Fatal("message content lost")
	}
}

func TestBuildContextModelChangeLatestWins(t *testing.T) {
	entries := []Entry{
		&ModelChangeEntry{Env: env(TypeModelChange, "11111111", "", ts0), Model: "onegw/gpt"},
		userMsg("22222222", "11111111", "hi"),
		&ModelChangeEntry{Env: env(TypeModelChange, "33333333", "22222222", ts0), Model: "router/free"},
	}
	r := ctx(t, entries, "33333333")
	if r.Model != "router/free" {
		t.Fatalf("model = %q, want router/free (latest wins)", r.Model)
	}
}

func TestBuildContextDanglingToolCallNeutralized(t *testing.T) {
	call := ai.ToolCallBlock{ID: "call_dangling", Name: "bash", Arguments: []byte(`{}`)}
	entries := []Entry{
		userMsg("11111111", "", "hi"),
		asstMsg("22222222", "11111111", "", call), // tool call with NO result following
		asstMsg("33333333", "22222222", "next turn"),
	}
	r := ctx(t, entries, "33333333")
	// The dangling tool call must be dropped; the assistant message keeps
	// nothing but its (empty) content — a message with no blocks is dropped
	// only if it became empty? Policy: keep the message shell.
	foundCall := false
	for _, m := range r.Messages {
		for _, b := range m.Content {
			if tc, ok := b.(ai.ToolCallBlock); ok && tc.ID == "call_dangling" {
				foundCall = true
			}
		}
	}
	if foundCall {
		t.Fatal("dangling toolCall survived into context")
	}
	if len(r.Messages) < 2 {
		t.Fatalf("messages = %d", len(r.Messages))
	}
}

func TestBuildContextToolCallKeptWhenAnswered(t *testing.T) {
	call := ai.ToolCallBlock{ID: "call_ok", Name: "bash", Arguments: []byte(`{}`)}
	entries := []Entry{
		userMsg("11111111", "", "hi"),
		asstMsg("22222222", "11111111", "", call),
		toolResultMsg("33333333", "22222222", "call_ok", "done"),
		asstMsg("44444444", "33333333", "final"),
	}
	r := ctx(t, entries, "44444444")
	found := false
	for _, m := range r.Messages {
		if m.Role == ai.RoleAssistant {
			for _, b := range m.Content {
				if tc, ok := b.(ai.ToolCallBlock); ok && tc.ID == "call_ok" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("answered toolCall was dropped")
	}
	if roles(r) != "user,assistant,toolResult,assistant," {
		t.Fatalf("roles = %s", roles(r))
	}
}

func TestBuildContextOrphanToolResultDropped(t *testing.T) {
	entries := []Entry{
		userMsg("11111111", "", "hi"),
		toolResultMsg("22222222", "11111111", "call_ghost", "orphan"),
		asstMsg("33333333", "22222222", "done"),
	}
	r := ctx(t, entries, "33333333")
	for _, m := range r.Messages {
		if m.Role == ai.RoleToolResult {
			t.Fatal("orphan toolResult survived into context")
		}
	}
	if roles(r) != "user,assistant," {
		t.Fatalf("roles = %s", roles(r))
	}
}

func TestBuildContextResetBoundaryCutsHistory(t *testing.T) {
	entries := []Entry{
		userMsg("11111111", "", "before reset 1"),
		asstMsg("22222222", "11111111", "before reset 2"),
		&ResetBoundaryEntry{Env: env(TypeResetBoundary, "33333333", "22222222", ts0)},
		userMsg("44444444", "33333333", "after reset"),
	}
	r := ctx(t, entries, "44444444")
	if roles(r) != "user," {
		t.Fatalf("roles = %s (everything before boundary must be dropped)", roles(r))
	}
	if r.Messages[0].Text() != "after reset" {
		t.Fatalf("text = %q", r.Messages[0].Text())
	}
}

func TestBuildContextLatestResetBoundaryWins(t *testing.T) {
	entries := []Entry{
		userMsg("11111111", "", "old1"),
		&ResetBoundaryEntry{Env: env(TypeResetBoundary, "22222222", "11111111", ts0)},
		userMsg("33333333", "22222222", "mid1"),
		&ResetBoundaryEntry{Env: env(TypeResetBoundary, "44444444", "33333333", ts0)},
		userMsg("55555555", "44444444", "new1"),
	}
	r := ctx(t, entries, "55555555")
	if roles(r) != "user," || r.Messages[0].Text() != "new1" {
		t.Fatalf("roles = %s text = %q", roles(r), r.Messages[0].Text())
	}
}

func TestBuildContextResetBoundaryAtLeaf(t *testing.T) {
	entries := []Entry{
		userMsg("11111111", "", "old"),
		&ResetBoundaryEntry{Env: env(TypeResetBoundary, "22222222", "11111111", ts0)},
	}
	r := ctx(t, entries, "22222222")
	if len(r.Messages) != 0 {
		t.Fatalf("messages = %d, want 0 (nothing after boundary)", len(r.Messages))
	}
}

func mkCompaction(id, parent, summary string, kept *string) *CompactionEntry {
	return &CompactionEntry{
		Env:              env(TypeCompaction, id, parent, ts0),
		Summary:          ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: summary}}},
		FirstKeptEntryID: kept,
	}
}

func TestBuildContextCompactionSummaryPlusKept(t *testing.T) {
	kept := "44444444"
	entries := []Entry{
		userMsg("11111111", "", "dropped 1"),
		asstMsg("22222222", "11111111", "dropped 2"),
		mkCompaction("33333333", "22222222", "summary text", &kept),
		userMsg("44444444", "33333333", "kept A"),
		userMsg("55555555", "44444444", "kept B"),
	}
	r := ctx(t, entries, "55555555")
	if roles(r) != "assistant,user,user," {
		t.Fatalf("roles = %s (summary + only entries after firstKeptEntryId)", roles(r))
	}
	if r.Messages[0].Text() != "summary text" {
		t.Fatalf("first message = %q", r.Messages[0].Text())
	}
	if r.Messages[1].Text() != "kept A" || r.Messages[2].Text() != "kept B" {
		t.Fatalf("kept window wrong: %q %q", r.Messages[1].Text(), r.Messages[2].Text())
	}
}

func TestBuildContextCompactionNullKept(t *testing.T) {
	entries := []Entry{
		userMsg("11111111", "", "dropped"),
		mkCompaction("22222222", "11111111", "only summary", nil),
		userMsg("33333333", "22222222", "after compaction"),
	}
	r := ctx(t, entries, "33333333")
	// firstKeptEntryId null → only the summary is emitted.
	if roles(r) != "assistant," {
		t.Fatalf("roles = %s", roles(r))
	}
	if r.Messages[0].Text() != "only summary" {
		t.Fatalf("text = %q", r.Messages[0].Text())
	}
}

func TestBuildContextEntryIDsAlignWithMessages(t *testing.T) {
	kept := "44444444"
	entries := []Entry{
		userMsg("11111111", "", "dropped 1"),
		asstMsg("22222222", "11111111", "dropped 2"),
		mkCompaction("33333333", "22222222", "summary text", &kept),
		userMsg("44444444", "33333333", "kept A"),
		asstMsg("55555555", "44444444", "kept B"),
	}
	r := ctx(t, entries, "55555555")
	// EntryIDs is parallel to Messages: "" for the synthesized summary,
	// the source entry id for every store-backed message.
	wantIDs := []string{"", "44444444", "55555555"}
	if len(r.EntryIDs) != len(r.Messages) {
		t.Fatalf("EntryIDs len %d != Messages len %d", len(r.EntryIDs), len(r.Messages))
	}
	for i, want := range wantIDs {
		if r.EntryIDs[i] != want {
			t.Fatalf("EntryIDs[%d] = %q, want %q", i, r.EntryIDs[i], want)
		}
	}
}

func TestBuildContextBranchSummaryAsUserMessage(t *testing.T) {
	entries := []Entry{
		userMsg("11111111", "", "hi"),
		&BranchSummaryEntry{
			Env:     env(TypeBranchSummary, "22222222", "11111111", ts0),
			Summary: ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "abandoned: tried X"}}},
		},
		userMsg("33333333", "22222222", "next"),
	}
	r := ctx(t, entries, "33333333")
	// Summary must surface as a user message so the model knows what was
	// abandoned, before the current user turn.
	if roles(r) != "user,user,user," {
		t.Fatalf("roles = %s", roles(r))
	}
	if r.Messages[1].Role != ai.RoleUser || r.Messages[1].Text() != "abandoned: tried X" {
		t.Fatalf("branch summary = %+v", r.Messages[1])
	}
}

func TestBuildContextTokensBeforeSumsUsage(t *testing.T) {
	entries := []Entry{
		userMsg("11111111", "", "hi"),
		&MessageEntry{Env: env(TypeMessage, "22222222", "11111111", ts0), Message: ai.Message{
			Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "a"}},
			Usage: &ai.Usage{Input: 100, Output: 23, TotalTokens: 123},
		}},
		&MessageEntry{Env: env(TypeMessage, "33333333", "22222222", ts0), Message: ai.Message{
			Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "b"}},
			Usage: &ai.Usage{Input: 200, Output: 77, TotalTokens: 277},
		}},
	}
	r := ctx(t, entries, "33333333")
	if r.TokensBefore != 400 {
		t.Fatalf("TokensBefore = %d, want 400", r.TokensBefore)
	}
}

func TestBuildContextCycleBounded(t *testing.T) {
	// a → b → a (cycle); buildContext must not hang.
	entries := []Entry{
		userMsg("11111111", "22222222", "a"),
		userMsg("22222222", "11111111", "b"),
	}
	if _, err := buildContext(entries, "11111111", SystemPrompt{Text: ""}); err != nil {
		t.Fatalf("cycle must be bounded, got %v", err)
	}
}

func TestBuildContextUnknownLeaf(t *testing.T) {
	entries := []Entry{userMsg("11111111", "", "a")}
	r, err := buildContext(entries, "nonexistent", SystemPrompt{Text: ""})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Messages) != 0 {
		t.Fatalf("messages = %d, want 0", len(r.Messages))
	}
}

func TestBuildContextOnlyPathFromLeaf(t *testing.T) {
	// A branched tree: entries not on the leaf's path must be excluded.
	shared := userMsg("11111111", "", "shared")
	branchA := asstMsg("22222222", "11111111", "A")
	branchB := asstMsg("33333333", "11111111", "B")
	entries := []Entry{shared, branchA, branchB}
	r := ctx(t, entries, "33333333")
	if roles(r) != "user,assistant," {
		t.Fatalf("roles = %s", roles(r))
	}
	if !strings.Contains(r.Messages[1].Text(), "B") {
		t.Fatal("wrong branch in context")
	}
}

func TestBuildContextSystemPassthrough(t *testing.T) {
	entries := []Entry{userMsg("11111111", "", "x")}
	r, err := buildContext(entries, "11111111", SystemPrompt{Text: "be helpful"})
	if err != nil {
		t.Fatal(err)
	}
	if r.System != "be helpful" {
		t.Fatalf("system = %q", r.System)
	}
}

func TestBuildContextDanglingParentStopsPath(t *testing.T) {
	// Parent id references an entry that is not in the slice (omp files with
	// skipped unknown-type entries in the chain).
	entries := []Entry{
		userMsg("11111111", "ghost000", "child of unknown type"),
	}
	r, err := buildContext(entries, "11111111", SystemPrompt{})
	if err != nil {
		t.Fatalf("dangling parent must not fail the build: %v", err)
	}
	if len(r.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(r.Messages))
	}
}
