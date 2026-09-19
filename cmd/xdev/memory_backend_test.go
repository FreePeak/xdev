package main

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/memory"
)

// TestBuildMemoryBackendSelection pins the dispatch: memory.backend is the
// only switch, an empty or unknown value stays off instead of picking a
// backend by accident, and "local" is the schema default (see defaultSettings).
func TestBuildMemoryBackendSelection(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	cases := map[string]struct {
		setting string
		want    string
	}{
		"unset":     {"", "<nil>"},
		"off":       {"off", "<nil>"},
		"typo":      {"mnemopii", "<nil>"},
		"local":     {"local", "*memory.Backend"},
		"hindsight": {"hindsight", "*memory.Hindsight"},
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

// TestBuildMemoryFollowsSchemaDefault: LoadSettings with no memory key
// must select the local markdown backend, which is the whole point of
// shipping Memory: "local" — a lesson on disk is injected next session
// without the user having to flip a switch first.
func TestBuildMemoryFollowsSchemaDefault(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	s, err := config.LoadSettings(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	store := buildMemory(s)
	if _, ok := store.(*memory.Backend); !ok {
		t.Fatalf("schema default selected %T, want *memory.Backend", store)
	}
}
