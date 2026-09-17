package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// compressTestSession writes a real session file with enough history that a
// compaction cut exists for a small kept tail.
func compressTestSession(t *testing.T, dataDir, cwd string) string {
	t.Helper()
	store := session.OpenMem(cwd, "compress test")
	path := session.SessionFilePath(dataDir, cwd, time.Now(), store.ID())
	store.EnableAutoPersist(path, session.Options{})
	now := time.Now().UnixMilli()
	// Multi-line turns on purpose: the deterministic members keep the first
	// line of each message (and a head of each tool result), so a realistic
	// transcript is what shows the reduction — one-line messages have
	// nothing left to elide.
	turns := []struct {
		role ai.Role
		text string
	}{
		{ai.RoleUser, "first question\n" + strings.Repeat("alpha alpha alpha\n", 40)},
		{ai.RoleAssistant, "first answer\n" + strings.Repeat("bravo bravo bravo\n", 40)},
		{ai.RoleUser, "second question\n" + strings.Repeat("charlie charlie charlie\n", 40)},
		{ai.RoleAssistant, "second answer\n" + strings.Repeat("delta delta delta\n", 40)},
		{ai.RoleUser, "third question\n" + strings.Repeat("echo echo echo\n", 40)},
		{ai.RoleAssistant, "third answer\n" + strings.Repeat("foxtrot foxtrot foxtrot\n", 40)},
	}
	for _, turn := range turns {
		msg := ai.Message{Role: turn.role, Content: []ai.Block{ai.TextBlock{Text: turn.text}}, UserTS: now}
		if err := store.Append(&session.MessageEntry{Message: msg}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return path
}

// compressTestShortID recovers the session id from the canonical
// "<timestamp>_<id>.jsonl" file name.
func compressTestShortID(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if i := strings.LastIndexByte(base, '_'); i >= 0 {
		base = base[i+1:]
	}
	return compressShort(base)
}

// TestCompressSessionCompactsDeterministically is the core contract: the
// session gains a ladder-tagged compaction entry, the context the store
// rebuilds shrinks, and the raw entries stay on disk.
func TestCompressSessionCompactsDeterministically(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dataDir)
	cwd := t.TempDir()
	path := compressTestSession(t, dataDir, cwd)

	ctx := context.Background()
	dry, err := compressSession(ctx, path, "shake", 30, true, nil, "")
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry.Applied {
		t.Fatal("dry run must not write the compaction entry")
	}
	if dry.Dropped < 1 || dry.TokensAfter >= dry.TokensBefore {
		t.Fatalf("dry run plan = dropped %d, tokens %d -> %d (want a shrinking plan)", dry.Dropped, dry.TokensBefore, dry.TokensAfter)
	}

	done, err := compressSession(ctx, path, "shake", 30, false, nil, "")
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !done.Applied || done.EntryID == "" {
		t.Fatalf("compaction was not persisted: %+v", done)
	}

	store, err := session.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store.Close()
	var compaction *session.CompactionEntry
	messages := 0
	for _, e := range store.Entries() {
		switch typed := e.(type) {
		case *session.CompactionEntry:
			compaction = typed
		case *session.MessageEntry:
			messages++
		}
	}
	if compaction == nil {
		t.Fatal("no compaction entry in the session file")
	}
	if compaction.Method != "shake" {
		t.Fatalf("compaction method = %q, want shake", compaction.Method)
	}
	if compaction.FirstKeptEntryID == nil || *compaction.FirstKeptEntryID != done.Anchor {
		t.Fatalf("anchor = %v, want %s", compaction.FirstKeptEntryID, done.Anchor)
	}
	if messages != 6 {
		t.Fatalf("raw message entries = %d, want 6 (compaction must not rewrite history)", messages)
	}
	if compaction.TokensBefore != done.TokensBefore || compaction.TokensBefore == 0 {
		t.Fatalf("entry recorded %d tokens before, report says %d", compaction.TokensBefore, done.TokensBefore)
	}

	// The rebuilt context now starts at the retained summary: the dropped
	// prefix is gone from what the model would see, which is the whole point.
	ctxRes, err := session.BuildContext(store.Entries(), store.LeafID(), session.SystemPrompt{})
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if len(ctxRes.Messages) >= 6 {
		t.Fatalf("rebuilt context has %d messages, want fewer than 6", len(ctxRes.Messages))
	}
	if ctxRes.Messages[0].Role != ai.RoleAssistant || strings.TrimSpace(ctxRes.Messages[0].Text()) == "" {
		t.Fatalf("rebuilt context does not start at the retained summary: %+v", ctxRes.Messages[0])
	}
}

// TestCompressCmdReportsDryRun pins the CLI surface: no --session means this
// directory's newest session, and the report names the method and sizes.
func TestCompressCmdReportsDryRun(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dataDir)
	cwd := t.TempDir()
	path := compressTestSession(t, dataDir, cwd)

	var out, errOut bytes.Buffer
	code := compressCmd([]string{"--keep-recent", "30", "--dry-run"}, cwd, nil, &out, &errOut)
	if code != 0 {
		t.Fatalf("compressCmd = %d (stderr: %s)", code, errOut.String())
	}
	got := out.String()
	for _, want := range []string{"would compact", compressTestShortID(path), "shake", "tokens", "dry run"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "  entry        ") {
		t.Fatalf("dry run reported a written entry:\n%s", got)
	}
}

// TestCompressCmdGuardsLiveSessions pins the guard that keeps a CLI compaction
// from appending to a transcript a running xdev still holds open.
func TestCompressCmdGuardsLiveSessions(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dataDir)
	cwd := t.TempDir()
	path := compressTestSession(t, dataDir, cwd)

	var out, errOut bytes.Buffer
	if code := compressCmd([]string{"--keep-recent", "30"}, cwd, nil, &out, &errOut); code != 1 {
		t.Fatalf("unguarded compress exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "live session") || !strings.Contains(errOut.String(), "--force") {
		t.Fatalf("guard output:\n%s", errOut.String())
	}
	if compressTestHasCompaction(t, path) {
		t.Fatal("the guard allowed the write")
	}

	out.Reset()
	errOut.Reset()
	if code := compressCmd([]string{"--keep-recent", "30", "--force"}, cwd, nil, &out, &errOut); code != 0 {
		t.Fatalf("compress --force = %d (%s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "compacted") {
		t.Fatalf("forced compress output:\n%s", out.String())
	}
	if !compressTestHasCompaction(t, path) {
		t.Fatal("--force did not write the compaction entry")
	}
}

// compressTestHasCompaction reports whether the session carries a compaction
// entry yet.
func compressTestHasCompaction(t *testing.T, path string) bool {
	t.Helper()
	store, err := session.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	for _, e := range store.Entries() {
		if _, ok := e.(*session.CompactionEntry); ok {
			return true
		}
	}
	return false
}

// TestDefaultCompressMethodStaysOffline pins the safety rule: an unattended
// compaction never picks the provider summarize.
func TestDefaultCompressMethodStaysOffline(t *testing.T) {
	if got := defaultCompressMethod(nil); got != "shake" {
		t.Fatalf("defaultCompressMethod(nil) = %q, want shake", got)
	}
}
