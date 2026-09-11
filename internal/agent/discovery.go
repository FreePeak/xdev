package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/FreePeak/xdev/internal/config"
	"gopkg.in/yaml.v3"
)

// AgentDefinition is one named subagent specification, discovered from
// markdown files with YAML frontmatter (M11 #12, research §1).
type AgentDefinition struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	// SystemPrompt is the file body below the frontmatter.
	SystemPrompt string `yaml:"-"`
	// Tools is a CSV string or YAML list of tool names; yield is always added.
	Tools stringList `yaml:"tools,omitempty"`
	// Spawns is "*" (unrestricted), a CSV allowlist, or empty (cannot spawn).
	Spawns interface{} `yaml:"spawns,omitempty"`
	// Model is a prioritized list; role aliases expand via modelRoles.
	Model string `yaml:"model,omitempty"`
	// ThinkingLevel caps the child's reasoning effort.
	ThinkingLevel string `yaml:"thinkingLevel,omitempty"`
	// OutputSchema (opaque) pins the child's yield contract.
	OutputSchema map[string]any `yaml:"output,omitempty"`
	// Blocking: parent always waits for the result.
	Blocking bool `yaml:"blocking,omitempty"`
	// AutoLoadSkills pulls this agent's skills into the child context.
	AutoLoadSkills bool `yaml:"autoloadSkills,omitempty"`
	// ReadSummarize: false gives children verbatim reads, no structural
	// summarization.
	ReadSummarize *bool `yaml:"readSummarize,omitempty"`
	// Prewalk arms the model-handoff for this agent type.
	Prewalk     *bool  `yaml:"prewalk,omitempty"`
	PrewalkInto string `yaml:"prewalkInto,omitempty"`

	path string // absolute source file (for diagnostics)
}

// stringList accepts a CSV string or a YAML list for tool/spawn fields.
type stringList []string

func (l *stringList) UnmarshalYAML(value *yaml.Node) error {
	var single string
	if err := value.Decode(&single); err == nil {
		*l = splitCSV(single)
		return nil
	}
	var list []string
	if err := value.Decode(&list); err != nil {
		return err
	}
	*l = list
	return nil
}

// userAgentsDir is ~/.xdev/agent/agents — a var so tests can swap it.
var userAgentsDir = func() string { return filepath.Join(config.DataDir(), "agents") }

// DiscoverAgents finds task-agent definitions for cwd. Precedence (first-
// wins by exact case-sensitive name): 1) project .xdev/agents, 2) user
// ~/.xdev/agent/agents, 3) bundled defaults. Missing name/description makes
// a file invalid (skipped with a warning, never fatal).
func DiscoverAgents(cwd string) ([]AgentDefinition, []string) {
	byName := map[string]AgentDefinition{}
	var warnings []string
	roots := []string{filepath.Join(cwd, ".xdev", "agents")}
	if dir := userAgentsDir(); dir != "" {
		roots = append(roots, dir)
	}
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue // missing root: not an error
		}
		for _, e := range entries {
			file := e.Name()
			if e.IsDir() || strings.HasPrefix(file, ".") || !strings.HasSuffix(file, ".md") {
				continue
			}
			def, err := parseAgentFile(filepath.Join(root, file))
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("agent %s: %v", file, err))
				continue // one bad file never aborts discovery
			}
			if _, seen := byName[def.Name]; seen {
				continue // earlier root wins
			}
			byName[def.Name] = def
		}
	}
	out := make([]AgentDefinition, 0, len(byName))
	for _, d := range byName {
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b AgentDefinition) int { return strings.Compare(a.Name, b.Name) })
	return out, warnings
}

// parseAgentFile reads one agent markdown file.
func parseAgentFile(path string) (AgentDefinition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return AgentDefinition{}, err
	}
	raw := string(data)
	def := AgentDefinition{path: path}
	if strings.HasPrefix(strings.TrimSpace(raw), "---") {
		end := strings.Index(raw[3:], "\n---")
		if end < 0 {
			return AgentDefinition{}, fmt.Errorf("unterminated frontmatter")
		}
		fm := raw[3 : 3+end]
		def.SystemPrompt = strings.TrimSpace(raw[3+end+4:])
		if err := yaml.Unmarshal([]byte(fm), &def); err != nil {
			return AgentDefinition{}, fmt.Errorf("frontmatter: %w", err)
		}
	} else {
		def.SystemPrompt = strings.TrimSpace(raw)
	}
	def.Name = strings.TrimSpace(def.Name)
	if def.Name == "" {
		def.Name = strings.TrimSuffix(filepath.Base(path), ".md")
	}
	if def.Description == "" {
		return AgentDefinition{}, fmt.Errorf("missing description (required)")
	}
	// yield is always added to the child tool set.
	if !slices.Contains(def.Tools, "yield") {
		def.Tools = append(def.Tools, "yield")
	}
	def.Tools = slices.Compact(def.Tools)
	return def, nil
}

// SpawnPolicy resolves the agent's spawns field to an allowlist.
type SpawnPolicy struct {
	AllowAll bool     // absent or "*": anything goes
	Allow    []string // CSV/list: only these
	None     bool     // false or []: nothing
}

// ResolveSpawnPolicy normalizes the spawns value (research §1: `*` / false
// / CSV): absent = unrestricted (the parent default), "*" = all,
// false or an empty list = none, CSV/array = the allowlist.
func (d AgentDefinition) ResolveSpawnPolicy() SpawnPolicy {
	switch v := d.Spawns.(type) {
	case nil:
		return SpawnPolicy{AllowAll: true}
	case string:
		if v == "*" {
			return SpawnPolicy{AllowAll: true}
		}
		return SpawnPolicy{Allow: splitCSV(v)}
	case bool:
		if v {
			return SpawnPolicy{AllowAll: true}
		}
		return SpawnPolicy{None: true}
	case []any:
		if len(v) == 0 {
			return SpawnPolicy{None: true}
		}
		var allow []string
		for _, item := range v {
			if s, ok := item.(string); ok {
				if s == "*" {
					return SpawnPolicy{AllowAll: true}
				}
				allow = append(allow, s)
			}
		}
		return SpawnPolicy{Allow: allow}
	}
	return SpawnPolicy{AllowAll: true}
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// FindAgent looks up one agent definition by exact name from a discovered set.
func FindAgent(defs []AgentDefinition, name string) (AgentDefinition, bool) {
	for _, d := range defs {
		if d.Name == name {
			return d, true
		}
	}
	return AgentDefinition{}, false
}
