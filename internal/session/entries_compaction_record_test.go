package session

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

// The compaction record (omp rule 27) rides the wire: the surviving size and
// the one-line summary travel with the entry.
func TestCompactionEntryCarriesTheRecord(t *testing.T) {
	entry := &CompactionEntry{
		Summary:      ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "the summary"}}},
		TokensBefore: 900,
		TokensAfter:  120,
		ShortSummary: "the summary in one line",
		Method:       "handoff",
	}
	line, err := MarshalEntry(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(line, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["tokensAfter"] != float64(120) {
		t.Errorf("wire tokensAfter = %v, want 120", wire["tokensAfter"])
	}
	if wire["shortSummary"] != "the summary in one line" {
		t.Errorf("wire shortSummary = %v", wire["shortSummary"])
	}
	back, err := ParseEntry(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c, ok := back.(*CompactionEntry)
	if !ok {
		t.Fatalf("entry = %T, want *CompactionEntry", back)
	}
	if c.TokensAfter != 120 {
		t.Errorf("tokensAfter = %d, want 120", c.TokensAfter)
	}
	if c.ShortSummary != "the summary in one line" {
		t.Errorf("shortSummary = %q", c.ShortSummary)
	}
}

// The record is optional: an entry written before it existed still parses,
// and the writer does not claim a measurement it does not have.
func TestCompactionEntryToleratesAMissingRecord(t *testing.T) {
	line := `{"type":"compaction","id":"a1b2c3d4","parentId":null,"timestamp":"2026-09-07T03:20:45.220Z","summary":{"role":"assistant","content":[]},"firstKeptEntryId":null,"tokensBefore":5}`
	e, err := ParseEntry([]byte(line))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c, ok := e.(*CompactionEntry)
	if !ok {
		t.Fatalf("entry = %T, want *CompactionEntry", e)
	}
	if c.TokensAfter != 0 {
		t.Errorf("tokensAfter = %d, want 0", c.TokensAfter)
	}
	if c.ShortSummary != "" {
		t.Errorf("shortSummary = %q, want empty", c.ShortSummary)
	}
	out, err := MarshalEntry(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "tokensAfter") {
		t.Errorf("the writer claimed a measurement it does not have: %s", out)
	}
	if strings.Contains(string(out), "shortSummary") {
		t.Errorf("the writer claimed a summary it does not have: %s", out)
	}
}
