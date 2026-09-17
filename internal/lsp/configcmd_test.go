package lsp

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/config"
)

// fakeBin drops an executable stub on PATH so the CLI tests do not depend on
// which language servers this machine happens to have.
func fakeBin(t *testing.T, dir, name string) {
	t.Helper()
	path := filepath.Join(dir, name)
	writeFile(t, path, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestConfigCommandListResolvesBinaries(t *testing.T) {
	dir := t.TempDir()
	fakeBin(t, dir, "gopls")
	t.Setenv("PATH", dir)

	var out, errb bytes.Buffer
	if code := ConfigCommand([]string{"list"}, &out, &errb, nil); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errb.String())
	}
	text := out.String()
	if !strings.Contains(text, "go") || !strings.Contains(text, filepath.Join(dir, "gopls")) {
		t.Fatalf("list output missing the resolved go binary:\n%s", text)
	}
	if !strings.Contains(text, "lazy (start on first use)") || !strings.Contains(text, "idle timeout 5m0s") {
		t.Fatalf("list output missing the lazy/idle summary:\n%s", text)
	}
	if !strings.Contains(text, "not found on PATH") {
		t.Fatalf("list output should flag the servers that are absent:\n%s", text)
	}
}

func TestConfigCommandValidate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)

	var out, errb bytes.Buffer
	if code := ConfigCommand([]string{"validate"}, &out, &errb, nil); code != 1 {
		t.Fatalf("exit = %d, want 1 when every binary is missing", code)
	}
	if !strings.Contains(errb.String(), "lsp.servers.<language>.command") {
		t.Fatalf("stderr should name the fix, got %q", errb.String())
	}

	for _, b := range []string{"gopls", "rust-analyzer", "typescript-language-server", "pyright-langserver"} {
		fakeBin(t, dir, b)
	}
	out.Reset()
	errb.Reset()
	if code := ConfigCommand([]string{"validate"}, &out, &errb, nil); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "ok: 4 server(s)") {
		t.Fatalf("validate output = %q", out.String())
	}
}

func TestConfigCommandValidateSkipsDisabled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	s := &config.Settings{LSP: &config.LSPConfig{
		Servers: map[string]config.LSPServer{
			"go":         {Disabled: true},
			"rust":       {Disabled: true},
			"typescript": {Disabled: true},
			"python":     {Disabled: true},
		},
	}}
	var out, errb bytes.Buffer
	if code := ConfigCommand([]string{"validate"}, &out, &errb, s); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "disabled") {
		t.Fatalf("output = %q", out.String())
	}
}

// TestConfigCommandReportsBadIdleTimeout: a malformed duration falls back to
// the default in the session, but validate must flag it.
func TestConfigCommandReportsBadIdleTimeout(t *testing.T) {
	s := &config.Settings{LSP: &config.LSPConfig{IdleTimeout: "soon"}}
	var out, errb bytes.Buffer
	if code := ConfigCommand([]string{"validate"}, &out, &errb, s); code != 1 {
		t.Fatalf("exit = %d, want 1 for a malformed duration", code)
	}
	if !strings.Contains(errb.String(), `lsp.idleTimeout "soon"`) {
		t.Fatalf("stderr = %q", errb.String())
	}
}

func TestConfigCommandRejectsUnknownSubcommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := ConfigCommand([]string{"frobnicate"}, &out, &errb, nil); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "want list|validate") {
		t.Fatalf("stderr = %q", errb.String())
	}
}
