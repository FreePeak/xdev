package config

import (
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
	s = &Settings{RunBudget: 120}
	if got := s.RunBudgetDuration(); got != 2*time.Minute {
		t.Errorf("RunBudgetDuration() = %v, want 2m", got)
	}

	// A negative value is the explicit opt-out: unbounded.
	s = &Settings{RunBudget: -1}
	if got := s.RunBudgetDuration(); got != -time.Second {
		t.Errorf("RunBudgetDuration() = %v, want -1s (negative = unbounded)", got)
	}
}
