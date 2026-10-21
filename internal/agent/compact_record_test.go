package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/session"
)

// The compaction record (omp rule 27): the entry says how big the context is
// AFTER the compaction and carries a one-line rendering of the summary, so a
// reader can see what the compaction was about and what it freed.
func TestTheCompactionRecordCarriesWhatSurvived(t *testing.T) {
	stubPressure(t, 0)
	p := &fakeProvider{}
	a, s, _ := storeAgent(t, p, ladderConfig("soft"))
	ladderFixture(t, s)
	_ = a.maybeCompact(context.Background(), ladderHistory(t, s))

	entries := compactionEntries(s)
	if len(entries) != 1 {
		t.Fatalf("compaction entries = %d, want 1", len(entries))
	}
	entry := entries[0]
	if entry.TokensAfter <= 0 {
		t.Fatalf("tokensAfter = %d, want the size of the surviving context", entry.TokensAfter)
	}
	// The surviving context is the summary plus the window the compaction
	// kept, so it cannot be smaller than the summary alone.
	if int64(len(entry.Summary.Text())) > entry.TokensAfter*charsPerToken {
		t.Fatalf("tokensAfter = %d, too small for a %d-char summary", entry.TokensAfter, len(entry.Summary.Text()))
	}
	if entry.ShortSummary == "" {
		t.Fatal("the compaction entry carries no short summary")
	}
	if strings.ContainsAny(entry.ShortSummary, "\n\r") {
		t.Fatalf("the short summary is not one line: %q", entry.ShortSummary)
	}
	if !strings.Contains(entry.Summary.Text(), strings.Split(entry.ShortSummary, " ")[0]) {
		t.Fatalf("the short summary is not the summary: %q", entry.ShortSummary)
	}
}

// The record rides the wire: the persisted line carries both fields.
func TestTheCompactionRecordReachesTheWire(t *testing.T) {
	stubPressure(t, 0)
	p := &fakeProvider{}
	a, s, _ := storeAgent(t, p, ladderConfig("soft"))
	ladderFixture(t, s)
	_ = a.maybeCompact(context.Background(), ladderHistory(t, s))

	entries := compactionEntries(s)
	if len(entries) != 1 {
		t.Fatalf("compaction entries = %d, want 1", len(entries))
	}
	line, err := session.MarshalEntry(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(line, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["tokensAfter"]; !ok {
		t.Fatalf("the compaction line carries no tokensAfter: %s", line)
	}
	short, ok := wire["shortSummary"].(string)
	if !ok || short == "" {
		t.Fatalf("the compaction line carries no shortSummary: %s", line)
	}
}

// The short summary is one line and bounded: a summary that runs to pages
// must not put pages into a field a reader shows in a list.
func TestTheShortSummaryIsOneBoundedLine(t *testing.T) {
	long := strings.Repeat("word ", 200)
	entry := &session.CompactionEntry{Summary: textSummary(long)}
	FillCompactionRecord(entry, nil)
	if strings.ContainsAny(entry.ShortSummary, "\n\r") {
		t.Fatalf("the short summary is not one line: %q", entry.ShortSummary)
	}
	if n := len([]rune(entry.ShortSummary)); n > 120 {
		t.Fatalf("short summary is %d runes, want at most 120", n)
	}
	if !strings.HasSuffix(entry.ShortSummary, "…") {
		t.Fatalf("a cut short summary must say it was cut: %q", entry.ShortSummary)
	}
}
