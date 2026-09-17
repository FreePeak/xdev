package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
)

// The fixtures mirror REAL Claude Code / Codex transcripts (shapes verified
// against live ~/.claude/projects and ~/.codex/sessions files, 2026-09).

func testdataPath(t *testing.T, parts ...string) string {
	t.Helper()
	return filepath.Join(append([]string{"testdata"}, parts...)...)
}

func importEntries(t *testing.T, s *Store) []Entry {
	t.Helper()
	return s.Entries()
}

// TestImportClaudeFixture: real-shaped transcript maps to user/assistant/
// toolResult entries, sidechains and metadata are skipped, the ai-title
// becomes the title, and the header entry records the source.
func TestImportClaudeFixture(t *testing.T) {
	src := testdataPath(t, "claude", "11111111-2222-3333-4444-555555555555.jsonl")
	res, err := ImportClaude(src, t.TempDir(), "/tmp/work")
	if err != nil {
		t.Fatal(err)
	}
	if res.Turns != 5 {
		t.Fatalf("turns = %d, want 5 (user, thinking, text+tool_use, tool_result, user)", res.Turns)
	}
	if res.ToolCalls != 1 || res.Dropped != 1 {
		t.Fatalf("toolCalls=%d dropped=%d, want 1/1 (sidechain skipped)", res.ToolCalls, res.Dropped)
	}
	if res.Store.Title() != "imported: claude router concurrent sessions" {
		t.Fatalf("title = %q", res.Store.Title())
	}
	if res.Store.CWD() != "/tmp/work" {
		t.Fatalf("cwd = %q", res.Store.CWD())
	}

	// Header entry first, recording source + origin path.
	entries := importEntries(t, res.Store)
	hdr, ok := entries[0].(*CustomEntry)
	if !ok || hdr.CustomType != "imported" {
		t.Fatalf("first entry = %T, want imported CustomEntry", entries[0])
	}
	if hdr.Data["source"] != "claude" || hdr.Data["path"] != src {
		t.Fatalf("header data = %v", hdr.Data)
	}

	// Round-trip: reopen the file from disk and check the mapped messages.
	reopened, err := Open(res.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	msgs := reopened.Entries()
	if len(msgs) != 6 {
		t.Fatalf("reopened %d entries, want 6 (header + 5 turns)", len(msgs))
	}
	var call *ai.ToolCallBlock
	var result *ai.Message
	for _, e := range msgs {
		me, ok := e.(*MessageEntry)
		if !ok {
			continue
		}
		for _, b := range me.Message.Content {
			if tc, ok := b.(ai.ToolCallBlock); ok {
				c := tc
				call = &c
			}
		}
		if me.Message.Role == ai.RoleToolResult {
			m := me.Message
			result = &m
		}
	}
	if call == nil || call.Name != "Grep" || !strings.Contains(string(call.Arguments), `"pattern":"session"`) {
		t.Fatalf("tool call not preserved: %+v", call)
	}
	if result == nil || result.ToolCallID != "toolu_01" || result.ToolName != "Grep" ||
		result.Text() != "store.go: session registry" {
		t.Fatalf("tool result not paired: %+v", result)
	}
	// Source file untouched: content is byte-identical after import.
	after, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), `"type":"ai-title"`) || len(after) != 1549 {
		t.Fatalf("source file mutated: %d bytes", len(after))
	}
}

// TestImportClaudeTimestampsPreserved: foreign stamps carry into entries.
func TestImportClaudeTimestampsPreserved(t *testing.T) {
	res, err := ImportClaude(testdataPath(t, "claude", "11111111-2222-3333-4444-555555555555.jsonl"),
		t.TempDir(), "/tmp/work")
	if err != nil {
		t.Fatal(err)
	}
	entries := importEntries(t, res.Store)
	me := entries[1].(*MessageEntry)
	want := time.Date(2026, 9, 6, 15, 20, 53, int(314*time.Millisecond), time.UTC)
	if !me.Env.Timestamp.Equal(want) {
		t.Fatalf("stamp = %v, want %v", me.Env.Timestamp, want)
	}
}

