// mcp.yml loading (M13 #57): mcp.go owns the live sessions; this file owns
// the config graph — imports, per-server overrides, and secret resolution.
package mcpclient

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/logx"
)

// maxImportDepth bounds the import graph. The visited set already makes a
// cycle finite; the depth cap makes a pathological diamond chain fail
// loudly instead of loading hundreds of files.
const maxImportDepth = 16

// commandTimeout bounds one `!command` secret substitution. A secret helper
// (1Password, keychain) that hangs must not stall startup.
var commandTimeout = 10 * time.Second

// secretCache memoizes successful `!command` results for the process: a
// secret helper is slow, and MCP config is loaded once per mode.
//
// ponytail: no TTL — a rotated secret needs a process restart. Upgrade path:
// expire entries on a timer, or clear the map from a `/mcp reload` hook.
var secretCache sync.Map // command string → stdout string

// LoadConfig reads mcp.yml from path; a missing file is not an error (MCP
// simply stays off). Imports and the Gemini extension manifests under the
// agent data dir are merged in.
func LoadConfig(path string) (*Config, error) {
	return LoadConfigIn(path, config.DataDir())
}

// LoadConfigIn is LoadConfig with an explicit extension root (production
// passes the agent data dir; tests and sandboxes pass their own).
//
// Precedence, first definition wins: the base file's own explicit entries,
// then its imports (in listed order, depth-first), then the extension
// manifests. A shadowed duplicate is skipped with a warning, and its
// `!command` never runs.
func LoadConfigIn(path, extRoot string) (*Config, error) {
	cfg := &Config{Servers: map[string]*ServerConfig{}}
	l := &loader{cfg: cfg, visited: map[string]bool{}}
	if err := l.file(path, 0); err != nil {
		return nil, err
	}
	for _, ext := range DiscoverExtensions(extRoot) {
		for _, name := range slices.Sorted(maps.Keys(ext.MCPServers)) {
			if err := l.add(name, ext.MCPServers[name], "gemini:"+ext.Name); err != nil {
				return nil, err
			}
		}
		cfg.CommandDirs = append(cfg.CommandDirs, ext.Commands...)
		cfg.SkillDirs = append(cfg.SkillDirs, ext.Skills...)
	}
	// Last, so the lists also govern manifest-discovered servers.
	l.overrides()
	return cfg, nil
}

// loader merges the import graph into one Config.
type loader struct {
	cfg     *Config
	visited map[string]bool
}

