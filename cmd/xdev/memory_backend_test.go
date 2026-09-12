package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/memory"
	"github.com/FreePeak/xdev/internal/tool"
)

// TestBuildMemoryBackendSelection pins the dispatch: memory.backend is the
// only switch, the shipped default is off, and an unknown value stays off
// instead of picking a backend by accident.
func TestBuildMemoryBackendSelection(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	cases := map[string]struct {
		setting string
		want    string
	}{
		"unset":        {"", "<nil>"},
		"off":          {"off", "<nil>"},
		"typo":         {"mnemopii", "<nil>"},
		"mnemopi":      {"mnemopi", "*memory.Mnemopi"},
		"hindsight":    {"hindsight", "*memory.Hindsight"},
		"sharpshooter": {"sharpshooter", "*memory.SharpShooter"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := buildMemory(&config.Settings{Memory: tc.setting})
			if got := fmt.Sprintf("%T", store); got != tc.want {
				t.Fatalf("backend = %s, want %s", got, tc.want)
			}
			switch b := store.(type) {
			case *memory.Backend:
				if want := filepath.Join(config.DataDir(), "memory"); b.Dir != want {
					t.Fatalf("local dir = %q, want %q", b.Dir, want)
				}
			case *memory.SharpShooter:
				if want := filepath.Join(config.DataDir(), "memories"); b.Dir != want {
					t.Fatalf("sharpshooter dir = %q, want %q", b.Dir, want)
				}
				if b.Off() {
					t.Fatal("selected sharpshooter backend must not be off")
				}
			case *memory.Mnemopi:
				if b.Off() {
					t.Fatal("selected mnemopi backend must not be off")
				}
			case *memory.Hindsight:
				if b.Off() {
					t.Fatal("selected hindsight backend must not be off")
				}
			}
		})
	}
	if buildMemory(nil) != nil {
		t.Fatal("a nil settings layer must not select a backend")
	}
}

// TestPromptInjectsSharpShooterDecisions: the selected backend reaches the
// system prompt through the same Store seam the local backend uses.
func TestPromptInjectsSharpShooterDecisions(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	store := buildMemory(&config.Settings{Memory: "sharpshooter"})
	if store == nil {
		t.Fatal("sharpshooter backend did not build")
	}
	if err := store.SaveLesson("keep the CLI flags boring", ""); err != nil {
		t.Fatal(err)
	}
	reg := tool.NewRegistry()
	got := promptFnWithMemory("BASE", t.TempDir(), reg, "", store)()
	if !strings.Contains(got, "# Memory Guidance") {
		t.Fatalf("memory guidance missing:\n%s", got)
	}
	if !strings.Contains(got, "keep the CLI flags boring") {
		t.Fatalf("decision bullet missing from the prompt:\n%s", got)
	}
	if strings.Contains(got, "source-session") {
		t.Fatalf("decision frontmatter leaked into the prompt:\n%s", got)
	}
}
