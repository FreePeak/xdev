package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/FreePeak/xdev/internal/logx"
)

// WATCHDOG.md/yml discovery (M11 #39, omp advisor-watchdog): project-level
// guidance and a named-advisor roster, both found by walking cwd upward.

// watchdogGuidanceMax bounds the WATCHDOG.md content in the advisor system
// prompt (a runaway file must not eat the reviewer's context).
const watchdogGuidanceMax = 8 << 10

// DiscoverWatchdogGuidance walks from cwd up to the repository root (or the
// home directory when no repo root is found) and returns the contents of
// every readable WATCHDOG.md, farther ancestors first so narrower
// (leaf-most) guidance lands last and stays most prominent. User-level
// <agentDir>/WATCHDOG.md leads. The assembled guidance is bounded to 8KB;
// unreadable files are skipped.
func DiscoverWatchdogGuidance(cwd, agentDir string) string {
	var blocks []string
	add := func(path string) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return // absent or unreadable: skip, never fail the run
		}
		blocks = append(blocks, string(raw))
	}
	if agentDir != "" {
		add(filepath.Join(agentDir, "WATCHDOG.md"))
	}
	for _, dir := range watchdogWalkDirs(cwd) {
		add(filepath.Join(dir, "WATCHDOG.md"))
	}
	return clipWatchdog(strings.Join(blocks, "\n\n"))
}

// clipWatchdog bounds the assembled guidance — the reviewer's prompt must
// not grow with the number of discovered files.
func clipWatchdog(s string) string {
	if len(s) <= watchdogGuidanceMax {
		return s
	}
	logx.Errorf("advisor: guidance exceeds %d bytes — truncated", watchdogGuidanceMax)
	return s[:watchdogGuidanceMax]
}

// AdvisorRosterEntry is one named advisor from WATCHDOG.yml: a model plus
// the constraint patterns that gate its Feed fan-out.
type AdvisorRosterEntry struct {
	Name     string   `yaml:"name"`
	Model    string   `yaml:"model"`
	Patterns []string `yaml:"patterns"`
}

// watchdogRosterFile is the on-disk shape of WATCHDOG.yml.
type watchdogRosterFile struct {
	Instructions string               `yaml:"instructions"`
	Advisors     []AdvisorRosterEntry `yaml:"advisors"`
}

// DiscoverWatchdogRoster finds every WATCHDOG.yml/yaml on the same walk as
// WATCHDOG.md and returns the roster entries plus the concatenated shared
// instructions. Entries with neither model nor patterns are dropped, as are
// files that fail to parse (logged, skipped — one bad file must not take
// the whole roster down).
func DiscoverWatchdogRoster(cwd, agentDir string) (entries []AdvisorRosterEntry, instructions string) {
	var files []watchdogRosterFile
	read := func(path string) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return
		}
		var r watchdogRosterFile
		if err := yaml.Unmarshal(raw, &r); err != nil {
			logx.Errorf("advisor: %s: skipping unparseable WATCHDOG.yml: %v", path, err)
			return
		}
		files = append(files, r)
	}
	if agentDir != "" {
		read(filepath.Join(agentDir, "WATCHDOG.yml"))
		read(filepath.Join(agentDir, "WATCHDOG.yaml"))
	}
	for _, dir := range watchdogWalkDirs(cwd) {
		read(filepath.Join(dir, "WATCHDOG.yml"))
		read(filepath.Join(dir, "WATCHDOG.yaml"))
	}
	for _, f := range files {
		if f.Instructions != "" {
			if instructions != "" {
				instructions += "\n\n"
			}
			instructions += f.Instructions
		}
		for _, e := range f.Advisors {
			if e.Name == "" || (e.Model == "" && len(e.Patterns) == 0) {
				continue
			}
			entries = append(entries, e)
		}
	}
	return entries, instructions
}

// watchdogWalkDirs lists cwd's ancestor dirs up to and including the
// repository root, or the home directory when no repo root exists — ordered
// from the farthest ancestor down to cwd, so callers can append and get
// "leaf-most last" prominence for free.
func watchdogWalkDirs(cwd string) []string {
	if cwd == "" {
		return nil
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return nil
	}
	stop := homeStopDir()
	var chain []string
	for dir := abs; ; {
		chain = append(chain, dir)
		parent := filepath.Dir(dir)
		if dir == stop || parent == dir || dir == string(filepath.Separator) || dirHasGit(dir) {
			break
		}
		dir = parent
	}
	slices.Reverse(chain) // cwd-first becomes ancestors-first
	return chain
}

// homeStopDir is the home-directory walk boundary ("" when unknown).
func homeStopDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// dirHasGit reports a repository root marker: .git is a directory in a
// normal checkout and a file in a linked worktree, so both count.
func dirHasGit(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// matchesPatterns reports whether text hits at least one of the advisor's
// constraint patterns. A pattern matches when it compiles as a regex and
// hits, or when it appears literally (case-insensitively) — so "sql" routes
// both "run the sql migration" and "SQL migration", while an odd string like
// "[unclosed" still routes on its literal text. No patterns = match
// everything (the default advisor has no specialization).
func matchesPatterns(patterns []string, text string) bool {
	if len(patterns) == 0 {
		return true
	}
	lower := strings.ToLower(text)
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if re, err := regexp.Compile(p); err == nil && re.MatchString(text) {
			return true
		}
		if strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// String renders a roster entry compactly (log/UI aid).
func (e AdvisorRosterEntry) String() string {
	if len(e.Patterns) == 0 {
		return fmt.Sprintf("%s (%s)", e.Name, e.Model)
	}
	return fmt.Sprintf("%s (%s, %s)", e.Name, e.Model, strings.Join(e.Patterns, ","))
}
