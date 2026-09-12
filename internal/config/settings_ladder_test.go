package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCompactionIdleAndAsyncKeys covers the M5 #24 trigger knobs: both are off
// by default, a layer turns them on, the list shows what is in force, and a
// malformed duration is rejected instead of silently disabling the trigger.
func TestCompactionIdleAndAsyncKeys(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()

	s, err := LoadSettings(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.CompactionIdleAfter(); got != 0 {
		t.Fatalf("default idleAfter = %v, want the trigger off", got)
	}
	if s.CompactionAsyncOn() {
		t.Fatal("async compaction must be off by default")
	}

	layer := writeFile(t, filepath.Join(t.TempDir(), "ladder.yml"),
		"compaction:\n  idleAfter: 10m\n  async: true\n")
	s, err = LoadSettings(cwd, []string{layer})
	if err != nil {
		t.Fatalf("layer must load: %v", err)
	}
	if got := s.CompactionIdleAfter(); got != 10*time.Minute {
		t.Fatalf("idleAfter = %v, want 10m", got)
	}
	if !s.CompactionAsyncOn() {
		t.Fatal("async must be on when the layer says so")
	}
	listed := strings.Join(List(s, "/tmp/config.yml"), "\n")
	for _, want := range []string{"compaction.idleAfter 10m", "compaction.async true"} {
		if !strings.Contains(listed, want) {
			t.Errorf("config list must show %q:\n%s", want, listed)
		}
	}

	// A typo'd duration is reported, not swallowed: the user asked for a
	// trigger and silently getting none is worse than a load error.
	bad := writeFile(t, filepath.Join(t.TempDir(), "bad.yml"),
		"compaction:\n  idleAfter: 10 min\n")
	if _, err := LoadSettings(cwd, []string{bad}); err == nil || !strings.Contains(err.Error(), "compaction.idleAfter") {
		t.Fatalf("err = %v, want a compaction.idleAfter complaint", err)
	}
	// A non-positive duration is refused for the same reason.
	zero := writeFile(t, filepath.Join(t.TempDir(), "zero.yml"),
		"compaction:\n  idleAfter: 0s\n")
	if _, err := LoadSettings(cwd, []string{zero}); err == nil || !strings.Contains(err.Error(), "positive") {
		t.Fatalf("err = %v, want a positive-duration complaint", err)
	}
}

// TestCompactionMethodNamesVocabulary pins the shared spelling: the shipped
// order is the trigger list, and the M5 #24 members are accepted by the
// parser (agent.ParseMethodOrder) without widening the default.
func TestCompactionMethodNamesVocabulary(t *testing.T) {
	want := []string{"threshold", "overflow", "promotion", "remote", "snapcompact", "handoff", "shake", "soft"}
	if len(CompactionMethodNames) != len(want) {
		t.Fatalf("vocabulary = %v, want %v", CompactionMethodNames, want)
	}
	for i, name := range want {
		if CompactionMethodNames[i] != name {
			t.Fatalf("vocabulary[%d] = %q, want %q", i, CompactionMethodNames[i], name)
		}
	}
	if DefaultCompactionMethodOrder != "threshold,overflow,promotion" {
		t.Fatalf("the shipped order must stay the trigger list, got %q", DefaultCompactionMethodOrder)
	}
}
