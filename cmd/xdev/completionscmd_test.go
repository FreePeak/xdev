package main

import (
	"bytes"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// completionTestFlagSet reproduces a realistic root FlagSet (a subset of
// main()'s registrations — the generator reads whatever FlagSet it is given).
func completionTestFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("xdev", flag.ContinueOnError)
	fs.String("model", "", "model to use (provider/model)")
	fs.Bool("continue", false, "continue the most recent session in this directory")
	fs.String("profile", "", "named profile: relocate the user base")
	return fs
}

// TestCompletionsSubcommandsCoverTheDispatchMap pins the vocabulary: every
// name the subcommands map carries appears as a candidate, so a new
// subcommand is completed from the moment it is dispatched.
func TestCompletionsSubcommandsCoverTheDispatchMap(t *testing.T) {
	names := completionNames()
	if len(names) != len(subcommands) {
		t.Fatalf("completionNames() = %d names, subcommands map holds %d", len(names), len(subcommands))
	}
	for _, shell := range []string{"bash", "zsh", "fish"} {
		var out, errOut bytes.Buffer
		if code := completionsCmd([]string{shell}, completionTestFlagSet(), &out, &errOut); code != 0 {
			t.Fatalf("completionsCmd(%s) = %d (%s)", shell, code, errOut.String())
		}
		script := out.String()
		for _, name := range names {
			if !strings.Contains(script, name) {
				t.Fatalf("%s completion is missing the %q subcommand", shell, name)
			}
		}
		// Flags are spelled per shell: bash/zsh complete both dash forms,
		// fish registers -o (single-dash long) and -l (--flag).
		want := []string{"-model", "--model", "-continue", "-profile"}
		if shell == "fish" {
			want = []string{"-o model", "-l model", "-o continue", "-l profile"}
		}
		for _, flag := range want {
			if !strings.Contains(script, flag) {
				t.Fatalf("%s completion is missing the %q flag", shell, flag)
			}
		}
	}
}

// TestCompletionsBashParses is the acceptance bar: bash must accept the
// generated script as real shell syntax, and the candidate list it defines
// must actually select a subcommand.
func TestCompletionsBashParses(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	script := completionTestScript(t, "bash")

	// -n parses without executing: a syntax error is exactly what this
	// guards (generated quoting is the failure mode).
	path := filepath.Join(t.TempDir(), "xdev.bash")
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("bash rejected the completion script: %v (%s)", err, out)
	}

	// The body's candidate list answers a partial subcommand.
	got, err := exec.Command("bash", "-c", "compgen -W \""+strings.Join(completionNames(), " ")+"\" -- tu").CombinedOutput()
	if err != nil {
		t.Fatalf("compgen probe: %v", err)
	}
	if strings.TrimSpace(string(got)) != "tui" {
		t.Fatalf("compgen(tu) = %q, want tui", got)
	}
}

// completionTestScript generates one shell's script through the CLI surface.
func completionTestScript(t *testing.T, shell string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := completionsCmd([]string{shell}, completionTestFlagSet(), &out, &errOut); code != 0 {
		t.Fatalf("completionsCmd(%s) = %d (%s)", shell, code, errOut.String())
	}
	return out.String()
}

// TestCompletionsZshParses proves the zsh script parses under zsh when zsh
// exists on the host (it does on every mac).
func TestCompletionsZshParses(t *testing.T) {
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh not available")
	}
	var out, errOut bytes.Buffer
	if code := completionsCmd([]string{"zsh"}, completionTestFlagSet(), &out, &errOut); code != 0 {
		t.Fatalf("completionsCmd(zsh) = %d", code)
	}
	cmd := exec.Command("zsh", "-n") // -n: parse only
	cmd.Stdin = strings.NewReader(out.String())
	if out2, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("zsh rejected the completion script: %v (%s)", err, out2)
	}
}

// TestCompletionsFishParses proves the fish script parses under fish when
// present, and falls back to structural checks when it is not.
func TestCompletionsFishParses(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := completionsCmd([]string{"fish"}, completionTestFlagSet(), &out, &errOut); code != 0 {
		t.Fatalf("completionsCmd(fish) = %d", code)
	}
	script := out.String()
	if path, err := exec.LookPath("fish"); err == nil {
		cmd := exec.Command(path, "-c", "source /dev/stdin")
		cmd.Stdin = strings.NewReader(script)
		if out2, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fish rejected the completion script: %v (%s)", err, out2)
		}
	}
	for _, want := range []string{"function __xdev_needs_subcommand", "complete -c xdev", "-l model"} {
		if !strings.Contains(script, want) {
			t.Fatalf("fish script missing %q:\n%s", want, script)
		}
	}
}

// TestCompletionsUsageErrors pins the shell selection contract.
func TestCompletionsUsageErrors(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := completionsCmd([]string{"powershell"}, completionTestFlagSet(), &out, &errOut); code != 2 {
		t.Fatalf("unknown shell exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "powershell") || !strings.Contains(errOut.String(), "bash") {
		t.Fatalf("error output:\n%s", errOut.String())
	}
	if code := completionsCmd([]string{"bash", "zsh"}, completionTestFlagSet(), &out, &errOut); code != 2 {
		t.Fatal("two shells should be a usage error")
	}
	// Flags survive the shell word (main parses them before dispatching).
	if code := completionsCmd([]string{"--json"}, completionTestFlagSet(), &out, &errOut); code != 2 {
		t.Fatalf("unknown flag-like shell = %d, want 2", code)
	}
}

// TestCompletionsSanitize strips shell-hostile characters from flag usage.
func TestCompletionsSanitize(t *testing.T) {
	in := "model to use (provider/model) [default: `free`]"
	got := completionSanitize(in)
	for _, bad := range []string{"`", "[", "]", "$", "\\"} {
		if strings.Contains(got, bad) {
			t.Fatalf("sanitize(%q) = %q still contains %q", in, got, bad)
		}
	}
	if !strings.Contains(got, "model to use") {
		t.Fatalf("sanitize stripped the description: %q", got)
	}
}
