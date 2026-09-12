package marketplace

import (
	"os"
)

// Discovery integration (M13 #53).
//
// Installed plugin roots are the LOWEST-precedence source for every
// capability kind. A consumer appends them after its own roots:
//
//	// internal/tui/discovery.go DiscoverCommands
//	roots := []string{filepath.Join(cwd, ".xdev", "commands")}
//	if dir := userCommandsDir(); dir != "" {
//		roots = append(roots, filepath.Join(dir, "commands"))
//	}
//	roots = append(roots, marketplace.CommandDirs()...) // last: never shadows
//
// so a plugin never shadows authored content: on a name collision the
// project, user, managed and custom roots win, and the plugin only supplies
// names nobody else claims. Uninstalling removes the plugin tree and its
// registry entry, so the root disappears from the next discovery pass.
//
// internal/rules already scans <dataDir>/plugins/<plugin>/rules on its own
// provider, which is what the install root below gives it for free.
//
// The accessors read installed.json on each call (a small file, once per
// startup) and return only directories that are inside <dataDir>/plugins and
// still exist on disk — a stale entry or a hand-edited registry contributes
// nothing.

// CommandDirs lists the slash-command roots contributed by installed plugins.
func CommandDirs() []string { return pluginRoots(func(d Dirs) []string { return d.Commands }) }

// SkillRoots lists the SKILL.md roots (`<root>/<name>/SKILL.md`) contributed
// by installed plugins.
func SkillRoots() []string { return pluginRoots(func(d Dirs) []string { return d.Skills }) }

// AgentDirs lists the agent-definition roots contributed by installed plugins.
func AgentDirs() []string { return pluginRoots(func(d Dirs) []string { return d.Agents }) }

// HookDirs lists the hook-declaration roots contributed by installed plugins.
func HookDirs() []string { return pluginRoots(func(d Dirs) []string { return d.Hooks }) }

func pluginRoots(pick func(Dirs) []string) []string {
	var out []string
	for _, inst := range InstalledPlugins() {
		if !insideRoot(inst.Path) {
			continue
		}
		for _, dir := range pick(inst.Dirs) {
			if !insideRootTree(dir) {
				continue
			}
			if st, err := os.Stat(dir); err == nil && st.IsDir() {
				out = append(out, dir)
			}
		}
	}
	return out
}
