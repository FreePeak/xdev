// Package marketplace implements the Claude-compatible plugin marketplace and
// plugin manager (M13 #53): catalogs that list plugins, and an installer that
// materializes one pinned plugin tree under <dataDir>/plugins/<name>.
//
// A marketplace is a directory or git repository holding a catalog manifest
// (.xdev-plugin/marketplace.json|yaml, marketplace.json|yaml, or the
// Claude-compatible .claude-plugin/marketplace.json) whose entries declare a
// plugin's source, revision and capability directories.
//
// A plugin is DATA, never code: markdown slash commands, SKILL.md packs,
// agent definitions and hook shell declarations. Nothing inside a plugin is
// loaded or executed in this process — install shells out to git only, and
// the discovered roots are handed to the existing command/skill/agent/hook
// discovery functions. Installed roots are appended AFTER every
// project/user/managed/custom root (see CommandDirs, SkillRoots, AgentDirs,
// HookDirs), so a plugin never shadows authored content.
package marketplace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ManifestFileNames are the catalog manifest paths tried inside a marketplace
// root, in order: the xdev layout first, then the Claude-compatible fallback.
var ManifestFileNames = []string{
	filepath.Join(".xdev-plugin", "marketplace.json"),
	filepath.Join(".xdev-plugin", "marketplace.yaml"),
	filepath.Join(".xdev-plugin", "marketplace.yml"),
	"marketplace.json",
	"marketplace.yaml",
	"marketplace.yml",
	filepath.Join(".claude-plugin", "marketplace.json"),
}

// PluginManifestFileNames are the plugin's own manifest paths tried inside a
// plugin tree, in order. A plugin that ships none is accepted as long as the
// catalog entry is unambiguous; a plugin that ships one must agree with it.
var PluginManifestFileNames = []string{
	filepath.Join(".xdev-plugin", "plugin.json"),
	filepath.Join(".xdev-plugin", "plugin.yaml"),
	filepath.Join(".xdev-plugin", "plugin.yml"),
	filepath.Join(".claude-plugin", "plugin.json"),
	"plugin.json",
	"plugin.yaml",
}

// Manifest is one marketplace catalog.
type Manifest struct {
	Name        string   `json:"name" yaml:"name"`
	Description string   `json:"description" yaml:"description"`
	Plugins     []Plugin `json:"plugins" yaml:"plugins"`
}

// Plugin is one catalog entry. It is also the shape of a plugin's own
// manifest (.xdev-plugin/plugin.json), where Source and Revision are unused.
type Plugin struct {
	Name        string `json:"name" yaml:"name"`
	Version     string `json:"version" yaml:"version"`
	Description string `json:"description" yaml:"description"`
	// Source is a git location (https/ssh/git/file URL or scp-like git@host)
	// or a path. A relative path resolves against the marketplace root; a
	// local source that is itself a git repository is cloned so the revision
	// pin applies, any other directory is copied.
	Source string `json:"source" yaml:"source"`
	// Revision pins the install: a git tag, branch or commit. Empty means the
	// source's default branch; the resolved revision is always recorded.
	Revision string `json:"revision" yaml:"revision"`
	// Commands/Skills/Agents/Hooks are plugin-root-relative directories.
	// Empty means the conventional layout: commands/, skills/, agents/,
	// hooks/ — whatever exists is registered.
	Commands []string `json:"commands" yaml:"commands"`
	Skills   []string `json:"skills" yaml:"skills"`
	Agents   []string `json:"agents" yaml:"agents"`
	Hooks    []string `json:"hooks" yaml:"hooks"`
}

