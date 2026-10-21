package main

import (
	"testing"

	"github.com/FreePeak/xdev/internal/session"
)

// A thinking level that changes mid-session is recorded in the session (omp
// thinking_level_change), so a resume knows which level produced which part
// of the conversation — and which RUNG, whose tool surface the following
// turns ran on.
func TestAppendThinkingLevelRecordsTheLevel(t *testing.T) {
	s := session.OpenMem("test", "t")
	appendThinkingLevel(s, "high", "high", "high")
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
	if tc.Effort != "high" {
		t.Errorf("effort = %q, want high (the rung whose tool surface the turns ran on)", tc.Effort)
	}
}

// The rung is xdev's own field on omp's record, and it is OPTIONAL: a file
// written before it existed must still load, and must read as "no rung".
func TestTheThinkingLevelEntryRungIsOptional(t *testing.T) {
	line := `{"type":"thinking_level_change","id":"a1b2c3d4","parentId":null,"timestamp":"2026-09-07T03:21:00.570Z","thinkingLevel":"high","configured":"high"}`
	e, err := session.ParseEntry([]byte(line))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tc, ok := e.(*session.ThinkingLevelChangeEntry)
	if !ok {
		t.Fatalf("entry = %T, want *session.ThinkingLevelChangeEntry", e)
	}
	if tc.Effort != "" {
		t.Fatalf("effort = %q, want empty for a record written before the field", tc.Effort)
	}
}

// A level that only follows the model (not chosen by the user) says so:
// omp's own sentinel is "auto".
func TestAppendThinkingLevelRecordsAnUnconfiguredLevel(t *testing.T) {
	s := session.OpenMem("test", "t")
	appendThinkingLevel(s, "medium", session.ConfiguredAuto, "medium")
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
	appendThinkingLevel(nil, "high", "high", "high")
}
