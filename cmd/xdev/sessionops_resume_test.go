package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

func TestRestoreChangesRejectsNonGit(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "sub", "f.go"), []byte("package sub"), 0o644)
	if err := restoreChanges(dir, []string{"sub/f.go"}); err == nil {
		t.Fatal("restoreChanges outside a git repo succeeded — it must refuse")
	}
}

func TestDiscardChangesRejectsNonGit(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "sub", "f.go"), []byte("package sub"), 0o644)
	if err := discardChanges(dir, []string{"sub/f.go"}); err == nil {
		t.Fatal("discardChanges outside a git repo succeeded — it must refuse")
	}
}

func TestDiscardChangesNoGitHistory(t *testing.T) {
	dir := t.TempDir()
	runGitT(t, dir, "init", "-q")
	runGitT(t, dir, "config", "user.email", "t@t")
	runGitT(t, dir, "config", "user.name", "t")
	_ = os.WriteFile(filepath.Join(dir, "f.go"), []byte("package t"), 0o644)
	if err := discardChanges(dir, []string{"f.go"}); err == nil {
		t.Fatal("discardChanges on a repo with no commits succeeded — it must refuse")
	}
}

func TestExitMenuTextOutsideCwd(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := session.OpenMem("/tmp/somewhere-else", "external session")
	_ = store.Append(&session.MessageEntry{Message: ai.Message{
		Role:    ai.RoleUser,
		Content: []ai.Block{ai.TextBlock{Text: "hi"}},
	}})
	defer store.Close()
	if got := exitMenuText(store, "/tmp/different-cwd"); got != "" {
		t.Fatalf("exitMenuText for another cwd = %q, want empty", got)
	}
}

func TestExitMenuTextNilStore(t *testing.T) {
	if got := exitMenuText(nil, "/tmp/whatever"); got != "" {
		t.Fatalf("exitMenuText(nil) = %q, want empty", got)
	}
}

func TestResumeProgSpellsInvokedName(t *testing.T) {
	for _, c := range []struct{ argv0, want string }{
		{"/home/u/.local/bin/omp", "omp"},
		{"./xdev", "xdev"},
		{"/tmp/build/xdev.exe", "xdev"},
		{"", "xdev"},
		{".", "xdev"},
		{"..", "xdev"},
		{string(filepath.Separator), "xdev"},
	} {
		if got := resumeProg(c.argv0); got != c.want {
			t.Errorf("resumeProg(%q) = %q, want %q", c.argv0, got, c.want)
		}
	}
}

func TestExitMenuIncludesOptions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := "/tmp/menu-test"
	id := "DDDD4444-0000-0000-0000-000000000000"
	path := writePickerSession(t, cwd, id, "menu session", "hello")
	st, err := session.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got := exitMenuText(st, cwd)
	if got == "" {
		t.Fatal("exitMenuText returned empty for a resumable session in cwd")
	}
	for _, opt := range []string{"─── resume ──", "restore", "discard", "claude", "keep"} {
		if !strings.Contains(got, opt) {
			t.Errorf("menu missing option %q:\n%s", opt, got)
		}
	}
	if !strings.Contains(got, "--resume "+id) {
		t.Errorf("menu missing the resume command:\n%s", got)
	}
}

func TestDiscardChangesKeepsUntrackedSafe(t *testing.T) {
	dir := t.TempDir()
	runGitT(t, dir, "init", "-q")
	runGitT(t, dir, "checkout", "-q", "-b", "main")
	runGitT(t, dir, "config", "user.email", "t@t")
	runGitT(t, dir, "config", "user.name", "t")
	_ = os.WriteFile(filepath.Join(dir, "tracked.go"), []byte("old"), 0o644)
	runGitT(t, dir, "add", "tracked.go")
	runGitT(t, dir, "commit", "-q", "-m", "init")
	_ = os.WriteFile(filepath.Join(dir, "tracked.go"), []byte("new content"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "untracked.go"), []byte("should survive"), 0o644)
	if err := discardChanges(dir, []string{"tracked.go"}); err != nil {
		t.Fatalf("discardChanges: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "tracked.go"))
	if string(b) != "old" {
		t.Fatalf("tracked.go = %q, want %q", string(b), "old")
	}
	b, _ = os.ReadFile(filepath.Join(dir, "untracked.go"))
	if string(b) != "should survive" {
		t.Fatalf("untracked.go = %q, want %q", string(b), "should survive")
	}
}

func TestRestoreChangesRestoresFile(t *testing.T) {
	dir := t.TempDir()
	runGitT(t, dir, "init", "-q")
	runGitT(t, dir, "checkout", "-q", "-b", "main")
	runGitT(t, dir, "config", "user.email", "t@t")
	runGitT(t, dir, "config", "user.name", "t")
	_ = os.WriteFile(filepath.Join(dir, "f.go"), []byte("committed"), 0o644)
	runGitT(t, dir, "add", "f.go")
	runGitT(t, dir, "commit", "-q", "-m", "init")
	_ = os.WriteFile(filepath.Join(dir, "f.go"), []byte("modified"), 0o644)
	if err := restoreChanges(dir, []string{"f.go"}); err != nil {
		t.Fatalf("restoreChanges: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "f.go"))
	if string(b) != "committed" {
		t.Fatalf("f.go = %q, want %q", string(b), "committed")
	}
}

// TestExitResumeLineSurvivesTheCloseDefer: the TUI's exit defers run in the
// opposite order to their registration — the store is flushed and closed
// first, and the resume line prints second. closeAll empties the tabset, so
// the line must name a store captured BEFORE that, not storeOf() read after
// it (#519: it printed nothing at all, on every quit).
func TestExitResumeLineSurvivesTheCloseDefer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := "/tmp/exit-capture-test"
	id := "EEEE5555-0000-0000-0000-000000000000"
	path := writePickerSession(t, cwd, id, "captured", "hello")
	st, err := session.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	tabs := newTabset(st)

	// the close defer, in runTUI's order: capture, then closeAll.
	var exitStore *session.Store
	if s := tabs.store(); s != nil {
		exitStore = s
	}
	tabs.closeAll()

	if got := tabs.store(); got != nil {
		t.Fatalf("the trap this guards is gone: storeOf() still resolves after closeAll (%v)", got)
	}
	if got := exitMenuText(tabs.store(), cwd); got != "" {
		t.Fatalf("exitMenuText(storeOf() after closeAll) = %q, want empty — nothing to name", got)
	}
	if got := exitMenuText(exitStore, cwd); !strings.Contains(got, "--resume "+id) {
		t.Fatalf("the captured store lost the resume line:\n%s", got)
	}
}

// The print defer itself is not reachable from a test (runTUI needs a tty), so
// this pins the one fact the fix rests on: the line names the store the close
// defer captured, not storeOf() read after the tabset has been emptied.
func TestExitLineReadsTheCapturedStore(t *testing.T) {
	src, err := os.ReadFile("tui.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	if !strings.Contains(s, "exitMenuText(exitStore, cwd)") {
		t.Fatal("the exit line no longer names the captured store: it reads storeOf(), which closeAll empties (#519)")
	}
	if !strings.Contains(s, "exitStore = s") {
		t.Fatal("the close defer no longer captures the store the exit line names")
	}
}

func runGitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, string(out))
	}
}
