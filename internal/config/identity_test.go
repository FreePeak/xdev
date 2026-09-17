package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installEnv isolates the install-id path in a sandbox agent dir and
// captures the lifecycle warnings.
func installEnv(t *testing.T) (dir string, warnings *strings.Builder) {
	t.Helper()
	dir = t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dir)
	t.Setenv("HOME", t.TempDir()) // the default ~/.xdev must stay untouched
	warnings = &strings.Builder{}
	prev := installIDWarn
	installIDWarn = warnings
	ResetInstallIDCacheForTests()
	t.Cleanup(func() {
		installIDWarn = prev
		ResetInstallIDCacheForTests()
	})
	return dir, warnings
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// TestInstallIDCreatesOnce covers creation, mode, format, and the
// read-only lifecycle: a second call returns the stored value and leaves
// the file untouched.
func TestInstallIDCreatesOnce(t *testing.T) {
	dir, warnings := installEnv(t)
	id := InstallID()

	path := filepath.Join(dir, "install-id")
	if got := InstallIDPath(); got != path {
		t.Fatalf("InstallIDPath() = %q, want %q", got, path)
	}
	if !installIDPattern.MatchString(id) {
		t.Fatalf("InstallID() = %q, not a UUID", id)
	}
	if id != strings.ToLower(id) {
		t.Errorf("generated id %q is not lowercase", id)
	}
	if got := readFile(t, path); got != id+"\n" {
		t.Errorf("file = %q, want %q", got, id+"\n")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}

	if again := InstallID(); again != id {
		t.Fatalf("second call = %q, want %q", again, id)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(info.ModTime()) {
		t.Errorf("install-id was rewritten on a later call")
	}
	if warnings.Len() != 0 {
		t.Errorf("unexpected warnings: %s", warnings)
	}
}

// TestInstallIDReturnsStoredValueAsWritten: omp's acceptance rule is
// case-insensitive on read, returned exactly as stored.
func TestInstallIDReturnsStoredValueAsWritten(t *testing.T) {
	dir, _ := installEnv(t)
	stored := "AABBCCDD-1122-3344-5566-778899AABBCC"
	if err := os.WriteFile(filepath.Join(dir, "install-id"), []byte(stored+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := InstallID(); got != stored {
		t.Fatalf("InstallID() = %q, want the stored %q", got, stored)
	}
	if got := readFile(t, filepath.Join(dir, "install-id")); got != stored+"\n" {
		t.Errorf("stored value was rewritten: %q", got)
	}
}

// TestInstallIDRegeneratesInvalidContentOnce: garbage is replaced once,
// with a warning, and the replacement then sticks across restarts.
func TestInstallIDRegeneratesInvalidContentOnce(t *testing.T) {
	dir, warnings := installEnv(t)
	path := filepath.Join(dir, "install-id")
	if err := os.WriteFile(path, []byte("not-a-uuid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := InstallID()
	if !installIDPattern.MatchString(id) {
		t.Fatalf("InstallID() = %q, not a UUID", id)
	}
	if got := readFile(t, path); got != id+"\n" {
		t.Errorf("file = %q, want the regenerated %q", got, id+"\n")
	}
	if n := strings.Count(warnings.String(), "regenerating"); n != 1 {
		t.Fatalf("regeneration warnings = %d (%s), want exactly 1", n, warnings)
	}

	// A restart reads the regenerated value back without warning again.
	ResetInstallIDCacheForTests()
	if got := InstallID(); got != id {
		t.Fatalf("after restart = %q, want %q", got, id)
	}
	if n := strings.Count(warnings.String(), "regenerating"); n != 1 {
		t.Errorf("regeneration warnings after restart = %d, want 1", n)
	}
}

// TestInstallIDAdoptsConcurrentWinner: when the exclusive create loses a
// race (the duplicate case), the winner's stored id is adopted and its
// file is left intact.
func TestInstallIDAdoptsConcurrentWinner(t *testing.T) {
	dir, warnings := installEnv(t)
	path := filepath.Join(dir, "install-id")
	winner := "0f9c1a2b-3d4e-4f50-8a6b-7c8d9e0f1a2b"
	if err := os.WriteFile(path, []byte(winner+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := mintInstallID(path); got != winner {
		t.Fatalf("mintInstallID = %q, want the winner %q", got, winner)
	}
	if got := readFile(t, path); got != winner+"\n" {
		t.Errorf("winner's file changed: %q", got)
	}
	if warnings.Len() != 0 {
		t.Errorf("adopting an existing id must not warn: %s", warnings)
	}
}

// TestInstallIDPathIsPerInstall: the default path is ~/.xdev/install-id —
// one level above the data dir — so every profile shares one id.
func TestInstallIDPathIsPerInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDEV_AGENT_DIR", "")
	t.Setenv("XDEV_PROFILE", "")
	if err := SetProfile(""); err != nil {
		t.Fatal(err)
	}
	ResetInstallIDCacheForTests()
	t.Cleanup(func() {
		ResetInstallIDCacheForTests()
		_ = SetProfile("")
	})

	want := filepath.Join(home, ".xdev", "install-id")
	if got := InstallIDPath(); got != want {
		t.Fatalf("InstallIDPath() = %q, want %q", got, want)
	}
	if err := SetProfile("work"); err != nil {
		t.Fatal(err)
	}
	if got := InstallIDPath(); got != want {
		t.Errorf("a profile moved install-id to %q, want %q", got, want)
	}
	if got := InstallID(); !installIDPattern.MatchString(got) {
		t.Errorf("InstallID() = %q, not a UUID", got)
	}
}
