package tool

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// todoHydrated builds a store holding a session that was resumed: the entries
// are appended in file order and LeafID follows the chain.
func todoStoreWithEntries(t *testing.T, entries ...session.Entry) *session.Store {
	t.Helper()
	store := session.OpenMem("/proj", "resume test")
	if _, err := store.EnsureOnDisk(filepath.Join(t.TempDir(), "s.jsonl"), session.Options{}); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := store.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func todoCustomEntry(phases []TodoPhase) *session.CustomEntry {
	return &session.CustomEntry{CustomType: "user_todo_edit", Data: map[string]any{"phases": phases}}
}

// TestWireTodoSinkRestoresTheList: the resumed session's TASKS section comes
// back from the transcript. This is the regression: WireTodoSink used to only
// attach the sink, so every /resume showed an empty list (#291).
func TestWireTodoSinkRestoresTheList(t *testing.T) {
	want := []TodoPhase{{Name: "Build", Tasks: []TodoItem{
		{Content: "ship it", Status: TodoCompleted},
		{Content: "write tests", Status: TodoInProgress},
	}}}
	// Round-trip the payload through JSON: that is what a real store reads
	// back ([]any of maps), not the in-process []TodoPhase.
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var disk []any
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatal(err)
	}
	store := todoStoreWithEntries(t,
		&session.MessageEntry{Message: ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "go"}}}},
		&session.CustomEntry{CustomType: "user_todo_edit", Data: map[string]any{"phases": disk}},
	)

	tt := testTodoTool()
	reg := NewRegistry()
	reg.Register(tt)
	if !WireTodoSink(reg, store) {
		t.Fatal("WireTodoSink must find the todo tool")
	}
	got := todoStatuses(t, tt)
	if got["ship it"] != TodoCompleted || got["write tests"] != TodoInProgress {
		t.Fatalf("restored statuses = %v", got)
	}
}

// TestWireTodoSinkNewestEditWins: two edits on the branch, the later one is
// the state — the same "newest user_todo_edit wins" rule omp replays with.
func TestWireTodoSinkNewestEditWins(t *testing.T) {
	first := []TodoPhase{{Name: "p", Tasks: []TodoItem{{Content: "old", Status: TodoCompleted}}}}
	second := []TodoPhase{{Name: "p", Tasks: []TodoItem{{Content: "new", Status: TodoInProgress}}}}
	store := todoStoreWithEntries(t, todoCustomEntry(first), todoCustomEntry(second))

	tt := testTodoTool()
	reg := NewRegistry()
	reg.Register(tt)
	WireTodoSink(reg, store)

	got := todoStatuses(t, tt)
	if _, ok := got["old"]; ok {
		t.Fatalf("the older edit must be hidden: %v", got)
	}
	if got["new"] != TodoInProgress {
		t.Fatalf("newest edit not restored: %v", got)
	}
}

// TestWireTodoSinkResetBoundaryHidesEarlierLists: /clear cuts the branch, so a
// list written before it must not come back into the new session.
func TestWireTodoSinkResetBoundaryHidesEarlierLists(t *testing.T) {
	stale := []TodoPhase{{Name: "p", Tasks: []TodoItem{{Content: "stale", Status: TodoPending}}}}
	store := todoStoreWithEntries(t, todoCustomEntry(stale), &session.ResetBoundaryEntry{})

	tt := testTodoTool()
	reg := NewRegistry()
	reg.Register(tt)
	WireTodoSink(reg, store)

	if got := todoStatuses(t, tt); len(got) != 0 {
		t.Fatalf("a reset must hide the pre-boundary list, got %v", got)
	}
}

// TestWireTodoSinkFallsBackToToolResultPhases: a store whose custom entries
// were pruned (or written by an older build) still reconstructs from the todo
// toolResult's details.phases — omp's second replay source.
func TestWireTodoSinkFallsBackToToolResultPhases(t *testing.T) {
	phases := []TodoPhase{{Name: "p", Tasks: []TodoItem{{Content: "from details", Status: TodoInProgress}}}}
	store := todoStoreWithEntries(t,
		&session.MessageEntry{Message: ai.Message{
			Role:     ai.RoleToolResult,
			ToolName: "todo",
			Details:  map[string]any{"phases": phases},
		}},
	)

	tt := testTodoTool()
	reg := NewRegistry()
	reg.Register(tt)
	WireTodoSink(reg, store)

	if got := todoStatuses(t, tt); got["from details"] != TodoInProgress {
		t.Fatalf("details.phases fallback not restored: %v", got)
	}
}

// TestWireTodoSinkEmptySessionClears: wiring a fresh session over a tool that
// already holds a list must not leak the previous session's tasks (/new).
func TestWireTodoSinkEmptySessionClears(t *testing.T) {
	tt := testTodoTool()
	reg := NewRegistry()
	reg.Register(tt)
	todoExec(t, tt, map[string]any{"op": "init", "items": []string{"previous session"}})

	WireTodoSink(reg, todoStoreWithEntries(t))
	if got := todoStatuses(t, tt); len(got) != 0 {
		t.Fatalf("a new session must start with no tasks, got %v", got)
	}
}
