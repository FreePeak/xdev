package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain isolates the user-level roots cmd/xdev reads at construction time.
//
// Two of them sit outside any per-test sandbox, and each one made a test in
// this package fail on a developer machine while passing on CI:
//
//   - skills.Discover walks the user skill root (<dataDir>/skills) and any
//     installed plugin skill roots. TestSkillPromptBlockEmptyWithoutSkills
//     asserts an EMPTY repo yields no prompt block, but the user's own
//     ~/.xdev/agent/skills/* — blender-web-pipeline, find-skills, marp and
//     thirty more — is not the repo, so the block is never empty and the
//     developer's real skill descriptions (including their file paths)
//     land in the failure message.
//
//   - config.ConnectOptions marks a provider "ready" when its credential
//     resolves from the machine, and the Cursor rung reads a local IDE
//     database: the macOS Keychain item "cursor-access-token", then
//     ~/Library/Application Support/Cursor/User/globalStorage/state.vscdb.
//     TestConnectPickerItems asserts a provider the test itself did not
//     supply does NOT lead the picker, but a developer with Cursor logged
//     in has exactly that credential, so cursor sorts first and the
//     developer's local credential source is printed in the failure.
//
// Both are read-only lookups, so there is nothing to fake but a missing file:
// move HOME to a temp dir and unset the override. config.rootDir() falls back
// to $HOME/.xdev/agent when no override is set, which carries <dataDir>/skills,
// <dataDir>/managed-skills and <dataDir>/plugins with it, so the user skill
// root, the managed root and the plugin registry all land in the temp dir.
//
// XDEV_AGENT_DIR is CLEARED rather than set to a temp path. Setting it would
// route the suite into a directory no test asked for, and the ~40 tests here
// that set XDEV_AGENT_DIR themselves expect their own value to win over any
// process-wide default; an override that is present would silently route their
// fixtures somewhere else. Clearing is the choice #527 made for the same reason
// in internal/tui, and the tests that need an agent dir already set one.
//
// HOME is a process-wide move, so tests that t.Setenv("HOME", …) still work:
// t.Setenv records the value present when it ran (the temp dir) and restores
// that, which is what the isolated run wants.

// realHome is the developer's own HOME as it was before TestMain moved it, or
// "" when there was nothing to move. Read only by the guard below.
var realHome string

func TestMain(m *testing.M) {

	tmp, err := os.MkdirTemp("", "xdev-cmd-test")
	if err != nil {
		// No temp dir: run anyway, and accept today's behaviour — a
		// developer's own skills and Cursor credential may reach the tests.
		os.Exit(m.Run())
	}
	defer os.RemoveAll(tmp)
	oldHome, oldAgent := os.Getenv("HOME"), os.Getenv("XDEV_AGENT_DIR")
	// Captured before the move so the guard can prove the suite is reading
	// somewhere else (see TestMainIsolationIsActuallyInPlace). A path only —
	// nothing is ever written there. It cannot come from os.UserHomeDir(),
	// which on darwin resolves $HOME and would hand back the temp dir.
	realHome = oldHome
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

// TestMainIsolationIsActuallyInPlace is the guard for the move above, and it
// asserts PATHS rather than planting a fixture. Writing a real SKILL.md into
// the developer's own skill root to prove it is read would be a test that
// clobbers the user's files to make a point — the same reason #527's guard
// checks paths instead.
//
// It fails if TestMain stops moving HOME — the case that returns both reported
// failures — or if a developer's XDEV_AGENT_DIR was preset, since that wins
// over the move and routes the suite back into their profile.
func TestMainIsolationIsActuallyInPlace(t *testing.T) {
	home := os.Getenv("HOME")
	if home == "" {
		t.Fatal("HOME is unset: the suite would read the real user roots")
	}
	if realHome == "" {
		t.Fatal("TestMain did not capture the pre-move HOME: the move itself may have failed")
	}
	if home == realHome {
		t.Fatalf("HOME = %q, which is the developer's own home: the user skill root and the Cursor credential are back in play", home)
	}
	// The path both failures came from must not exist inside the run. The
	// developer's own copy is outside this HOME, so this is a real check
	// rather than a restatement of the comparison above.
	if _, err := os.Stat(filepath.Join(home, ".xdev", "agent", "skills")); err == nil {
		t.Errorf("a user skill root exists inside the isolated HOME: discovery is not isolated")
	}
}
