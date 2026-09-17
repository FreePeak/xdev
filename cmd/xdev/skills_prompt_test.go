package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The skills block must list model-visible skills and keep hidden /
// model-invocation-disabled ones out of the prompt while they remain
// reachable via skill://.
func TestSkillPromptBlockFiltersHidden(t *testing.T) {
	proj := t.TempDir()
	mk := func(name, body string) {
		dir := filepath.Join(proj, ".xdev", "skills", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("visible", "---\ndescription: a visible skill\n---\nbody")
	mk("secret", "---\ndescription: hidden skill\nhide: true\n---\nbody")
	mk("manual", "---\ndescription: explicit only\ndisableModelInvocation: true\n---\nbody")

	block := skillPromptBlock(proj)
	if !strings.Contains(block, "visible: a visible skill") {
		t.Fatalf("visible skill missing: %q", block)
	}
	for _, gone := range []string{"hidden skill", "explicit only"} {
		if strings.Contains(block, gone) {
			t.Fatalf("%q must stay out of the prompt: %q", gone, block)
		}
	}
	if !strings.Contains(block, "skill://") {
		t.Fatalf("the block must teach the skill:// path: %q", block)
	}
}

func TestSkillPromptBlockEmptyWithoutSkills(t *testing.T) {
	if block := skillPromptBlock(t.TempDir()); block != "" {
		t.Fatalf("empty repo must yield no block, got %q", block)
	}
}
