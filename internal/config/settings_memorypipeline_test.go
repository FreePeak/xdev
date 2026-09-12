package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMemoryPipelineSetting pins the additive memory.pipeline switch (M12
// #13): off by default, turned on by a layer, and rejected when unknown.
func TestMemoryPipelineSetting(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir()) // keep the user's real config out
	cwd := t.TempDir()

	base, err := LoadSettings(cwd, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if base.MemoryPipelineOn() {
		t.Fatal("memoryPipeline must default to off")
	}

	overlay := filepath.Join(t.TempDir(), "overlay.yml")
	if err := os.WriteFile(overlay, []byte("memory: local\nmemoryPipeline: on\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings(cwd, []string{overlay})
	if err != nil {
		t.Fatalf("load overlay: %v", err)
	}
	if !s.MemoryPipelineOn() {
		t.Fatalf("memoryPipeline = %q, want on", s.MemoryPipeline)
	}
	if !strings.Contains(strings.Join(List(s, "x"), "\n"), "memoryPipeline on") {
		t.Fatal("config list does not surface memoryPipeline")
	}

	bad := filepath.Join(t.TempDir(), "bad.yml")
	if err := os.WriteFile(bad, []byte("memoryPipeline: yes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSettings(cwd, []string{bad}); err == nil || !strings.Contains(err.Error(), "memoryPipeline") {
		t.Fatalf("unknown memoryPipeline error = %v, want a memoryPipeline message", err)
	}
}
