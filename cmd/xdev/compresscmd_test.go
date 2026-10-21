package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/agent"
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
	// The persisted record names the member that produced it (omp's
	// contract): the CLI ran shake, and the entry says so.
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

// TestCompressHandoffUsesTheStructuredPrompt drives the built binary's real
// compaction path (compressSession) with a handoff provider that records the
// system prompt it was handed, and pins the two prompt arms: the structured
// history template on an ordinary cut, and the split-turn prefix prompt when
// the kept tail lands inside a turn.
func TestCompressHandoffUsesTheStructuredPrompt(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dataDir)
	cwd := t.TempDir()
	path := compressTestSession(t, dataDir, cwd)

	// keepRecent 200 lands the cut inside the fixture's LAST turn — the split
	// arm — so this drive proves the real CLI path picks the prefix prompt for
	// a mid-turn cut rather than the generic one.
	prov := &compressPromptProvider{}
	stats, err := compressSession(context.Background(), path, agent.MethodHandoff, 200, true, prov, "test-model")
	if err != nil {
		t.Fatalf("compressSession: %v", err)
	}
	if stats.Method != agent.MethodHandoff {
		t.Fatalf("method = %q, want %q", stats.Method, agent.MethodHandoff)
	}
	if len(prov.systems) != 1 {
		t.Fatalf("summarize calls = %d, want 1", len(prov.systems))
	}
	if !strings.Contains(prov.systems[0], "Summarize the prefix to provide context for the retained suffix") {
		t.Errorf("a mid-turn cut must summarize the prefix, got %q", prov.systems[0])
	}

}

// compressPromptProvider records the system prompt of every summarize call it
// is handed and answers with a fixed structured summary, so the CLI path can be
// driven end to end without a provider.
type compressPromptProvider struct{ systems []string }

func (p *compressPromptProvider) Stream(_ context.Context, req ai.StreamRequest) (<-chan ai.Event, error) {
	p.systems = append(p.systems, req.System)
	ch := make(chan ai.Event, 2)
	ch <- ai.Donef(ai.StopReasonStop, nil, &ai.Message{
		Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "## Goal\nkeep going"}}, StopReason: ai.StopReasonStop,
	})
	close(ch)
	return ch, nil
}

func (p *compressPromptProvider) Name() string { return "compress-probe" }
func (p *compressPromptProvider) API() string  { return "compress-probe-api" }

// TestCompressHandoffKeepsWholeTurnsAboveTheAllowance is the counterpart drive:
// a kept tail large enough to swallow whole turns leaves the cut on a user
// message, so the summarize carries the structured history template rather than
// the prefix one.
func TestCompressHandoffKeepsWholeTurnsAboveTheAllowance(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dataDir)
	cwd := t.TempDir()
	path := compressTestSession(t, dataDir, cwd)

	// 250 tokens keeps the fixture's last whole turn (the user message at index 4
	// plus the reply after it), so the walk stops AT that user message: a turn
	// boundary, not a split.
	prov := &compressPromptProvider{}
	if _, err := compressSession(context.Background(), path, agent.MethodHandoff, 250, true, prov, "test-model"); err != nil {
		t.Fatalf("compressSession: %v", err)
	}
	if len(prov.systems) != 1 {
		t.Fatalf("summarize calls = %d, want 1", len(prov.systems))
	}
	if !strings.Contains(prov.systems[0], "## Goal") {
		t.Errorf("a turn-boundary cut must summarize with the structured prompt, got %q", prov.systems[0])
	}
	if strings.Contains(prov.systems[0], "PREFIX") {
		t.Errorf("a turn-boundary cut is not a split turn, got %q", prov.systems[0])
	}
}
