package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The compaction trigger is omp's: an absolute pin and a percentage pin.
// A layer carries them, `xdev config set` writes them, and the settings
// list shows them.
func TestCompactionThresholdKeys(t *testing.T) {
	cwd := t.TempDir()
	s, err := LoadSettings(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.CompactionThresholdTokens(); got != 0 {
		t.Fatalf("default thresholdTokens = %d, want 0 (unset)", got)
	}
	if got := s.CompactionThresholdPercent(); got != 0 {
		t.Fatalf("default thresholdPercent = %d, want 0 (unset)", got)
	}

	layer := filepath.Join(t.TempDir(), "th.yml")
	if err := os.WriteFile(layer, []byte("compaction:\n  thresholdTokens: 50000\n  thresholdPercent: 60\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err = LoadSettings(cwd, []string{layer})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.CompactionThresholdTokens(); got != 50000 {
		t.Fatalf("thresholdTokens = %d, want 50000", got)
	}
	if got := s.CompactionThresholdPercent(); got != 60 {
		t.Fatalf("thresholdPercent = %d, want 60", got)
	}
}

// A percent outside the vocabulary is a failed load, not a threshold: the
// range is 1..99 because a threshold at 100% never fires and one at 0% fires
// on every message. Zero is not a percentage — it is the unset value, the
// way the other settings spell "no pin".
func TestCompactionThresholdPercentIsValidated(t *testing.T) {
	cwd := t.TempDir()
	for _, bad := range []string{"100", "120", "-5"} {
		layer := filepath.Join(t.TempDir(), "th.yml")
		if err := os.WriteFile(layer, []byte("compaction:\n  thresholdPercent: "+bad+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := LoadSettings(cwd, []string{layer})
		if err == nil {
			t.Fatalf("thresholdPercent %s must not load", bad)
		}
		if !strings.Contains(err.Error(), "compaction.thresholdPercent") {
			t.Fatalf("error names the key: %v", err)
		}
	}
	unset := filepath.Join(t.TempDir(), "th.yml")
	if err := os.WriteFile(unset, []byte("compaction:\n  thresholdPercent: 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings(cwd, []string{unset})
	if err != nil {
		t.Fatalf("thresholdPercent 0 must load as unset: %v", err)
	}
	if got := s.CompactionThresholdPercent(); got != 0 {
		t.Fatalf("thresholdPercent = %d, want 0 (unset)", got)
	}
}

// config set must write the two keys, and refuse what the merge would
// refuse — the write never lands a file the next start moves aside.
func TestCompactionThresholdSetRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := Set(path, "compaction.thresholdTokens", "50000"); err != nil {
		t.Fatal(err)
	}
	if err := Set(path, "compaction.thresholdPercent", "60"); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings(dir, []string{path})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.CompactionThresholdTokens(); got != 50000 {
		t.Fatalf("thresholdTokens = %d, want 50000", got)
	}
	if got := s.CompactionThresholdPercent(); got != 60 {
		t.Fatalf("thresholdPercent = %d, want 60", got)
	}
	// Zero clears the pin: it is the unset value, not an error.
	if err := Set(path, "compaction.thresholdPercent", "0"); err != nil {
		t.Fatalf("set 0 must clear the pin: %v", err)
	}
	s, err = LoadSettings(dir, []string{path})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.CompactionThresholdPercent(); got != 0 {
		t.Fatalf("thresholdPercent = %d, want 0 (unset)", got)
	}
	for _, bad := range []struct{ key, value string }{
		{"compaction.thresholdPercent", "120"},
		{"compaction.thresholdPercent", "-1"},
		{"compaction.thresholdTokens", "many"},
		{"compaction.thresholdTokens", "-1"},
	} {
		if err := Set(path, bad.key, bad.value); err == nil {
			t.Fatalf("set %s=%s must be refused", bad.key, bad.value)
		}
	}
}

// The settings list is how a user finds the key at all.
func TestCompactionThresholdListed(t *testing.T) {
	s := &Settings{}
	joined := strings.Join(List(s, "/tmp/config.yml"), "\n")
	if !strings.Contains(joined, "compaction.thresholdTokens unset") {
		t.Fatalf("config list must show compaction.thresholdTokens:\n%s", joined)
	}
	if !strings.Contains(joined, "compaction.thresholdPercent unset") {
		t.Fatalf("config list must show compaction.thresholdPercent:\n%s", joined)
	}
}
