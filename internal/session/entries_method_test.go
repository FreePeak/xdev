package session

import (
	"bytes"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
)

// TestCompactionEntryMethodRoundTrip pins the wire field the compaction ladder
// records (M5 #24): the method survives marshal/parse, an entry written before
// the field existed still parses (forward compat), and an unset method is
// omitted from the line rather than written as an empty string.
func TestCompactionEntryMethodRoundTrip(t *testing.T) {
	entry := &CompactionEntry{
		Env:     Envelope{Type: TypeCompaction, ID: "a1b2c3d4", Timestamp: time.Unix(0, 0).UTC()},
		Summary: ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "kept"}}},
		Method:  "snapcompact",
	}
	raw, err := MarshalEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseEntry(raw)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := parsed.(*CompactionEntry)
	if !ok {
		t.Fatalf("parsed %T, want *CompactionEntry", parsed)
	}
	if got.Method != "snapcompact" {
		t.Fatalf("method = %q, want snapcompact", got.Method)
	}

	// A line from before the ladder existed (no "method") keeps parsing, with
	// the method left empty.
	legacy, err := ParseEntry([]byte(`{"type":"compaction","id":"a1b2c3d4","parentId":null,"timestamp":"2026-09-07T03:20:45.220Z","summary":{"role":"assistant","content":[]},"firstKeptEntryId":null,"tokensBefore":5}`))
	if err != nil {
		t.Fatal(err)
	}
	if c := legacy.(*CompactionEntry); c.Method != "" {
		t.Fatalf("legacy entry method = %q, want empty", c.Method)
	}

	// An unset method is omitted: an older reader must not see a field it has
	// no meaning for (the summary is what it needs).
	blank, err := MarshalEntry(&CompactionEntry{Env: Envelope{Type: TypeCompaction, ID: "b1b2c3d4"}})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blank, []byte(`"method"`)) {
		t.Fatalf("empty method must be omitted:\n%s", blank)
	}
}
