package config

import (
	"strings"
	"testing"
)

// TestHandoffSaveToDiskKey covers settings handoff.saveToDisk (M5 #23). The
// key must be part of the schema: the loader rejects unknown keys loudly (a
// typo'd config is quarantined as broken), so a handoff setting with no
// schema entry could never be turned on.
func TestHandoffSaveToDiskKey(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir()) // keep the user's real config out
	cwd := t.TempDir()

	// Shipped default: off — handoff artifacts appear only when asked for.
	s, err := LoadSettings(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.HandoffSaveToDisk() {
		t.Fatal("handoff.saveToDisk defaults on")
	}
	var nilSettings *Settings
	if nilSettings.HandoffSaveToDisk() {
		t.Fatal("nil Settings must report off")
	}

	writeFile(t, GlobalSettingsPath(), "handoff:\n  saveToDisk: true\n")
	s, err = LoadSettings(cwd, nil)
	if err != nil {
		t.Fatalf("the handoff key must load, not quarantine: %v", err)
	}
	if !s.HandoffSaveToDisk() {
		t.Fatal("handoff.saveToDisk did not stick")
	}
	if got := strings.Join(List(s, GlobalSettingsPath()), "\n"); !strings.Contains(got, "handoff.saveToDisk true") {
		t.Fatalf("config list missing the key:\n%s", got)
	}
}
