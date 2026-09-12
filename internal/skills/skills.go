// Package skills implements file-backed capability packs (M12, research
// F2 CORE): `<root>/skills/<name>/SKILL.md`, discovered non-recursively,
// exposed to the model as name+description metadata with the full body
// reachable on demand through the read tool's skill:// scheme.
package skills

import (
	"fmt"
	"github.com/FreePeak/xdev/internal/marketplace"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/FreePeak/xdev/internal/config"
)

// Skill is one discovered capability pack.
type Skill struct {
	Name        string
	Description string
	// Path is the absolute SKILL.md path; Dir is its parent (the skill's
	// base directory, used for relative-path resolution by callers).
	Path string
	Dir  string
	// Body is the markdown below the frontmatter.
	Body string
	// Globs, AlwaysApply, Hide, DisableModelInvocation mirror omp's
	// frontmatter keys. Hide keeps the skill out of the prompt list but
	// reachable via skill:// and /skill:.
	Globs                  []string
	AlwaysApply            bool
	Hide                   bool
	DisableModelInvocation bool
	// Source names the discovery root kind for diagnostics: "native"
	// (project), "user", or "managed".
	Source string
}

// frontmatter is the parsed YAML header.
type frontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Globs       any    `yaml:"globs"`
	AlwaysApply bool   `yaml:"alwaysApply"`
	Hide        bool   `yaml:"hide"`
	// disableModelInvocation: reachable only by explicit request.
	DisableModelInvocation bool `yaml:"disableModelInvocation"`
}

// UserRoot is <data dir>/skills — the user-level native root.
func UserRoot() string { return filepath.Join(dataDir(), "skills") }

// ManagedRoot is <data dir>/managed-skills — agent-authored skills. They
// never override an authored pack (project/user roots), only the
// configured custom directories.
func ManagedRoot() string { return filepath.Join(dataDir(), "managed-skills") }

// dataDir is config.DataDir — the one place that resolves the native base
// (XDEV_AGENT_DIR, a named profile, the XDG record), so skills follow a
// relocated base instead of re-deriving it and reading the wrong root.
var dataDirFunc = defaultDataDir

func dataDir() string { return dataDirFunc() }

// defaultDataDir is the production default, named so a test can restore it
// after SetDataDir.
func defaultDataDir() string { return config.DataDir() }

// SetDataDir overrides the data directory (tests).
func SetDataDir(dir string) { dataDirFunc = func() string { return dir } }

// projectRoot is <cwd>/.xdev/skills.
func projectRoot(cwd string) string { return filepath.Join(cwd, ".xdev", "skills") }

// SkillRoot is one discovery root: the directory holding `<name>/SKILL.md`
// and the source label that identifies it in diagnostics.
type SkillRoot struct {
	Dir    string
	Source string
}

// customDirectories is the settings layer's extra root list
// (skills.customDirectories), installed once at startup.
var customDirectories []string

// SetCustomDirectories installs the settings-sourced extra skill roots
// (nil clears them). Entries are trimmed and empty ones dropped.
func SetCustomDirectories(dirs []string) {
	customDirectories = nil
	for _, d := range dirs {
		if d = strings.TrimSpace(d); d != "" {
			customDirectories = append(customDirectories, d)
		}
	}
}

// Roots returns the discovery roots for cwd in precedence order: project
// native, user native, managed (agent-authored), then the configured
// custom directories — an authored or learned pack always outranks a
// configured extra root. A relative custom entry resolves against cwd.
func Roots(cwd string) []SkillRoot {
	out := []SkillRoot{
		{projectRoot(cwd), "native"},
		{UserRoot(), "user"},
		{ManagedRoot(), "managed"},
	}
	for _, dir := range customDirectories {
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(cwd, dir)
		}
		out = append(out, SkillRoot{Dir: filepath.Clean(dir), Source: "custom"})
	}
	// Plugin skill roots last: lowest precedence, never shadowing (#85).
	for _, dir := range marketplace.SkillRoots() {
		out = append(out, SkillRoot{Dir: dir, Source: "plugin"})
	}
	return out
}

