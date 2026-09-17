package tool

// The #192 integrity floor: what the model may not write through the file
// tools, what it still may, and the two properties that make the difference —
// a symlink must not walk around it, and no configuration can lift it.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// isolateRoots points the floor at one directory for a test's duration, the way
// internal/config does at startup.
func isolateRoots(t *testing.T, roots ...string) {
	t.Helper()
	SetProtectedRoots(func() []string { return roots })
	t.Cleanup(func() { SetProtectedRoots(nil) })
}

func writeVia(t *testing.T, path, content string) (string, bool) {
	t.Helper()
	res, err := NewWriteTool().Execute(context.Background(),
		fsToolArgs(t, map[string]any{"path": path, "content": content}))
	if err != nil {
		t.Fatalf("write Execute: %v", err)
	}
	return res.Text, res.IsError
}

func editVia(t *testing.T, path, oldText, newText string) (string, bool) {
	t.Helper()
	res, err := NewEditTool().Execute(context.Background(), fsToolArgs(t, map[string]any{
		"path": path,
		"ops":  []any{map[string]any{"oldText": oldText, "newText": newText}},
	}))
	if err != nil {
		t.Fatalf("edit Execute: %v", err)
	}
	return res.Text, res.IsError
}

func TestWriteAndEditRefuseProfileState(t *testing.T) {
	data := t.TempDir()
	targets := map[string]string{
		"session jsonl":   filepath.Join(data, "sessions", "-proj", "2026-09-14T10-00-00.000Z_abcd.jsonl"),
		"subagent file":   filepath.Join(data, "sessions", "-proj", "subagents", "child.jsonl"),
		"blob":            filepath.Join(data, "blobs", "aa", "aabbccdd"),
		"settings":        filepath.Join(data, "config.yml"),
		"models":          filepath.Join(data, "models.yml"),
		"credentials":     filepath.Join(data, "credentials.json"),
		"trust record":    filepath.Join(data, "trusted-workspaces.yml"),
		"the data dir":    data,
		"stall dump dir":  filepath.Join(data, "dumps", "heap.out"),
		"memory store":    filepath.Join(data, "memory", "MEMORY.md"),
		"installed skill": filepath.Join(data, "skills", "own-skill", "SKILL.md"),
	}
	for name, path := range targets {
		t.Run("write "+name, func(t *testing.T) {
			isolateRoots(t, data)
			text, isErr := writeVia(t, path, "pwned")
			if !isErr || !strings.Contains(text, ProtectedStateReason) {
				t.Fatalf("write was allowed: isErr=%v text=%q", isErr, text)
			}
			if _, err := os.ReadFile(path); err == nil {
				t.Fatalf("%s was written despite the denial", path)
			}
		})
	}
	for name, path := range targets {
		t.Run("edit "+name, func(t *testing.T) {
			isolateRoots(t, data)
			text, isErr := editVia(t, path, "x", "y")
			if !isErr || !strings.Contains(text, ProtectedStateReason) {
				t.Fatalf("edit was allowed: isErr=%v text=%q", isErr, text)
			}
		})
	}
}