// file loads one config file: its own explicit entries first, then its
// imports in listed order, depth-first. A cycle (or diamond) is cut by the
// visited set, so a file is never merged twice.
func (l *loader) file(path string, depth int) error {
	abs, err := expandPath(path)
	if err != nil {
		return fmt.Errorf("mcp: resolve %s: %w", path, err)
	}
	if depth > maxImportDepth {
		return fmt.Errorf("mcp: %s: import depth exceeds %d", abs, maxImportDepth)
	}
	if l.visited[abs] {
		logx.Warnf("mcp: %s already imported — skipping (cycle or diamond)", abs)
		return nil
	}
	l.visited[abs] = true
	raw, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) && depth == 0 {
			return nil // no mcp.yml at all: MCP is off, not broken
		}
		return fmt.Errorf("mcp: read %s: %w", abs, err)
	}
	var f Config
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("mcp: parse %s: %w", abs, err)
	}
	for _, name := range slices.Sorted(maps.Keys(f.Servers)) {
		if err := l.add(name, f.Servers[name], abs); err != nil {
			return err
		}
	}
	l.cfg.DisabledServers = append(l.cfg.DisabledServers, f.DisabledServers...)
	l.cfg.EnabledServers = append(l.cfg.EnabledServers, f.EnabledServers...)
	for _, imp := range f.Imports {
		p := strings.TrimSpace(imp)
		if p == "" {
			continue
		}
		// A relative import resolves against the importing file's dir.
		if !filepath.IsAbs(p) && !strings.HasPrefix(p, "~") {
			p = filepath.Join(filepath.Dir(abs), p)
		}
		if err := l.file(p, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// add inserts one server and resolves its secret values; nil entries and
// shadowed duplicates are dropped.
func (l *loader) add(name string, sc *ServerConfig, source string) error {
	if sc == nil {
		return nil
	}
	if prev, dup := l.cfg.Servers[name]; dup {
		logx.Warnf("mcp: server %q from %s ignored: already defined by %s", name, source, prev.Source)
		return nil
	}
	if err := sc.resolveSecrets(name); err != nil {
		return err
	}
	sc.Source = source
	l.cfg.Servers[name] = sc
	return nil
}

// overrides applies the top-level enable/disable lists: disabledServers
// hides a server from any source, enabledServers force-enables one whose
// source said `enabled: false` — and disabled still wins.
func (l *loader) overrides() {
	on, off := true, false
	for _, name := range l.cfg.EnabledServers {
		if sc, ok := l.cfg.Servers[name]; ok {
			sc.Enabled = &on
		}
	}
	for _, name := range l.cfg.DisabledServers {
		if sc, ok := l.cfg.Servers[name]; ok {
			sc.Enabled = &off
		}
	}
}

// resolveSecrets expands ${VAR} placeholders and resolves the entry's
// env/header values, at load time and fail-closed: a broken secret helper
// is a config error, never a silently empty credential.
func (sc *ServerConfig) resolveSecrets(server string) error {
	sc.Command = expandVars(sc.Command)
	sc.Cwd = expandVars(sc.Cwd)
	sc.URL = expandVars(sc.URL)
	for i, a := range sc.Args {
		sc.Args[i] = expandVars(a)
	}
	env, err := resolveValues(server, "env", sc.Env)
	if err != nil {
		return err
	}
	headers, err := resolveValues(server, "headers", sc.Headers)
	if err != nil {
		return err
	}
	sc.Env, sc.Headers = env, headers
	return nil
}

// resolveValues resolves one env/header map. An empty map is returned
// unchanged.
func resolveValues(server, field string, in map[string]string) (map[string]string, error) {
	if len(in) == 0 {
		return in, nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		rv, err := resolveValue(expandVars(v))
		if err != nil {
			return nil, fmt.Errorf("mcp: server %q %s %q: %w", server, field, k, err)
		}
		out[k] = rv
	}
	return out, nil
}

// resolveValue implements the documented value forms:
//
//	!cmd args        → stdout of the command (bounded, cached per process)
//	ENV_VAR_NAME     → that variable's value when set and non-empty
//	anything else    → the literal value
func resolveValue(v string) (string, error) {
	if cmd, ok := strings.CutPrefix(v, "!"); ok {
		return runSecretCommand(strings.TrimSpace(cmd))
	}
	if name, ok := envName(v); ok {
		if val := os.Getenv(name); val != "" {
			return val, nil
		}
	}
	return v, nil
}

// runSecretCommand runs one `!command` and returns its trimmed stdout.
// Failure, timeout, and empty output are errors — never an empty secret: a
// server launched with a missing credential fails later and confusingly.
func runSecretCommand(cmd string) (string, error) {
	if cmd == "" {
		return "", fmt.Errorf("!command is empty")
	}
	if v, ok := secretCache.Load(cmd); ok {
		return v.(string), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	// A helper that leaves a background child holding stdout would keep
	// Output() blocked past the kill; WaitDelay closes the pipe so the whole
	// substitution stays bounded.
	proc := exec.CommandContext(ctx, "sh", "-c", cmd)
	proc.WaitDelay = 2 * time.Second
	out, err := proc.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("!command %q timed out after %s", cmd, commandTimeout)
		}
		return "", fmt.Errorf("!command %q failed: %w", cmd, err)
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "", fmt.Errorf("!command %q produced no output", cmd)
	}
	secretCache.Store(cmd, v)
	return v, nil
}

// expandVars replaces ${NAME} and ${NAME:-default} from the environment. An
// unresolved placeholder stays literal, and a string without "${" — the
// overwhelmingly common case — is returned without allocating.
func expandVars(s string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		j := strings.Index(s[i:], "${")
		if j < 0 {
			b.WriteString(s[i:])
			break
		}
		b.WriteString(s[i : i+j])
		rest := s[i+j+2:]
		end := strings.IndexByte(rest, '}')
		if end < 0 {
			b.WriteString(s[i+j:]) // unterminated: literal
			break
		}
		if v, ok := lookupVar(rest[:end]); ok {
			b.WriteString(v)
		} else {
			b.WriteString(s[i+j : i+j+2+end+1])
		}
		i += j + 2 + end + 1
	}
	return b.String()
}

// lookupVar resolves one ${...} expression: NAME, or NAME:-default where an
// empty value counts as unset so the default still applies.
func lookupVar(expr string) (string, bool) {
	name, def, hasDefault := strings.Cut(expr, ":-")
	if name == "" {
		return "", false
	}
	if v := os.Getenv(name); v != "" {
		return v, true
	}
	if hasDefault {
		return def, true
	}
	return "", false
}

// envName reports whether v is a bare environment-variable name — the
// documented indirection that copies a value from the current shell
// ("GITHUB_TOKEN": "GITHUB_TOKEN").
func envName(v string) (string, bool) {
	if v == "" {
		return "", false
	}
	for i := range v {
		c := v[i]
		switch {
		case c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z'):
		case c >= '0' && c <= '9' && i > 0:
		default:
			return "", false
		}
	}
	return v, true
}

// expandPath resolves a leading ~, makes the path absolute, and cleans it.
func expandPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}
