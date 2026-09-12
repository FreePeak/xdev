package marketplace

import (
	"os"
	"path/filepath"
	"testing"
)

// The four accessors are the whole point of the registry: each discovery path
// appends them, so an installed plugin contributes real content (#85). These
// pin that an installed plugin's directories are surfaced, and that a stale or
// escaping entry is not.

// A registry entry whose directory no longer exists (uninstalled by hand) or
// points outside the plugin root contributes nothing: no phantom content, and
// a hand-edited installed.json cannot redirect discovery anywhere.
func TestAccessorsSkipStaleAndEscaping(t *testing.T) {
	dir := DataDir()
	outside := filepath.Join(dir, "elsewhere", "commands")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	// Registry writes are per-process (the data dir is memoized), so this
	// test adds entries and asserts only about its own paths.
	reg, _ := Load()
	if reg == nil {
		reg = &Registry{Version: RegistryVersion}
	}
	reg.Add(Installed{Name: "ghost", Source: "s", Path: filepath.Join(dir, "plugins", "ghost"),
		Dirs: Dirs{Commands: []string{filepath.Join(dir, "plugins", "ghost", "commands")}}})
	reg.Add(Installed{Name: "escape", Source: "s", Path: filepath.Join(dir, "plugins", "escape"),
		Dirs: Dirs{Commands: []string{outside}}})
	if err := reg.save(); err != nil {
		t.Fatal(err)
	}
	for _, got := range CommandDirs() {
		if filepath.Clean(got) == filepath.Clean(outside) || filepath.Base(got) == "ghost" {
			t.Fatalf("stale/escaping entry surfaced: %v", got)
		}
	}
}

// --plugin-dir roots behave like an installed plugin tree (same per-kind
// subdirectories) and disappear when unset.
func TestExtraRootsBehaveLikePlugins(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "extra")
	for _, sub := range []string{"commands", "skills"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	SetExtraRoots(nil)
	before := len(CommandDirs())
	SetExtraRoots([]string{root})
	t.Cleanup(func() { SetExtraRoots(nil) })
	got := CommandDirs()
	if len(got) != before+1 || filepath.Clean(got[len(got)-1]) != filepath.Join(root, "commands") {
		t.Fatalf("extra root not surfaced (before=%d): %v", before, got)
	}
	// A kind the root does not have is simply absent (no phantom dir).
	for _, h := range HookDirs() {
		if filepath.Dir(h) == filepath.Clean(root) {
			t.Fatalf("hooks invented for a root without hooks/: %v", h)
		}
	}
}
