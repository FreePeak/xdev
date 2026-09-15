package agent

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/marketplace"
	"gopkg.in/yaml.v3"
)

// AgentDefinition is one named subagent specification, discovered from
// markdown files with YAML frontmatter (M11 #12, research §1). Every field
// here is consumed: an unsupported key in a file is reported by
// unknownFrontmatterKeys rather than parsed into a dead field (#272).
type AgentDefinition struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	// SystemPrompt is the file body below the frontmatter.
	SystemPrompt string `yaml:"-"`
	// Tools is a CSV string or YAML list of tool names; yield is always added.
	Tools stringList `yaml:"tools,omitempty"`
	// Spawns is "*" (unrestricted), a CSV allowlist, or empty (cannot spawn).
	Spawns interface{} `yaml:"spawns,omitempty"`
	// Model is a role alias (@role, expanded through modelRoles by the host's
	// TaskTool.ExpandModel) or a literal provider/model.
	Model string `yaml:"model,omitempty"`
	// ThinkingLevel sets the child's reasoning effort (a config.EffortLevels
	// name); empty inherits the parent's.
	ThinkingLevel string `yaml:"thinkingLevel,omitempty"`

	// Unsupported lists frontmatter keys the parser does not consume; the
	// caller surfaces them as warnings so a stale `output:` never reads as a
	// working setting (#272). Never a frontmatter field itself.
	Unsupported []string `yaml:"-"`

	path string // absolute source file, or "bundled/<name>.md" for an embedded def
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

// bundledAgents ships the stock task agents inside the binary (#272). Without
// this lowest-precedence root a fresh install discovers zero agents, and the
// names every model has learned from other harnesses (`scout`, `reviewer`,
// `task`, `sonic`) fail as `unknown agent (available: none)`.
//
//go:embed agents/*.md
var bundledAgents embed.FS

// bundledAgentNames lists the shipped agent files by filename. The definition
// may rename itself with frontmatter `name:`; discovery applies that.
func bundledAgentNames() []string {
	entries, err := fs.ReadDir(bundledAgents, agentsDir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			out = append(out, strings.TrimSuffix(e.Name(), ".md"))
		}
	}
	return out
}

const agentsDir = "agents"

// discoverBundledAgents parses the embedded root on its own — what the tests
// assert against, so a shipped definition that fails to parse is reported
// rather than silently missing from the stock set.
func discoverBundledAgents() ([]AgentDefinition, []string) {
	defs, warnings := discoverIn([]string{AgentRootBundled})
	if len(defs) == 0 && len(warnings) == 0 {
		warnings = append(warnings, "bundled root ships no agent definitions")
	}
	return defs, warnings
}

// readBundled parses one embedded definition. A bundled file that fails to
// parse is a build bug; it surfaces as a warning so one bad file never
// aborts discovery.
func readBundled(name string) (AgentDefinition, error) {
	data, err := bundledAgents.ReadFile(agentsDir + "/" + name + ".md")
	if err != nil {
		return AgentDefinition{}, err
	}
	return parseAgentDef(data, "bundled/"+name+".md")
}

// AgentDiscoveryRoots lists the discovery roots for cwd, in precedence
// order: project .xdev/agents, the user dir, installed plugin roots, then the
// bundled root (a marker, not a path — see AgentRootBundled). The host
// renders it in its startup notice, so "where did this definition come from"
// is answerable without reading the source.
func AgentDiscoveryRoots(cwd string) []string {
	roots := []string{filepath.Join(cwd, ".xdev", "agents")}
	if dir := userAgentsDir(); dir != "" {
		roots = append(roots, dir)
	}
	// Installed plugin agent dirs, ahead of the bundled root so a plugin can
	// replace a stock definition but never an authored one (#85).
	roots = append(roots, marketplace.AgentDirs()...)
	return append(roots, AgentRootBundled)
}

// AgentRootBundled names the embedded root in an AgentDiscoveryRoots list.
const AgentRootBundled = "bundled"

// DiscoverAgents finds task-agent definitions for cwd. Precedence is
// first-wins by exact case-sensitive name over AgentDiscoveryRoots: project,
// user, plugin, then the bundled defaults, which exist so a stock install is
// never empty (#272). A missing name/description makes a file invalid: it is
// skipped with a warning, never fatal.
func DiscoverAgents(cwd string) ([]AgentDefinition, []string) {
	return discoverIn(AgentDiscoveryRoots(cwd))
}