// TestImportCodexFixture: preambles and developer/turn_context lines are
// dropped, reasoning maps to thinking, function_call/output pair up.
func TestImportCodexFixture(t *testing.T) {
	src := testdataPath(t, "codex", "rollout-2026-04-28T12-33-27-019dd293-fc90-7023-a178-f09154ca651e.jsonl")
	res, err := ImportCodex(src, t.TempDir(), "/here")
	if err != nil {
		t.Fatal(err)
	}
	// turns: user, reasoning, function_call, function_call_output, assistant
	if res.Turns != 5 || res.ToolCalls != 1 {
		t.Fatalf("turns=%d toolCalls=%d, want 5/1", res.Turns, res.ToolCalls)
	}
	if res.Dropped != 2 { // environment_context preamble + developer message
		t.Fatalf("dropped=%d, want 2", res.Dropped)
	}
	if res.Store.Title() != "imported: codex summarize the build config" {
		t.Fatalf("title = %q", res.Store.Title())
	}
	reopened, err := Open(res.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	entries := reopened.Entries()
	hdr, ok := entries[0].(*CustomEntry)
	if !ok || hdr.Data["source"] != "codex" {
		t.Fatalf("header = %T %v", entries[0], hdr)
	}
	sawThinking, sawCall := false, false
	for _, e := range entries {
		me, ok := e.(*MessageEntry)
		if !ok {
			continue
		}
		switch me.Message.Role {
		case ai.RoleAssistant:
			for _, b := range me.Message.Content {
				switch tb := b.(type) {
				case ai.ThinkingBlock:
					if tb.Thinking != "Reading Makefile first." {
						t.Fatalf("thinking = %q", tb.Thinking)
					}
					sawThinking = true
				case ai.ToolCallBlock:
					if tb.Name != "shell" || tb.ID != "call_abc" {
						t.Fatalf("call = %+v", tb)
					}
					var args map[string]any
					if json.Unmarshal(tb.Arguments, &args) != nil || args["cmd"] == nil {
						t.Fatalf("args = %s", tb.Arguments)
					}
					sawCall = true
				}
			}
		case ai.RoleToolResult:
			if me.Message.ToolCallID != "call_abc" || me.Message.ToolName != "shell" ||
				!strings.Contains(me.Message.Text(), "all: build") {
				t.Fatalf("tool result = %+v %q", me.Message.ToolCallID, me.Message.Text())
			}
		}
	}
	if !sawThinking || !sawCall {
		t.Fatalf("missing mapping: thinking=%v call=%v", sawThinking, sawCall)
	}
}

// TestImportForeignNoTurns: a transcript without conversation turns errors
// instead of littering an empty session file.
func TestImportForeignNoTurns(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(src, []byte("{\"type\":\"mode\",\"mode\":\"normal\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportClaude(src, dir, "/tmp"); err == nil {
		t.Fatal("expected error for a transcript with no turns")
	}
	if _, err := os.Stat(filepath.Join(dir, "sessions")); err == nil {
		t.Fatal("empty import must not materialize a session file")
	}
}

// TestListAndResolveForeign: listing is newest-first and cwd-filtered per
// source layout; resolution handles id prefixes and path prefixes.
func TestListAndResolveForeign(t *testing.T) {
	home := t.TempDir()

	// Claude: one transcript in this project's encoded dir, one elsewhere
	// (newer mtime, must be filtered out by cwd), plus an old one here.
	projSlug := ClaudeProjectSlug("/tmp/work")
	otherSlug := ClaudeProjectSlug("/elsewhere")
	here := filepath.Join(home, ".claude", "projects", projSlug)
	other := filepath.Join(home, ".claude", "projects", otherSlug)
	for _, d := range []string{here, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p string) {
		if err := os.WriteFile(p, []byte("{\"type\":\"mode\"}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	newTime := time.Now()
	write(filepath.Join(here, "aaaaaaaa-0000-0000-0000-000000000001.jsonl"))
	write(filepath.Join(here, "bbbbbbbb-0000-0000-0000-000000000002.jsonl"))
	write(filepath.Join(other, "cccccccc-0000-0000-0000-000000000003.jsonl"))
	os.Chtimes(filepath.Join(here, "aaaaaaaa-0000-0000-0000-000000000001.jsonl"), oldTime, oldTime)
	os.Chtimes(filepath.Join(here, "bbbbbbbb-0000-0000-0000-000000000002.jsonl"), newTime, newTime)
	os.Chtimes(filepath.Join(other, "cccccccc-0000-0000-0000-000000000003.jsonl"), newTime, newTime)

	// Codex: a dated rollout tree.
	codexDir := filepath.Join(home, ".codex", "sessions", "2026", "04", "28")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	codexFile := filepath.Join(codexDir,
		"rollout-2026-04-28T12-33-27-019dd293-fc90-7023-a178-f09154ca651e.jsonl")
	if err := os.WriteFile(codexFile, []byte(
		`{"type":"session_meta","payload":{"id":"019dd293-fc90-7023-a178-f09154ca651e","cwd":"/tmp/work"}}`+"\n"+
			`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}`+"\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}

	// Claude listing: cwd filter keeps this project's two, newest first.
	cl, err := ListForeignRoot("claude", ClaudeProjectsRoot(home), "/tmp/work")
	if err != nil {
		t.Fatal(err)
	}
	if len(cl) != 2 || filepath.Base(cl[0].Path) != "bbbbbbbb-0000-0000-0000-000000000002.jsonl" {
		t.Fatalf("claude list = %v", cl)
	}
	if cl[0].ID != "bbbbbbbb-0000-0000-0000-000000000002" {
		t.Fatalf("claude id = %q", cl[0].ID)
	}

	// Codex listing: session_meta cwd filter.
	cx, err := ListForeignRoot("codex", CodexSessionsRoot(home), "/tmp/work")
	if err != nil {
		t.Fatal(err)
	}
	if len(cx) != 1 || cx[0].CWD != "/tmp/work" {
		t.Fatalf("codex list = %v", cx)
	}
	if cx[0].ID != "019dd293-fc90-7023-a178-f09154ca651e" {
		t.Fatalf("codex id = %q", cx[0].ID)
	}
	// ...and excluded from another cwd.
	cxOther, err := ListForeignRoot("codex", CodexSessionsRoot(home), "/nope")
	if err != nil {
		t.Fatal(err)
	}
	if len(cxOther) != 0 {
		t.Fatalf("codex cwd filter leaked %v", cxOther)
	}

	// Resolution: id prefix (case-insensitive) for both kinds.
	for _, c := range []struct{ kind, q string }{
		{"claude", "AAAA"}, {"claude", "bbbb"}, {"codex", "019DD2"}, {"codex", "019dd293"},
	} {
		root := ClaudeProjectsRoot(home)
		if c.kind == "codex" {
			root = CodexSessionsRoot(home)
		}
		p, err := ResolveForeign(c.kind, c.q, root)
		if err != nil {
			t.Fatalf("resolve %s %q: %v", c.kind, c.q, err)
		}
		if !strings.Contains(p, strings.ToLower(c.q)) && !strings.Contains(p, c.q) {
			t.Fatalf("resolve %s %q = %q", c.kind, c.q, p)
		}
	}
	// Path-prefix resolution (no separator-free ambiguity involved).
	p, err := ResolveForeign("claude", filepath.Join(here, "aaaa"), here)
	if err != nil || filepath.Base(p) != "aaaaaaaa-0000-0000-0000-000000000001.jsonl" {
		t.Fatalf("path resolve = %q err=%v", p, err)
	}
	// Misses error.
	if _, err := ResolveForeign("claude", "zzzz", ClaudeProjectsRoot(home)); err == nil {
		t.Fatal("expected error for unmatched claude prefix")
	}
}
