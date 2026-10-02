package tui

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain isolates the user-level roots the TUI reads at construction time.
//
// New() calls LoadKeyMap(), which reads <dataDir>/keybindings.yml —
// ~/.xdev/agent/keybindings.yml by default. Without this, every test that
// builds an App through newTestApp/loopApp/idxApp loads the developer's real
// bindings, so a machine that has remapped an action fails tests that never
// asked about keys at all (issue #524: a keybindings.yml that replaces `quit`
// with ["C-c", "<leader>q"] fails TestQuitChordExitsWhileATurnRuns on C-d).
//
// The HOME move is what does it: config.rootDir() falls back to
// $HOME/.xdev/agent when no override is set, so keybindingsPath, mcp.yml and
// the extensions dir all land in the temp dir, and the XDG record that would
// redirect them (installDir() → $HOME/.xdev/xdg) is gone with it. A developer's
// own XDEV_AGENT_DIR would win over the move, so it is cleared for the run.
//
// XDEV_AGENT_DIR is cleared rather than SET: a developer's own value would win
// over the HOME move and route the suite back into their profile. It is not
// replaced with a temp path because the keymap tests write their fixtures under
// their own HOME and expect keybindingsPath to follow HOME there — an override
// would make them write where nothing reads.
func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "tui-test")
	if err != nil {
		// No temp dir: run anyway, and accept that a developer's own
		// keybindings.yml may reach the tests (today's behaviour).
		os.Exit(m.Run())
	}
	defer os.RemoveAll(tmp)
	oldHome, oldAgent := os.Getenv("HOME"), os.Getenv("XDEV_AGENT_DIR")
	// Captured before the move so the guard can prove the suite is reading
	// somewhere else (keymap_isolation_test.go). A path only — nothing is ever
	// written there.
	realAgentHome = filepath.Join(oldHome, ".xdev", "agent")
	if oldAgent != "" {
		realAgentHome = oldAgent
	}
	os.Setenv("HOME", tmp)
	os.Unsetenv("XDEV_AGENT_DIR")
	defer func() {
		os.Setenv("HOME", oldHome)
		if oldAgent != "" {
			os.Setenv("XDEV_AGENT_DIR", oldAgent)
		}
	}()
	os.Exit(m.Run())
}
