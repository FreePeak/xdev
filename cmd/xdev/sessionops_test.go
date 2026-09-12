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

	"github.com/FreePeak/xdev/internal/theme"
	"github.com/FreePeak/xdev/internal/tui"
	"github.com/gdamore/tcell/v2"
)

// TestBreadcrumbRoundTrip: save/read keyed by the terminal, overwrite wins.
func TestBreadcrumbRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir) // config.DataDir() derives from HOME
	_ = os.MkdirAll(filepath.Join(dir, ".xdev", "agent"), 0o755)

	saveBreadcrumb("/tmp/sess-a.jsonl")
	if got := readBreadcrumb(); got != "/tmp/sess-a.jsonl" {
		t.Fatalf("breadcrumb = %q, want /tmp/sess-a.jsonl", got)
	}
	saveBreadcrumb("/tmp/sess-b.jsonl")
	if got := readBreadcrumb(); got != "/tmp/sess-b.jsonl" {
		t.Fatalf("overwrite breadcrumb = %q, want /tmp/sess-b.jsonl", got)
	}
}

// TestResolveResumeID: exact-prefix match (case-insensitive), no-match
// error, empty query returns the newest session in cwd.
func TestResolveResumeID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := config.DataDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cwd := "/tmp/resume-test"
	older := session.SessionFilePath(dir, cwd, now.Add(-time.Hour), "AAAA1111-0000-0000-0000-000000000000")
	newer := session.SessionFilePath(dir, cwd, now, "BBBB2222-0000-0000-0000-000000000000")
	for _, p := range []string{older, newer} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		ts time.Time
		id string
	}{{now.Add(-time.Hour), "AAAA1111-0000-0000-0000-000000000000"},
		{now, "BBBB2222-0000-0000-0000-000000000000"}} {
		var b strings.Builder
		b.Write(session.MarshalTitleSlot("sess "+c.id[:4], session.TitleSourceAuto, now))
		b.Write(session.MarshalHeader(session.SessionHeader{
			Version: 3, ID: c.id, Timestamp: c.ts, CWD: cwd, Title: "t", TitleSource: session.TitleSourceAuto,
		}))
		b.WriteString("\n")
		if err := os.WriteFile(session.SessionFilePath(dir, cwd, c.ts, c.id), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Case-insensitive prefix.
	got, err := resolveResumeID(cwd, "aaaa")
	if err != nil || !strings.HasSuffix(got, "AAAA1111-0000-0000-0000-000000000000.jsonl") {
		t.Fatalf("resolve aaaa = %q err=%v", got, err)
	}
	// No match errors.
	if _, err := resolveResumeID(cwd, "zzzz"); err == nil {
		t.Fatal("expected error for zzzz")
	}
	// Empty query → newest (BBBB).
	got, err = resolveResumeID(cwd, "")
	if err != nil || !strings.HasSuffix(got, "BBBB2222-0000-0000-0000-000000000000.jsonl") {
		t.Fatalf("empty query should resolve newest, got %q err=%v", got, err)
	}
}

// TestDumpSession: markdown contains header and both message roles.
func TestDumpSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	src := session.OpenMem("/proj", "dump test")
	if err := src.Append(&session.MessageEntry{Env: session.Envelope{ID: "u1"}, Message: ai.Message{
		Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hello dump"}},
	}}); err != nil {
		t.Fatal(err)
	}
	path, err := dumpSession(src)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, "# xdev transcript") || !strings.Contains(s, "## User") || !strings.Contains(s, "hello dump") {
		t.Fatalf("dump content incomplete:\n%s", s)
	}
}

// TestReplayTranscriptIncludesThinking: resumed/branched sessions must show
// past reasoning blocks, and the showThinking toggle must gate them the
// same way it gates fresh turns.
func TestReplayTranscriptIncludesThinking(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	defer scr.Fini()
	scr.SetSize(80, 24)
	app := tui.New(scr, theme.Load("groknight"), "test/free", "sess")

	msgs := []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "question"}}},
		{Role: ai.RoleAssistant, Content: []ai.Block{
			ai.ThinkingBlock{Thinking: "pondering deeply"},
			ai.TextBlock{Text: "answer"},
		}},
	}
	replayTranscript(app, msgs)
	blocks := app.Blocks()
	if len(blocks) != 3 || blocks[0].Kind != tui.KindUser || blocks[1].Kind != tui.KindThinking || blocks[2].Kind != tui.KindAssistant {
		t.Fatalf("replay blocks = %v", blocks)
	}
	if blocks[1].Text != "pondering deeply" {
		t.Fatalf("thinking text = %q", blocks[1].Text)
	}
	// Toggle off: replay must drop the thinking block, keep the text.
	app.Reset()
	app.SetShowThinking(false)
	replayTranscript(app, msgs)
	blocks = app.Blocks()
	if len(blocks) != 2 || blocks[0].Kind != tui.KindUser || blocks[1].Kind != tui.KindAssistant {
		t.Fatalf("replay with thinking off = %v", blocks)
	}
	if blocks[1].Text != "answer" {
		t.Fatalf("assistant text = %q", blocks[1].Text)
	}
}

