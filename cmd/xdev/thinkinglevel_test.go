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
	appendThinkingLevel(s, "high", "high")
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
	if tc.Configured != "high" {
		t.Errorf("configured = %q, want high (the level the user pinned)", tc.Configured)
	}
}

// A level that only follows the model (not chosen by the user) says so:
// omp's own sentinel is "auto".
func TestAppendThinkingLevelRecordsAnUnconfiguredLevel(t *testing.T) {
	s := session.OpenMem("test", "t")
	appendThinkingLevel(s, "medium", session.ConfiguredAuto)
	entries := s.Entries()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	tc, ok := entries[0].(*session.ThinkingLevelChangeEntry)
	if !ok {
		t.Fatalf("entry = %T, want *session.ThinkingLevelChangeEntry", entries[0])
	}
	if tc.Configured != session.ConfiguredAuto {
		t.Errorf("configured = %q, want %q", tc.Configured, session.ConfiguredAuto)
	}
}

// A mode with no session (a one-shot print run) must not crash on the
// record.
func TestAppendThinkingLevelWithoutAStoreIsANoOp(t *testing.T) {
	appendThinkingLevel(nil, "high", "high")
}
