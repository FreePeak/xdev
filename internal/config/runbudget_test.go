package config

import "testing"

// The per-run wall clock is GONE (2026-10-24): a run is bounded by its turn cap
// and by the person watching it. The key is removed from the schema, so
// `xdev config set runBudget 600` fails LOUDLY instead of writing a key the
// next start would reject — and a settled file that still carries it is
// migrated away (see TestLegacyRunBudgetKeyIsDropped in settings_test.go) rather
// than quarantined as *.broken-* with every sibling key lost.
func TestRunBudgetIsNoLongerASettingsKey(t *testing.T) {
	if settingsKeyOK("runBudget") {
		t.Fatal("runBudget is still settable — the budget is gone; a leftover key must be rejected, not silently stored")
	}
}
