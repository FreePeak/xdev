// Package rules implements the rulebook pipeline (M10 #30/#31, research
// parity-session-ux §8 + omp rulebook-matching-pipeline): every rule
// source normalizes to one Rule shape, providers run in priority order
// with name-based first-wins dedupe, and the active set is reachable
// through rule://<name> and ForPath (edit/write gating).
package rules

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Provider priorities (issue #31; omp rulebook-matching-pipeline).
const (
	PriorityNative     = 100 // .omp/rules + sticky RULES.md (project + user)
	PriorityOMPPlugins = 90  // .omp/plugins/*/rules (optional root)
	PriorityAgents     = 70  // .agent/rules + .agents/rules (walk-up + user)
	PriorityCursor     = 50  // .cursor/rules
	PriorityWindsurf   = 50  // .windsurf/rules
	PriorityCline      = 40  // .clinerules (file or dir)
	PriorityGitHub     = 30  // .github/copilot-instructions.md
	PriorityBuiltin    = 1   // compiled-in defaults, shadowed by everything
)

// Rule is the canonical shape every provider normalizes to.
type Rule struct {
	Name    string
	Path    string
	Content string
	Globs   []string
	// AlwaysApply rules inject unconditionally; glob-scoped rules gate on
	// the touched path (ForPath / Shorthand).
	AlwaysApply bool
	Description string
	// Condition is the raw TTSR condition (regex form); stored verbatim,
	// consumed by the M11 TTSR bucketing (no evaluation here).
	Condition     string
	Scope         string
	Agents        []string
	InterruptMode string
	Priority      int
	// Source names the provider for diagnostics ("native", "cursor", …).
	Source string
}

// Shorthand renders a glob-scoped rule as its edit/write gating form
// (issue #31: condition globs become tool:edit()/tool:write() shorthands
// consumed at edit/write time). Rules without globs return "".
func (r Rule) Shorthand() string {
	if r.AlwaysApply || len(r.Globs) == 0 {
		return ""
	}
	return "tool:edit()/tool:write() when path matches `" + strings.Join(r.Globs, ", ") + "`"
}

// MatchesPath reports whether the rule applies to path: always-apply and
// glob-less rules always do; glob-scoped rules match the slash path, its
// base name, or a trailing "/**" directory prefix.
// ponytail: path.Match has no real "**" support — the trailing "/**"
// prefix case is handled explicitly, richer doublestar globs need a
// glob library (upgrade path: swap in a doublestar matcher here).
func (r Rule) MatchesPath(p string) bool {
	if r.AlwaysApply || len(r.Globs) == 0 {
		return true
	}
	s := path.Clean(filepath.ToSlash(p))
	base := path.Base(s)
	for _, g := range r.Globs {
		if ok, _ := path.Match(g, s); ok {
			return true
		}
		if ok, _ := path.Match(g, base); ok {
			return true
		}
		if strings.HasSuffix(g, "/**") && strings.HasPrefix(s+"/", strings.TrimSuffix(g, "/**")+"/") {
			return true
		}
	}
	return false
}

// Discover walks every enabled provider for cwd in priority order
// (native → omp-plugins → agents → cursor → windsurf → cline → github →
// builtin) and dedupes by rule name, first-wins. enabled lists provider
// names (the settings enabledProviders gate); empty means all. Ties
// within one provider resolve by discovery order (project before user,
// user RULES.md before project — omp: user shadows project).
func Discover(cwd string, enabled []string) []Rule {
	byName := map[string]Rule{}
	add := func(rs []Rule) {
		for _, r := range rs {
			if r.Name == "" {
				continue
			}
			if _, seen := byName[r.Name]; !seen {
				byName[r.Name] = r
			}
		}
	}
	for _, p := range []struct {
		name string
		fn   func() []Rule
	}{
		{"native", func() []Rule { return native(cwd) }},
		{"omp-plugins", func() []Rule { return ompPlugins(cwd) }},
		{"agents", func() []Rule { return agents(cwd) }},
		{"cursor", func() []Rule { return mdDir(filepath.Join(cwd, ".cursor", "rules"), PriorityCursor, "cursor") }},
		{"windsurf", func() []Rule { return mdDir(filepath.Join(cwd, ".windsurf", "rules"), PriorityWindsurf, "windsurf") }},
		{"cline", func() []Rule { return cline(cwd) }},
		{"github", func() []Rule {
			return single(filepath.Join(cwd, ".github", "copilot-instructions.md"), PriorityGitHub, "github")
		}},
		{"builtin", builtin},
	} {
		if providerEnabled(p.name, enabled) {
			add(p.fn())
		}
	}
	out := make([]Rule, 0, len(byName))
	for _, r := range byName {
		out = append(out, r)
	}
	// Deterministic render order: priority desc, then name.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// providerEnabled: the enabledProviders gate — empty (or "all"/"*")
// enables every provider; otherwise membership is case-insensitive.
func providerEnabled(name string, enabled []string) bool {
	if len(enabled) == 0 {
		return true
	}
	for _, e := range enabled {
		e = strings.TrimSpace(e)
		if strings.EqualFold(e, name) || e == "*" || strings.EqualFold(e, "all") {
			return true
		}
	}
	return false
}

// native discovers .omp/rules plus the sticky RULES.md files: user agent
// dir, then project .omp/RULES.md, then project-root RULES.md. All
// synthesize the rule name "RULES", forced alwaysApply (frontmatter
// cannot unstick); the user file shadows the project one via first-wins.
func native(cwd string) []Rule {
	var out []Rule
	for _, p := range []string{
		filepath.Join(dataDir(), "RULES.md"),
		filepath.Join(cwd, ".omp", "RULES.md"),
		filepath.Join(cwd, "RULES.md"),
	} {
		if r, ok := parseRule(p, PriorityNative, "native"); ok {
			r.Name = "RULES"
			r.AlwaysApply = true
			out = append(out, r)
		}
	}
	out = append(out, mdDir(filepath.Join(cwd, ".omp", "rules"), PriorityNative, "native")...)
	out = append(out, mdDir(filepath.Join(dataDir(), "rules"), PriorityNative, "native")...)
	return out
}

// ompPlugins scans <base>/<plugin>/rules/ for every plugin directory.
// Both roots are optional — a missing plugins directory is normal.
func ompPlugins(cwd string) []Rule {
	var out []Rule
	for _, base := range []string{
		filepath.Join(cwd, ".omp", "plugins"),
		filepath.Join(dataDir(), "plugins"),
	} {
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				out = append(out, mdDir(filepath.Join(base, e.Name(), "rules"), PriorityOMPPlugins, "omp-plugins")...)
			}
		}
	}
	return out
}

