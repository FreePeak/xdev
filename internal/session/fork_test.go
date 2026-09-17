package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
)

// TestForkSession proves: new file with rewritten header (new id,
// parentSession = source id), entries copied verbatim, and the returned
// Store reads the fork's own identity.
func TestForkSession(t *testing.T) {
	dir := t.TempDir()
	src := OpenMem("/proj", "original")
	path, err := src.EnsureOnDisk(filepath.Join(dir, "src.jsonl"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Append(&MessageEntry{Env: Envelope{ID: "u1"}, Message: ai.Message{
		Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hello"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := src.Append(&MessageEntry{Env: Envelope{ID: "a1", ParentID: "u1"}, Message: ai.Message{
		Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "hi"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}

	forkID := newForkID()
	fork, err := ForkSession(path, SessionFilePath(dir, "/proj", time.Now(), forkID), "")
	if err != nil {
		t.Fatal(err)
	}
	defer fork.Close()

	if fork.ID() == src.ID() || fork.ID() == "" {
		t.Fatalf("fork id must be new and non-empty: %q", fork.ID())
	}
	// Header id MUST equal the file-name id: /resume <prefix> matches
	// header ids, so a mismatched header leaves the fork unaddressable.
	if fork.ID() != forkID {
		t.Fatalf("fork header id %q != file-name id %q", fork.ID(), forkID)
	}
	res, err := BuildContext(fork.Entries(), fork.LeafID(), SystemPrompt{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) < 2 {
		t.Fatalf("fork lost entries: %d messages", len(res.Messages))
	}
	raw, err := os.ReadFile(fork.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !stringContains(string(raw), `"parentSession":"`+src.ID()+`"`) {
		t.Fatalf("fork header missing parentSession=%s", src.ID())
	}
}

// TestForkSessionRejectsInvalid proves graceful failure on a file without
// a session header.
func TestForkSessionRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.jsonl")
	if err := os.WriteFile(bad, []byte("only a title slot\nnot a header\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ForkSession(bad, dir, ""); err == nil {
		t.Fatal("expected error for missing session header")
	}
}

// TestForkSessionFreshTimestamp: the fork header timestamp moves forward.
func TestForkSessionFreshTimestamp(t *testing.T) {
	dir := t.TempDir()
	src := OpenMem("/proj", "t")
	path, err := src.EnsureOnDisk(filepath.Join(dir, "s.jsonl"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	_ = src.Append(&MessageEntry{Env: Envelope{ID: "u1"}, Message: ai.Message{
		Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "x"}},
	}})
	_ = src.Close()

	fork, err := ForkSession(path, SessionFilePath(dir, "/proj", time.Now(), newForkID()), "my fork")
	if err != nil {
		t.Fatal(err)
	}
	defer fork.Close()
	if fork.Title() != "my fork" {
		t.Fatalf("fork title = %q, want 'my fork'", fork.Title())
	}
	if time.Since(fork.Entries()[0].Envelope().Timestamp) > time.Minute {
		t.Fatal("fork timestamp must be fresh")
	}
}

// TestForkSessionIDFromFilename pins the id source: the header id is
// derived from the canonical <timestamp>_<uuid>.jsonl file name, with a
// fresh-uuid fallback when the name carries none.
func TestForkSessionIDFromFilename(t *testing.T) {
	cases := []struct {
		name     string
		destPath string
		want     string // "" = fallback: any fresh uuid
	}{
		{"canonical name", "/d/2026-09-11T00-00-00.000Z_abc123.jsonl", "abc123"},
		{"no underscore", "/d/plain.jsonl", ""},
		{"empty suffix", "/d/2026-09-11T00-00-00.000Z_.jsonl", ""},
		{"no .jsonl suffix", "/d/2026-09-11T00-00-00.000Z_abc123", "abc123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionIDFromPath(tc.destPath)
			if tc.want != "" {
				if got != tc.want {
					t.Fatalf("id = %q, want %q", got, tc.want)
				}
				return
			}
			// Fallback path: must mint a fresh uuid, never reuse the name.
			if got == "" || got == tc.destPath {
				t.Fatalf("fallback id = %q, want a fresh uuid", got)
			}
		})
	}
}

// newForkID mints a fork id via the package uuid helper.
func newForkID() string { return newUUID() }

// --- helpers ---

func stringContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

var _ = time.Now

// parentID returns a pointer to s (Go <1.26 toolchain form).
func parentID(s string) *string { return &s }
