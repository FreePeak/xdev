// Package skills implements file-backed capability packs (M12, research
// F2 CORE): `<root>/skills/<name>/SKILL.md`, discovered non-recursively,
// exposed to the model as name+description metadata with the full body
// reachable on demand through the read tool's skill:// scheme.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
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

// UserRoot is ~/.xdev/agent/skills — the user-level native root.
func UserRoot() string { return filepath.Join(dataDir(), "skills") }

// ManagedRoot is ~/.xdev/agent/managed-skills — agent-authored skills,
// dead last in precedence (never override an authored skill).
func ManagedRoot() string { return filepath.Join(dataDir(), "managed-skills") }

// dataDir mirrors config.DataDir without importing it (avoids a cycle:
// config does not know skills, and skills only needs the path).
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

// projectRoot is <cwd>/.xdev/skills.
func projectRoot(cwd string) string { return filepath.Join(cwd, ".xdev", "skills") }

// Discover finds skills for cwd, first-wins by exact case-sensitive name
// across roots in precedence order: project native, user native, managed.
// Unreadable/malformed files skip without aborting discovery.
func Discover(cwd string) []Skill {
	roots := []struct {
		dir    string
		source string
	}{
		{projectRoot(cwd), "native"},
		{UserRoot(), "user"},
		{ManagedRoot(), "managed"},
	}
	byName := map[string]Skill{}
	var out []Skill
	for _, root := range roots {
		for _, s := range scanRoot(root.dir, root.source) {
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