func discoverIn(roots []string) ([]AgentDefinition, []string) {
	byName := map[string]AgentDefinition{}
	var warnings []string
	for _, root := range roots {
		files, err := listAgentFiles(root)
		if err != nil {
			continue // missing root: not an error
		}
		for _, file := range files {
			def, err := readAgentFile(root, file)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("agent %s: %v", file, err))
				continue // one bad file never aborts discovery
			}
			for _, key := range def.Unsupported {
				warnings = append(warnings, fmt.Sprintf("agent %s: unsupported frontmatter key %q (ignored)", file, key))
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
	sortDefs(out)
	return out, warnings
}

// listAgentFiles names the markdown files a root contributes.
func listAgentFiles(root string) ([]string, error) {
	if root == AgentRootBundled {
		return bundledAgentNames(), nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// readAgentFile loads one entry of one root, from disk or from the binary.
func readAgentFile(root, file string) (AgentDefinition, error) {
	if root == AgentRootBundled {
		return readBundled(strings.TrimSuffix(file, ".md"))
	}
	path := filepath.Join(root, file)
	data, err := os.ReadFile(path)
	if err != nil {
		return AgentDefinition{}, err
	}
	return parseAgentDef(data, path)
}

func sortDefs(defs []AgentDefinition) {
	slices.SortFunc(defs, func(a, b AgentDefinition) int { return strings.Compare(a.Name, b.Name) })
}

// parseAgentDef parses one agent document. The source names the file (or the
// bundled entry) in diagnostics and supplies the name when frontmatter omits
// one. Unsupported frontmatter keys are not fatal: the definition loads and
// the caller gets the warning, so a stale key never costs an agent.
func parseAgentDef(data []byte, source string) (AgentDefinition, error) {
	raw := string(data)
	def := AgentDefinition{path: source}
	if strings.HasPrefix(strings.TrimSpace(raw), "---") {
		end := strings.Index(raw[3:], "\n---")
		if end < 0 {
			return AgentDefinition{}, fmt.Errorf("unterminated frontmatter")
		}
		fm := raw[3 : 3+end]
		def.SystemPrompt = strings.TrimSpace(raw[3+end+4:])
		quoted := quoteAtValues(fm)
		if err := yaml.Unmarshal([]byte(quoted), &def); err != nil {
			return AgentDefinition{}, fmt.Errorf("frontmatter: %w", err)
		}
		def.Unsupported = unknownFrontmatterKeys(quoted)
	} else {
		def.SystemPrompt = strings.TrimSpace(raw)
	}
	def.Name = strings.TrimSpace(def.Name)
	if def.Name == "" {
		def.Name = strings.TrimSuffix(filepath.Base(source), ".md")
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

// quoteAtValues quotes a bare "@..." scalar value, which YAML 1.2 reserves:
// `model: @smol` — the exact form discovery documents — otherwise dies as
// "found character that cannot start any token" and the agent is skipped
// (#272). Only a whole-value alias is touched; a quoted or list value is
// already valid, and a value with trailing text is left to the parser.
func quoteAtValues(fm string) string {
	return atValueRe.ReplaceAllString(fm, `$1"$2"`)
}

var atValueRe = regexp.MustCompile(`(?m)^(\s*[^:\s][^:]*:\s*)(@[^\s#]+)\s*$`)

// unknownFrontmatterKeys lists mapping keys the parser does not consume.
// Silently ignoring a field is how `output:` and `readSummarize:` advertised
// a config surface that did nothing (#272); naming them in the discovery
// warning tells the author the key is unsupported instead.
func unknownFrontmatterKeys(fm string) []string {
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(fm), &node); err != nil {
		return nil // the real decode reports the syntax error
	}
	// Unmarshalling into a Node yields a document wrapper around the mapping.
	root := &node
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil
	}
	supported := supportedFrontmatterKeys()
	var out []string
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := strings.TrimSpace(root.Content[i].Value)
		if !supported[key] {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}

// supportedFrontmatterKeys derives the key set from the yaml tags, so the
// warning list can never drift from what the parser actually reads.
func supportedFrontmatterKeys() map[string]bool {
	out := map[string]bool{}
	rt := reflect.TypeOf(AgentDefinition{})
	for i := 0; i < rt.NumField(); i++ {
		// The key is the tag before any ",omitempty" — Cut's found flag says
		// only whether a comma was there, so it cannot gate the name.
		name, _, _ := strings.Cut(rt.Field(i).Tag.Get("yaml"), ",")
		if name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
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
