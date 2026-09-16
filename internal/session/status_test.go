package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

// TestClassifyStatus: the badge comes from the last message entry, past the
// bookkeeping the store appends after it (model_change, session_exit, branch
// markers). Each case is a terminal shape seen in the on-disk store.
func TestClassifyStatus(t *testing.T) {
	const modelChange = `{"type":"model_change","id":"e8","parentId":"e7","model":"onegw/free"}`
	const exit = `{"type":"custom","id":"e9","parentId":"e8","customType":"session_exit","data":{"code":0,"mode":"tui"}}`
	const branch = `{"type":"custom","id":"ea","parentId":"e9","customType":"branch","data":{"to":"abc12345"}}`

	line := func(e Entry) string { return string(MarshalMust(t, e)) }
	user := line(userMsg("e1", "", "do it"))
	asstStop := line(asstMsg("e2", "e1", "done"))
	asstCalls := line(asstMsg("e3", "e1", "", ai.ToolCallBlock{ID: "c1", Name: "bash"}))
	toolResult := line(toolResultMsg("e4", "e3", "c1", "output"))
	withStop := func(r ai.StopReason) string {
		return line(&MessageEntry{Message: ai.Message{Role: ai.RoleAssistant, StopReason: r}})
	}

	for _, tc := range []struct {
		name string
		tail string
		want SessionStatus
	}{
		{"assistant stopped", asstStop, StatusDone},
		{"stopped, then model change and exit", asstStop + "\n" + modelChange + "\n" + exit, StatusDone},
		{"stopped, then a branch marker", asstStop + "\n" + exit + "\n" + branch, StatusDone},
		// A stop reason this build does not know (a wider vocabulary on disk)
		// must still classify: one file never breaks the listing.
		{"unknown stop reason", withStop("cancelled"), StatusDone},
		{"unknown block type", `{"type":"message","id":"e5","parentId":"e1","message":{"role":"assistant","stopReason":"stop","content":[{"type":"future-block","whatever":1}]}}`, StatusDone},
		{"assistant aborted", withStop(ai.StopReasonAborted), StatusInterrupted},
		{"assistant errored", withStop(ai.StopReasonError), StatusInterrupted},
		{"assistant hit the output limit", withStop(ai.StopReasonLength), StatusInterrupted},
		{"open tool call", asstCalls, StatusInterrupted},
		{"dangling tool result", toolResult + "\n" + exit, StatusInterrupted},
		{"bookkeeping after a dangling result", toolResult + "\n" + modelChange + "\n" + exit + "\n" + branch, StatusInterrupted},
		// An aborted turn persists nothing past the prompt it was answering,
		// so a user-last transcript IS the signature of the interruption.
		{"unanswered user prompt", user, StatusInterrupted},
		{"user prompt after a finished turn", asstStop + "\n" + user, StatusInterrupted},
		{"empty file", "", StatusInterrupted},
		{"header only (no message yet)", `{"type":"session","id":"e0"}`, StatusInterrupted},
		{"truncated first line is skipped", "...truncated\n" + asstStop, StatusDone},
		{"garbage lines are skipped", "not json\n" + asstStop + "\n{oops", StatusDone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyStatus([]byte(tc.tail)); got != tc.want {
				t.Fatalf("ClassifyStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestListSetsStatus: the listing carries the badge, and the stat-keyed cache
// refreshes it when the session grows (a finished turn flips to done).
func TestListSetsStatus(t *testing.T) {
	dir := t.TempDir()
	p := writeSession(t, dir, "-x", "s.jsonl", "sess", "/x")

	metas, err := listWithCache(dir, newStatCache())
	if err != nil || len(metas) != 1 {
		t.Fatalf("list: %+v %v", metas, err)
	}
	if metas[0].Status != StatusInterrupted {
		t.Fatalf("fresh session status = %q, want %q", metas[0].Status, StatusInterrupted)
	}

	// The assistant answers and the process exits cleanly: the tail — not the
	// prefix the header comes from — carries the verdict.
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []Entry{asstMsg("a1", "11111111", "answer"), &CustomEntry{CustomType: "session_exit", Data: map[string]any{"code": 0}}} {
		if _, err := f.WriteString(string(MarshalMust(t, e)) + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	metas, err = listWithCache(dir, newStatCache())
	if err != nil || len(metas) != 1 {
		t.Fatalf("list after append: %+v %v", metas, err)
	}
	if metas[0].Status != StatusDone {
		t.Fatalf("status after a finished turn = %q, want %q", metas[0].Status, StatusDone)
	}
}

// TestStatPathStatusBeyondPrefix: the terminal entry sits past the 4 KiB the
// listing reads for the header, and the badge still lands.
func TestStatPathStatusBeyondPrefix(t *testing.T) {
	dir := t.TempDir()
	p := writeSession(t, dir, "-x", "s.jsonl", "sess", "/x")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// One oversized tool result pushes the last message past the prefix (the
	// store holds lines far longer than this) and leaves a dangling call.
	if _, err := f.WriteString(string(MarshalMust(t, toolResultMsg("big", "11111111", "c1", strings.Repeat("x", 8192)))) + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	meta, ok := statPath(p, newStatCache())
	if !ok {
		t.Fatal("statPath failed")
	}
	if meta.SizeBytes <= 4096 {
		t.Fatalf("fixture too small (%d bytes): it must exceed the prefix", meta.SizeBytes)
	}
	if meta.Status != StatusInterrupted {
		t.Fatalf("status = %q, want %q", meta.Status, StatusInterrupted)
	}
}

// TestStatPathRejectsNonSession: a file with no header is refused, including
// one shorter than the prefix (the tail read must not misfire).
func TestStatPathRejectsNonSession(t *testing.T) {
	dir := t.TempDir()
	bucket := filepath.Join(dir, "sessions", "-x")
	if err := os.MkdirAll(bucket, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(bucket, "junk.jsonl")
	if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := statPath(p, newStatCache()); ok {
		t.Fatal("statPath accepted a file with no session header")
	}
}
