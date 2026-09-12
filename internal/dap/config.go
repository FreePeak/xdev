package dap

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/config"
)

// DefaultTimeout bounds one DAP request — and, for the stepping ops, one
// post-continue stop wait.
const DefaultTimeout = 30 * time.Second

// AdapterSpec describes one debug adapter command. The dap-side twin of
// config.DebugAdapter, converted by ConfigFromSettings.
type AdapterSpec struct {
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
	// Languages are the languages this adapter serves. They pick an adapter
	// from the debugged file's extension when none is named.
	Languages []string `yaml:"languages"`
	// Socket marks an adapter that answers DAP on a TCP port instead of
	// stdio: xdev listens on a loopback port and the adapter dials back
	// (`--client-addr`, dlv's mode). The port is picked by xdev, so no
	// "listening at" banner has to be parsed.
	Socket bool `yaml:"socket"`
}

// DefaultAdapters are the built-in adapters, all opt-in-lazy: an adapter
// starts only when the debug tool first launches or attaches, and only if its
// binary is on PATH. A user entry in debug.adapters overrides any field.
func DefaultAdapters() map[string]AdapterSpec {
	return map[string]AdapterSpec{
		"dlv":      {Command: "dlv", Args: []string{"dap"}, Languages: []string{"go"}, Socket: true},
		"debugpy":  {Command: "python", Args: []string{"-m", "debugpy.adapter"}, Languages: []string{"python"}},
		"lldb-dap": {Command: "lldb-dap", Languages: []string{"c", "cpp", "rust"}},
	}
}

// Config is the resolved debug configuration.
type Config struct {
	Adapters map[string]AdapterSpec
	Timeout  time.Duration
}

// ConfigFromSettings layers user settings under DefaultAdapters: an entry for
// a known adapter merges onto the default (override the command, keep the
// languages); an unknown key adds an adapter. A malformed timeout keeps the
// default — the settings loader already reports it.
func ConfigFromSettings(s *config.Settings) Config {
	cfg := Config{Adapters: DefaultAdapters(), Timeout: DefaultTimeout}
	if s == nil || s.Debug == nil {
		return cfg
	}
	if d, err := time.ParseDuration(s.Debug.Timeout); err == nil && d > 0 {
		cfg.Timeout = d
	}
	for name, a := range s.Debug.Adapters {
		over := AdapterSpec{Command: a.Command, Args: a.Args, Languages: a.Languages, Socket: a.Socket}
		if base, ok := cfg.Adapters[name]; ok {
			cfg.Adapters[name] = mergeAdapter(base, over)
			continue
		}
		cfg.Adapters[name] = over
	}
	return cfg
}

// mergeAdapter lets a user entry override a built-in's command/args while
// keeping its languages (the same rule as lsp.servers). Socket is sticky: a
// layer can turn it on for its own adapter but a bool has no "unset", so it
// cannot turn a built-in's socket transport back off.
func mergeAdapter(base, over AdapterSpec) AdapterSpec {
	if over.Command != "" {
		base.Command = over.Command
	}
	if over.Args != nil {
		base.Args = over.Args
	}
	if len(over.Languages) > 0 {
		base.Languages = over.Languages
	}
	base.Socket = base.Socket || over.Socket
	return base
}

// Names lists the configured adapters, sorted for deterministic output.
func (c Config) Names() []string {
	names := make([]string, 0, len(c.Adapters))
	for n := range c.Adapters {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// list renders the configured adapters for an error message.
func (c Config) list() string {
	names := c.Names()
	if len(names) == 0 {
		return "none — set debug.adapters"
	}
	return strings.Join(names, ", ")
}

// AdapterFor resolves the adapter for a request: the named one, else the
// adapter whose languages claim file's extension, else — with exactly one
// adapter configured — that one.
func (c Config) AdapterFor(name, file string) (string, AdapterSpec, error) {
	if name != "" {
		spec, ok := c.Adapters[name]
		if !ok {
			return "", AdapterSpec{}, fmt.Errorf("debug: unknown adapter %q (configured: %s)", name, c.list())
		}
		return name, spec, nil
	}
	if lang := languageOf(file); lang != "" {
		for _, n := range c.Names() {
			if contains(c.Adapters[n].Languages, lang) {
				return n, c.Adapters[n], nil
			}
		}
	}
	if len(c.Adapters) == 1 {
		n := c.Names()[0]
		return n, c.Adapters[n], nil
	}
	return "", AdapterSpec{}, fmt.Errorf("debug: cannot pick an adapter for %q — name one of %s", file, c.list())
}

// extLanguage maps a source extension to the language an adapter declares.
var extLanguage = map[string]string{
	".go": "go", ".py": "python",
	".c": "c", ".h": "c",
	".cc": "cpp", ".cpp": "cpp", ".cxx": "cpp", ".hpp": "cpp",
	".rs": "rust",
}

func languageOf(file string) string {
	if file == "" {
		return ""
	}
	return extLanguage[strings.ToLower(filepath.Ext(file))]
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
