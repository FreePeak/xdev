package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/session"
)

// claudeFixture is a minimal Claude Code transcript in the real wire shape
// (see internal/session/import_claude.go for the verified field set).
const claudeFixture = `{"type":"mode","mode":"normal","sessionId":"11111111-2222-3333-4444-555555555555"}
{"parentUuid":null,"isSidechain":false,"type":"user","timestamp":"2026-09-06T15:20:53.314Z","message":{"role":"user","content":"fix the parser"}}
{"parentUuid":"a1","isSidechain":false,"type":"assistant","timestamp":"2026-09-06T15:21:28.134Z","message":{"id":"m1","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"Looking now."}]}}
{"type":"ai-title","aiTitle":"parser fix","sessionId":"11111111-2222-3333-4444-555555555555"}
`

// codexFixture is a minimal Codex rollout in the real wire shape.
const codexFixture = `{"timestamp":"2026-04-28T05:33:29.716Z","type":"session_meta","payload":{"id":"019dd293-fc90-7023-a178-f09154ca651e","cwd":"/tmp/import-proj"}}
{"timestamp":"2026-04-28T05:34:00.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"summarize the build config"}]}}
{"timestamp":"2026-04-28T05:34:10.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"It builds everything."}]}}
`

// TestSplitForeignQuery: /resume @claude|@codex recognition (bare = list).
func TestSplitForeignQuery(t *testing.T) {
	for _, c := range []struct {
		in, kind, ref string
		ok            bool
	}{
		{"@claude", "claude", "", true},
		{"@codex", "codex", "", true},
		{"@claude ab12", "claude", "ab12", true},
		{"@codex  rollout-2026-04-28T12-33-27-019dd293-fc90-7023-a178-f09154ca651e", "codex",
			"rollout-2026-04-28T12-33-27-019dd293-fc90-7023-a178-f09154ca651e", true},
		{"  @claude   spaced id  ", "claude", "spaced id", true},
		{"@claudex", "", "", false},
		{"@claude-x", "", "", false},
		{"ab12", "", "", false},
		{"", "", "", false},
	} {
		kind, ref, ok := splitForeignQuery(c.in)
		if ok != c.ok || kind != c.kind || ref != c.ref {
			t.Errorf("splitForeignQuery(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, kind, ref, ok, c.kind, c.ref, c.ok)
		}
	}
}

// setupForeignHome writes fixture transcripts into a temp HOME and returns
// the cwd they belong to.
func setupForeignHome(t *testing.T) (home, cwd string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	cwd = "/tmp/import-proj"

	claudeDir := filepath.Join(home, ".claude", "projects", session.ClaudeProjectSlug(cwd))
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "11111111-2222-3333-4444-555555555555.jsonl"),
		[]byte(claudeFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	codexDir := filepath.Join(home, ".codex", "sessions", "2026", "04", "28")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir,
		"rollout-2026-04-28T12-33-27-019dd293-fc90-7023-a178-f09154ca651e.jsonl"),
		[]byte(codexFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	return home, cwd
}

// TestImportForeignSessionCLI: --from-claude/--from-codex resolve a foreign
// id prefix, import it as a NEW on-disk session, and leave the source
// untouched; the listing helper is cwd-filtered.
func TestImportForeignSessionCLI(t *testing.T) {
	home, cwd := setupForeignHome(t)

	listed, err := listForeignTranscripts("claude", cwd)
	if err != nil || len(listed) != 1 {
		t.Fatalf("claude listing = %v err=%v", listed, err)
	}
	if other, err := listForeignTranscripts("claude", "/elsewhere"); err != nil || len(other) != 0 {
		t.Fatalf("claude listing must be cwd-filtered, got %v err=%v", other, err)
	}
	if cx, err := listForeignTranscripts("codex", cwd); err != nil || len(cx) != 1 {
		t.Fatalf("codex listing = %v err=%v", cx, err)
	}

	src := filepath.Join(home, ".claude", "projects", session.ClaudeProjectSlug(cwd),
		"11111111-2222-3333-4444-555555555555.jsonl")
	before, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	st, err := importForeignSession("claude", "11111111", cwd)
	if err != nil {
		t.Fatal(err)
	}
	if st.Path() == "" {
		t.Fatal("imported session must be on disk (--from-claude continues it)")
	}
	if !strings.HasPrefix(st.Title(), "imported: claude") {
		t.Fatalf("title = %q", st.Title())
	}
	if st.CWD() != cwd {
		t.Fatalf("cwd = %q, want the importer's cwd", st.CWD())
	}
	// Round-trip: the new file reopens and carries both turns.
	reopened, err := session.Open(st.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.Entries()) != 3 { // import header + user + assistant
		t.Fatalf("imported entries = %d, want 3", len(reopened.Entries()))
	}
	after, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("foreign source was mutated")
	}

	// Codex id prefix resolves the rollout and imports it.
	cx, err := importForeignSession("codex", "019dd293", cwd)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cx.Title(), "imported: codex summarize the build config") {
		t.Fatalf("codex title = %q", cx.Title())
	}

	// Unknown prefix is an error, never a silent new session.
	if _, err := importForeignSession("claude", "zzzz", cwd); err == nil {
		t.Fatal("expected error for unmatched transcript")
	}
}

