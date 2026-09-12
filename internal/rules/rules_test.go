package rules

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func withDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	SetDataDir(dir)
	t.Cleanup(func() { SetDataDir("") })
	return dir
}

// TestMain isolates the user-level roots (data dir + home) so discovery
// never picks up the developer's real ~/.xdev/agent/rules.
func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "rules-test")
	if err == nil {
		defer os.RemoveAll(tmp)
		oldHome := os.Getenv("HOME")
		os.Setenv("HOME", tmp)
		defer os.Setenv("HOME", oldHome)
		SetDataDir(filepath.Join(tmp, ".xdev", "agent"))
	}
	os.Exit(m.Run())
}

func find(list []Rule, name string) (Rule, bool) {
	for _, r := range list {
		if r.Name == name {
			return r, true
		}
	}
	return Rule{}, false
}

func TestDiscoverProviders(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "sub")
	// Project-relative providers (native, cursor, windsurf, cline,
	// github) resolve against cwd; the agents provider walks up.
	write(t, filepath.Join(cwd, ".omp", "rules", "native.md"), "native body\n")
	write(t, filepath.Join(cwd, ".cursor", "rules", "cursor.mdc"), "cursor body\n")
	write(t, filepath.Join(cwd, ".windsurf", "rules", "surf.md"), "windsurf body\n")
	write(t, filepath.Join(cwd, ".clinerules", "cline.md"), "cline body\n")
	write(t, filepath.Join(cwd, ".github", "copilot-instructions.md"), "copilot body\n")
	write(t, filepath.Join(root, ".agent", "rules", "walked.md"), "walked body\n")

	got := Discover(cwd, nil)
	want := map[string]struct {
		priority int
		source   string
	}{
		"native":               {PriorityNative, "native"},
		"cursor":               {PriorityCursor, "cursor"},
		"surf":                 {PriorityWindsurf, "windsurf"},
		"cline":                {PriorityCline, "cline"},
		"copilot-instructions": {PriorityGitHub, "github"},
		"walked":               {PriorityAgents, "agents"},
		"git-hygiene":          {PriorityBuiltin, "builtin"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rules [%s], want %d", len(got), ruleNames(got), len(want))
	}
	for _, r := range got {
		w, ok := want[r.Name]
		if !ok {
			t.Fatalf("unexpected rule %q", r.Name)
		}
		if r.Priority != w.priority || r.Source != w.source {
			t.Errorf("%s: priority=%d source=%s, want %d/%s", r.Name, r.Priority, r.Source, w.priority, w.source)
		}
	}
}

