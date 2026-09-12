package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/hooks"
	"github.com/FreePeak/xdev/internal/marketplace"
	"github.com/FreePeak/xdev/internal/skills"
	"github.com/FreePeak/xdev/internal/tui"
)

// #85: installing a plugin contributed nothing — the marketplace accessors had
// zero consumers, so `plugin install` wrote a tree nothing read. This drives a
// real install and checks all four discovery paths.
func TestInstalledPluginSurfacesContent(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", data)
	cwd := t.TempDir()

	src := filepath.Join(data, "src")
	writeFileAt(t, filepath.Join(src, ".xdev-plugin", "plugin.json"),
		`{"name":"demo","version":"1.0.0","description":"demo"}`)
	writeFileAt(t, filepath.Join(src, "commands", "count.md"), "---\ndescription: count\n---\nSay PLUGIN-CMD\n")
	writeFileAt(t, filepath.Join(src, "skills", "demoskill", "SKILL.md"),
		"---\nname: demoskill\ndescription: plugin skill\n---\nbody\n")
	writeFileAt(t, filepath.Join(src, "agents", "pluginie.md"),
		"---\nname: pluginie\ndescription: plugin agent\n---\nsystem\n")
	writeFileAt(t, filepath.Join(src, "hooks", "guard.yml"), "name: guardplugin\nevent: tool_call\ncommand: echo ok\n")
	cat := filepath.Join(data, "mkt")
	raw, _ := json.Marshal(marketplace.Manifest{Name: "local", Plugins: []marketplace.Plugin{{Name: "demo", Source: src}}})
	writeFileAt(t, filepath.Join(cat, ".xdev-plugin", "marketplace.json"), string(raw))

	if _, _, err := marketplace.Install(context.Background(), "demo",
		marketplace.Options{Marketplaces: []string{cat}}); err != nil {
		t.Fatalf("install: %v", err)
	}

	if got := tui.DiscoverCommands(cwd); !hasName(tuiCommands(got), "count") {
		t.Fatalf("plugin command not discovered: %v", tuiCommands(got))
	}
	if got := skills.Discover(cwd); !hasName(skillNames(got), "demoskill") {
		t.Fatalf("plugin skill not discovered: %v", skillNames(got))
	}
	agents, _ := agent.DiscoverAgents(cwd)
	if !hasName(agentNames(agents), "pluginie") {
		t.Fatalf("plugin agent not discovered: %v", agentNames(agents))
	}
	hs, _ := hooks.Discover(cwd, nil)
	if !hasName(hookNames(hs), "guardplugin") {
		t.Fatalf("plugin hook not discovered: %v", hookNames(hs))
	}
}

func writeFileAt(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hasName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func tuiCommands(cs []tui.MarkdownCommand) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}

func skillNames(ss []skills.Skill) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Name
	}
	return out
}

func agentNames(as []agent.AgentDefinition) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.Name
	}
	return out
}

func hookNames(hs []hooks.Hook) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = h.Name
	}
	return out
}
