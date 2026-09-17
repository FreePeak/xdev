package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeEnv(t *testing.T, dir, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, ".env")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseDotEnvLine(t *testing.T) {
	tests := []struct {
		line      string
		key, want string
		ok        bool
	}{
		{line: "FOO=bar", key: "FOO", want: "bar", ok: true},
		{line: "  export FOO=bar  ", key: "FOO", want: "bar", ok: true},
		{line: `FOO="quoted value"`, key: "FOO", want: "quoted value", ok: true},
		{line: `FOO='literal # not a comment'`, key: "FOO", want: "literal # not a comment", ok: true},
		{line: "FOO=bar # trailing comment", key: "FOO", want: "bar", ok: true},
		// Escapes expand only inside double quotes (dotenv convention):
		// unquoted stays literal, quoted decodes.
		{line: `FOO=line\nbreak`, key: "FOO", want: `line\nbreak`, ok: true},
		{line: `FOO="line\nbreak"`, key: "FOO", want: "line\nbreak", ok: true},
		{line: "# just a comment", ok: false},
		{line: "", ok: false},
		{line: "NO_EQUALS", ok: false},
		{line: "=novalue", ok: false},
		{line: "9BAD=starting digit", ok: false},
		{line: "WITH-DASH=bad", ok: false},
		{line: "ONE_WELL_2=ok", key: "ONE_WELL_2", want: "ok", ok: true},
	}
	for _, tc := range tests {
		k, v, ok := parseDotEnvLine(tc.line)
		if ok != tc.ok {
			t.Errorf("parseDotEnvLine(%q) ok = %v, want %v", tc.line, ok, tc.ok)
			continue
		}
		if ok && (k != tc.key || v != tc.want) {
			t.Errorf("parseDotEnvLine(%q) = (%q,%q), want (%q,%q)", tc.line, k, v, tc.key, tc.want)
		}
	}
}

func TestLoadEnvProcessValueWins(t *testing.T) {
	dir := t.TempDir()
	writeEnv(t, dir, "XDEV_TEST_ONLY=from-dotenv\n")
	t.Setenv("XDEV_TEST_ONLY", "from-process")
	// A key the shell already exported must never be overwritten.
	applied := LoadEnv(dir)
	for _, k := range applied {
		if k == "XDEV_TEST_ONLY" {
			t.Fatal("dotenv overwrote an existing process value")
		}
	}
	if got := os.Getenv("XDEV_TEST_ONLY"); got != "from-process" {
		t.Fatalf("process value changed: %q", got)
	}
}

func TestLoadEnvFillsUnsetKeys(t *testing.T) {
	dir := t.TempDir()
	writeEnv(t, dir, "XDEV_TEST_UNSET=from-dotenv\n")
	os.Unsetenv("XDEV_TEST_UNSET")
	t.Cleanup(func() { os.Unsetenv("XDEV_TEST_UNSET") })
	applied := LoadEnv(dir)
	found := false
	for _, k := range applied {
		if k == "XDEV_TEST_UNSET" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unset key not applied: %v", applied)
	}
	if got := os.Getenv("XDEV_TEST_UNSET"); got != "from-dotenv" {
		t.Fatalf("value = %q", got)
	}
}

func TestLoadEnvNearestLayerWins(t *testing.T) {
	repo := t.TempDir()
	nested := filepath.Join(repo, "deep", "deeper")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	writeEnv(t, repo, "XDEV_TEST_RANK=outer\n")
	writeEnv(t, nested, "XDEV_TEST_RANK=inner\n")
	os.Unsetenv("XDEV_TEST_RANK")
	t.Cleanup(func() { os.Unsetenv("XDEV_TEST_RANK") })
	// Walking up from the deepest directory: the nearest .env sets the key,
	// and outer layers can only fill gaps.
	LoadEnv(nested)
	if got := os.Getenv("XDEV_TEST_RANK"); got != "inner" {
		t.Fatalf("nearest layer lost: %q", got)
	}
}

// TestLoadEnvStopsAtRepoBoundary: without a boundary the walk would read a
// home-directory .env into every project, leaking unrelated credentials.
func TestLoadEnvStopsAtRepoBoundary(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	proj := filepath.Join(repo, "sub")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	writeEnv(t, root, "XDEV_TEST_LEAK=yes\n")  // outside the repo
	writeEnv(t, repo, "XDEV_TEST_LOCAL=yes\n") // repo root
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("XDEV_TEST_LEAK")
	os.Unsetenv("XDEV_TEST_LOCAL")
	t.Cleanup(func() {
		os.Unsetenv("XDEV_TEST_LEAK")
		os.Unsetenv("XDEV_TEST_LOCAL")
	})
	LoadEnv(proj)
	if got := os.Getenv("XDEV_TEST_LOCAL"); got != "yes" {
		t.Fatalf("repo layer not applied: %q", got)
	}
	if got := os.Getenv("XDEV_TEST_LEAK"); got != "" {
		t.Fatalf("leaked an env var from above the repo boundary: %q", got)
	}
}

func TestLoadEnvMissingFilesAreQuiet(t *testing.T) {
	// Absent files are the normal case; nothing to assert but "no panic,
	// no error", which the signature already encodes (no error return).
	got := LoadEnv(filepath.Join(t.TempDir(), "does-not-exist"))
	if got == nil {
		got = []string{}
	}
}

func TestProxyURLChain(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "")
	os.Unsetenv("PI_PROXY_HTTPS")
	if got := ProxyURL("https"); got != "" {
		t.Fatalf("empty env must give no proxy: %q", got)
	}
	t.Setenv("HTTPS_PROXY", "http://one:3128")
	if got := ProxyURL("https"); got != "http://one:3128" {
		t.Fatalf("standard var ignored: %q", got)
	}
	t.Setenv("PI_PROXY_HTTPS", "http://two:3128")
	if got := ProxyURL("https"); got != "http://two:3128" {
		t.Fatalf("PI_PROXY_* must take precedence: %q", got)
	}
}