// writePickerSession lays down a session JSONL (title slot + header +
// one user prompt line) for picker/search/delete tests.
func writePickerSession(t *testing.T, cwd, id, title, prompt string) string {
	t.Helper()
	now := time.Now().UTC()
	path := session.SessionFilePath(config.DataDir(), cwd, now, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.Write(session.MarshalTitleSlot(title, session.TitleSourceAuto, now))
	b.Write(session.MarshalHeader(session.SessionHeader{
		Version: 3, ID: id, Timestamp: now, CWD: cwd, Title: title, TitleSource: session.TitleSourceAuto,
	}))
	if prompt != "" {
		line, err := session.MarshalEntry(&session.MessageEntry{
			Env:     session.Envelope{ID: id[:8] + "-0000"},
			Message: ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: prompt}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSessionPinsRoundTrip: toggle persists to session-pins.json, toggle
// back clears it.
func TestSessionPinsRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(config.DataDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := toggleSessionPin("aaaa1111"); err != nil {
		t.Fatal(err)
	}
	if !loadSessionPins()["aaaa1111"] {
		t.Fatal("pin not persisted")
	}
	b, err := os.ReadFile(pinsPath())
	if err != nil || !strings.Contains(string(b), "aaaa1111") {
		t.Fatalf("sidecar = %q err=%v", b, err)
	}
	if err := toggleSessionPin("aaaa1111"); err != nil {
		t.Fatal(err)
	}
	if pins := loadSessionPins(); len(pins) != 0 {
		t.Fatalf("unpin left %v", pins)
	}
}

// TestDeleteSessionRemovesJSONLAndPin: confirmed delete removes the
// session JSONL and its pin, refuses the active session, and errors on
// unknown ids.
func TestDeleteSessionRemovesJSONLAndPin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := "/tmp/picker-del"
	path := writePickerSession(t, cwd, "AAAA1111-0000-0000-0000-000000000000", "delete me", "hello")
	if err := toggleSessionPin("AAAA1111"); err != nil {
		t.Fatal(err)
	}
	if err := deleteSessionByShortID("AAAA1111", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("jsonl still present: %v", err)
	}
	if loadSessionPins()["AAAA1111"] {
		t.Fatal("pin survived the delete")
	}
	active := writePickerSession(t, cwd, "BBBB2222-0000-0000-0000-000000000000", "active", "hi")
	if err := deleteSessionByShortID("BBBB2222", active); err == nil {
		t.Fatal("deleting the active session must be refused")
	}
	if _, err := os.Stat(active); err != nil {
		t.Fatalf("active session file was removed: %v", err)
	}
	if err := deleteSessionByShortID("zzzz9999", ""); err == nil {
		t.Fatal("unknown id must error")
	}
}

// TestSearchPickerItemsRanksMatches: all tokens must hit; id/title
// matches outrank prompt-text (body) matches; other projects' bodies
// count too; body-only non-matches drop out.
func TestSearchPickerItemsRanksMatches(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := "/tmp/picker-search"
	writePickerSession(t, cwd, "AAAA1111-0000-0000-0000-000000000000", "alpha parser", "irrelevant prompt")
	writePickerSession(t, cwd, "BBBB2222-0000-0000-0000-000000000000", "unrelated", "please fix the alpha parser now; alpha again")
	writePickerSession(t, cwd, "CCCC3333-0000-0000-0000-000000000000", "alpha only", "no second token here")
	writePickerSession(t, "/tmp/picker-other", "DDDD4444-0000-0000-0000-000000000000", "elsewhere", "alpha parser there")

	got := searchPickerItems(cwd, "alpha parser")
	if len(got) != 3 {
		t.Fatalf("matches = %v, want 3 (CCCC3333 lacks 'parser' in id/title/body)", got)
	}
	// id/title match first; then body matches by occurrence count
	// (BBBB2222 has 3, DDDD4444 has 2).
	if got[0].ID != "AAAA1111" || got[1].ID != "BBBB2222" || got[2].ID != "DDDD4444" {
		t.Fatalf("ranking = %v, want AAAA1111, BBBB2222, DDDD4444", got)
	}

	got = searchPickerItems(cwd, "alpha")
	if len(got) != 4 {
		t.Fatalf("single-token matches = %v, want all 4 sessions", got)
	}
}
