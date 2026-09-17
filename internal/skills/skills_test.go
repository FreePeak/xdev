package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverParsesFrontmatterAndBodies(t *testing.T) {
	home := t.TempDir()
	SetDataDir(home)
	t.Cleanup(func() { SetDataDir(defaultDataDir()) })
	proj := t.TempDir()
	writeSkill(t, projectRoot(proj), "fmt", `---
name: fmt
description: formatting conventions
globs: "*.go,*.md"
hide: false
---
Use tabs.`)

	list := Discover(proj)
	if len(list) != 1 {
		t.Fatalf("skills = %+v", list)
	}
	s := list[0]
	if s.Name != "fmt" || s.Description != "formatting conventions" {
		t.Fatalf("frontmatter = %+v", s)
	}
	if len(s.Globs) != 2 || s.Globs[0] != "*.go" {
		t.Fatalf("globs = %v", s.Globs)
	}
	if s.Body != "Use tabs." {
		t.Fatalf("body = %q", s.Body)
	}
	if s.Source != "native" {
		t.Fatalf("source = %q", s.Source)
	}
}

func TestDiscoverPrecedenceProjectBeatsUser(t *testing.T) {
	home := t.TempDir()
	SetDataDir(home)
	t.Cleanup(func() { SetDataDir(defaultDataDir()) })
	proj := t.TempDir()
	writeSkill(t, projectRoot(proj), "dup", "---\ndescription: project version\n---\np")
	writeSkill(t, UserRoot(), "dup", "---\ndescription: user version\n---\nu")

	list := Discover(proj)
	if len(list) != 1 || list[0].Description != "project version" {
		t.Fatalf("project must win: %+v", list)
	}
}

func TestDiscoverSkipsDescriptionlessSkill(t *testing.T) {
	home := t.TempDir()
	SetDataDir(home)
	t.Cleanup(func() { SetDataDir(defaultDataDir()) })
	proj := t.TempDir()
	writeSkill(t, projectRoot(proj), "good", "---\ndescription: ok\n---\nbody")
	writeSkill(t, projectRoot(proj), "bad", "---\nname: bad\n---\nno description")
	list := Discover(proj)
	if len(list) != 1 || list[0].Name != "good" {
		t.Fatalf("list = %+v", list)
	}
}

func TestResolveByNameAndRelativePath(t *testing.T) {
	home := t.TempDir()
	SetDataDir(home)
	t.Cleanup(func() { SetDataDir(defaultDataDir()) })
	proj := t.TempDir()
	dir := filepath.Join(projectRoot(proj), "deploy")
	writeSkill(t, projectRoot(proj), "deploy", "---\ndescription: deploy steps\n---\nStep one.")
	if err := os.WriteFile(filepath.Join(dir, "checklist.md"), []byte("check a\ncheck b"), 0o644); err != nil {
		t.Fatal(err)
	}
	SetGetwd(func() (string, error) { return proj, nil })
	t.Cleanup(func() { SetGetwd(os.Getwd) })

	body, err := Resolve("skill://deploy")
	if err != nil || !strings.Contains(body, "Step one.") {
		t.Fatalf("skill://deploy = %q, %v", body, err)
	}
	rel, err := Resolve("skill://deploy/checklist.md")
	if err != nil || !strings.Contains(rel, "check a") {
		t.Fatalf("relative read = %q, %v", rel, err)
	}
}

func TestResolveRejectsTraversalAndMissing(t *testing.T) {
	home := t.TempDir()
	SetDataDir(home)
	t.Cleanup(func() { SetDataDir(defaultDataDir()) })
	proj := t.TempDir()
	writeSkill(t, projectRoot(proj), "safe", "---\ndescription: d\n---\nbody")
	SetGetwd(func() (string, error) { return proj, nil })
	t.Cleanup(func() { SetGetwd(os.Getwd) })

	for _, uri := range []string{
		"skill://safe/../../../etc/passwd",
		"skill://../escape",
		"skill://",
		"skill://nope",
	} {
		if _, err := Resolve(uri); err == nil {
			t.Fatalf("%q must be rejected", uri)
		}
	}
}

