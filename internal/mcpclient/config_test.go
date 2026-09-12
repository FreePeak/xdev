package mcpclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFile creates path (and its parents) with body.
func writeFile(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestImportMergeOrder pins the merge ladder (M13 #57): a file's own
// explicit entries outrank what it imports, imports win in listed order,
// the first definition of a duplicate name wins, and both a relative and a
// ~-rooted import resolve.
func TestImportMergeOrder(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir) // `~/…` imports land in the temp home

	writeFile(t, filepath.Join(dir, "mcp.d", "one.yml"), `
servers:
  fromBase: {command: /one}
  fromOne: {command: /one}
imports: [nested.yml]
`)
	writeFile(t, filepath.Join(dir, "mcp.d", "nested.yml"), `
servers:
  fromNested: {command: /nested}
  fromOne: {command: /nested-loses}
`)
	writeFile(t, filepath.Join(dir, "mcp.d", "two.yml"), `
servers:
  fromTwo: {command: /two}
  fromNested: {command: /two-loses}
`)
	base := writeFile(t, filepath.Join(dir, "mcp.yml"), `
imports:
  - mcp.d/one.yml
  - ~/mcp.d/two.yml
servers:
  fromBase: {command: /base}
`)
	cfg, err := LoadConfigIn(base, filepath.Join(dir, "agent"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"fromBase":   "/base", // the explicit entry beats the import's copy
		"fromOne":    "/one",  // first import wins over nested and two
		"fromNested": "/nested",
		"fromTwo":    "/two",
	}
	if len(cfg.Servers) != len(want) {
		t.Fatalf("servers = %d (%v), want %d", len(cfg.Servers), cfg.Servers, len(want))
	}
	for name, command := range want {
		sc, ok := cfg.Servers[name]
		if !ok {
			t.Errorf("server %q missing", name)
			continue
		}
		if sc.Command != command {
			t.Errorf("server %q command = %q, want %q", name, sc.Command, command)
		}
	}
	if got := cfg.Servers["fromNested"].Source; !strings.HasSuffix(got, "nested.yml") {
		t.Errorf("fromNested source = %q, want the importing file", got)
	}
	if got := cfg.Servers["fromBase"].Source; got != base {
		t.Errorf("fromBase source = %q, want %q", got, base)
	}
}

// TestImportCycleIsGuarded is a regression test for the merge's termination:
// without the visited set this load never returns.
func TestImportCycleIsGuarded(t *testing.T) {
	dir := t.TempDir()
	a := writeFile(t, filepath.Join(dir, "a.yml"), "imports: [b.yml]\nservers:\n  a: {command: /a}\n")
	writeFile(t, filepath.Join(dir, "b.yml"), "imports: [a.yml]\nservers:\n  b: {command: /b}\n")

	cfg, err := LoadConfigIn(a, filepath.Join(dir, "agent"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 2 {
		t.Fatalf("servers = %v, want both cycle members", cfg.Servers)
	}
}

// TestMissingImportFailsClosed: an import the user named explicitly is a
// hard error — silently dropping servers is worse than refusing to start.
func TestMissingImportFailsClosed(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, filepath.Join(dir, "mcp.yml"), "imports: [ghost.yml]\n")
	if _, err := LoadConfigIn(p, dir); err == nil || !strings.Contains(err.Error(), "ghost.yml") {
		t.Fatalf("err = %v, want a missing-import error naming the file", err)
	}
}

// TestCommandSubstitution pins the env/header value forms: `!command`
// stdout, the env-name indirection, and the literal fallback.
func TestCommandSubstitution(t *testing.T) {
	t.Setenv("XDEV_TEST_TOKEN", "from-env")
	cases := []struct {
		name, in, want, wantErr string
	}{
		{name: "literal", in: "Bearer abc", want: "Bearer abc"},
		{name: "env indirection", in: "XDEV_TEST_TOKEN", want: "from-env"},
		{name: "unset env stays literal", in: "XDEV_TEST_TOKEN_UNSET", want: "XDEV_TEST_TOKEN_UNSET"},
		{name: "command", in: "!printf s3cret", want: "s3cret"},
		{name: "command failure", in: "!exit 3", wantErr: "failed"},
		{name: "command with no output", in: "!true", wantErr: "no output"},
		{name: "empty command", in: "!", wantErr: "empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveValue(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveValue(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("resolveValue(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCommandTimeoutFailsClosed: a hanging secret helper must not stall
// startup, and the server must not connect with an empty credential.
func TestCommandTimeoutFailsClosed(t *testing.T) {
	old := commandTimeout
	commandTimeout = 200 * time.Millisecond
	defer func() { commandTimeout = old }()

	start := time.Now()
	// A busy shell loop, so the killed process has no child holding stdout.
	_, err := resolveValue("!while :; do :; done")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout not honored: %s", elapsed)
	}
}

// TestSecretsResolveInConfig: the resolved values land on the server entry
// that the client builder consumes.
func TestSecretsResolveInConfig(t *testing.T) {
	t.Setenv("XDEV_TEST_TOKEN", "env-secret")
	dir := t.TempDir()
	p := writeFile(t, filepath.Join(dir, "mcp.yml"), `
servers:
  s:
    command: /bin/server
    env:
      TOKEN: "!printf 'tok-%s' abc"
      COPY: "XDEV_TEST_TOKEN"
    headers:
      Authorization: "Bearer ${XDEV_TEST_TOKEN}"
`)
	cfg, err := LoadConfigIn(p, dir)
	if err != nil {
		t.Fatal(err)
	}
	sc := cfg.Servers["s"]
	if got := sc.Env["TOKEN"]; got != "tok-abc" {
		t.Errorf("env TOKEN = %q", got)
	}
	if got := sc.Env["COPY"]; got != "env-secret" {
		t.Errorf("env COPY = %q", got)
	}
	if got := sc.Headers["Authorization"]; got != "Bearer env-secret" {
		t.Errorf("header = %q", got)
	}
}

// TestShadowedServerSecretNeverRuns: a duplicate entry is dropped before
// its values resolve, so a losing config cannot execute shell commands.
func TestShadowedServerSecretNeverRuns(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	base := writeFile(t, filepath.Join(dir, "mcp.yml"), `
imports: [imported.yml]
servers:
  s: {command: /bin/true}
`)
	writeFile(t, filepath.Join(dir, "imported.yml"), `
servers:
  s:
    command: /bin/true
    env:
      TOKEN: "!touch `+marker+`; printf t"
`)
	if _, err := LoadConfigIn(base, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("a shadowed server's !command ran")
	}
}

// TestPerServerOverrides pins per-server {enabled, timeout, cwd} and the
// top-level enable/disable lists, including their precedence.
func TestPerServerOverrides(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, filepath.Join(dir, "mcp.yml"), `
servers:
  off:
    command: /bin/true
    enabled: false
  forced:
    command: /bin/true
    enabled: false
  hidden:
    command: /bin/true
  tuned:
    command: /bin/true
    cwd: /tmp
    timeout: 1500
  noDeadline:
    command: /bin/true
    timeout: 0
disabledServers: [hidden]
enabledServers: [forced, hidden]
`)
	cfg, err := LoadConfigIn(p, dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Servers["off"].IsEnabled() {
		t.Error("enabled: false must not connect")
	}
	if !cfg.Servers["forced"].IsEnabled() {
		t.Error("enabledServers must force-enable an entry whose source said enabled: false")
	}
	if cfg.Servers["hidden"].IsEnabled() {
		t.Error("disabledServers must win over enabledServers")
	}
	if got := cfg.Servers["tuned"].requestTimeout(); got != 1500*time.Millisecond {
		t.Errorf("timeout = %s, want 1.5s", got)
	}
	if cfg.Servers["tuned"].Cwd != "/tmp" {
		t.Errorf("cwd = %q", cfg.Servers["tuned"].Cwd)
	}
	if got := cfg.Servers["noDeadline"].requestTimeout(); got != 0 {
		t.Errorf("timeout: 0 must disable the deadline, got %s", got)
	}
	if got := cfg.Servers["off"].requestTimeout(); got != mcpToolTimeout {
		t.Errorf("absent timeout = %s, want the default %s", got, mcpToolTimeout)
	}
}

// TestExpandVars pins ${VAR} / ${VAR:-default} expansion, where an
// unresolved placeholder stays literal.
func TestExpandVars(t *testing.T) {
	t.Setenv("XDEV_TEST_VAR", "v")
	cases := []struct{ in, want string }{
		{"no placeholders", "no placeholders"},
		{"${XDEV_TEST_VAR}", "v"},
		{"Bearer ${XDEV_TEST_VAR}!", "Bearer v!"},
		{"${XDEV_TEST_MISSING:-fallback}", "fallback"},
		{"${XDEV_TEST_MISSING}", "${XDEV_TEST_MISSING}"},
		{"${}", "${}"},
		{"${XDEV_TEST_VAR_UNTERMINATED", "${XDEV_TEST_VAR_UNTERMINATED"},
	}
	for _, tc := range cases {
		if got := expandVars(tc.in); got != tc.want {
			t.Errorf("expandVars(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestLoadConfigHonorsAgentDir: the production entry point takes its
// extension root from the agent data dir (XDEV_AGENT_DIR relocates it).
func TestLoadConfigHonorsAgentDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", filepath.Join(dir, "agent"))
	writeFile(t, filepath.Join(dir, "agent", "extensions", "e1", "gemini-extension.json"),
		`{"mcpServers": {"ext": {"command": "/from-ext"}}}`)
	p := writeFile(t, filepath.Join(dir, "mcp.yml"), "servers: {}\n")

	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Servers["ext"] == nil {
		t.Fatalf("servers = %v, want the extension's server", cfg.Servers)
	}
}
