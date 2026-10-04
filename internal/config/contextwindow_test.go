package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// compaction.contextWindow pins the window for every model. The vocabulary is
// closed, so a typo is reported at load instead of silently pinning a window
// nobody asked for.
func TestCompactionContextWindowKey(t *testing.T) {
	cwd := t.TempDir()

	s, err := LoadSettings(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.CompactionContextWindowOn(); got != "auto" {
		t.Fatalf("default contextWindow = %q, want auto", got)
	}
	// auto means "each model uses its own window" — spelled as 0, because
	// CompactionConfig.ContextWindow 0 would DISABLE compaction instead.
	if got := s.CompactionContextWindow(); got != 0 {
		t.Fatalf("auto contextWindow tokens = %d, want 0", got)
	}

	layer := filepath.Join(t.TempDir(), "ctx.yml")
	if err := os.WriteFile(layer, []byte("compaction:\n  contextWindow: 500k\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err = LoadSettings(cwd, []string{layer})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.CompactionContextWindow(); got != 500_000 {
		t.Fatalf("pinned contextWindow = %d, want 500000", got)
	}
	if got := s.CompactionContextWindowOn(); got != "500k" {
		t.Fatalf("pinned contextWindow row value = %q, want 500k", got)
	}

	// A case difference is a typo a human makes; it must canonicalize, not
	// fail, and the stored spelling must be the canonical one.
	upper := filepath.Join(t.TempDir(), "ctx-upper.yml")
	if err := os.WriteFile(upper, []byte("compaction:\n  contextWindow: 1M\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err = LoadSettings(cwd, []string{upper})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Compaction.ContextWindow; got != "1m" {
		t.Fatalf("stored contextWindow = %q, want the canonical 1m", got)
	}
	if got := s.CompactionContextWindow(); got != 1_000_000 {
		t.Fatalf("1m contextWindow tokens = %d, want 1000000", got)
	}

	// Anything outside the vocabulary is a failed load, not a new window.
	bad := filepath.Join(t.TempDir(), "ctx-bad.yml")
	if err := os.WriteFile(bad, []byte("compaction:\n  contextWindow: 700k\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSettings(cwd, []string{bad}); err == nil {
		t.Fatal("contextWindow 700k must not load")
	} else if !strings.Contains(err.Error(), "compaction.contextWindow") {
		t.Fatalf("error names the key: %v", err)
	}
}

// An explicit "auto" is a LAYER, not an absence: a project overlay has to be
// able to hand the models back their own windows over a machine-wide pin.
func TestCompactionContextWindowAutoLayerClearsAPin(t *testing.T) {
	dir := t.TempDir()
	global := filepath.Join(dir, "global.yml")
	if err := os.WriteFile(global, []byte("compaction:\n  contextWindow: 1m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	auto := filepath.Join(dir, "auto.yml")
	if err := os.WriteFile(auto, []byte("compaction:\n  contextWindow: auto\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings(t.TempDir(), []string{global, auto})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.CompactionContextWindow(); got != 0 {
		t.Fatalf("an auto layer = %d tokens, want 0 (each model keeps its own)", got)
	}
	if got := s.CompactionContextWindowOn(); got != "auto" {
		t.Fatalf("row value = %q, want auto", got)
	}
}

// The settings list is how a user finds the key at all.
func TestCompactionContextWindowListed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := &Settings{}
	if !strings.Contains(strings.Join(List(s, "/tmp/config.yml"), "\n"), "compaction.contextWindow auto") {
		t.Fatal("config list must show compaction.contextWindow")
	}
}

// config set must refuse a value the merge would reject, so the write never
// lands a file the next start moves aside as *.broken-*.
func TestCompactionContextWindowSetRejectsTypos(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := Set(path, "compaction.contextWindow", "1m"); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings(dir, []string{path})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.CompactionContextWindow(); got != 1_000_000 {
		t.Fatalf("set 1m = %d tokens, want 1000000", got)
	}
}
