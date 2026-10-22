package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The run budget was a fixed six minutes with no way to change it, and the
// six minutes is the answer's worst case: the RCA's patched run of a "tell
// me where we are" question took 4 min 53 s, while the same question asked
// as "list me all pr urls" has a right answer in 30 seconds.
//
// The clock is the only thing that ends a model that has decided to keep
// gathering, so the clock is the thing to configure.
func TestRunBudgetSetting(t *testing.T) {
	// The key walks a real struct path, so `xdev config set runBudget 120`
	// is accepted and a typo is caught at load.
	if !settingsKeyOK("runBudget") {
		t.Error("runBudget is not a known settings key — `xdev config set runBudget 120` would be rejected")
	}

	// Unset means the agent's own default applies.
	s := &Settings{}
	if got := s.RunBudgetDuration(); got != 0 {
		t.Errorf("RunBudgetDuration() with the setting unset = %v, want 0 (the agent's default applies)", got)
	}

	// A value is a wall clock in seconds.
	s = &Settings{RunBudget: intPtr(120)}
	if got := s.RunBudgetDuration(); got != 2*time.Minute {
		t.Errorf("RunBudgetDuration() = %v, want 2m", got)
	}

	// A negative value is the explicit opt-out: unbounded.
	s = &Settings{RunBudget: intPtr(-1)}
	if got := s.RunBudgetDuration(); got != -time.Second {
		t.Errorf("RunBudgetDuration() = %v, want -1s (negative = unbounded)", got)
	}
}

// The merge is where a setting is either WIRED or merely parsed.
//
// The budget was parsed, listed by `xdev config get`, and never reached the
// agent: the merge is hand-written per field, and the budget was not in it.
// The same shape as #82 — "the two compaction triggers were parsed,
// validated and listed in `config list`, but no build site copied them into
// CompactionConfig".
func TestRunBudgetReachesTheMergedSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte("runBudget: 180\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	layer, _, err := readSettingsFile(path, true)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	merged := defaultSettings()
	if err := merged.merge(layer); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if merged.RunBudget == nil || *merged.RunBudget != 180 {
		t.Fatalf("the merged settings carry runBudget %v, want 180 — the setting is parsed but never merged", merged.RunBudget)
	}
	if got := merged.RunBudgetDuration(); got != 3*time.Minute {
		t.Fatalf("RunBudgetDuration() after the merge = %v, want 3m", got)
	}
}

// And a closer layer can RESET a global budget: the pointer is what makes
// "set to 0" distinguishable from "not set".
func TestRunBudgetCanBeResetByACloserLayer(t *testing.T) {
	global, _, err := readSettingsFile(writeSettings(t, "runBudget: 180\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	closer, _, err := readSettingsFile(writeSettings(t, "runBudget: 0\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	merged := defaultSettings()
	if err := merged.merge(global); err != nil {
		t.Fatal(err)
	}
	if err := merged.merge(closer); err != nil {
		t.Fatal(err)
	}
	if merged.RunBudget == nil || *merged.RunBudget != 0 {
		t.Fatalf("the closer layer's reset did not land: runBudget = %v", merged.RunBudget)
	}
}

func writeSettings(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func intPtr(v int) *int { return &v }