// agents walks cwd → filesystem root for .agent/rules and .agents/rules
// (nearest first), then the user-level counterparts.
func agents(cwd string) []Rule {
	var out []Rule
	dir, err := filepath.Abs(cwd)
	if err != nil {
		dir = cwd
	}
	for {
		out = append(out, mdDir(filepath.Join(dir, ".agent", "rules"), PriorityAgents, "agents")...)
		out = append(out, mdDir(filepath.Join(dir, ".agents", "rules"), PriorityAgents, "agents")...)
		if parent := filepath.Dir(dir); parent == dir {
			break
		} else {
			dir = parent
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, mdDir(filepath.Join(home, ".agent", "rules"), PriorityAgents, "agents")...)
		out = append(out, mdDir(filepath.Join(home, ".agents", "rules"), PriorityAgents, "agents")...)
	}
	return out
}

// cline reads .clinerules as a single file or a rules directory.
func cline(cwd string) []Rule {
	p := filepath.Join(cwd, ".clinerules")
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		return mdDir(p, PriorityCline, "cline")
	}
	return single(p, PriorityCline, "cline")
}

// mdDir parses dir/*.md and dir/*.mdc non-recursively, filename order.
// Missing directories contribute nothing.
func mdDir(dir string, priority int, source string) []Rule {
	var out []Rule
	for _, pattern := range []string{"*.md", "*.mdc"} {
		files, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			continue
		}
		sort.Strings(files)
		for _, f := range files {
			if r, ok := parseRule(f, priority, source); ok {
				out = append(out, r)
			}
		}
	}
	return out
}

// single parses one optional file.
func single(p string, priority int, source string) []Rule {
	if r, ok := parseRule(p, priority, source); ok {
		return []Rule{r}
	}
	return nil
}

// builtin returns the compiled-in defaults: priority 1, so any discovered
// rule with the same name shadows them.
func builtin() []Rule {
	return []Rule{{
		Name:        "git-hygiene",
		Priority:    PriorityBuiltin,
		Source:      "builtin",
		AlwaysApply: true,
		Description: "Built-in workflow defaults",
		Content: "- Never commit, push, or force-push unless the user explicitly asks.\n" +
			"- Never stage with `git add -A` or `git add .`; stage explicit paths.\n" +
			"- Keep diffs minimal: fix the cause, never reformat unrelated code.\n",
	}}
}