func ruleNames(list []Rule) string {
	out := make([]string, 0, len(list))
	for _, r := range list {
		out = append(out, r.Name)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func TestDiscoverFirstWinsByPriority(t *testing.T) {
	cwd := t.TempDir()
	write(t, filepath.Join(cwd, ".omp", "rules", "dup.md"), "native wins\n")
	write(t, filepath.Join(cwd, ".cursor", "rules", "dup.mdc"), "cursor loses\n")

	got := Discover(cwd, nil)
	r, ok := find(got, "dup")
	if !ok {
		t.Fatal("dup rule missing")
	}
	if r.Source != "native" || r.Content != "native wins\n" {
		t.Fatalf("first-wins violated: source=%s content=%q", r.Source, r.Content)
	}
}

func TestMdcFrontmatterParse(t *testing.T) {
	cwd := t.TempDir()
	write(t, filepath.Join(cwd, ".cursor", "rules", "ts.mdc"), `---
description: TypeScript conventions
globs:
  - "**/*.ts"
  - "*.tsx"
alwaysApply: false
condition: "panic"
scope: repo
agents:
  - fixer
interruptMode: confirm
---
Use explicit types.
`)
	got := Discover(cwd, nil)
	r, ok := find(got, "ts")
	if !ok {
		t.Fatal("mdc rule missing")
	}
	if r.Description != "TypeScript conventions" {
		t.Errorf("description = %q", r.Description)
	}
	if len(r.Globs) != 2 || r.Globs[0] != "**/*.ts" {
		t.Errorf("globs = %v", r.Globs)
	}
	if r.AlwaysApply {
		t.Error("alwaysApply should be false")
	}
	if r.Condition != "panic" || r.Scope != "repo" || r.InterruptMode != "confirm" {
		t.Errorf("condition/scope/interruptMode = %q/%q/%q", r.Condition, r.Scope, r.InterruptMode)
	}
	if len(r.Agents) != 1 || r.Agents[0] != "fixer" {
		t.Errorf("agents = %v", r.Agents)
	}
	if !strings.HasPrefix(r.Content, "Use explicit types.") {
		t.Errorf("content = %q", r.Content)
	}
	// Globs as a comma string also parse.
	write(t, filepath.Join(cwd, ".cursor", "rules", "go.mdc"), "---\nglobs: \"*.go, *.mod\"\n---\nbody\n")
	r2, ok := find(Discover(cwd, nil), "go")
	if !ok || len(r2.Globs) != 2 || r2.Globs[1] != "*.mod" {
		t.Fatalf("comma globs not parsed: %+v", r2)
	}
}

func TestShorthand(t *testing.T) {
	gated := Rule{Name: "g", Globs: []string{"*.go"}}
	if got, want := gated.Shorthand(), "tool:edit()/tool:write() when path matches `*.go`"; got != want {
		t.Fatalf("shorthand = %q, want %q", got, want)
	}
	if (Rule{Name: "a", AlwaysApply: true, Globs: []string{"*.go"}}).Shorthand() != "" {
		t.Error("always-apply rule must not produce a shorthand")
	}
	if (Rule{Name: "p"}).Shorthand() != "" {
		t.Error("glob-less rule must not produce a shorthand")
	}
}

func TestMatchesPath(t *testing.T) {
	gated := Rule{Globs: []string{"*.go"}}
	if !gated.MatchesPath("internal/rules/rules.go") {
		t.Error("basename glob must match")
	}
	if gated.MatchesPath("README.md") {
		t.Error("non-matching path must not match")
	}
	dir := Rule{Globs: []string{"internal/**"}}
	if !dir.MatchesPath("internal/rules/rules.go") {
		t.Error("dir prefix glob must match")
	}
	if !(Rule{AlwaysApply: true}).MatchesPath("anything") {
		t.Error("always-apply matches everything")
	}
	if !(Rule{}).MatchesPath("anything") {
		t.Error("glob-less rule matches everything")
	}
}

func TestStickyRulesShadowing(t *testing.T) {
	user := withDataDir(t)
	cwd := t.TempDir()
	write(t, filepath.Join(user, "RULES.md"), "user rules win\n")
	write(t, filepath.Join(cwd, ".omp", "RULES.md"), "---\nalwaysApply: false\n---\nproject rules\n")

	got := Discover(cwd, nil)
	r, ok := find(got, "RULES")
	if !ok {
		t.Fatal("sticky RULES rule missing")
	}
	if r.Content != "user rules win\n" {
		t.Fatalf("user RULES.md must shadow project: %q", r.Content)
	}
	if !r.AlwaysApply {
		t.Error("sticky RULES must be forced alwaysApply (frontmatter cannot unstick)")
	}
	if r.Priority != PriorityNative {
		t.Errorf("sticky priority = %d", r.Priority)
	}
}

func TestEnabledProvidersGate(t *testing.T) {
	cwd := t.TempDir()
	write(t, filepath.Join(cwd, ".omp", "rules", "n.md"), "n\n")
	write(t, filepath.Join(cwd, ".cursor", "rules", "c.mdc"), "c\n")

	all := Discover(cwd, nil)
	if _, ok := find(all, "n"); !ok {
		t.Error("empty gate must enable native")
	}
	if _, ok := find(all, "c"); !ok {
		t.Error("empty gate must enable cursor")
	}
	only := Discover(cwd, []string{"cursor"})
	if _, ok := find(only, "n"); ok {
		t.Error("native must be excluded when not listed")
	}
	if _, ok := find(only, "c"); !ok {
		t.Error("cursor must run when listed")
	}
	wild := Discover(cwd, []string{"all"})
	if _, ok := find(wild, "n"); !ok {
		t.Error("wildcard must enable native")
	}
	if _, ok := find(wild, "c"); !ok {
		t.Error("wildcard must enable cursor")
	}
}

func TestResolveRuleURI(t *testing.T) {
	Set([]Rule{
		{Name: "known", Content: "the body"},
		{Name: "gated", Content: "gated body", Globs: []string{"*.go"}},
	})
	t.Cleanup(func() { Set(nil) })

	body, err := Resolve("rule://known")
	if err != nil || body != "the body" {
		t.Fatalf("resolve = %q, %v", body, err)
	}
	if _, err := Resolve("rule://missing"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown rule must 404-style fail, got %v", err)
	}
	if _, err := Resolve("rule://"); err == nil {
		t.Fatal("empty reference must fail")
	}
}

func TestForPath(t *testing.T) {
	Set([]Rule{
		{Name: "always", AlwaysApply: true},
		{Name: "go-only", Globs: []string{"*.go"}},
		{Name: "md-only", Globs: []string{"*.md"}},
	})
	t.Cleanup(func() { Set(nil) })

	got := ForPath("a/b/c.go")
	if len(got) != 2 {
		t.Fatalf("ForPath(go) = %s, want always+go-only", ruleNames(got))
	}
	if got := ForPath("x.md"); len(got) != 2 || got[1].Name != "md-only" {
		t.Fatalf("ForPath(md) = %s", ruleNames(got))
	}
}

func TestOMPPluginsOptionalRoot(t *testing.T) {
	cwd := t.TempDir()
	// No plugins dir at all: discovery must not fail or fabricate rules.
	for _, r := range Discover(cwd, nil) {
		if r.Source == "omp-plugins" {
			t.Fatalf("unexpected plugin rule %q without a plugins dir", r.Name)
		}
	}
	write(t, filepath.Join(cwd, ".omp", "plugins", "core", "rules", "plug.md"), "plugin body\n")
	r, ok := find(Discover(cwd, nil), "plug")
	if !ok || r.Priority != PriorityOMPPlugins {
		t.Fatalf("plugin rule missing or wrong priority: %+v", r)
	}
}
