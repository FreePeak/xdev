package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

// TestResumedStoreHandsBackToolResultsAndTodoEdits is the guard on the resume
// path's *input* — the thing #291's empty dock was diagnosed against. A real
// session file on disk must open with its tool results carrying the details
// (exit code, unified diff) the dock's FILES section and the todo hydration
// both read, and its user_todo_edit entries intact. If this ever regresses, the
// dock goes blank on resume again with no error anywhere.
func TestResumedStoreHandsBackToolResultsAndTodoEdits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	lines := []Entry{
		&MessageEntry{Message: ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "edit the file"}}}},
		&MessageEntry{Message: ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "on it"}}}},
		&MessageEntry{Message: ai.Message{
			Role: ai.RoleToolResult, ToolName: "edit", DurationMS: 70,
			Details: map[string]any{"unifiedDiff": "--- a/x.go\n+++ b/x.go\n", "resolvedPath": "x.go"},
		}},
		&MessageEntry{Message: ai.Message{
			Role: ai.RoleToolResult, ToolName: "bash",
			Details: map[string]any{"exitCode": 2, "truncated": true},
		}},
		&CustomEntry{CustomType: "user_todo_edit", Data: map[string]any{"phases": []any{
			map[string]any{"name": "Build", "tasks": []any{
				map[string]any{"content": "ship it", "status": "completed"},
			}},
		}}},
	}
	raw := MarshalHeader(SessionHeader{Version: 3, ID: "s", CWD: "/proj", Title: "resume"})
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// A resumed session is written by appending to the reopened file, so the
	// fixture mirrors the tool that produced it.
	for _, e := range lines {
		if err := store.Append(e); err != nil {
			t.Fatal(err)
		}
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	var toolResults int
	var withDiff, withExit, withTodo int
	for _, e := range reopened.Entries() {
		switch v := e.(type) {
		case *MessageEntry:
			if v.Message.Role != ai.RoleToolResult {
				continue
			}
			toolResults++
			d, _ := v.Message.Details.(map[string]any)
			if diff, _ := d["unifiedDiff"].(string); diff != "" {
				withDiff++
			}
			if code, ok := d["exitCode"].(float64); ok && int(code) == 2 {
				withExit++
			}
			if v.Message.ToolName == "edit" && v.Message.DurationMS != 70 {
				t.Fatalf("duration did not survive the file: %d", v.Message.DurationMS)
			}
		case *CustomEntry:
			if v.CustomType == "user_todo_edit" {
				withTodo++
			}
		}
	}
	if toolResults != 2 || withDiff != 1 || withExit != 1 || withTodo != 1 {
		t.Fatalf("resumed store: %d tool results (%d with a diff, %d with exit 2), %d todo edits",
			toolResults, withDiff, withExit, withTodo)
	}
	// The resume path builds its context from the same entries the dock reads.
	res, err := BuildContext(reopened.Entries(), reopened.LeafID(), SystemPrompt{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) == 0 {
		t.Fatal("rebuilt context is empty")
	}
}
