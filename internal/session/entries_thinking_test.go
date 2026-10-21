package session

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

// The thinking level record (omp thinking_level_change) round-trips on the
// wire: the level in force and the CONFIGURED LEVEL — the level the user
// pinned, or "auto" when the level is inherited from the model role's
// ":effort" default — are the two facts a resumed session needs to know how
// the earlier turns were reasoned.
func TestThinkingLevelChangeEntryRoundTrips(t *testing.T) {
	line := `{"type":"thinking_level_change","id":"a1b2c3d4","parentId":null,"timestamp":"2026-09-07T03:21:00.570Z","thinkingLevel":"high","configured":"high"}`
	e, err := ParseEntry([]byte(line))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tc, ok := e.(*ThinkingLevelChangeEntry)
	if !ok {
		t.Fatalf("entry = %T, want *ThinkingLevelChangeEntry", e)
	}
	if tc.ThinkingLevel != "high" {
		t.Errorf("thinkingLevel = %q, want high", tc.ThinkingLevel)
	}
	if tc.Configured != "high" {
		t.Errorf("configured = %q, want high", tc.Configured)
	}
	out, err := MarshalEntry(tc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["type"] != TypeThinkingLevelChange {
		t.Errorf("type = %v, want %q", wire["type"], TypeThinkingLevelChange)
	}
	if wire["thinkingLevel"] != "high" {
		t.Errorf("wire thinkingLevel = %v, want high", wire["thinkingLevel"])
	}
	if wire["configured"] != "high" {
		t.Errorf("wire configured = %v, want high", wire["configured"])
	}
}

// The fields are optional on the wire: an entry written before them existed
// (or by omp, which writes null) still parses.
func TestThinkingLevelChangeEntryToleratesMissingFields(t *testing.T) {
	line := `{"type":"thinking_level_change","id":"a1b2c3d4","parentId":null,"timestamp":"2026-09-07T03:21:00.570Z","thinkingLevel":null,"configured":null}`
	e, err := ParseEntry([]byte(line))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tc, ok := e.(*ThinkingLevelChangeEntry)
	if !ok {
		t.Fatalf("entry = %T, want *ThinkingLevelChangeEntry", e)
	}
	if tc.ThinkingLevel != "" {
		t.Errorf("thinkingLevel = %q, want the empty level", tc.ThinkingLevel)
	}
	if tc.Configured != "" {
		t.Errorf("configured = %q, want the empty level", tc.Configured)
	}
}

// The context builder ignores the record: a state record is not a message, so
// it must not reach the provider request.
func TestTheContextBuilderIgnoresTheThinkingLevelEntry(t *testing.T) {
	s := OpenMem("test", "t")
	if err := s.Append(&MessageEntry{Message: ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hello"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(&ThinkingLevelChangeEntry{ThinkingLevel: "high", Configured: "high"}); err != nil {
		t.Fatal(err)
	}
	res, err := BuildContext(s.Entries(), s.LeafID(), SystemPrompt{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 1 {
		t.Fatalf("messages = %d, want 1 (the level record is not a message)", len(res.Messages))
	}
	if got := res.Messages[0].Text(); got != "hello" {
		t.Fatalf("message = %q, want hello", got)
	}
}

// The store keeps the record in the chain, so a resume sees when the level
// changed.
func TestTheStoreKeepsTheThinkingLevelRecord(t *testing.T) {
	s := OpenMem("test", "t")
	if err := s.Append(&ThinkingLevelChangeEntry{ThinkingLevel: "medium", Configured: ConfiguredAuto}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range s.Entries() {
		if tc, ok := e.(*ThinkingLevelChangeEntry); ok {
			found = true
			if tc.ThinkingLevel != "medium" {
				t.Errorf("thinkingLevel = %q, want medium", tc.ThinkingLevel)
			}
		}
	}
	if !found {
		t.Fatal("the store dropped the thinking level record")
	}
}

// A record that a reader must not see as a message: the wire type is the
// omp one, spelled exactly.
func TestTheThinkingLevelWireTypeIsOMPs(t *testing.T) {
	if TypeThinkingLevelChange != "thinking_level_change" {
		t.Fatalf("wire type = %q, want thinking_level_change", TypeThinkingLevelChange)
	}
	out, err := MarshalEntry(&ThinkingLevelChangeEntry{ThinkingLevel: "low", Configured: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"type":"thinking_level_change"`) {
		t.Fatalf("wire = %s", out)
	}
}
