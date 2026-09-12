package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/rules"
	"github.com/FreePeak/xdev/internal/tool"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRulesPromptInjectionAndNoRulesFlag(t *testing.T) {
	cwd := t.TempDir()
	write(t, filepath.Join(cwd, ".omp", "rules", "style.md"), "stay terse\n")

	t.Run("injected", func(t *testing.T) {
		noRulesFlag = false
		t.Cleanup(func() { noRulesFlag = false; rules.Set(nil) })
		got := promptFn("BASE", cwd, tool.NewRegistry(), "")()
		if !strings.Contains(got, "# Rules") || !strings.Contains(got, "## style") || !strings.Contains(got, "stay terse") {
			t.Fatalf("rules block missing from prompt:\n%s", tail(got, 400))
		}
	})

	t.Run("disabled", func(t *testing.T) {
		noRulesFlag = true
		t.Cleanup(func() { noRulesFlag = false; rules.Set(nil) })
		got := promptFn("BASE", cwd, tool.NewRegistry(), "")()
		if strings.Contains(got, "# Rules") || strings.Contains(got, "stay terse") {
			t.Fatalf("--no-rules must suppress the rules block:\n%s", tail(got, 400))
		}
	})
}

// TestEnabledProvidersGateEndToEnd wires the settings key through the
// real prompt assembly: only listed providers contribute.
func TestEnabledProvidersGateEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	write(t, filepath.Join(cwd, ".omp", "rules", "native-only.md"), "native body\n")
	write(t, filepath.Join(cwd, ".cursor", "rules", "cursor-only.mdc"), "cursor body\n")
	overlay := filepath.Join(home, "overlay.yml")
	write(t, overlay, "enabledProviders: [cursor]\n")

	s, err := config.LoadSettings(cwd, []string{overlay})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.EnabledProviders) != 1 || s.EnabledProviders[0] != "cursor" {
		t.Fatalf("enabledProviders did not survive settings layering: %v", s.EnabledProviders)
	}
	prev := loadedSettings
	loadedSettings = s
	noRulesFlag = false
	t.Cleanup(func() { loadedSettings = prev; noRulesFlag = false; rules.Set(nil) })

	got := promptFn("BASE", cwd, tool.NewRegistry(), "")()
	if strings.Contains(got, "native-only") {
		t.Fatalf("native provider must be gated off:\n%s", tail(got, 400))
	}
	if !strings.Contains(got, "cursor-only") {
		t.Fatalf("listed provider must inject:\n%s", tail(got, 400))
	}
}

func TestRuleURISchemeResolvesDiscoveredRule(t *testing.T) {
	cwd := t.TempDir()
	write(t, filepath.Join(cwd, ".omp", "rules", "known.md"), "rule body\n")
	noRulesFlag = false
	t.Cleanup(func() { noRulesFlag = false; rules.Set(nil) })
	registerURISchemes()
	// promptFnWithMemory installs the active set; the read tool's rule://
	// resolver must then serve the same rules.
	promptFn("BASE", cwd, tool.NewRegistry(), "")()

	rt := &tool.ReadTool{}
	res, err := rt.Execute(context.Background(), json.RawMessage(`{"path":"rule://known"}`))
	if err != nil {
		t.Fatalf("rule:// read failed: %v", err)
	}
	if !strings.Contains(res.Text, "rule body") {
		t.Fatalf("rule content missing: %q", res.Text)
	}

	res, err = rt.Execute(context.Background(), json.RawMessage(`{"path":"rule://missing"}`))
	if err == nil && !res.IsError {
		t.Fatalf("unknown rule must error: %+v", res)
	}
	if !strings.Contains(res.Text, "not found") {
		t.Fatalf("404-style error expected, got %q", res.Text)
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
