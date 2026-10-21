package session

import (
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

// The reasoning level the session's own transcript records is folded back
// when the context is rebuilt, and handed to the caller.
//
// omp folds the same record back when it rebuilds the context
// (packages__coding-agent__src__session__session-context.ts:137-140) and
// exposes it as "thinkingLevel" at :412. xdev needs it for one more reason:
// the level is what a resumed session continues at, and the rung it was
// reached through is what its tool surface follows.
func TestBuildContextFoldsThePersistedThinkingLevel(t *testing.T) {
	s := OpenMem("test", "t")
	if err := s.Append(&MessageEntry{Message: ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hello"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(&ThinkingLevelChangeEntry{ThinkingLevel: "low", Configured: "low"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(&MessageEntry{Message: ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(&ThinkingLevelChangeEntry{ThinkingLevel: "xhigh", Configured: "xhigh"}); err != nil {
		t.Fatal(err)
	}
	res, err := BuildContext(s.Entries(), s.LeafID(), SystemPrompt{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ThinkingLevel != "xhigh" {
		t.Errorf("thinking level = %q, want xhigh (the LAST record on the path)", res.ThinkingLevel)
	}
	if res.ConfiguredThinkingLevel != "xhigh" {
		t.Errorf("configured level = %q, want xhigh", res.ConfiguredThinkingLevel)
	}
	// The record is a state record, not a message: it must not reach the
	// provider request.
	if len(res.Messages) != 2 {
		t.Fatalf("messages = %d, want 2 (the level records are not messages)", len(res.Messages))
	}
}

// A session that never changed its level records nothing, and the fold says
// nothing: the caller falls back to the machine-wide setting.
func TestBuildContextFoldsNoThinkingLevelWhenTheSessionRecordsNone(t *testing.T) {
	s := OpenMem("test", "t")
	if err := s.Append(&MessageEntry{Message: ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hello"}}}}); err != nil {
		t.Fatal(err)
	}
	res, err := BuildContext(s.Entries(), s.LeafID(), SystemPrompt{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ThinkingLevel != "" || res.ConfiguredThinkingLevel != "" {
		t.Fatalf("folded level = %q/%q, want empty for a session that records none",
			res.ThinkingLevel, res.ConfiguredThinkingLevel)
	}
}

// The accessor next to BuildContext reads the same record without rebuilding
// the whole conversation — the callers that need the level (a resume) do not
// want the messages.
func TestPersistedThinkingLevelReadsTheRecord(t *testing.T) {
	s := OpenMem("test", "t")
	if err := s.Append(&ThinkingLevelChangeEntry{ThinkingLevel: "high", Configured: "high"}); err != nil {
		t.Fatal(err)
	}
	level, configured := PersistedThinkingLevel(s.Entries(), s.LeafID())
	if level != "high" || configured != "high" {
		t.Fatalf("level = %q/%q, want high/high", level, configured)
	}

	// An inherited level says so: "auto" is omp's own sentinel for a level
	// that follows the model role's ":effort" default.
	s2 := OpenMem("test", "t2")
	if err := s2.Append(&ThinkingLevelChangeEntry{ThinkingLevel: "medium", Configured: ConfiguredAuto}); err != nil {
		t.Fatal(err)
	}
	level, configured = PersistedThinkingLevel(s2.Entries(), s2.LeafID())
	if level != "medium" || configured != ConfiguredAuto {
		t.Fatalf("level = %q/%q, want medium/auto", level, configured)
	}
}
