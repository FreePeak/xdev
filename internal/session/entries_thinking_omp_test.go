package session

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTheConfiguredLevelIsTheLevelString is the omp contract: the record
// names the CONFIGURED LEVEL — the level the user pinned, or "auto" when
// the level is inherited from the model role's ":effort" default — and it
// reads back as that level.
func TestTheConfiguredLevelIsTheLevelString(t *testing.T) {
	s, err := Open(ompThinkingFixture)
	if err != nil {
		t.Fatalf("open the omp fixture: %v", err)
	}
	defer s.Close()

	found := thinkingEntry(t, s)
	if found.Configured != "auto" {
		t.Errorf("configured = %q, want %q", found.Configured, "auto")
	}
	if found.ThinkingLevel != "high" {
		t.Errorf("thinkingLevel = %q, want high", found.ThinkingLevel)
	}
}

// A reader must also accept what xdev's own first cut wrote: a BOOLEAN.
// A bool carries no level name, so it reads as "not recorded" — but the
// file still opens, which is the point.
func TestOpenAcceptsABooleanConfigured(t *testing.T) {
	path := writeThinkingFixture(t, "true")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open a file whose configured is a bool: %v", err)
	}
	defer s.Close()

	found := thinkingEntry(t, s)
	if found.Configured != "" {
		t.Errorf("configured = %q, want the empty level (a bool names none)", found.Configured)
	}
}

// A value that names no level at all (a number, an object, an array) is
// nonsense: the entry cannot be decoded, so it falls back to the
// UnknownEntry path — the line is preserved verbatim and the REST of the
// file still opens. One unreadable field never refuses a whole session.
func TestOpenKeepsANonsenseConfiguredAsAnUnknownEntry(t *testing.T) {
	for _, nonsense := range []string{"42", `{"level":"high"}`, `["auto"]`} {
		t.Run(nonsense, func(t *testing.T) {
			path := writeThinkingFixture(t, nonsense)
			s, err := Open(path)
			if err != nil {
				t.Fatalf("open a file whose configured is %s: %v", nonsense, err)
			}
			defer s.Close()

			// The entry is kept verbatim as an unknown chain node.
			var unknown *UnknownEntry
			for _, e := range s.Entries() {
				if u, ok := e.(*UnknownEntry); ok {
					unknown = u
				}
			}
			if unknown == nil {
				t.Fatal("the malformed entry was dropped instead of kept verbatim")
			}
			if !strings.Contains(string(unknown.Raw), `"configured":`+nonsense) {
				t.Errorf("the raw line was not preserved: %s", unknown.Raw)
			}

			// The other entries are intact.
			if got := len(s.Entries()); got != 4 {
				t.Errorf("entries = %d, want 4 (the malformed one is kept as an unknown)", got)
			}
			messages, models := 0, 0
			for _, e := range s.Entries() {
				switch e.(type) {
				case *MessageEntry:
					messages++
				case *ModelChangeEntry:
					models++
				}
			}
			if messages != 2 || models != 1 {
				t.Errorf("messages = %d, models = %d, want 2 and 1", messages, models)
			}
		})
	}
}

// The record round-trips: what the writer puts on the wire is what the
// reader reads back — for the "auto" sentinel and for a pinned level name.
func TestThinkingLevelConfiguredRoundTrips(t *testing.T) {
	for _, configured := range []string{"auto", "high"} {
		t.Run(configured, func(t *testing.T) {
			s := OpenMem("test", "t")
			if err := s.Append(&ThinkingLevelChangeEntry{ThinkingLevel: "high", Configured: configured}); err != nil {
				t.Fatal(err)
			}
			entries := s.Entries()
			tc, ok := entries[0].(*ThinkingLevelChangeEntry)
			if !ok {
				t.Fatalf("entry = %T, want *ThinkingLevelChangeEntry", entries[0])
			}
			if tc.Configured != configured {
				t.Fatalf("configured = %q, want %q", tc.Configured, configured)
			}
			line, err := MarshalEntry(tc)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			if err := json.Unmarshal(line, &wire); err != nil {
				t.Fatal(err)
			}
			if wire["configured"] != configured {
				t.Errorf("wire configured = %v, want %q", wire["configured"], configured)
			}
			back, err := ParseEntry(line)
			if err != nil {
				t.Fatalf("re-parse: %v", err)
			}
			btc, ok := back.(*ThinkingLevelChangeEntry)
			if !ok {
				t.Fatalf("re-parsed entry = %T, want *ThinkingLevelChangeEntry", back)
			}
			if btc.Configured != configured {
				t.Errorf("re-parsed configured = %q, want %q", btc.Configured, configured)
			}
		})
	}
}

// thinkingEntry returns the fixture's thinking_level_change entry.
func thinkingEntry(t *testing.T, s *Store) *ThinkingLevelChangeEntry {
	t.Helper()
	for _, e := range s.Entries() {
		if tc, ok := e.(*ThinkingLevelChangeEntry); ok {
			return tc
		}
	}
	t.Fatal("no thinking_level_change entry in the session")
	return nil
}
