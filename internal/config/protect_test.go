package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

func TestProfileInitInstallsTheDataRoot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dir)
	// Nothing in this test calls SetProtectedRoots: what is registered here is
	// what the package init installed, which is what every real binary gets.
	root := ""
	for _, r := range tool.ProtectedRoots() {
		if filepath.Clean(r) == filepath.Clean(dir) {
			root = r
		}
	}
	if root == "" {
		t.Fatalf("the active data dir %s is not protected: roots = %v", dir, tool.ProtectedRoots())
	}
	// A settings write must be refused by the file tools on the strength of the
	// registration alone.
	if err := tool.CheckProtectedPath(filepath.Join(dir, "config.yml")); err == nil ||
		!strings.Contains(err.Error(), "cannot be written through the file tools") {
		t.Fatalf("config.yml in the data dir was not refused: %v", err)
	}
}

func TestHarnessWritersAreUnaffected(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dir)

	// The session store writes into the very directory the floor protects.
	path := filepath.Join(dir, "sessions", "-proj", "2026-09-14T10-00-00.000Z_abcd.jsonl")
	s := session.OpenMem("/proj", "unaffected")
	got, err := s.EnsureOnDisk(path, session.Options{})
	if err != nil || got != path {
		t.Fatalf("EnsureOnDisk: %v (%q)", err, got)
	}
	if err := s.Append(&session.MessageEntry{Message: ai.Message{
		Role:    ai.RoleUser,
		Content: []ai.Block{ai.TextBlock{Text: "keep"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || st.Size() == 0 {
		t.Fatalf("the session file was not written: %v", err)
	}
	reopened, err := session.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.Entries()) != 1 {
		t.Fatalf("entries = %d", len(reopened.Entries()))
	}

	// The config CLI writes settings the same way the model is refused.
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte("theme: groknight\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSettings("/tmp", nil); err != nil {
		t.Fatalf("the profile file the config CLI writes must load: %v", err)
	}
	// …while the model-facing seam still refuses it.
	if err := tool.CheckProtectedPath(filepath.Join(dir, "config.yml")); err == nil {
		t.Fatal("the file tools must still refuse it")
	}
}
