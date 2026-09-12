package mcpclient

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/FreePeak/xdev/internal/logx"
)

// manifestName is the Gemini CLI extension manifest filename.
const manifestName = "gemini-extension.json"

// Extension is one discovered Gemini-CLI extension manifest (M13 #57).
// Manifests are the LOWEST-priority source: an entry in mcp.yml with the
// same server name always wins.
type Extension struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
	// MCPServers use the mcp.yml server shape (command/args/env/cwd/url/
	// headers/timeout/enabled/…).
	MCPServers map[string]*ServerConfig `yaml:"mcpServers"`
	// Commands and Skills are directories — relative to the extension dir,
	// absolute, or ~-rooted — that join the slash-command and skill
	// discovery roots at the lowest priority.
	Commands stringList `yaml:"commands"`
	Skills   stringList `yaml:"skills"`
	// Dir is the extension's absolute directory (set by discovery, never
	// read from the manifest).
	Dir string `yaml:"-"`
}

// stringList accepts a bare string as well as a list, so a manifest may
// write `"commands": "cmds"` or `"commands": ["cmds", "more"]`.
type stringList []string

func (s *stringList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var v string
		if err := node.Decode(&v); err != nil {
			return err
		}
		if v = strings.TrimSpace(v); v == "" {
			*s = nil
			return nil
		}
		*s = stringList{v}
		return nil
	case yaml.SequenceNode:
		var v []string
		if err := node.Decode(&v); err != nil {
			return err
		}
		*s = v
		return nil
	default:
		return fmt.Errorf("expected a string or a list of strings")
	}
}

// DiscoverExtensions scans <extRoot>/extensions/<name>/gemini-extension.json
// one level deep and returns the manifests sorted by name. A missing root, a
// directory without a manifest, and an empty manifest are silent skips; a
// malformed manifest is warned and skipped — a broken third-party extension
// must never disable MCP.
func DiscoverExtensions(extRoot string) []Extension {
	root := filepath.Join(extRoot, "extensions")
	ents, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			logx.Warnf("mcp: read %s: %v", root, err)
		}
		return nil
	}
	var out []Extension
	for _, ent := range ents {
		if !ent.IsDir() {
			continue
		}
		dir := filepath.Join(root, ent.Name())
		path := filepath.Join(dir, manifestName)
		raw, err := os.ReadFile(path)
		if err != nil || len(bytes.TrimSpace(raw)) == 0 {
			continue // absent/unreadable/empty manifest: silent skip
		}
		var ext Extension
		if err := yaml.Unmarshal(raw, &ext); err != nil {
			logx.Warnf("mcp: invalid JSON in %s: %v", path, err)
			continue
		}
		if ext.Name == "" {
			ext.Name = ent.Name() // the directory names the extension
		}
		ext.Dir = dir
		ext.Commands = resolveDirs(dir, ext.Commands)
		ext.Skills = resolveDirs(dir, ext.Skills)
		out = append(out, ext)
	}
	slices.SortFunc(out, func(a, b Extension) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// ExtensionRoots is the seam for callers that want the manifests' command
// and skill directories without the MCP servers — the host appends them
// after its native discovery roots, so they are the lowest priority.
func ExtensionRoots(extRoot string) (commands, skills []string) {
	for _, ext := range DiscoverExtensions(extRoot) {
		commands = append(commands, ext.Commands...)
		skills = append(skills, ext.Skills...)
	}
	return commands, skills
}

// resolveDirs makes declared directories absolute: a relative entry
// resolves against the extension directory, ~ against home, and empty
// entries are dropped.
func resolveDirs(base string, in []string) []string {
	out := make([]string, 0, len(in))
	for _, d := range in {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		if filepath.IsAbs(d) || strings.HasPrefix(d, "~") {
			if abs, err := expandPath(d); err == nil {
				out = append(out, abs)
			}
			continue
		}
		out = append(out, filepath.Clean(filepath.Join(base, d)))
	}
	return out
}
