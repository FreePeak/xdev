package session

import (
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

// TestBuildContextNamesReplaySafeCalls pins the replay-safety seam: when every
// call a turn lost is declared free to repeat, the notice says so, so a
// resumed session does not have to re-derive whether re-running is free.
func TestBuildContextNamesReplaySafeCalls(t *testing.T) {
	prev := ReplaySafety
	ReplaySafety = func(name string) bool { return name == "read" }
	defer func() { ReplaySafety = prev }()

	entries := []Entry{
		userMsg("11111111", "", "read the config"),
		asstMsg("22222222", "11111111", "", ai.ToolCallBlock{ID: "call_1", Name: "read", Arguments: []byte(`{}`)}),
	}
	got := ctx(t, entries, "22222222").Messages[1].Text()
	if !strings.HasPrefix(got, UnansweredToolCallNotice) {
		t.Fatalf("notice = %q, want it to keep the plain wording", got)
	}
	if !strings.Contains(got, "read") {
		t.Fatalf("notice = %q, want it to name the safe call", got)
	}
	if got == UnansweredToolCallNotice {
		t.Fatal("notice carries no replay guidance for a safe call")
	}
}

// TestBuildContextUnsafeCallsKeepThePlainNotice: one unsafe call in the set is
// enough to withhold the guidance, because a partial "this one is free" reads
// as permission to repeat the whole turn.
func TestBuildContextUnsafeCallsKeepThePlainNotice(t *testing.T) {
	prev := ReplaySafety
	ReplaySafety = func(name string) bool { return name == "read" }
	defer func() { ReplaySafety = prev }()

	entries := []Entry{
		userMsg("11111111", "", "do the thing"),
		&MessageEntry{
			Env: Envelope{Type: TypeMessage, ID: "22222222", ParentID: "11111111"},
			Message: ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{
				ai.ToolCallBlock{ID: "call_1", Name: "read", Arguments: []byte(`{}`)},
				ai.ToolCallBlock{ID: "call_2", Name: "bash", Arguments: []byte(`{}`)},
			}},
		},
	}
	if got := ctx(t, entries, "22222222").Messages[1].Text(); got != UnansweredToolCallNotice {
		t.Fatalf("notice = %q, want the plain notice while a call is unsafe", got)
	}
}

// TestReplaySafetyDefaultsUnsafe: an unwired build must behave exactly as it
// did before the seam existed.
func TestReplaySafetyDefaultsUnsafe(t *testing.T) {
	prev := ReplaySafety
	ReplaySafety = nil
	defer func() { ReplaySafety = prev }()

	if got := unansweredToolCallText([]string{"read"}); got != UnansweredToolCallNotice {
		t.Fatalf("notice = %q, want the plain notice with no seam wired", got)
	}
}

// TestReplaySafetyUnknownToolIsUnsafe: a session may name a tool this process
// does not have (a --tools filter, a remote transcript). Assuming an
// uninspectable tool is harmless is what duplicates a side effect.
func TestReplaySafetyUnknownToolIsUnsafe(t *testing.T) {
	prev := ReplaySafety
	ReplaySafety = func(string) bool { return false }
	defer func() { ReplaySafety = prev }()

	if got := unansweredToolCallText([]string{"read"}); got != UnansweredToolCallNotice {
		t.Fatalf("notice = %q, want the plain notice for an unknown tool", got)
	}
}