func TestRepoXdevDirIsRefused(t *testing.T) {
	repo := t.TempDir()
	isolateRoots(t, filepath.Join(repo, "elsewhere")) // the profile is NOT this dir
	t.Chdir(repo)
	for _, path := range []string{
		filepath.Join(repo, ".xdev", "config.yml"),
		filepath.Join(repo, ".xdev", "agents", "scout.md"),
		filepath.Join(repo, ".xdev", "hooks", "agent_start.sh"),
		filepath.Join(repo, ".xdev", "commands", "deploy.md"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		text, isErr := writeVia(t, path, "x")
		if !isErr || !strings.Contains(text, ProtectedStateReason) {
			t.Errorf("write to %s was allowed: %q", filepath.Base(path), text)
		}
	}
}

func TestTheDenialNamesTheWayThrough(t *testing.T) {
	data := t.TempDir()
	isolateRoots(t, data)

	// Settings: the supported route exists, so name it.
	text, _ := writeVia(t, filepath.Join(data, "config.yml"), "approvalMode: yolo\n")
	for _, want := range []string{"xdev config set", "Ask the user", ProtectedStateReason} {
		if !strings.Contains(text, want) {
			t.Errorf("settings denial missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "will not write it") {
		t.Errorf("the stable refusal phrasing was lost:\n%s", text)
	}
	// Session history: no route, but the reason must be stated, not just "no".
	if text, _ = writeVia(t, filepath.Join(data, "sessions", "a.jsonl"), "x"); !strings.Contains(text, "session API") {
		t.Errorf("session denial = %q", text)
	}
	// A repository file: point at #114/#241, the machinery that owns it.
	repo := t.TempDir()
	t.Chdir(repo)
	if err := os.MkdirAll(filepath.Join(repo, ".xdev"), 0o755); err != nil {
		t.Fatal(err)
	}
	if text, _ := writeVia(t, filepath.Join(repo, ".xdev", "agents", "x.md"), "y"); !strings.Contains(text, "Ask the user to change it") {
		t.Errorf("repo denial = %q", text)
	}
}

func TestOrdinaryProjectFilesAreUnaffected(t *testing.T) {
	dir := t.TempDir()
	isolateRoots(t, filepath.Join(dir, "profile"))
	for _, rel := range []string{
		"main.go", "README.md", "src/deep/nested/file.ts",
		// Look-alikes: the containment test must be a real one.
		"xdev/config.yml", ".xdevnotes.md", "app.xdev/config.yml", "notes.xdev",
	} {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		text, isErr := writeVia(t, p, "hello")
		if isErr {
			t.Fatalf("write of %s refused: %s", rel, text)
		}
		if raw, err := os.ReadFile(p); err != nil || string(raw) != "hello" {
			t.Fatalf("write of %s did not land: %v %q", rel, err, raw)
		}
	}
}

func TestSymlinksDoNotWalkAroundTheFloor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs a privilege on windows")
	}
	data := t.TempDir()
	isolateRoots(t, data)
	secret := filepath.Join(data, "credentials.json")
	if err := os.WriteFile(secret, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()

	t.Run("a link to the file", func(t *testing.T) {
		link := filepath.Join(project, "notes.json")
		if err := os.Symlink(secret, link); err != nil {
			t.Fatal(err)
		}
		text, isErr := writeVia(t, link, "stolen")
		if !isErr || !strings.Contains(text, ProtectedStateReason) {
			t.Fatalf("write through a symlink was allowed: %q", text)
		}
		if !strings.Contains(text, "symlink") {
			t.Errorf("the denial should say the path was reached through a link: %q", text)
		}
		if raw, _ := os.ReadFile(secret); string(raw) != "{}\n" {
			t.Fatal("the state file was modified through the link")
		}
	})

	t.Run("a link above the target directory", func(t *testing.T) {
		dirLink := filepath.Join(project, "state")
		if err := os.Symlink(data, dirLink); err != nil {
			t.Fatal(err)
		}
		text, isErr := writeVia(t, filepath.Join(dirLink, "keybindings.yml"), "x")
		if !isErr || !strings.Contains(text, ProtectedStateReason) {
			t.Fatalf("write through a linked directory was allowed: %q", text)
		}
	})
}

// TestRepoFloorSurvivesAnUnregisteredProfile pins the half of the rule that no
// wiring can switch off: with no roots registered at all, the checkout's own
// .xdev is still state, because the process cwd is enough to know that.
func TestRepoFloorSurvivesAnUnregisteredProfile(t *testing.T) {
	SetProtectedRoots(nil)
	repo := t.TempDir()
	t.Chdir(repo)
	if err := os.MkdirAll(filepath.Join(repo, ".xdev"), 0o755); err != nil {
		t.Fatal(err)
	}
	text, isErr := writeVia(t, filepath.Join(repo, ".xdev", "config.yml"), "approvalMode: yolo\n")
	if !isErr || !strings.Contains(text, ProtectedStateReason) {
		t.Fatalf(".xdev write allowed with no roots registered: %q", text)
	}
	// And a normal file in the same repo is still writable: the floor is the
	// .xdev directory, not the checkout.
	if text, isErr = writeVia(t, filepath.Join(repo, "main.go"), "package main"); isErr {
		t.Fatalf("ordinary file refused: %s", text)
	}
}

func TestContainmentIsNotAPrefixMatch(t *testing.T) {
	isolateRoots(t, "/home/u/.xdev/agent")
	cases := map[string]bool{
		"/home/u/.xdev/agent/config.yml":  true,
		"/home/u/.xdev/agent":             true,
		"/home/u/.xdev/agentx/config.yml": false, // sibling, not inside
		"/home/u/.xdev/agents/x.md":       false, // a different directory
		"/home/u/.xdev":                   false, // the parent, not the root
	}
	for path, want := range cases {
		if got := CheckProtectedPath(path, path) != nil; got != want {
			t.Errorf("%s protected = %v, want %v", path, got, want)
		}
	}
	if err := CheckProtectedPath("/home/u/.xdev/agentx/notes.md", "/home/u/.xdev/agentx/notes.md"); err != nil {
		t.Errorf("a sibling directory was denied: %v", err)
	}
}

// TestBashIsNotTheFloor documents the boundary of this guard, so nobody reads it
// as broader than it is: the shell can still write anywhere the OS lets it, which
// is containment's job (PRD §1) and #161's floor list.
func TestBashIsNotCoveredByTheFileToolFloor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell")
	}
	dir := t.TempDir()
	isolateRoots(t, dir)
	target := filepath.Join(dir, "config.yml")
	res, err := NewBashTool(dir).Execute(context.Background(),
		fsToolArgs(t, map[string]any{"command": "echo pwned > config.yml"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("bash should not be blocked by this floor: %s", res.Text)
	}
	if raw, _ := os.ReadFile(target); !strings.Contains(string(raw), "pwned") {
		t.Fatal("the redirect did not happen, so this test stopped describing reality")
	}
}
