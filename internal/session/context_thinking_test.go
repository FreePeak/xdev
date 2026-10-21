package session

import (
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

// The reasoning state the session's own transcript records is folded back
// when the context is rebuilt, and handed to the caller.
//
// omp folds the same record back when it rebuilds the context
// (packages__coding-agent__src__session__session-context.ts:137-140) and
// exposes it as "thinkingLevel" at :412. xdev needs it for two more
// reasons: the level is what a resumed session continues at, and the RUNG
// is what its tool surface follows.
func TestBuildContextFoldsThePersistedThinkingLevel(t *testing.T) {
	s := OpenMem("test", "t")
	if err := s.Append(&MessageEntry{Message: ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hello"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(&ThinkingLevelChangeEntry{ThinkingLevel: "low", Configured: "low", Effort: "low"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(&MessageEntry{Message: ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(&ThinkingLevelChangeEntry{ThinkingLevel: "xhigh", Configured: "xhigh", Effort: "high"}); err != nil {
		t.Fatal(err)
	}
	res, err := BuildContext(s.Entries(), s.LeafID(), SystemPrompt{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Thinking.Level != "xhigh" {
		t.Errorf("thinking level = %q, want xhigh (the LAST record on the path)", res.Thinking.Level)
	}
	if res.Thinking.Configured != "xhigh" {
		t.Errorf("configured level = %q, want xhigh", res.Thinking.Configured)
	}
	if res.Thinking.Rung != "high" {
		t.Errorf("rung = %q, want high (the record names the rung, not the level)", res.Thinking.Rung)
	}
	if !res.Thinking.Found {
		t.Error("found = false, want true (the path records a level)")
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
	if res.Thinking.Found {
		t.Fatal("found = true, want false for a session that records none")
	}
	if res.Thinking.Level != "" || res.Thinking.Configured != "" || res.Thinking.Rung != "" {
		t.Fatalf("folded state = %+v, want empty for a session that records none", res.Thinking)
	}
}

// A record that pins reasoning OFF is a record, not an absence: the caller
// must be able to tell the two apart, or a session that turned reasoning
// off resumes with the machine-wide setting's reasoning.
func TestTheFoldDistinguishesNoRecordFromARecordThatSaysOff(t *testing.T) {
	s := OpenMem("test", "t")
	if err := s.Append(&ThinkingLevelChangeEntry{ThinkingLevel: "", Configured: "off", Effort: "medium"}); err != nil {
		t.Fatal(err)
	}
	st := PersistedThinkingState(s.Entries(), s.LeafID())
	if !st.Found {
		t.Fatal("found = false, want true (the record exists)")
	}
	if st.Level != "" {
		t.Errorf("level = %q, want empty (the session pinned reasoning off)", st.Level)
	}
	if st.Configured != "off" {
		t.Errorf("configured = %q, want off", st.Configured)
	}
	if st.Rung != "medium" {
		t.Errorf("rung = %q, want medium", st.Rung)
	}
}

// The accessor next to BuildContext reads the same record without rebuilding
// the whole conversation — the callers that need the state (a resume) do not
// want the messages.
func TestPersistedThinkingStateReadsTheRecord(t *testing.T) {
	s := OpenMem("test", "t")
	if err := s.Append(&ThinkingLevelChangeEntry{ThinkingLevel: "high", Configured: "high", Effort: "high"}); err != nil {
		t.Fatal(err)
	}
	st := PersistedThinkingState(s.Entries(), s.LeafID())
	if st.Level != "high" || st.Configured != "high" || st.Rung != "high" {
		t.Fatalf("state = %+v, want high/high/high", st)
	}

	// An inherited level says so: "auto" is omp's own sentinel for a level
	// that follows the model role's ":effort" default.
	s2 := OpenMem("test", "t2")
	if err := s2.Append(&ThinkingLevelChangeEntry{ThinkingLevel: "medium", Configured: ConfiguredAuto}); err != nil {
		t.Fatal(err)
	}
	st = PersistedThinkingState(s2.Entries(), s2.LeafID())
	if st.Level != "medium" || st.Configured != ConfiguredAuto {
		t.Fatalf("state = %+v, want medium/auto", st)
	}
	// A record written before the rung field existed carries no rung: the
	// field is optional and the file still loads.
	if st.Rung != "" {
		t.Fatalf("rung = %q, want empty for a record written before the field", st.Rung)
	}
}
