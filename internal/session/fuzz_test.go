package session

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzSessionLoad feeds untrusted bytes through the loader's parsers
// (M8): a session file may come from another harness or a corrupt disk,
// and the loader must never panic — only accept, skip, or error.
func FuzzSessionLoad(f *testing.F) {
	f.Add([]byte(`{"type":"message","id":"a","parentId":"","timestamp":"2026-09-11T00:00:00Z","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`))
	f.Add([]byte(`{"type":"compaction","id":"c","parentId":"a","summary":{},"firstKeptEntryId":"a"}`))
	f.Add([]byte(`{"type":"reset_boundary","id":"r","parentId":""}`))
	f.Add([]byte(`{"type":"unknown-thing","id":"u","parentId":"a"}`))
	f.Add([]byte(`{`))
	f.Add([]byte(``))

	f.Fuzz(func(t *testing.T, line []byte) {
		// Direct parser entry points: must not panic.
		if _, err := ParseEntry(line); err != nil {
			_ = err
		}
		if _, err := ParseEnvelope(line); err != nil {
			_ = err
		}
		// End to end through Open, with the bytes as one line of a file.
		dir := t.TempDir()
		path := filepath.Join(dir, "f.jsonl")
		content := append(append([]byte(nil), line...), '\n')
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		s, err := Open(path)
		if err != nil {
			return // a rejected file is a fine outcome
		}
		if s.LeafID() != "" {
			_ = s.Tree()
			_, _ = BuildContext(s.Entries(), s.LeafID(), SystemPrompt{})
		}
	})
}