// FindManifest returns the first catalog manifest under dir, "" when none.
func FindManifest(dir string) string {
	for _, name := range ManifestFileNames {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// FindPluginManifest returns the first plugin manifest under dir, "" when none.
func FindPluginManifest(dir string) string {
	for _, name := range PluginManifestFileNames {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// ParseManifest reads and validates a catalog manifest.
func ParseManifest(path string) (*Manifest, error) {
	var m Manifest
	if err := decodeFile(path, &m); err != nil {
		return nil, err
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &m, nil
}

// ParsePluginManifest reads and validates a plugin's own manifest.
func ParsePluginManifest(path string) (*Plugin, error) {
	var p Plugin
	if err := decodeFile(path, &p); err != nil {
		return nil, err
	}
	if err := p.validateCapabilities(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := validateName(p.Name); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &p, nil
}

// Find returns the entry for name (and, when version is non-empty, the entry
// declaring exactly that version). The version may be empty in a manifest —
// such an entry only matches a version-less lookup.
func (m *Manifest) Find(name, version string) (Plugin, bool) {
	for _, p := range m.Plugins {
		if p.Name != name {
			continue
		}
		if version != "" && p.Version != version {
			continue
		}
		return p, true
	}
	return Plugin{}, false
}

// validate reports every problem at once: a catalog is edited by hand, and
// one round trip per mistake helps nobody.
func (m *Manifest) validate() error {
	var problems []error
	if strings.TrimSpace(m.Name) == "" {
		problems = append(problems, errors.New("name is required"))
	}
	if len(m.Plugins) == 0 {
		problems = append(problems, errors.New("at least one plugin is required"))
	}
	seen := map[string]bool{}
	for i, p := range m.Plugins {
		if err := validateName(p.Name); err != nil {
			problems = append(problems, fmt.Errorf("plugins[%d]: %w", i, err))
		} else if seen[strings.ToLower(p.Name)] {
			// Case-insensitive: plugin names become directory names, and the
			// default macOS filesystem folds case.
			problems = append(problems, fmt.Errorf("plugins[%d]: duplicate plugin name %q", i, p.Name))
		}
		seen[strings.ToLower(p.Name)] = true
		if strings.TrimSpace(p.Source) == "" {
			problems = append(problems, fmt.Errorf("plugins[%d] (%s): source is required", i, p.Name))
		}
		if err := p.validateCapabilities(); err != nil {
			problems = append(problems, fmt.Errorf("plugins[%d] (%s): %w", i, p.Name, err))
		}
	}
	return errors.Join(problems...)
}

// validateCapabilities rejects a capability directory that is absolute or
// escapes the plugin root: these paths are resolved against the installed
// tree and handed to discovery, so traversal stops here.
func (p Plugin) validateCapabilities() error {
	var problems []error
	for kind, list := range map[string][]string{
		"commands": p.Commands,
		"skills":   p.Skills,
		"agents":   p.Agents,
		"hooks":    p.Hooks,
	} {
		for _, rel := range list {
			clean := filepath.Clean(strings.TrimSpace(rel))
			switch {
			case strings.TrimSpace(rel) == "":
				problems = append(problems, fmt.Errorf("%s: empty path", kind))
			case filepath.IsAbs(clean):
				problems = append(problems, fmt.Errorf("%s: %q must be relative to the plugin root", kind, rel))
			case clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)):
				problems = append(problems, fmt.Errorf("%s: %q escapes the plugin root", kind, rel))
			}
		}
	}
	return errors.Join(problems...)
}

// validateName rejects a plugin name that is not a single safe path segment:
// install targets are <dataDir>/plugins/<name>, so separators, traversal and
// hidden names must never reach the filesystem.
func validateName(name string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return errors.New("name is required")
	case name != strings.TrimSpace(name):
		return fmt.Errorf("name %q has surrounding whitespace", name)
	case name == "." || name == ".." || strings.HasPrefix(name, "."):
		return fmt.Errorf("name %q is not a usable directory name", name)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("name %q must not contain a path separator", name)
	case strings.ContainsAny(name, " \t\n"):
		return fmt.Errorf("name %q must not contain whitespace", name)
	}
	return nil
}

// SplitRef splits "name" or "name@version" (the CLI install argument).
func SplitRef(ref string) (name, version string) {
	name, version, _ = strings.Cut(strings.TrimSpace(ref), "@")
	return name, strings.TrimSpace(version)
}

// decodeFile reads JSON by extension and YAML otherwise (yaml covers .yml;
// a catalog served from a .json path stays JSON so malformed input is
// reported as JSON, not as a confusing YAML error).
func decodeFile(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if strings.EqualFold(filepath.Ext(path), ".json") {
		if err := json.Unmarshal(raw, v); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		return nil
	}
	if err := yaml.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