// Discover finds skills for cwd, first-wins by exact case-sensitive name
// across the roots in precedence order. Missing roots and
// unreadable/malformed files skip without aborting discovery.
func Discover(cwd string) []Skill {
	byName := map[string]Skill{}
	var out []Skill
	for _, root := range Roots(cwd) {
		for _, s := range scanRoot(root.Dir, root.Source) {
			if _, seen := byName[s.Name]; seen {
				continue // earlier root wins
			}
			byName[s.Name] = s
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// scanRoot reads `<dir>/<name>/SKILL.md` (non-recursive).
func scanRoot(dir, source string) []Skill {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // missing root: not an error
	}
	var out []Skill
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(dir, e.Name(), "SKILL.md")
		s, err := Parse(path)
		if err != nil {
			continue // one bad skill never aborts discovery
		}
		if s.Name == "" {
			s.Name = e.Name()
		}
		s.Source = source
		out = append(out, s)
	}
	return out
}

// Parse reads one SKILL.md. The name falls back to the directory name and
// the description is required for discovery (omp: description-less skills
// are not model-visible).
func Parse(path string) (Skill, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, err
	}
	s := Skill{Path: path, Dir: filepath.Dir(path)}
	text := string(raw)
	if strings.HasPrefix(strings.TrimSpace(text), "---") {
		end := strings.Index(text[3:], "\n---")
		if end < 0 {
			return Skill{}, fmt.Errorf("skill %s: unterminated frontmatter", path)
		}
		var fm frontmatter
		if err := yaml.Unmarshal([]byte(text[3:3+end]), &fm); err != nil {
			return Skill{}, fmt.Errorf("skill %s: frontmatter: %w", path, err)
		}
		s.Name = strings.TrimSpace(fm.Name)
		s.Description = strings.TrimSpace(fm.Description)
		s.Globs = stringList(fm.Globs)
		s.AlwaysApply = fm.AlwaysApply
		s.Hide = fm.Hide
		s.DisableModelInvocation = fm.DisableModelInvocation
		s.Body = strings.TrimSpace(text[3+end+4:])
	} else {
		s.Body = strings.TrimSpace(text)
	}
	if s.Description == "" {
		return Skill{}, fmt.Errorf("skill %s: description is required", path)
	}
	return s, nil
}

func stringList(v any) []string {
	switch t := v.(type) {
	case string:
		var out []string
		for _, p := range strings.Split(t, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	case []any:
		var out []string
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// Find looks up one skill by exact name.
func Find(list []Skill, name string) (Skill, bool) {
	for _, s := range list {
		if s.Name == name {
			return s, true
		}
	}
	return Skill{}, false
}

// Conflict is a same-named skill found outside the managed root, together
// with the side first-wins discovery favors.
type Conflict struct {
	Skill Skill
	// BeatsManaged is true for a root that outranks the managed root
	// (native, user): that pack shadows an agent-authored one. Custom
	// directories rank below managed, so their conflict is the reverse —
	// the managed pack shadows them.
	BeatsManaged bool
}

// Conflicts returns the same-named skills in the non-managed roots, in
// precedence order. The learn tool reports these instead of refusing the
// write: the authored name is kept and discovery first-wins decides which
// pack a session actually loads.
func Conflicts(cwd, name string) []Conflict {
	var out []Conflict
	for _, root := range Roots(cwd) {
		if root.Source == "managed" {
			continue
		}
		for _, s := range scanRoot(root.Dir, root.Source) {
			if s.Name == name {
				out = append(out, Conflict{Skill: s, BeatsManaged: root.Source != "custom"})
			}
		}
	}
	return out
}

// Resolve handles a skill:// URL: skill://<name> or skill://<name>/<rel>.
// Absolute paths and any traversal outside the skill's base directory are
// rejected (omp: no fallback search, explicit not-found error).
func Resolve(uri string) (string, error) {
	rest := strings.TrimPrefix(uri, "skill://")
	if rest == "" || rest == uri {
		return "", fmt.Errorf("skill: empty reference")
	}
	name, rel, _ := strings.Cut(rest, "/")
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("skill: empty name in %q", uri)
	}
	if strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("skill: invalid name %q", name)
	}
	list := Discover(mustGetwd())
	s, ok := Find(list, name)
	if !ok {
		return "", fmt.Errorf("skill: %q not found", name)
	}
	if rel == "" {
		return s.Body, nil
	}
	// Resolve the relative path strictly inside the skill dir.
	clean := filepath.Clean(filepath.Join(s.Dir, filepath.FromSlash(rel)))
	base := filepath.Clean(s.Dir) + string(filepath.Separator)
	if !strings.HasPrefix(clean, base) {
		return "", fmt.Errorf("skill: %q escapes the skill directory", rel)
	}
	raw, err := os.ReadFile(clean)
	if err != nil {
		return "", fmt.Errorf("skill: %v", err)
	}
	return string(raw), nil
}

var getwd = os.Getwd

func mustGetwd() string {
	if d, err := getwd(); err == nil {
		return d
	}
	return "."
}

// SetGetwd overrides the working-directory lookup (tests).
func SetGetwd(fn func() (string, error)) { getwd = fn }
