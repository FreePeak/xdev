package hooks

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/marketplace"
	"gopkg.in/yaml.v3"
)

// hookFile is the on-disk declaration shape:
//
//	# .xdev/hooks/guard.yml
//	name: guard                 # optional; defaults to the file stem
//	event: tool_call
//	command: ./guard.sh         # run through `sh -c`, payload on stdin
//	matcher: "^(bash|write)$"   # optional stage-2 regex
//	if: "Bash(git *)"           # optional permission-syntax prefilter
type hookFile struct {
	Name    string `yaml:"name"`
	Event   string `yaml:"event"`
	Command string `yaml:"command"`
	Matcher string `yaml:"matcher"`
	If      string `yaml:"if"`
}

// Discover finds hook declarations for cwd, mirroring agent/command
// discovery: 1) project <cwd>/.xdev/hooks, 2) user <dataDir>/hooks,
// 3) <dataDir>/extensions/<name>/hooks for each trusted extension. Names
// are first-wins in that order; an invalid file is skipped with a warning.
//
// Extension hook dirs are opt-in: an extension is third-party code, so its
// hooks load only when named by --trusted-extension (otherwise they are
// skipped with a warning, never silently run).
//
// Files: *.yml/*.yaml/*.json declare {name?, event, command, matcher?, if?}.
// A *.sh file declares itself: the event is the file stem (session_switch.sh
// → event session_switch) and the command is the file path.
func Discover(cwd string, trustedExtensions []string) ([]Hook, []string) {
	type root struct {
		dir    string
		source string
	}
	roots := []root{{filepath.Join(cwd, ".xdev", "hooks"), "project"}}
	var warns []string
	pluginDirs := marketplace.HookDirs()
	if dir := config.DataDir(); dir != "" {
		roots = append(roots, root{filepath.Join(dir, "hooks"), "user"})
		extRoot := filepath.Join(dir, "extensions")
		if entries, err := os.ReadDir(extRoot); err == nil {
			for _, e := range entries {
				if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
					continue
				}
				dir := filepath.Join(extRoot, e.Name(), "hooks")
				if !slices.Contains(trustedExtensions, e.Name()) {
					if st, err := os.Stat(dir); err == nil && st.IsDir() {
						// Warned, not loaded: trust is explicit.
						warns = append(warns, fmt.Sprintf(
							"extension hooks %s skipped: add --trusted-extension %s", dir, e.Name()))
					}
					continue
				}
				roots = append(roots, root{dir, "extension:" + e.Name()})
			}
		}
	}

	// Plugin hooks come last: roots are first-wins above, and an installed
	// plugin must never shadow a project, user or explicitly trusted hook.
	for _, dir := range pluginDirs {
		roots = append(roots, root{dir, "plugin"})
	}

	seen := map[string]bool{}
	var out []Hook
	for _, r := range roots {
		entries, err := os.ReadDir(r.dir)
		if err != nil {
			continue // missing root is not an error
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names) // deterministic order within a root
		for _, file := range names {
			h, err := parseHookFile(filepath.Join(r.dir, file), r.source)
			if err != nil {
				warns = append(warns, fmt.Sprintf("hook %s: %v", file, err))
				continue
			}
			if seen[h.Name] {
				continue // earlier root wins
			}
			seen[h.Name] = true
			out = append(out, h)
		}
	}
	return out, warns
}

// parseHookFile reads one hook declaration.
func parseHookFile(path, source string) (Hook, error) {
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".yml", ".yaml", ".json":
		data, err := os.ReadFile(path)
		if err != nil {
			return Hook{}, err
		}
		var f hookFile
		// yaml.v3 parses JSON too (JSON is a YAML subset), so one decoder
		// covers both extensions and reports where a bad file breaks.
		if err := yaml.Unmarshal(data, &f); err != nil {
			return Hook{}, err
		}
		h := Hook{
			Name:    strings.TrimSpace(f.Name),
			Event:   strings.TrimSpace(f.Event),
			Command: strings.TrimSpace(f.Command),
			Matcher: strings.TrimSpace(f.Matcher),
			If:      strings.TrimSpace(f.If),
			Source:  source,
			Path:    path,
		}
		if h.Name == "" {
			h.Name = stem
		}
		if h.Event == "" || h.Command == "" {
			return Hook{}, fmt.Errorf("missing event or command")
		}
		return h, nil
	case ".sh":
		// Self-declaring script: the file name is the event. The execute
		// bit is the contract (a non-executable hook would fail closed on
		// every tool_call), so it is warned about, not guessed around.
		if stem == "" {
			return Hook{}, fmt.Errorf("empty script name")
		}
		if st, err := os.Stat(path); err != nil || st.Mode().Perm()&0o111 == 0 {
			return Hook{}, fmt.Errorf("script is not executable")
		}
		return Hook{Name: stem, Event: stem, Command: shellQuote(path), Source: source, Path: path}, nil
	default:
		return Hook{}, fmt.Errorf("unsupported hook file %s (want .yml/.yaml/.json/.sh)", ext)
	}
}

// shellQuote single-quotes a path for `sh -c`.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