// TestOpenStartupSessionForeign: the startup resolver honors
// --from-claude/--from-codex, rejects both-at-once, and still handles
// plain --resume afterwards.
func TestOpenStartupSessionForeign(t *testing.T) {
	_, cwd := setupForeignHome(t)

	if _, err := openStartupSession(cwd, printOptions{FromClaude: "1111", FromCodex: "019d"}); err == nil {
		t.Fatal("both foreign flags must be rejected")
	}

	st, err := openStartupSession(cwd, printOptions{FromCodex: "019dd293"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(st.Title(), "imported: codex") {
		t.Fatalf("startup store = %q", st.Title())
	}

	// A new session (no flags) still works: memory-only until used.
	fresh, err := openStartupSession(cwd, printOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Path() != "" {
		t.Fatalf("plain startup must not materialize a file, got %q", fresh.Path())
	}
}

// TestForkSessionByID: --fork <id|path> copies the session into a new file
// with a parentSession header, and the fork reopens with the same entries.
func TestForkSessionByID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := "/tmp/fork-proj"

	src := session.OpenMem(cwd, "fork source")
	// Production writes the file name from the store's own id, so resolve
	// by that same id (the header is what resolveResumeID matches).
	srcPath := session.SessionFilePath(config.DataDir(), cwd, timeNow(), src.ID())
	if _, err := src.EnsureOnDisk(srcPath, session.Options{}); err != nil {
		t.Fatal(err)
	}
	if err := src.Append(userMessage(t, "hello fork")); err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	srcID := src.ID()

	// By id prefix (case-insensitive).
	fork, err := forkSessionByID(cwd, strings.ToLower(srcID[:6]))
	if err != nil {
		t.Fatal(err)
	}
	if fork.Path() == srcPath || fork.ID() == srcID {
		t.Fatalf("fork did not mint a new session: %q", fork.Path())
	}

	assertParent(t, fork, srcID)
	// By direct path.
	byPath, err := forkSessionByID(cwd, srcPath)
	if err != nil {
		t.Fatal(err)
	}
	assertParent(t, byPath, srcID)

	// A bad reference errors with both possibilities named.
	if _, err := forkSessionByID(cwd, "zzzz"); err == nil {
		t.Fatal("expected error for unresolved fork source")
	}

	// --fork takes precedence in the startup resolver.
	st, err := openStartupSession(cwd, printOptions{ForkID: strings.ToLower(srcID[:6])})
	if err != nil {
		t.Fatal(err)
	}
	assertParent(t, st, srcID)
}

func timeNow() time.Time { return time.Now() }

func userMessage(t *testing.T, text string) *session.MessageEntry {
	t.Helper()
	return &session.MessageEntry{Message: ai.Message{
		Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: text}},
	}}
}

// assertParent reads the session header off disk and checks parentSession.
func assertParent(t *testing.T, st *session.Store, want string) {
	t.Helper()
	raw, err := os.ReadFile(st.Path())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	if len(lines) < 2 {
		t.Fatalf("session file has %d lines", len(lines))
	}
	hdr, ok := session.ParseHeader([]byte(lines[1]))
	if !ok {
		t.Fatalf("line 2 is not a session header: %q", lines[1])
	}
	if hdr.ParentSession != want {
		t.Fatalf("parentSession = %q, want %q", hdr.ParentSession, want)
	}
	// A fork carries the source entries.
	reopened, err := session.Open(st.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.Entries()) == 0 {
		t.Fatal("fork lost the source transcript")
	}
}
