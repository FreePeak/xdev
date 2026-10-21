package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The omp-shaped fixture: a thinking_level_change whose "configured" is a
// LEVEL STRING ("auto" — the level is inherited from the model role's
// ":effort" default, not pinned by the user).
const ompThinkingFixture = "testdata/omp-thinking-configured.jsonl"

// TestOpenReadsTheOMPFixture is the contract that broke the file: omp
// writes "configured" as a level string, and a reader that only knew a
// boolean refused the WHOLE session file on the type mismatch. The file
// must open, and every entry in it must survive.
func TestOpenReadsTheOMPFixture(t *testing.T) {
	s, err := Open(ompThinkingFixture)
	if err != nil {
		t.Fatalf("open the omp fixture: %v", err)
	}
	defer s.Close()

	// The fixture holds 2 messages, 1 model change and 1 thinking level
	// change.
	entries := s.Entries()
	if len(entries) != 4 {
		t.Fatalf("entries = %d, want 4", len(entries))
	}
	messages, models, levels := 0, 0, 0
	for _, e := range entries {
		switch e.(type) {
		case *MessageEntry:
			messages++
		case *ModelChangeEntry:
			models++
		case *ThinkingLevelChangeEntry:
			levels++
		}
	}
	if messages != 2 || models != 1 || levels != 1 {
		t.Errorf("messages = %d, models = %d, levels = %d, want 2, 1 and 1", messages, models, levels)
	}
}

// writeThinkingFixture copies the omp fixture with its "configured" value
// replaced, so the bool and the nonsense cases run against the same file
// shape a real session has.
func writeThinkingFixture(t *testing.T, configured string) string {
	t.Helper()
	raw, err := os.ReadFile(ompThinkingFixture)
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Replace(string(raw), `"configured":"auto"`, `"configured":`+configured, 1)
	if out == string(raw) {
		t.Fatal("the fixture does not carry the configured value to replace")
	}
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
