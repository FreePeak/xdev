package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/config"
)

// cleanseTestTranscript writes a session-shaped transcript holding the secret
// in one line, and returns its path.
func cleanseTestTranscript(t *testing.T, dataDir string) string {
	t.Helper()
	path := filepath.Join(dataDir, "sessions", "-tmp-clean", "2026-01-01T00-00-00.000Z_abcdefgh.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := `{"type":"session","id":"abcdefgh","cwd":"/tmp/clean"}` + "\n" +
		`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"here is the key sk-test-secret-abc"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func cleanseTestRedactor() *config.Redactor {
	return config.NewRedactor([]config.SecretEntry{{Name: "test", Value: "sk-test-secret-abc"}}, nil)
}

// TestCleanseFileRedactsInPlaceWithBackup is the core contract: the secret
// leaves the transcript, the placeholder takes its place, and the original
// survives as <path>.bak.
func TestCleanseFileRedactsInPlaceWithBackup(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	dataDir := t.TempDir()
	path := cleanseTestTranscript(t, dataDir)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	red := cleanseTestRedactor()
	if red == nil {
		t.Fatal("NewRedactor returned nil")
	}

	// Dry run: report only.
	dry, err := cleanseFile(path, red, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry.Applied || dry.LinesChanged != 1 || dry.Placeholders == 0 {
		t.Fatalf("dry run report = %+v", dry)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, original) {
		t.Fatal("dry run rewrote the transcript")
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Fatal("dry run wrote a backup")
	}

	// Apply.
	done, err := cleanseFile(path, red, false)
	if err != nil {
		t.Fatalf("cleanse: %v", err)
	}
	if !done.Applied || done.Backup != path+".bak" || done.SessionID != "abcdefgh" {
		t.Fatalf("cleanse report = %+v", done)
	}
	cleansed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cleansed: %v", err)
	}
	if strings.Contains(string(cleansed), "sk-test-secret-abc") {
		t.Fatal("the secret survived the cleanse")
	}
	if !strings.Contains(string(cleansed), "$$") {
		t.Fatalf("no placeholder in the cleansed transcript:\n%s", cleansed)
	}
	if !strings.Contains(string(cleansed), "here is the key") {
		t.Fatal("cleansing dropped the surrounding text")
	}
	backup, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if !bytes.Equal(backup, original) {
		t.Fatal("the backup is not the original transcript")
	}

	// Idempotent: a cleansed transcript has no secret left to match.
	again, err := cleanseFile(path, red, false)
	if err != nil {
		t.Fatalf("second cleanse: %v", err)
	}
	if again.LinesChanged != 0 || again.Applied {
		t.Fatalf("second cleanse changed %d lines (want 0): %+v", again.LinesChanged, again)
	}
}

// TestCleanseCmdSurfaces pins the CLI: selector by path, JSON report, and the
// honest "nothing matched" case.
func TestCleanseCmdSurfaces(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	dataDir := t.TempDir()
	path := cleanseTestTranscript(t, dataDir)
	cwd := t.TempDir()
	red := cleanseTestRedactor()

	var out, errOut bytes.Buffer
	code := cleanseCmd([]string{"--session", path, "--dry-run"}, cwd, &out, &errOut, func() *config.Redactor { return red })
	if code != 0 {
		t.Fatalf("cleanseCmd = %d (%s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "would redact") || !strings.Contains(out.String(), "abcdefgh") {
		t.Fatalf("dry-run output:\n%s", out.String())
	}

	// A freshly written transcript looks live: the guard refuses to touch it
	// without --force (several xdev processes share a working directory).
	out.Reset()
	if code := cleanseCmd([]string{"--session", path}, cwd, &out, &errOut, func() *config.Redactor { return red }); code != 1 {
		t.Fatalf("guard exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "live session") || !strings.Contains(errOut.String(), "--force") {
		t.Fatalf("guard output:\n%s", errOut.String())
	}
	if strings.Contains(out.String(), "redacted") {
		t.Fatal("the guard allowed the write")
	}

	out.Reset()
	errOut.Reset()
	code = cleanseCmd([]string{"--session", path, "--force"}, cwd, &out, &errOut, func() *config.Redactor { return red })
	if code != 0 || !strings.Contains(out.String(), "redacted") {
		t.Fatalf("cleanseCmd --force = %d (%s)", code, out.String())
	}

	out.Reset()
	code = cleanseCmd([]string{"--session", path, "--json", "--force"}, cwd, &out, &errOut, func() *config.Redactor { return red })
	if code != 0 {
		t.Fatalf("cleanseCmd --json = %d", code)
	}
	var stats cleanseStats
	if err := json.Unmarshal(out.Bytes(), &stats); err != nil {
		t.Fatalf("json report: %v (%s)", err, out.String())
	}
	if stats.LinesChanged != 0 || stats.Placeholders != 0 {
		t.Fatalf("re-cleansing an already cleansed transcript reported %+v", stats)
	}

	// A redactor that matches nothing explains itself instead of pretending.
	out.Reset()
	noMatch := config.NewRedactor([]config.SecretEntry{{Name: "none", Value: "value-that-is-absent"}}, nil)
	if code := cleanseCmd([]string{"--session", path, "--force"}, cwd, &out, &errOut, func() *config.Redactor { return noMatch }); code != 0 {
		t.Fatalf("no-match cleanse = %d", code)
	}
	if !strings.Contains(out.String(), "nothing matched") {
		t.Fatalf("no-match output:\n%s", out.String())
	}

	// An unusable selector is an error, not a silent no-op.
	out.Reset()
	if code := cleanseCmd([]string{"--session", "no-such-session"}, cwd, &out, &errOut, func() *config.Redactor { return red }); code != 1 {
		t.Fatalf("unresolvable selector exit = %d, want 1", code)
	}
}
