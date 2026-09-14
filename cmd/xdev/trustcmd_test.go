package main

// The `xdev trust` verb itself (#241): what a user sees, what it records, and
// the two properties that matter for a tool that gates code execution — a
// headless invocation must never block or record anything, and nothing is
// recorded before the hooks have been shown.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	hookbus "github.com/FreePeak/xdev/internal/hooks"
)

// trustRepo is a clone shipping one repository hook that would run on session
// start. It returns the workspace and the file that hook would create.
func trustRepo(t *testing.T) (cwd, marker string) {
	t.Helper()
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	cwd = t.TempDir()
	marker = filepath.Join(t.TempDir(), "hook-ran")
	dir := filepath.Join(cwd, ".xdev", "hooks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\ntouch " + marker + "\n"
	if err := os.WriteFile(filepath.Join(dir, "agent_start.sh"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return cwd, marker
}

func runTrustCmd(t *testing.T, verb string, cwd string, stdin string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := trustCmd(verb, []string{}, cwd, strings.NewReader(stdin), &out, &errBuf)
	return code, out.String(), errBuf.String()
}

func pendingCount(t *testing.T, cwd string) int {
	t.Helper()
	pending, err := hookbus.PendingProjectHooks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	return len(pending)
}

func TestTrustCmdNeverBlocksAHeadlessRun(t *testing.T) {
	cwd, marker := trustRepo(t)
	// A pipe, not a terminal: asking is impossible, so the command must refuse
	// with instructions rather than hang or decide for the user.
	code, out, errOut := runTrustCmd(t, "trust", cwd, "yes\n")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (no terminal to ask on)\nout=%s err=%s", code, out, errOut)
	}
	if !strings.Contains(out, "agent_start") || !strings.Contains(out, "touch") {
		t.Errorf("the review must show the event and the command before anything is allowed:\n%s", out)
	}
	if !strings.Contains(errOut, "--yes") {
		t.Errorf("err = %q, want the repair named", errOut)
	}
	if pendingCount(t, cwd) != 1 {
		t.Error("a refused prompt must record nothing")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the hook executed")
	}
}

func TestTrustCmdYesRecordsWhatItShowed(t *testing.T) {
	cwd, _ := trustRepo(t)
	var out, errBuf bytes.Buffer
	code := trustCmd("trust", []string{"--yes"}, cwd, strings.NewReader(""), &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit = %d err = %s", code, errBuf.String())
	}
	for _, want := range []string{"agent_start", "trusted 1 hook(s)", "withheld again automatically"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output = %q, missing %q", out.String(), want)
		}
	}
	if pendingCount(t, cwd) != 0 {
		t.Fatal("--yes must record the decision it printed")
	}
	// And the record is in the profile, not the repository.
	if _, err := os.Stat(filepath.Join(cwd, ".xdev", "trusted-workspaces.yml")); err == nil {
		t.Error("trust must never be stored inside the thing it constrains")
	}
}

func TestTrustCmdListReviewsWithoutDeciding(t *testing.T) {
	cwd, _ := trustRepo(t)
	var out, errBuf bytes.Buffer
	if code := trustCmd("trust", []string{"--list"}, cwd, strings.NewReader(""), &out, &errBuf); code != 0 {
		t.Fatalf("exit = %d err = %s", code, errBuf.String())
	}
	for _, want := range []string{"waiting", "agent_start", "no workspace is trusted"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output = %q, missing %q", out.String(), want)
		}
	}
	if pendingCount(t, cwd) != 1 {
		t.Fatal("--list must change nothing")
	}
}

func TestDistrustCmdWithdraws(t *testing.T) {
	cwd, _ := trustRepo(t)
	var out bytes.Buffer
	if code := trustCmd("trust", []string{"--yes"}, cwd, strings.NewReader(""), &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup trust: %d %s", code, out.String())
	}
	code, outStr, errOut := runTrustCmd(t, "distrust", cwd, "")
	if code != 0 || !strings.Contains(outStr, "distrusted") {
		t.Fatalf("exit = %d out = %q err = %q", code, outStr, errOut)
	}
	if pendingCount(t, cwd) != 1 {
		t.Fatal("distrust must withdraw the decision")
	}
	// Idempotent, and says so.
	code, outStr, _ = runTrustCmd(t, "distrust", cwd, "")
	if code != 0 || !strings.Contains(outStr, "was not trusted") {
		t.Fatalf("second distrust: exit = %d out = %q", code, outStr)
	}
}

func TestTrustCmdTargetsAnExplicitPath(t *testing.T) {
	// `xdev trust ~/other/repo` from anywhere: the review and the record are for
	// that directory, and a bare invocation means the current one.
	cwd, _ := trustRepo(t)
	here := t.TempDir()
	var out bytes.Buffer
	if code := trustCmd("trust", []string{cwd}, here, strings.NewReader("y\n"), &out, &bytes.Buffer{}); code != 2 {
		t.Fatalf("exit = %d (a non-terminal stdin must refuse), out = %s", code, out.String())
	}
	if !strings.Contains(out.String(), filepath.Join(cwd, ".xdev", "hooks")) {
		t.Errorf("the review must name the target directory, not the caller's: %s", out.String())
	}
	if pendingCount(t, cwd) != 1 || pendingCount(t, here) != 0 {
		t.Fatal("the wrong workspace was reviewed")
	}
}

func TestAnswersYesRefusesByDefault(t *testing.T) {
	// Anything that is not an explicit yes means no — including EOF at the
	// prompt, which is what a closed stdin looks like.
	for _, line := range []string{"", "\n", "n", "N", "no", "maybe", "y es", "yep"} {
		if answersYes(line) {
			t.Errorf("answersYes(%q) = true", line)
		}
	}
	for _, line := range []string{"y", "Y", "yes", "YES\n", "  y  "} {
		if !answersYes(line) {
			t.Errorf("answersYes(%q) = false", line)
		}
	}
}
