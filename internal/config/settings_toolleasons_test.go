package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestToolLessonsSetting pins the tool-usage learning switch: on by
// default (a recipe that is learned but never read is a write-only store),
// turned off by a layer, and surfaced by `config list`.
func TestToolLessonsSetting(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir()) // keep the user's real config out
	cwd := t.TempDir()

	base, err := LoadSettings(cwd, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !base.ToolLessonsOn() {
		t.Fatal("the tool-usage learning must default to on")
	}

	overlay := filepath.Join(t.TempDir(), "overlay.yml")
	if err := os.WriteFile(overlay, []byte("toolLessons: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings(cwd, []string{overlay})
	if err != nil {
		t.Fatalf("load overlay: %v", err)
	}
	if s.ToolLessonsOn() {
		t.Fatal("toolLessons: false must turn the learning off")
	}
	if !strings.Contains(strings.Join(List(s, "x"), "\n"), "toolLessons false") {
		t.Fatal("config list does not surface toolLessons")
	}
}