func TestFindIsExactCaseSensitive(t *testing.T) {
	list := []Skill{{Name: "Fmt"}, {Name: "fmt"}}
	if _, ok := Find(list, "fmt"); !ok {
		t.Fatal("exact match must be found")
	}
	if _, ok := Find(list, "FMT"); ok {
		t.Fatal("case mismatch must not match")
	}
}

// TestCustomDirectoriesDiscoveryAndPrecedence: settings-listed extra roots
// are scanned after native/user/managed, resolve relative entries against
// the cwd, and skip entries that are not directories.
func TestCustomDirectoriesDiscoveryAndPrecedence(t *testing.T) {
	home := t.TempDir()
	SetDataDir(home)
	t.Cleanup(func() { SetDataDir(defaultDataDir()) })
	t.Cleanup(func() { SetCustomDirectories(nil) })

	proj := t.TempDir()
	shared := filepath.Join(t.TempDir(), "shared-packs")
	writeSkill(t, shared, "lint", "---\ndescription: shared lint\n---\nshared body")
	writeSkill(t, shared, "agentpack", "---\ndescription: shared agentpack\n---\ns")
	writeSkill(t, ManagedRoot(), "agentpack", "---\ndescription: learned agentpack\n---\nm")
	writeSkill(t, filepath.Join(proj, "vendor-skills"), "rel", "---\ndescription: relative pack\n---\nr")
	writeSkill(t, projectRoot(proj), "lint", "---\ndescription: project lint\n---\np")

	SetCustomDirectories([]string{shared, "vendor-skills", "   ", filepath.Join(shared, "SKILL.md")})
	list := Discover(proj)

	lint, ok := Find(list, "lint")
	if !ok || lint.Source != "native" || lint.Description != "project lint" {
		t.Fatalf("native must outrank a custom root: %+v", lint)
	}
	rel, ok := Find(list, "rel")
	if !ok || rel.Source != "custom" {
		t.Fatalf("relative custom root not discovered: %+v", list)
	}
	if pack, ok := Find(list, "agentpack"); !ok || pack.Source != "managed" {
		t.Fatalf("managed must outrank a custom root: %+v", pack)
	}
	if len(list) != 3 {
		t.Fatalf("invalid custom entries must contribute nothing: %+v", list)
	}
}

// TestConflictsReportTheShadowingPair pins the direction the learn tool
// reports: an authored pack beats managed, a custom directory loses to it.
func TestConflictsReportTheShadowingPair(t *testing.T) {
	home := t.TempDir()
	SetDataDir(home)
	t.Cleanup(func() { SetDataDir(defaultDataDir()) })
	t.Cleanup(func() { SetCustomDirectories(nil) })

	proj := t.TempDir()
	native := filepath.Join(projectRoot(proj), "native-pack", "SKILL.md")
	writeSkill(t, projectRoot(proj), "native-pack", "---\ndescription: d\n---\nb")
	shared := t.TempDir()
	custom := filepath.Join(shared, "custom-only", "SKILL.md")
	writeSkill(t, shared, "custom-only", "---\ndescription: d\n---\nb")
	SetCustomDirectories([]string{shared})

	cs := Conflicts(proj, "native-pack")
	if len(cs) != 1 || !cs[0].BeatsManaged || cs[0].Skill.Path != native {
		t.Fatalf("authored conflict = %+v", cs)
	}
	cs = Conflicts(proj, "custom-only")
	if len(cs) != 1 || cs[0].BeatsManaged || cs[0].Skill.Path != custom {
		t.Fatalf("custom conflict = %+v", cs)
	}
	if cs := Conflicts(proj, "absent"); len(cs) != 0 {
		t.Fatalf("no collision expected: %+v", cs)
	}
}

// TestDefaultDataDirHonorsAgentDirEnv: a sandboxed run (XDEV_AGENT_DIR) must
// not read the real ~/.xdev/agent, the same override config.DataDir applies.
func TestDefaultDataDirHonorsAgentDirEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dir)
	if got := defaultDataDir(); got != dir {
		t.Fatalf("defaultDataDir = %q, want %q", got, dir)
	}
}
