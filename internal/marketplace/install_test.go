package marketplace

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// --- fixtures ---------------------------------------------------------------

func testDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	SetDataDir(dir)
	return dir
}

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{
		"-c", "commit.gpgsign=false",
		"-c", "init.defaultBranch=main",
		"-c", "user.name=xdev test",
		"-c", "user.email=test@example.com",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	// Hermetic: the developer's global/system git config must not leak in.
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// newGitFixture builds a plugin repository with two tagged versions: the
// v1 tree (the tag to pin) and a v2 tree that adds commands/newer.md.
func newGitFixture(t *testing.T) (dir, v1, v2 string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	dir = t.TempDir()
	gitCmd(t, dir, "init", "-q")
	writePluginTree(t, dir, "1.0.0")
	gitCmd(t, dir, "add", ".")
	gitCmd(t, dir, "commit", "-q", "-m", "v1")
	gitCmd(t, dir, "tag", "v1.0.0")
	v1 = gitCmd(t, dir, "rev-parse", "HEAD")

	writeFile(t, filepath.Join(dir, "commands", "newer.md"), "Only in 2.0\n")
	writeFile(t, filepath.Join(dir, ".xdev-plugin", "plugin.json"),
		`{"name":"hello","version":"2.0.0","description":"greets twice"}`)
	gitCmd(t, dir, "add", ".")
	gitCmd(t, dir, "commit", "-q", "-m", "v2")
	gitCmd(t, dir, "tag", "v2.0.0")
	v2 = gitCmd(t, dir, "rev-parse", "HEAD")
	return dir, v1, v2
}

func writePluginTree(t *testing.T, dir, version string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, ".xdev-plugin", "plugin.json"),
		`{"name":"hello","version":"`+version+`","description":"greets"}`)
	writeFile(t, filepath.Join(dir, "commands", "hello.md"), "Say hello\n")
	writeFile(t, filepath.Join(dir, "skills", "greet", "SKILL.md"),
		"---\nname: greet\ndescription: greet the user\n---\nBe nice.\n")
	writeFile(t, filepath.Join(dir, "agents", "helper.md"),
		"---\nname: helper\ndescription: helps out\n---\nHelp.\n")
	writeFile(t, filepath.Join(dir, "hooks", "guard.yml"),
		"name: guard\nevent: tool_call\ncommand: ./guard.sh\n")
}

func writeCatalog(t *testing.T, dir string, plugins ...Plugin) string {
	t.Helper()
	raw, err := json.MarshalIndent(Manifest{Name: "shop", Plugins: plugins}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, ".xdev-plugin", "marketplace.json"), string(raw))
	return dir
}

func mustInstall(t *testing.T, ref string, opts Options) *Installed {
	t.Helper()
	inst, _, err := Install(context.Background(), ref, opts)
	if err != nil {
		t.Fatalf("Install(%s): %v", ref, err)
	}
	return inst
}

// --- install ----------------------------------------------------------------

func TestInstallGitSourcePinsRevision(t *testing.T) {
	dataDir := testDataDir(t)
	catDir := t.TempDir()
	repo, v1, _ := newGitFixture(t)
	// The fixture lives inside the catalog: a relative source resolves
	// against the marketplace root.
	if err := os.Rename(repo, filepath.Join(catDir, "hello")); err != nil {
		t.Fatal(err)
	}
	cat := writeCatalog(t, catDir, Plugin{
		Name: "hello", Version: "1.0.0", Description: "greets",
		Source: "./hello", Revision: "v1.0.0",
	})

	inst, warns, err := Install(context.Background(), "hello@1.0.0", Options{Marketplaces: []string{cat}})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(warns) != 0 {
		t.Fatalf("warnings = %v", warns)
	}
	if inst.Revision != v1 {
		t.Fatalf("revision = %q, want the pinned commit %q", inst.Revision, v1)
	}
	if want := filepath.Join(dataDir, "plugins", "hello"); inst.Path != want {
		t.Fatalf("path = %q, want %q", inst.Path, want)
	}
	if _, err := os.Stat(filepath.Join(inst.Path, "commands", "hello.md")); err != nil {
		t.Fatalf("pinned tree is missing commands/hello.md: %v", err)
	}
	if _, err := os.Stat(filepath.Join(inst.Path, "commands", "newer.md")); err == nil {
		t.Fatal("v2 file leaked into a v1.0.0 pinned install")
	}

	// The registry round-trips the pin, and every capability root is
	// registered by path.
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reg.Find("hello")
	if !ok || got.Revision != v1 || got.Version != "1.0.0" {
		t.Fatalf("registry entry = %+v", got)
	}
	assertDirs(t, inst, "commands", "skills", "agents", "hooks")
}

