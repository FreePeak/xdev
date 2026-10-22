package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// childFixtureMsgs are the messages a finished child leaves in its file.
var (
	childFixtureUser      = ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "list the files"}}}
	childFixtureAssistant = ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "three of them"}}}
	childFixtureTool      = ai.Message{Role: ai.RoleToolResult, ToolName: "read", Content: []ai.Block{ai.TextBlock{Text: "1:x"}}}
)

// writeChildSession writes a REAL session file — the same writer a child uses
// (session.OpenMem + EnsureOnDisk, internal/agent/subagent.go:255-265) — so
// the reader is exercised against the bytes a finished child leaves behind,
// not against a hand-written JSON blob.
func writeChildSession(t *testing.T, path string, msgs ...ai.Message) {
	t.Helper()
	store := session.OpenMem(t.TempDir(), "subagent: reader")
	for _, m := range msgs {
		if err := store.Append(&session.MessageEntry{Message: m}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.EnsureOnDisk(path, session.Options{}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

// tornFragment is the tail of a record whose flush was interrupted: valid
// JSON so far, never terminated.
const tornFragment = `{"type":"message","id":"half-writ`

// appendTornTail simulates a child that is still writing: the last record
// landed half-way and no newline followed it.
func appendTornTail(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(tornFragment); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReadChildTranscript(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(t *testing.T, path string)
		wantRole []string
		wantText []string
		wantErr  string
	}{
		{
			name: "a finished child",
			setup: func(t *testing.T, path string) {
				writeChildSession(t, path, childFixtureUser, childFixtureAssistant, childFixtureTool)
			},
			wantRole: []string{"user", "assistant", "toolResult"},
			wantText: []string{"list the files", "three of them", "1:x"},
		},
		{
			name: "a child that is still being written",
			setup: func(t *testing.T, path string) {
				writeChildSession(t, path, childFixtureUser, childFixtureAssistant)
				appendTornTail(t, path)
			},
			wantRole: []string{"user", "assistant"},
			wantText: []string{"list the files", "three of them"},
		},
		{
			name: "a child that never said anything",
			setup: func(t *testing.T, path string) {
				writeChildSession(t, path)
			},
		},
		{
			name:    "a missing file",
			setup:   func(t *testing.T, path string) {},
			wantErr: "no such file",
		},
		{
			name: "a file that is not a session transcript",
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte(`{"type":"message","id":"nope"}`+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "not a session transcript",
		},
		{
			name: "a file that is not JSON at all",
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("hello, this is not a session\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "not a session transcript",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "child.jsonl")
			tc.setup(t, path)

			got, err := ReadChildTranscript(path)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want an error mentioning %q, got entries %+v", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("read child transcript: %v", err)
			}
			if len(got) != len(tc.wantRole) {
				t.Fatalf("entries = %+v, want %d of them", got, len(tc.wantRole))
			}
			for i, e := range got {
				if e.Seq != i {
					t.Fatalf("entry %d has seq %d, want %d", i, e.Seq, i)
				}
				if e.Role != tc.wantRole[i] {
					t.Fatalf("entry %d role = %q, want %q", i, e.Role, tc.wantRole[i])
				}
				if e.Text != tc.wantText[i] {
					t.Fatalf("entry %d text = %q, want %q", i, e.Text, tc.wantText[i])
				}
			}
		})
	}
}

// The reader is read-only: it must not repair the torn tail, must not
// rewrite the file, and must not leave anything of its own behind.
func TestReadChildTranscriptLeavesTheFileAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "child.jsonl")
	writeChildSession(t, path, childFixtureUser, childFixtureAssistant)
	appendTornTail(t, path)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	entries, err := ReadChildTranscript(path)
	if err != nil {
		t.Fatalf("read child transcript: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %+v, want the 2 intact records", entries)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the reader rewrote the session file")
	}
	if !bytes.HasSuffix(after, []byte(tornFragment)) {
		t.Fatal("the reader repaired the torn tail; reading must not touch it")
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("the reader created %d file(s) next to the session", len(files)-1)
	}
}