// frontmatter mirrors the md/mdc YAML header keys (cursor .mdc and native
// .md share the same shape). Unknown keys are ignored.
type frontmatter struct {
	Name          string   `yaml:"name"`
	Description   string   `yaml:"description"`
	Globs         any      `yaml:"globs"` // string (comma-separated) or list
	AlwaysApply   bool     `yaml:"alwaysApply"`
	Condition     string   `yaml:"condition"`
	Scope         string   `yaml:"scope"`
	Agents        []string `yaml:"agents"`
	InterruptMode string   `yaml:"interruptMode"`
}

// parseRule reads one rule file: name from frontmatter or filename
// (extension stripped), content from the body below the frontmatter.
func parseRule(p string, priority int, source string) (Rule, bool) {
	raw, err := os.ReadFile(p)
	if err != nil {
		return Rule{}, false
	}
	name := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
	r := Rule{Name: name, Path: p, Content: string(raw), Priority: priority, Source: source}
	head, body, ok := splitFrontmatter(string(raw))
	if ok {
		var fm frontmatter
		if err := yaml.Unmarshal([]byte(head), &fm); err == nil {
			if fm.Name != "" {
				r.Name = fm.Name
			}
			r.Content = body
			r.Description = fm.Description
			r.Globs = stringList(fm.Globs)
			r.AlwaysApply = fm.AlwaysApply
			r.Condition = fm.Condition
			r.Scope = fm.Scope
			r.Agents = fm.Agents
			r.InterruptMode = fm.InterruptMode
		}
		// A malformed header degrades to plain-content parsing (warn
		// semantics, like third-party command frontmatter).
	}
	return r, true
}

// splitFrontmatter splits a leading --- YAML block from the body.
func splitFrontmatter(s string) (head, body string, ok bool) {
	s = strings.TrimPrefix(s, "\ufeff")
	lines := strings.SplitN(s, "\n", 2)
	if len(lines) < 2 || strings.TrimRight(lines[0], "\r") != "---" {
		return "", s, false
	}
	all := strings.Split(lines[1], "\n")
	for i, line := range all {
		if strings.TrimRight(line, "\r") == "---" {
			return strings.Join(all[:i], "\n"), strings.Join(all[i+1:], "\n"), true
		}
	}
	return "", s, false
}

// stringList normalizes a frontmatter list value: a plain string is split
// on commas, a YAML list passes through, anything else is dropped.
func stringList(v any) []string {
	switch t := v.(type) {
	case string:
		var out []string
		for _, part := range strings.Split(t, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
		return out
	case []any:
		var out []string
		for _, item := range t {
			if s, ok := item.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
		}
		return out
	}
	return nil
}

// --- active set: the process-wide discovered rules, shared by rule://
// resolution and ForPath (the edit/write gating hook).

var (
	activeMu sync.RWMutex
	active   []Rule
)

// Set installs the active rule set (cmd calls once after Discover; nil
// clears it — the --no-rules path).
func Set(list []Rule) {
	activeMu.Lock()
	active = list
	activeMu.Unlock()
}

// Active returns the current rule set.
func Active() []Rule {
	activeMu.RLock()
	defer activeMu.RUnlock()
	return active
}

// ForPath returns the active rules that apply to path: always-apply and
// glob-less rules plus glob-scoped rules matching it. The edit/write
// approval path calls this to gate glob-scoped rules at tool time
// (wiring note for the integrator: internal/tool write/edit approval).
func ForPath(p string) []Rule {
	var out []Rule
	for _, r := range Active() {
		if r.MatchesPath(p) {
			out = append(out, r)
		}
	}
	return out
}

// Resolve handles a rule://<name> URI, returning the rule content.
// Unknown names fail with an explicit not-found error (404-style).
func Resolve(uri string) (string, error) {
	rest := strings.TrimPrefix(uri, "rule://")
	if rest == "" || rest == uri {
		return "", fmt.Errorf("rule: empty reference")
	}
	name := strings.TrimSpace(strings.SplitN(rest, "/", 2)[0])
	if name == "" {
		return "", fmt.Errorf("rule: empty name in %q", uri)
	}
	for _, r := range Active() {
		if r.Name == name {
			return r.Content, nil
		}
	}
	return "", fmt.Errorf("rule: %q not found", name)
}

// dataDir mirrors config.DataDir without importing it (same pattern as
// internal/skills: config does not know rules, and rules only needs the
// path).
var dataDirFunc = defaultDataDir

func dataDir() string { return dataDirFunc() }

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".xdev"
	}
	return filepath.Join(home, ".xdev", "agent")
}

// SetDataDir overrides the data directory (tests).
func SetDataDir(dir string) { dataDirFunc = func() string { return dir } }
