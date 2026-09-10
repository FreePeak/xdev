package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// Env framework (M9 #10, parity-session-ux §11): a dotenv chain — process
// env → project .env → agent ~/.xdev/agent/.env — where each layer fills
// only keys still unset. An already-exported variable always wins, because
// that is the user making an explicit choice for this shell.

// LoadEnv walks the dotenv chain from the current directory up to the
// filesystem root (project layers nearest first), then applies the agent
// file last, and returns the keys it actually set. It never overwrites an
// existing environment value.
func LoadEnv(cwd string) (applied []string) {
	for _, dir := range projectEnvChain(cwd) {
		applied = append(applied, applyDotEnv(filepath.Join(dir, ".env"))...)
	}
	applied = append(applied, applyDotEnv(filepath.Join(DataDir(), ".env"))...)
	return applied
}

// projectEnvChain collects .env directories from cwd upward, stopping at
// the filesystem root or a directory that looks like a repo boundary
// (contains .git), so a stray ~/.env cannot leak into an unrelated tree.
func projectEnvChain(cwd string) []string {
	var dirs []string
	cur := cwd
	for i := 0; i < 32; i++ { // bounded walk: a symlink cycle must not loop
		if cur == "" || cur == "/" || cur == "." {
			break
		}
		dirs = append(dirs, cur)
		if isDir(filepath.Join(cur, ".git")) {
			break // repo boundary
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	// Nearest-first: the deepest (most specific) .env sets a key first and
	// every outer layer can only fill gaps.
	return dirs
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// applyDotEnv reads one dotenv file and sets only variables that are not
// already present in the environment. Malformed lines are skipped; a whole
// unreadable file is not an error (env layering is best-effort by design).
func applyDotEnv(path string) (applied []string) {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		key, value, ok := parseDotEnvLine(sc.Text())
		if !ok {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue // process env wins, per the layering contract
		}
		if err := os.Setenv(key, value); err != nil {
			continue
		}
		applied = append(applied, key)
	}
	return applied
}

// parseDotEnvLine handles `KEY=value`, optional `export `, `#` comments,
// blank lines, single/double quoting, and inline comments outside quotes.
func parseDotEnvLine(line string) (key, value string, ok bool) {
	s := strings.TrimSpace(line)
	if s == "" || strings.HasPrefix(s, "#") {
		return "", "", false
	}
	s = strings.TrimPrefix(s, "export ")
	k, v, found := strings.Cut(s, "=")
	if !found {
		return "", "", false
	}
	key = strings.TrimSpace(k)
	if key == "" || !validEnvKey(key) {
		return "", "", false
	}
	v = strings.TrimSpace(v)
	switch {
	case len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'':
		value = v[1 : len(v)-1] // single quotes: literal, no inline comment
	case len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"':
		value = unescapeDouble(v[1 : len(v)-1])
	default:
		if i := strings.Index(v, " #"); i >= 0 {
			v = v[:i]
		}
		value = strings.TrimSpace(v)
	}
	return key, value, true
}

func validEnvKey(key string) bool {
	for i, r := range key {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// unescapeDouble expands the escapes dotenv writers emit inside double
// quotes. Unquoted and single-quoted values stay literal, matching the
// mainstream dotenv contract (python-dotenv, dotenv-cli).
func unescapeDouble(v string) string {
	r := strings.NewReplacer(`\n`, "\n", `\t`, "\t", `\"`, `"`, `\\`, `\`)
	return r.Replace(v)
}

// ProxyURL resolves the proxy override chain: PI_PROXY_* (omp compat) →
// HTTPS_PROXY/HTTP_PROXY/ALL_PROXY, first non-empty wins.
func ProxyURL(kind string) string {
	for _, key := range []string{"PI_PROXY_" + strings.ToUpper(kind), strings.ToUpper(kind) + "_PROXY"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return ""
}
