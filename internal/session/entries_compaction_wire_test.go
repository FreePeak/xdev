package session

import (
	"strings"
	"testing"
)

// The compaction record (omp rule 27) must survive the wire: an entry that
// carries the surviving size and the one-line summary must still carry them
// after a parse/marshal round trip, or a resumed session loses what the
// compaction was and what it freed.
func TestTheCompactionRecordSurvivesTheWire(t *testing.T) {
	line := `{"type":"compaction","id":"a1b2c3d4","parentId":null,"timestamp":"2026-09-07T03:20:45.220Z","summary":{"role":"assistant","content":[]},"firstKeptEntryId":null,"tokensBefore":900,"tokensAfter":120,"shortSummary":"the summary in one line"}`
	e, err := ParseEntry([]byte(line))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := MarshalEntry(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"tokensAfter":120`) {
		t.Fatalf("the record lost the surviving size: %s", out)
	}
	if !strings.Contains(string(out), `"shortSummary":"the summary in one line"`) {
		t.Fatalf("the record lost the short summary: %s", out)
	}
}
