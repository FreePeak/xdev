package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/config"
)

// realAgentHome is the agent dir the DEVELOPER would use, captured before
// TestMain moved HOME. It is only ever a path: the guard below compares against
// it and never writes to it.
var realAgentHome string

// TestUserKeybindingsCannotReachTheSuite is the regression guard for #524: the
// package must run as if ~/.xdev/agent did not exist. LoadKeyMap() reads
// <dataDir>/keybindings.yml at New() time, so without TestMain's isolation every
// App-constructing test loads the developer's real bindings — and a machine that
// has remapped an action fails tests that never asked about keys (the repro: a
// keybindings.yml replacing `quit` with ["C-c", "<leader>q"] fails
// TestQuitChordExitsWhileATurnRuns on C-d).
//
// It asserts PATHS, never a planted fixture. Writing a keybindings.yml into the
// developer's real agent dir to prove it is read would be a test that clobbers
// the user's file to make a point; the property that matters is that the suite's
// data dir is a temp dir of its own, which holds for every file in it.
func TestUserKeybindingsCannotReachTheSuite(t *testing.T) {
	got := config.DataDir()
	if realAgentHome == "" {
		t.Fatal("realAgentHome was never captured — TestMain must set it before moving HOME")
	}
	if !strings.HasPrefix(filepath.Clean(got), filepath.Clean(os.TempDir())+string(filepath.Separator)) {
		t.Fatalf("the suite reads %s, which is outside %s: a keybindings.yml there silently rebinds the app under test", got, os.TempDir())
	}
	if filepath.Clean(got) == filepath.Clean(realAgentHome) {
		t.Fatalf("the isolated data dir IS the developer's own agent dir (%s)", got)
	}
	// Belt and braces: the path the TUI actually opens must be inside that dir, so
	// a future rootDir() change cannot quietly route it back to the user's.
	if kp := keybindingsPath(); filepath.Clean(filepath.Dir(kp)) != filepath.Clean(got) {
		t.Fatalf("keybindingsPath reads %s, outside the data dir %s", kp, got)
	}
	// And the suite's dir is usable, so the keymap tests' fixtures (which set
	// their own HOME) still have somewhere to land.
	if err := os.MkdirAll(got, 0o755); err != nil {
		t.Fatalf("the isolated data dir is not usable: %v", err)
	}
}
