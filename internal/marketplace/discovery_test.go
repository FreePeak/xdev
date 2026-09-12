package marketplace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRegistry(t *testing.T, entries ...Installed) {
	t.Helper()
	if err := os.MkdirAll(Root(), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(Registry{Version: RegistryVersion, Plugins: entries})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, registryPath(), string(raw))
}

// firstWins mirrors the loop every consumer runs over its roots (see the
// discovery integration doc in discovery.go): earlier roots shadow later ones.
func firstWins(t *testing.T, roots []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue // missing root: not an error
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".md")
			if _, seen := out[name]; seen {
				continue
			}
			data, err := os.ReadFile(filepath.Join(root, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			out[name] = strings.TrimSpace(string(data))
		}
	}
	return out
}

func TestPluginRootsAreLowestPriority(t *testing.T) {
	dataDir := testDataDir(t)
	cwd := t.TempDir()
	project := filepath.Join(cwd, ".xdev", "commands")
	writeFile(t, filepath.Join(project, "deploy.md"), "project\n")

	pluginRoot := filepath.Join(dataDir, "plugins", "pkg")
	pluginCommands := filepath.Join(pluginRoot, "commands")
	writeFile(t, filepath.Join(pluginCommands, "deploy.md"), "plugin\n")
	writeFile(t, filepath.Join(pluginCommands, "extra.md"), "extra\n")
	writeRegistry(t, Installed{Name: "pkg", Path: pluginRoot, Dirs: Dirs{Commands: []string{pluginCommands}}})

	if got := CommandDirs(); len(got) != 1 || got[0] != pluginCommands {
		t.Fatalf("CommandDirs = %v, want [%s]", got, pluginCommands)
	}
	// Appending the plugin roots LAST is the documented contract: a plugin
	// never shadows an authored command and only fills the gaps.
	resolved := firstWins(t, append([]string{project}, CommandDirs()...))
	if resolved["deploy"] != "project" {
		t.Fatalf("deploy resolved to %q, want the project root to win", resolved["deploy"])
	}
	if resolved["extra"] != "extra" {
		t.Fatalf("plugin-only command missing: %v", resolved)
	}
}

func TestPluginRootsSkipStaleAndForeignEntries(t *testing.T) {
	dataDir := testDataDir(t)
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "commands", "foreign.md"), "foreign\n")

	stale := filepath.Join(dataDir, "plugins", "stale")
	gone := filepath.Join(stale, "commands") // never created
	ok := filepath.Join(dataDir, "plugins", "ok")
	writeFile(t, filepath.Join(ok, "commands", "real.md"), "real\n")

	writeRegistry(t,
		Installed{Name: "foreign", Path: outside, Dirs: Dirs{Commands: []string{filepath.Join(outside, "commands")}}},
		Installed{Name: "ok", Path: ok, Dirs: Dirs{
			Commands: []string{filepath.Join(ok, "commands"), filepath.Join(outside, "commands")},
			Skills:   []string{gone},
		}},
	)
	got := CommandDirs()
	if len(got) != 1 || got[0] != filepath.Join(ok, "commands") {
		t.Fatalf("CommandDirs = %v, want only the in-root existing directory", got)
	}
	if dirs := SkillRoots(); len(dirs) != 0 {
		t.Fatalf("SkillRoots = %v, want none for a missing directory", dirs)
	}
	if dirs := AgentDirs(); len(dirs) != 0 {
		t.Fatalf("AgentDirs = %v, want none", dirs)
	}
	if dirs := HookDirs(); len(dirs) != 0 {
		t.Fatalf("HookDirs = %v, want none", dirs)
	}
}

func TestPluginRootsConflictMatchesFirstWins(t *testing.T) {
	dataDir := testDataDir(t)
	cwd := t.TempDir()
	user := filepath.Join(cwd, "user-commands")
	writeFile(t, filepath.Join(user, "bash.md"), "user\n")

	pluginRoot := filepath.Join(dataDir, "plugins", "pkg")
	writeFile(t, filepath.Join(pluginRoot, "commands", "bash.md"), "plugin\n")
	writeRegistry(t, Installed{Name: "pkg", Path: pluginRoot,
		Dirs: Dirs{Commands: []string{filepath.Join(pluginRoot, "commands")}}})

	resolved := firstWins(t, append([]string{user}, CommandDirs()...))
	if resolved["bash"] != "user" {
		t.Fatalf("bash resolved to %q, want the user root to win", resolved["bash"])
	}
}
