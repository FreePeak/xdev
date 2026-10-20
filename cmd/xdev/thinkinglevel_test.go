package main

import (
	"testing"

	"github.com/FreePeak/xdev/internal/session"
)

// A thinking level that changes mid-session is recorded in the session (omp
// thinking_level_change), so a resume knows which level produced which part
// of the conversation.
func TestAppendThinkingLevelRecordsTheLevel(t *testing.T) {
	s := session.OpenMem("test", "t")
	appendThinkingLevel(s, "high", true)
	entries := s.Entries()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	tc, ok := entries[0].(*session.ThinkingLevelChangeEntry)
	if !ok {
		t.Fatalf("entry = %T, want *session.ThinkingLevelChangeEntry", entries[0])
	}
	if tc.ThinkingLevel != "high" {
		t.Errorf("thinkingLevel = %q, want high", tc.ThinkingLevel)
	}
	if !tc.Configured {
		t.Errorf("configured = false, want true")
	}
}

// A level that only follows the model (not chosen by the user) says so.
func TestAppendThinkingLevelRecordsAnUnconfiguredLevel(t *testing.T) {
	s := session.OpenMem("test", "t")
	appendThinkingLevel(s, "medium", false)
	entries := s.Entries()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	tc, ok := entries[0].(*session.ThinkingLevelChangeEntry)
	if !ok {
		t.Fatalf("entry = %T, want *session.ThinkingLevelChangeEntry", entries[0])
	}
	if tc.Configured {
		t.Errorf("configured = true, want false")
	}
}

// A mode with no session (a one-shot print run) must not crash on the
// record.
func TestAppendThinkingLevelWithoutAStoreIsANoOp(t *testing.T) {
	appendThinkingLevel(nil, "high", true)
}
