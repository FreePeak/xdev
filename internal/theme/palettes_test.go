package theme

import (
	"sort"
	"strings"
	"testing"
)

// TestShippedPalettesParse pins the contract every shipped palette holds to:
// the file list, that Load and the picker agree on one name per palette, that
// each names every required slot, and that the diff is actually painted — a
// palette that left a band to the terminal would render a diff the user's own
// colours decide, which is the bug the launch themes were fixed for.
func TestShippedPalettesParse(t *testing.T) {
	want := []string{"catppuccin", "dracula", "gruvbox", "nord", "one-dark", "one-light", "rose-pine", "tokyo-night"}

	entries, err := builtinFS.ReadDir("builtin")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		files = append(files, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(files)
	if strings.Join(files, ",") != strings.Join(want, ",") {
		t.Fatalf("shipped palettes = %v, want %v", files, want)
	}

	diffSlots := []string{ToolDiffAdded, ToolDiffRemoved, ToolDiffContext,
		ToolDiffAddedBg, ToolDiffRemovedBg, ToolDiffAddedWordBg, ToolDiffRemovedWordBg}
	for _, name := range want {
		th := Load(name)
		if th == nil || th.Name != name {
			t.Fatalf("Load(%q) = %+v", name, th)
		}
		if !contains(AvailableThemes(""), name) {
			t.Errorf("%s loads but is not offered by the picker", name)
		}
		for _, slot := range RequiredSlots() {
			if _, ok := th.Slots[slot]; !ok {
				t.Errorf("%s: required slot %s missing", name, slot)
			}
		}
		for _, slot := range diffSlots {
			if c, ok := th.Slot(slot); !ok || c == (Color{}) {
				t.Errorf("%s: %s must pin a colour, got %+v (ok=%v)", name, slot, c, ok)
			}
		}
		rowAdd, _ := th.Slot(ToolDiffAddedBg)
		wordAdd, _ := th.Slot(ToolDiffAddedWordBg)
		rowRem, _ := th.Slot(ToolDiffRemovedBg)
		wordRem, _ := th.Slot(ToolDiffRemovedWordBg)
		if rowAdd == wordAdd || rowRem == wordRem {
			t.Errorf("%s: a word band equal to its row band teaches nothing", name)
		}
		if rowAdd == rowRem {
			t.Errorf("%s: added and removed collapsed onto one band", name)
		}
	}

	// The aliases the launch themes always answered to keep their answers, a
	// ported palette is settable case-insensitively like any other, and an
	// unknown name still falls back to the auto pair rather than to nothing.
	for _, tc := range []struct{ name, want string }{
		{"dark", "groknight"}, {"light", "grokday"}, {"Tokyo-Night", "tokyo-night"},
	} {
		if got := Load(tc.name); got == nil || got.Name != tc.want {
			t.Errorf("Load(%q) = %+v, want %q", tc.name, got, tc.want)
		}
	}
	if got := Load("no-such-palette"); got == nil || (got.Name != "groknight" && got.Name != "grokday") {
		t.Errorf("an unknown name must fall back to auto, got %+v", got)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
