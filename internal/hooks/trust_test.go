package hooks

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The #241 acceptance: in an untrusted clone none of its hooks run, the user is
// told what was withheld and how to allow it, and both facts fail the tests if
// the gate is removed. Every case here is asserted through Build and a real
// Notify — the chain the agent loop calls — not against the classifier.

// projectRepo writes one repository hook whose command touches marker, and
// returns the workspace plus the marker path.
func projectRepo(t *testing.T, marker string) string {
	t.Helper()
	cwd := t.TempDir()
	dir := filepath.Join(cwd, ".xdev", "hooks")
	writeFile(t, filepath.Join(dir, "agent_start.sh"), "#!/bin/sh\ntouch "+marker+"\n")
	if err := os.Chmod(filepath.Join(dir, "agent_start.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	return cwd
}

func markerRan(t *testing.T, marker string) bool {
	t.Helper()
	_, err := os.Stat(marker)
	if err == nil {
		return true
	}
	if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return false
}

func TestProjectHooksRunOnlyAfterTrust(t *testing.T) {
	isolated(t)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	cwd := projectRepo(t, marker)

	b, warns := Build(Options{CWD: cwd})
	if b != nil {
		t.Fatalf("an untrusted repository built a bus: %+v", b.Hooks)
	}
	if len(warns) != 1 {
		t.Fatalf("warnings = %v, want exactly one naming the withheld hook", warns)
	}
	w := warns[0]
	for _, want := range []string{"NOT run", "not trusted", "agent_start", "`xdev trust`", ".xdev/hooks"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning = %q, missing %q", w, want)
		}
	}
	// The command must not have executed — the whole point.
	b.Notify(context.Background(), "agent_start", nil) // nil bus is safe
	if markerRan(t, marker) {
		t.Fatal("the withheld hook executed")
	}

	// Trust it, exactly as the verb does, and the same call now runs it.
	approved, err := TrustWorkspace(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(approved) != 1 || approved[0].Name != "agent_start" {
		t.Fatalf("approved = %+v", approved)
	}
	b, warns = Build(Options{CWD: cwd})
	if len(warns) != 0 {
		t.Fatalf("a trusted workspace must warn nothing: %v", warns)
	}
	if b == nil || len(b.Hooks) != 1 {
		t.Fatalf("trusted hooks missing: %+v", b)
	}
	b.Notify(context.Background(), "agent_start", nil)
	if !markerRan(t, marker) {
		t.Fatal("a trusted repository hook did not run")
	}
}

func TestEditingATrustedHookWithdrawsIt(t *testing.T) {
	isolated(t)
	marker := filepath.Join(t.TempDir(), "edited-ran")
	cwd := projectRepo(t, marker)
	if _, err := TrustWorkspace(cwd); err != nil {
		t.Fatal(err)
	}

	// The repository ships a second commit that turns the approved hook into an
	// information leak. The earlier yes must not cover it.
	marker2 := filepath.Join(t.TempDir(), "after-edit-ran")
	writeFile(t, filepath.Join(cwd, ".xdev", "hooks", "agent_start.sh"), "#!/bin/sh\ntouch "+marker2+"\n")
	if err := os.Chmod(filepath.Join(cwd, ".xdev", "hooks", "agent_start.sh"), 0o755); err != nil {
		t.Fatal(err)
	}

	pending, err := PendingProjectHooks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("an edited hook must be pending again, got %v", pending)
	}
	b, warns := Build(Options{CWD: cwd})
	if b != nil {
		t.Fatal("the edited hook ran without a new decision")
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "changed since they were approved") {
		t.Fatalf("warnings = %v, want the changed-bytes reason", warns)
	}
	b.Notify(context.Background(), "agent_start", nil)
	if markerRan(t, marker2) {
		t.Fatal("the edited command executed")
	}
}

func TestOnlyRepositoryHooksAreGated(t *testing.T) {
	// A user who installed their own hooks, or passed --hook, or set them in
	// settings, chose them: the gate must not touch those sources, or it is a
	// regression dressed as a fix.
	dataDir := isolated(t)
	marker := filepath.Join(t.TempDir(), "user-ran")
	writeFile(t, filepath.Join(dataDir, "hooks", "agent_start.sh"), "#!/bin/sh\ntouch "+marker+"\n")
	if err := os.Chmod(filepath.Join(dataDir, "hooks", "agent_start.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir() // no .xdev/hooks at all

	b, warns := Build(Options{
		CWD:      cwd,
		CLI:      []string{"agent_start=echo cli"},
		Settings: map[string]any{"turn_start": []any{"echo from-settings"}},
	})
	if len(warns) != 0 {
		t.Fatalf("warnings = %v", warns)
	}
	if b == nil || len(b.Hooks) != 3 {
		t.Fatalf("hooks = %+v", b)
	}
	b.Notify(context.Background(), "agent_start", nil)
	if !markerRan(t, marker) {
		t.Fatal("a user-installed hook was gated")
	}
}

func TestTrustRecordLivesInTheProfileAndRevokeWorks(t *testing.T) {
	isolated(t)
	cwd := projectRepo(t, filepath.Join(t.TempDir(), "unused"))
	if _, err := TrustWorkspace(cwd); err != nil {
		t.Fatal(err)
	}
	// Never in the repository: a decision recorded inside the thing it
	// constrains is not a constraint.
	if _, err := os.Stat(filepath.Join(cwd, ".xdev", "trusted-workspaces.yml")); err == nil {
		t.Fatal("trust was written into the repository")
	}
	list, err := TrustedList()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Hooks[0] != "agent_start" {
		t.Fatalf("TrustedList = %+v", list)
	}
	if fi, err := os.Stat(TrustPath()); err != nil {
		t.Fatal(err)
	} else if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("trust file is %o, want 0600 (it lists what you have decided to run)", perm)
	}

	removed, err := UntrustWorkspace(cwd)
	if err != nil || !removed {
		t.Fatalf("UntrustWorkspace = %v, %v", removed, err)
	}
	pending, err := PendingProjectHooks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("after revoke, pending = %v", pending)
	}
	if removed, err := UntrustWorkspace(cwd); err != nil || removed {
		t.Fatalf("second revoke = %v, %v; want nothing removed and no error", removed, err)
	}
}

func TestSymlinkedCheckoutSharesOneDecision(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink needs a privilege on windows; the resolution is POSIX path semantics")
	}
	isolated(t)
	cwd := projectRepo(t, filepath.Join(t.TempDir(), "unused"))
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(cwd, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := TrustWorkspace(cwd); err != nil {
		t.Fatal(err)
	}
	// The same files through another path are the same decision — otherwise the
	// user approves twice and learns to approve without reading.
	if pending, err := PendingProjectHooks(link); err != nil || len(pending) != 0 {
		t.Fatalf("pending through the alias = %v (%v), want none", pending, err)
	}
}

func TestUnreadableTrustFileWithholdsRatherThanReprompts(t *testing.T) {
	isolated(t)
	cwd := projectRepo(t, filepath.Join(t.TempDir(), "unused"))
	if _, err := TrustWorkspace(cwd); err != nil {
		t.Fatal(err)
	}
	// Corrupt the record. A silent "nothing is approved" would let a bad actor
	// rewrite the file and have the user re-approve blindly; the gate must
	// withhold and say the file is the problem.
	if err := os.WriteFile(TrustPath(), []byte("workspaces: [this is not a mapping\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, warns := Build(Options{CWD: cwd})
	if b != nil {
		t.Fatal("hooks ran on an unreadable trust file")
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "unreadable") {
		t.Fatalf("warnings = %v", warns)
	}
}