func TestInstallWithoutRevisionTakesHead(t *testing.T) {
	testDataDir(t)
	repo, _, v2 := newGitFixture(t)
	cat := writeCatalog(t, t.TempDir(), Plugin{Name: "hello", Version: "2.0.0", Source: repo})

	inst := mustInstall(t, "hello", Options{Marketplaces: []string{cat}})
	if inst.Revision != v2 {
		t.Fatalf("revision = %q, want HEAD %q", inst.Revision, v2)
	}
	if _, err := os.Stat(filepath.Join(inst.Path, "commands", "newer.md")); err != nil {
		t.Fatalf("unpinned install must take HEAD: %v", err)
	}
}

func TestInstallRefusesManifestMismatch(t *testing.T) {
	dataDir := testDataDir(t)
	repo, _, _ := newGitFixture(t)

	cases := []struct {
		name  string
		entry Plugin
		want  string
	}{
		{
			name:  "name",
			entry: Plugin{Name: "greeter", Version: "1.0.0", Source: repo, Revision: "v1.0.0"},
			want:  `declares name "hello"`,
		},
		{
			name:  "version",
			entry: Plugin{Name: "hello", Version: "2.0.0", Source: repo, Revision: "v1.0.0"},
			want:  `declares version "1.0.0"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := filepath.Join(dataDir, "plugins", "hello")
			_ = os.RemoveAll(target)
			if err := os.Remove(filepath.Join(Root(), "installed.json")); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			cat := writeCatalog(t, t.TempDir(), tc.entry)
			_, _, err := Install(context.Background(), tc.entry.Name, Options{Marketplaces: []string{cat}})
			if err == nil || !strings.Contains(err.Error(), "manifest mismatch") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a manifest mismatch mentioning %q", err, tc.want)
			}
			if _, err := os.Stat(target); err == nil {
				t.Fatal("a refused install must leave no tree behind")
			}
			assertNoRegistryEntry(t, "hello")
			assertStagingEmpty(t)
		})
	}
}

func TestInstallForceReplaces(t *testing.T) {
	testDataDir(t)
	repo, v1, _ := newGitFixture(t)
	cat := writeCatalog(t, t.TempDir(), Plugin{Name: "hello", Version: "2.0.0", Source: repo})

	first := mustInstall(t, "hello", Options{Marketplaces: []string{cat}})
	if _, _, err := Install(context.Background(), "hello", Options{Marketplaces: []string{cat}}); err == nil {
		t.Fatal("a second install without -force must fail")
	}

	cat2 := writeCatalog(t, t.TempDir(), Plugin{Name: "hello", Version: "1.0.0", Source: repo, Revision: "v1.0.0"})
	again := mustInstall(t, "hello", Options{Marketplaces: []string{cat2}, Force: true})
	if again.Revision != v1 || again.Revision == first.Revision {
		t.Fatalf("revision = %q, want the v1 pin %q", again.Revision, v1)
	}
	if _, err := os.Stat(filepath.Join(again.Path, "commands", "newer.md")); err == nil {
		t.Fatal("-force must replace the tree, not merge into it")
	}
}

func TestInstallLocalDirectoryCopy(t *testing.T) {
	testDataDir(t)
	src := t.TempDir()
	writePluginTree(t, src, "0.1.0")
	writeFile(t, filepath.Join(src, "commands", "local.md"), "local only\n")
	cat := writeCatalog(t, t.TempDir(), Plugin{Name: "hello", Version: "0.1.0", Source: src})

	inst := mustInstall(t, "hello", Options{Marketplaces: []string{cat}})
	if !strings.HasPrefix(inst.Revision, "sha256:") {
		t.Fatalf("revision = %q, want a content fingerprint for a plain copy", inst.Revision)
	}
	if _, err := os.Stat(filepath.Join(inst.Path, "commands", "local.md")); err != nil {
		t.Fatalf("copy missed a file: %v", err)
	}
	// The install is a copy, not a symlink: editing the source changes nothing.
	if err := os.Remove(filepath.Join(src, "commands", "local.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(inst.Path, "commands", "local.md")); err != nil {
		t.Fatalf("install must not follow the source: %v", err)
	}
}

// TestInstallClaudeLayout covers the Claude-compatible paths: a catalog at
// .claude-plugin/marketplace.json and a plugin manifest at
// .claude-plugin/plugin.json. A relative source resolves against the
// marketplace root (the directory holding .claude-plugin/).
func TestInstallClaudeLayout(t *testing.T) {
	testDataDir(t)
	catDir := t.TempDir()
	src := filepath.Join(catDir, "plugin")
	writeFile(t, filepath.Join(src, ".claude-plugin", "plugin.json"),
		`{"name":"hello","version":"3.1.0","description":"claude style","commands":["cmds"]}`)
	writeFile(t, filepath.Join(src, "cmds", "hello.md"), "Say hello\n")
	writeFile(t, filepath.Join(catDir, ".claude-plugin", "marketplace.json"),
		`{"name":"claude-shop","plugins":[{"name":"hello","version":"3.1.0","source":"./plugin"}]}`)

	inst := mustInstall(t, "hello@3.1.0", Options{Marketplaces: []string{catDir}})
	if inst.Version != "3.1.0" || len(inst.Dirs.Commands) != 1 {
		t.Fatalf("install = %+v", inst)
	}
	if !strings.HasSuffix(inst.Dirs.Commands[0], filepath.Join("cmds")) {
		t.Fatalf("declared command dir not registered: %v", inst.Dirs.Commands)
	}
}

func TestInstallRefusesEscapingRelativeSource(t *testing.T) {
	testDataDir(t)
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "commands", "evil.md"), "evil\n")
	cat := writeCatalog(t, t.TempDir(), Plugin{Name: "hello", Source: "../outside"})

	_, _, err := Install(context.Background(), "hello", Options{Marketplaces: []string{cat}})
	if err == nil || !strings.Contains(err.Error(), "escapes the marketplace root") {
		t.Fatalf("err = %v, want an escaping-source refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(Root(), "hello")); statErr == nil {
		t.Fatal("a refused install must leave no tree behind")
	}
}

func TestInstallUnknownPlugin(t *testing.T) {
	testDataDir(t)
	cat := writeCatalog(t, t.TempDir(), Plugin{Name: "hello", Source: t.TempDir()})

	if _, _, err := Install(context.Background(), "nope", Options{Marketplaces: []string{cat}}); err == nil ||
		!strings.Contains(err.Error(), `plugin "nope" not found`) {
		t.Fatalf("err = %v, want a not-found error", err)
	}
	if _, _, err := Install(context.Background(), "hello@9.9.9", Options{Marketplaces: []string{cat}}); err == nil ||
		!strings.Contains(err.Error(), "hello@9.9.9") {
		t.Fatalf("err = %v, want the version in the not-found error", err)
	}
	if _, _, err := Install(context.Background(), "hello", Options{}); err == nil ||
		!strings.Contains(err.Error(), "no marketplace could be loaded") {
		t.Fatalf("err = %v, want an explanation about missing catalogs", err)
	}
	if _, _, err := Install(context.Background(), "a/b", Options{Marketplaces: []string{cat}}); err == nil ||
		!strings.Contains(err.Error(), "path separator") {
		t.Fatalf("err = %v, want a name validation error", err)
	}
}

func TestInstallBrokenCatalogWarns(t *testing.T) {
	testDataDir(t)
	empty := t.TempDir() // no manifest
	bad := t.TempDir()
	writeFile(t, filepath.Join(bad, "marketplace.json"), `{"name":"shop","plugins":[{"name":"a"}]}`)
	cat := writeCatalog(t, t.TempDir(), Plugin{Name: "hello", Source: t.TempDir()})

	catalogs, warns := Catalogs(context.Background(), []string{empty, bad, cat})
	if len(catalogs) != 1 || catalogs[0].Name != "shop" {
		t.Fatalf("catalogs = %+v", catalogs)
	}
	joined := strings.Join(warns, "\n")
	if !strings.Contains(joined, "no catalog manifest") || !strings.Contains(joined, "source is required") {
		t.Fatalf("warnings = %v", warns)
	}
}

func TestRemoveCleansTreeAndRoots(t *testing.T) {
	testDataDir(t)
	repo, _, _ := newGitFixture(t)
	cat := writeCatalog(t, t.TempDir(), Plugin{Name: "hello", Source: repo})
	inst := mustInstall(t, "hello", Options{Marketplaces: []string{cat}})

	if len(CommandDirs()) != 1 {
		t.Fatalf("CommandDirs = %v, want the installed plugin root", CommandDirs())
	}
	removed, err := Remove("hello")
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if removed.Name != "hello" {
		t.Fatalf("removed = %+v", removed)
	}
	if _, err := os.Stat(inst.Path); err == nil {
		t.Fatal("the plugin tree must be gone")
	}
	assertNoRegistryEntry(t, "hello")
	if dirs := CommandDirs(); len(dirs) != 0 {
		t.Fatalf("CommandDirs = %v after remove, want none", dirs)
	}
	if dirs := SkillRoots(); len(dirs) != 0 {
		t.Fatalf("SkillRoots = %v after remove, want none", dirs)
	}
	if _, err := Remove("hello"); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("err = %v, want a not-installed error", err)
	}
}

func TestRemoveRefusesPathOutsidePluginRoot(t *testing.T) {
	testDataDir(t)
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "keep.md"), "keep\n")
	if err := os.MkdirAll(Root(), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(Registry{Version: RegistryVersion, Plugins: []Installed{
		{Name: "evil", Path: outside, Source: outside},
	}})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, registryPath(), string(raw))

	if _, err := Remove("evil"); err == nil || !strings.Contains(err.Error(), "refusing to delete") {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.md")); err != nil {
		t.Fatalf("a foreign path must survive: %v", err)
	}
}

func TestSearchFiltersCatalogs(t *testing.T) {
	catalogs, warns := Catalogs(context.Background(), []string{writeCatalog(t, t.TempDir(),
		Plugin{Name: "alpha", Version: "1.0.0", Description: "widget toolkit", Source: "s1"},
		Plugin{Name: "beta", Version: "1.0.0", Description: "greeting helper", Source: "s2"},
	)})
	if len(warns) != 0 {
		t.Fatalf("warnings = %v", warns)
	}
	if got := Available(catalogs); len(got) != 2 || got[0].Name != "alpha" {
		t.Fatalf("Available = %+v", got)
	}
	if got := Search(catalogs, "beta"); len(got) != 1 || got[0].Name != "beta" {
		t.Fatalf("Search(beta) = %+v", got)
	}
	if got := Search(catalogs, "WIDGET"); len(got) != 1 || got[0].Name != "alpha" {
		t.Fatalf("Search(WIDGET) = %+v", got)
	}
	if got := Search(catalogs, "shop"); len(got) != 2 {
		t.Fatalf("Search(shop) = %+v, want both entries via the marketplace name", got)
	}
	if got := Search(catalogs, "nope"); len(got) != 0 {
		t.Fatalf("Search(nope) = %+v", got)
	}
	if got := Search(catalogs, "  "); len(got) != 2 {
		t.Fatalf("Search(blank) = %+v, want every entry", got)
	}
}

// --- assertions -------------------------------------------------------------

func assertDirs(t *testing.T, inst *Installed, kinds ...string) {
	t.Helper()
	for _, kind := range kinds {
		var got []string
		switch kind {
		case "commands":
			got = inst.Dirs.Commands
		case "skills":
			got = inst.Dirs.Skills
		case "agents":
			got = inst.Dirs.Agents
		case "hooks":
			got = inst.Dirs.Hooks
		}
		if len(got) != 1 || got[0] != filepath.Join(inst.Path, kind) {
			t.Fatalf("%s dirs = %v, want %s", kind, got, filepath.Join(inst.Path, kind))
		}
	}
}

func assertNoRegistryEntry(t *testing.T, name string) {
	t.Helper()
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if inst, ok := reg.Find(name); ok {
		t.Fatalf("registry still lists %+v", inst)
	}
}

func assertStagingEmpty(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(Root(), ".staging"))
	if err != nil {
		return
	}
	if len(entries) != 0 {
		t.Fatalf("staging leftovers: %v", entries)
	}
}
