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
