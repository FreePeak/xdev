package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile drops one hook declaration under dir, creating parents.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// isolated points config.DataDir() at a scratch dir so the test never sees
// (or writes) the developer's real ~/.xdev/agent.
func isolated(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dir)
	return dir
}

// TestDiscoverPrecedenceAndShape pins the discovery contract: project roots
// beat user roots by name, a .sh file declares its event by file name, and
// settings win over a hook file that claims the same name.
func TestDiscoverPrecedenceAndShape(t *testing.T) {
	dataDir := isolated(t)
	cwd := t.TempDir()
	writeFile(t, filepath.Join(cwd, ".xdev", "hooks", "guard.yml"),
		"event: tool_call\ncommand: echo project\n")
	writeFile(t, filepath.Join(dataDir, "hooks", "guard.yml"),
		"event: tool_call\ncommand: echo user\n")
	writeFile(t, filepath.Join(dataDir, "hooks", "user_only.yml"),
		"event: turn_end\ncommand: echo user-only\n")
	script := filepath.Join(cwd, ".xdev", "hooks", "session_switch.sh")
	writeFile(t, script, "#!/bin/sh\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}

	b, warns := Build(Options{CWD: cwd})
	if len(warns) != 0 {
		t.Fatalf("warnings = %v", warns)
	}
	byName := map[string]Hook{}
	for _, h := range b.Hooks {
		byName[h.Name] = h
	}
	if got := byName["guard"]; got.Source != "project" || got.Command != "echo project" {
		t.Fatalf("guard = %+v (project must win the name)", got)
	}
	if got := byName["user_only"]; got.Source != "user" {
		t.Fatalf("user_only = %+v", got)
	}
	sw := byName["session_switch"]
	if sw.Event != "session_switch" || !strings.Contains(sw.Command, "session_switch.sh") {
		t.Fatalf(".sh hook = %+v", sw)
	}

	// Settings own their event name: the discovered guard.yml is dropped.
	b2, _ := Build(Options{Settings: map[string]any{"guard": "echo settings"}, CWD: cwd})
	for _, h := range b2.Hooks {
		if h.Name == "guard" {
			if h.Source != "settings" {
				t.Fatalf("settings must win the name, got %+v", h)
			}
			return
		}
	}
	t.Fatal("settings hook missing")
}

// TestDiscoverBadFileWarns: one malformed file is skipped, never fatal.
func TestDiscoverBadFileWarns(t *testing.T) {
	isolated(t)
	cwd := t.TempDir()
	writeFile(t, filepath.Join(cwd, ".xdev", "hooks", "broken.yml"), "event: tool_call\n")
	writeFile(t, filepath.Join(cwd, ".xdev", "hooks", "notes.txt"), "not a hook\n")

	b, warns := Build(Options{CWD: cwd})
	if b != nil {
		t.Fatalf("no usable hooks expected, got %+v", b)
	}
	if len(warns) != 2 {
		t.Fatalf("warnings = %v", warns)
	}
}

// TestExtensionHooksRequireTrust: an extension's hooks dir is third-party
// code — it loads only when the extension is named by --trusted-extension.
func TestExtensionHooksRequireTrust(t *testing.T) {
	dataDir := isolated(t)
	writeFile(t, filepath.Join(dataDir, "extensions", "evil", "hooks", "call.yml"),
		"event: tool_call\ncommand: echo evil\n")
	cwd := t.TempDir()

	disc, warns := Discover(cwd, nil)
	if len(disc) != 0 {
		t.Fatalf("untrusted extension hooks must not load: %+v", disc)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "--trusted-extension evil") {
		t.Fatalf("warnings = %v", warns)
	}

	disc, warns = Discover(cwd, []string{"evil"})
	if len(disc) != 1 || disc[0].Source != "extension:evil" {
		t.Fatalf("trusted discovery = %+v (warns %v)", disc, warns)
	}
}

// TestMatcherStageTwo: stage 1 is the event, stage 2 the regex over the
// payload's tool name — a non-matching tool never runs the hook.
func TestMatcherStageTwo(t *testing.T) {
	isolated(t)
	cwd := t.TempDir()
	writeFile(t, filepath.Join(cwd, ".xdev", "hooks", "rewrite.yml"),
		`event: tool_call
matcher: "^bash$"
command: echo '{"input":"rewritten"}'
`)
	b, warns := Build(Options{CWD: cwd})
	if len(warns) != 0 {
		t.Fatalf("warnings = %v", warns)
	}

	res, err := b.Run(context.Background(), "tool_call", map[string]any{"tool": "bash", "input": "original"})
	if err != nil {
		t.Fatal(err)
	}
	if res["input"] != "rewritten" {
		t.Fatalf("matcher stage 2 must run the hook for bash: %v", res)
	}
	res, err = b.Run(context.Background(), "tool_call", map[string]any{"tool": "read", "input": "original"})
	if err != nil {
		t.Fatal(err)
	}
	if res["input"] != "original" {
		t.Fatalf("hook ran for a non-matching tool: %v", res)
	}
}

// TestIfPrefilterUsesPolicyMatchers: `if` is permission syntax resolved by
// the approval policy's own matchers (Bash(git *)), so a hook and the
// approval rules can never disagree.
func TestIfPrefilterUsesPolicyMatchers(t *testing.T) {
	isolated(t)
	cwd := t.TempDir()
	writeFile(t, filepath.Join(cwd, ".xdev", "hooks", "gitguard.yml"),
		`event: tool_call
if: "Bash(git *)"
command: echo '{"block":true,"reason":"no git"}'
`)
	b, warns := Build(Options{CWD: cwd})
	if len(warns) != 0 {
		t.Fatalf("warnings = %v", warns)
	}

	_, err := b.Run(context.Background(), "tool_call", map[string]any{
		"tool": "bash", "input": map[string]any{"command": "git push origin main"},
	})
	if err == nil || !strings.Contains(err.Error(), "no git") {
		t.Fatalf("Bash(git *) must prefilter to a deny, got %v", err)
	}
	// Same tool, command outside the pattern: the hook must not run.
	if _, err := b.Run(context.Background(), "tool_call", map[string]any{
		"tool": "bash", "input": map[string]any{"command": "ls -la"},
	}); err != nil {
		t.Fatalf("non-matching command blocked: %v", err)
	}
	// Another tool never matches a Bash() spec.
	if _, err := b.Run(context.Background(), "tool_call", map[string]any{
		"tool": "write", "input": map[string]any{"path": "git-notes.md"},
	}); err != nil {
		t.Fatalf("non-bash tool blocked: %v", err)
	}
}

// TestCLIHookSpecs: --hook adds an inline hook, or pulls a discovered hook
// in by name; an unknown name is a warning, never a failure.
func TestCLIHookSpecs(t *testing.T) {
	isolated(t)
	cwd := t.TempDir()
	writeFile(t, filepath.Join(cwd, ".xdev", "hooks", "guard.yml"),
		"event: tool_call\ncommand: echo guard\n")

	b, warns := Build(Options{CWD: cwd, CLI: []string{"turn_start=echo cli", "guard", "nope"}})
	if len(warns) != 1 || !strings.Contains(warns[0], "no discovered hook") {
		t.Fatalf("warnings = %v", warns)
	}
	seen := map[string]int{}
	for _, h := range b.Hooks {
		seen[h.Name]++
	}
	if seen["cli:turn_start"] != 1 || seen["guard"] != 1 {
		t.Fatalf("cli hooks = %+v", b.Hooks)
	}
	// Order matters: CLI hooks run before settings/discovered ones.
	if b.Hooks[0].Source != "cli" {
		t.Fatalf("first hook = %+v, want the CLI source", b.Hooks[0])
	}
}

// TestContextHookReplacesSystemPrompt: the `context` event is value
// returning — a hook's {"text": …} replaces the prompt, and a broken hook
// keeps the original rather than running promptless.
func TestContextHookReplacesSystemPrompt(t *testing.T) {
	if got := (*Bus)(nil).Context(context.Background(), "orig"); got != "orig" {
		t.Fatalf("nil bus context = %q", got)
	}
	b := FromSettings(map[string]any{"context": `read -r _; echo '{"text":"hooked"}'`})
	if got := b.Context(context.Background(), "orig"); got != "hooked" {
		t.Fatalf("context = %q, want replaced prompt", got)
	}
	bad := FromSettings(map[string]any{"context": "exit 1"})
	if got := bad.Context(context.Background(), "orig"); got != "orig" {
		t.Fatalf("failing hook must keep the prompt, got %q", got)
	}
}
