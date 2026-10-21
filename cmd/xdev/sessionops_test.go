package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/share"

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
		// A real message, not just the header: a session holding only its
		// title slot and header reads StatusEmpty and is not a resume target
		// (see resolveResumeID), so a header-only fixture tests nothing.
		line, merr := session.MarshalEntry(&session.MessageEntry{
			Env: session.Envelope{ID: "m1", ParentID: "", Timestamp: c.ts},
			Message: ai.Message{
				Role:    ai.RoleUser,
				Content: []ai.Block{ai.TextBlock{Text: "the prompt that made this session"}},
			},
		})
		if merr != nil {
			t.Fatal(merr)
		}
		b.Write(line)
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

// TestEmptySessionIsNotAResumeTarget is the guard on the empty-resume defect.
// A session file holding only its title slot and header — what /fork and the
// detach path write when a session never received a prompt — used to be the
// newest file in the directory, so --continue landed on it and every resume
// opened a blank transcript. Two assertions, both from the real on-disk shape:
// the newest-empty is skipped in favour of the older session that has a
// conversation, and naming it by prefix says why instead of resuming it.
func TestEmptySessionIsNotAResumeTarget(t *testing.T) {
	sandbox(t)
	cwd := t.TempDir()
	now := time.Now()
	withConversation := "CCCC3333-0000-0000-0000-000000000000"
	empty := "DDDD4444-0000-0000-0000-000000000000"
	for _, c := range []struct {
		ts  time.Time
		id  string
		msg bool
	}{
		{now.Add(-time.Hour), withConversation, true}, // an hour older
		{now, empty, false},                           // and the newest file
	} {
		var b strings.Builder
		b.Write(session.MarshalTitleSlot("sess "+c.id[:4], session.TitleSourceAuto, now))
		b.Write(session.MarshalHeader(session.SessionHeader{
			Version: 3, ID: c.id, Timestamp: c.ts, CWD: cwd, Title: "t", TitleSource: session.TitleSourceAuto,
		}))
		if c.msg {
			line, err := session.MarshalEntry(&session.MessageEntry{
				Env:     session.Envelope{ID: "m1", Timestamp: c.ts},
				Message: ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "real work"}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			b.Write(line)
		}
		p := session.SessionFilePath(sessionDataDir(), cwd, c.ts, c.id)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The newest session in cwd is the empty one; --continue must not pick it.
	got, err := resolveResumeID(cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, withConversation) {
		t.Fatalf("--continue resolved %s, want the session that has a conversation (%s)", got, withConversation)
	}

	// Naming it explicitly is an error that says why, not a blank transcript.
	if _, err := resolveResumeID(cwd, "dddd"); err == nil || !strings.Contains(err.Error(), "nothing to resume") {
		t.Fatalf("explicit --resume of an empty session = %v, want a nothing-to-resume error", err)
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

// TestReplayTranscriptRestoresToolResults: the dock's FILES section is read
// off the finished result blocks, so a resumed session that dropped them on
// replay showed an empty panel next to a transcript full of edits (#291).
func TestReplayTranscriptRestoresToolResults(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	defer scr.Fini()
	scr.SetSize(80, 24)
	app := tui.New(scr, theme.Load("groknight"), "test/free", "sess")

	diff := "--- a/app.go\n+++ b/app.go\n@@ -1 +1 @@\n-old\n+new\n"
	replayTranscript(app, []ai.Message{
		{Role: ai.RoleToolResult, ToolName: "edit", DurationMS: 70,
			Details: map[string]any{"unifiedDiff": diff, "linesBefore": 1, "linesAfter": 1}},
		{Role: ai.RoleToolResult, ToolName: "bash", IsError: true,
			Details: map[string]any{"exitCode": 1, "truncated": true}},
		// A result the store recorded without a duration must not claim 0s.
		{Role: ai.RoleToolResult, ToolName: "read"},
	})
	blocks := app.Blocks()
	// One call row plus one result row per tool result, in transcript order.
	if len(blocks) != 6 {
		t.Fatalf("replay blocks = %d, want 6", len(blocks))
	}
	edit := blocks[1]
	if edit.Kind != tui.KindToolDone || edit.Diff != diff {
		t.Fatalf("edit result = %+v, want the uniffed diff on a done block", edit)
	}
	if edit.Dur != "70ms" {
		t.Fatalf("duration = %q, want the recorded 70ms", edit.Dur)
	}
	bash := blocks[3]
	if !bash.Err || !bash.HasExit || bash.Exit != 1 || !bash.Truncated {
		t.Fatalf("bash result lost its outcome: %+v", bash)
	}
	if blocks[0].Kind != tui.KindTool || blocks[2].Kind != tui.KindTool {
		t.Fatalf("the call rows are missing: %+v", blocks)
	}
}

// TestReplayTranscriptTurnBudgetNotice: the turn-budget wrap-up prompt is
// harness text (#283 lineage). Replaying it as a ❯ block would invent a user
// turn that never happened; it must surface as the system event that ends the
// episode instead.
func TestReplayTranscriptTurnBudgetNotice(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	defer scr.Fini()
	scr.SetSize(80, 24)
	app := tui.New(scr, theme.Load("groknight"), "test/free", "sess")

	replayTranscript(app, []ai.Message{{
		Role:        ai.RoleUser,
		Content:     []ai.Block{ai.TextBlock{Text: "turn limit reached — wrap up the current step and report status"}},
		Attribution: "turn-limit",
	}})
	blocks := app.Blocks()
	if len(blocks) != 1 || blocks[0].Kind != tui.KindUser {
		t.Fatalf("wrap-up replayed as %v, want one user block", blocks)
	}
	if !strings.Contains(blocks[0].Text, "turn limit reached — wrap up the current step and report status") {
		t.Fatalf("the harness prompt missing from the transcript: %q", blocks[0].Text)
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

// TestWorkOfSumsRunSpans: the HUD's re-based total is the ACTIVE work a
// rebuilt history banked — provider-request spans AND tool spans, because the
// live timer (markRun) counts both and a resumed session reading at half what
// it showed before it closed is the defect. User messages carry no span and
// add nothing, and neither does a message from before durations were recorded.
func TestWorkOfSumsRunSpans(t *testing.T) {
	msgs := []ai.Message{
		{Role: ai.RoleUser},
		{Role: ai.RoleAssistant, DurationMS: 4000},
		{Role: ai.RoleToolResult, DurationMS: 9000},
		{Role: ai.RoleAssistant, DurationMS: 2500},
		{Role: ai.RoleAssistant},              // pre-timing history: contributes 0
		{Role: ai.RoleUser, DurationMS: 9999}, // no span of its own: contributes 0
	}
	if got := workOf(msgs); got != 15500*time.Millisecond {
		t.Fatalf("workOf = %v, want 15.5s", got)
	}
}

// TestUsageOfSumsPersistedBuckets: the token pill and /usage's cost line are
// read off these sums after a resume, so every bucket the wire reported has
// to land — the split is the point (a session that only summed input+output
// would read a 65k cached prompt as 479 fresh tokens). Messages with no usage
// (an older history, a provider that reports none) contribute nothing.
func TestUsageOfSumsPersistedBuckets(t *testing.T) {
	msgs := []ai.Message{
		{Role: ai.RoleUser},
		{Role: ai.RoleAssistant, Usage: &ai.Usage{
			Input: 479, Output: 1770, CacheRead: 64575, CacheWrite: 1100,
			TotalTokens: 66824, ReasoningTokens: 1264,
			Cost: &ai.UsageCost{Input: 0.001, Output: 0.011, CacheRead: 0.0003, Total: 0.0123},
		}},
		{Role: ai.RoleToolResult}, // no usage: contributes nothing
		{Role: ai.RoleAssistant, Usage: &ai.Usage{Input: 500, Output: 100, TotalTokens: 600}},
		{Role: ai.RoleAssistant}, // pre-usage history: contributes 0
	}
	in, out, cache, think, cacheWrite, cost := usageOf(msgs)
	if in != 979 || out != 1870 || cache != 64575 || think != 1264 || cacheWrite != 1100 {
		t.Fatalf("usageOf buckets = %d/%d/%d/%d/%d, want 979/1870/64575/1264/1100",
			in, out, cache, think, cacheWrite)
	}
	if cost < 0.0123-1e-9 || cost > 0.0123+1e-9 {
		t.Fatalf("usageOf cost = %v, want 0.0123", cost)
	}
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

// exportTestStore lays down a session carrying a message with markup, an
// assistant turn with thinking + a tool call, and a tool result.
func exportTestStore(t *testing.T) *session.Store {
	t.Helper()
	st := session.OpenMem("/proj/export", "export command test")
	entries := []session.Entry{
		&session.MessageEntry{Message: ai.Message{
			Role:    ai.RoleUser,
			Content: []ai.Block{ai.TextBlock{Text: "please read <script>alert(1)</script>"}},
		}},
		&session.MessageEntry{Message: ai.Message{
			Role: ai.RoleAssistant,
			Content: []ai.Block{
				ai.ThinkingBlock{Thinking: "which file?"},
				ai.TextBlock{Text: "reading main.go"},
				ai.ToolCallBlock{ID: "call-9", Name: "read", Arguments: []byte(`{"path":"main.go"}`)},
			},
			Model: "onegw/free",
		}},
		&session.MessageEntry{Message: ai.Message{
			Role:       ai.RoleToolResult,
			ToolCallID: "call-9",
			ToolName:   "read",
			IsError:    true,
			Content:    []ai.Block{ai.TextBlock{Text: "permission denied"}},
		}},
	}
	for _, e := range entries {
		if err := st.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

// TestExportSessionWritesHTML: the /export op writes one self-contained HTML
// file at the requested path, escapes transcript text, and carries the system
// prompt plus the metadata header.
func TestExportSessionWritesHTML(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	st := exportTestStore(t)

	path := filepath.Join(t.TempDir(), "nested", "session.html")
	got, err := exportSession(st, "SYS <b>prompt</b>", "onegw/live", path)
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Fatalf("written path = %q, want %q", got, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	html := string(data)
	for _, want := range []string{
		"<!doctype html>", "<style>", "SYS &lt;b&gt;prompt&lt;/b&gt;",
		"&lt;script&gt;alert(1)&lt;/script&gt;", "which file?", "main.go", "call-9",
		"permission denied", "onegw/live", st.ID()[:8], "/proj/export",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("export missing %q", want)
		}
	}
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Fatal("transcript markup was not escaped")
	}
	if strings.Contains(strings.ToLower(html), "<script") {
		t.Fatal("export must not embed any script element")
	}

	// No path: the default export dir, beside /dump's dumps.
	def, err := exportSession(st, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(config.DataDir(), "exports"); !strings.HasPrefix(def, want) {
		t.Fatalf("default export path = %q, want under %q", def, want)
	}
}

// TestExportEmptySessionIsValidHTML: exporting a session with no entries still
// writes a complete document.
func TestExportEmptySessionIsValidHTML(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "empty.html")
	if _, err := exportSession(session.OpenMem("/proj/empty", "empty"), "", "", path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	html := string(data)
	if !strings.HasPrefix(html, "<!doctype html>") || !strings.Contains(html, "</html>") {
		t.Fatalf("empty export is not a complete document:\n%.120s", html)
	}
	if strings.Count(html, "<article") != 0 {
		t.Fatal("empty export rendered entries")
	}
}

// TestRunExportNeverCreatesASession: --export reads a session; with nothing to
// read it fails instead of exporting a brand-new empty one.
// The -export refuse-to-clobber guard (docs/parity testers T4/T5): omp's
// --export takes the SESSION as its argument, so an omp-following user points
// xdev's output flag at the transcript itself and would destroy it. The
// guard refuses any non-HTML target — a .jsonl session file is refused, a
// re-export over a previous export is allowed (xdev emits lowercase
// "<!doctype html>", which is why the check folds case).
func TestRunExportRefusesNonHTMLTarget(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	st := exportTestStore(t)

	// A real disk session so -fork resolution works (exportSourceSession
	// resolves ids against sessionDataDir(), cwd-matched).
	// exportSourceSession resolves the -fork selector against sessionDataDir()
	// and the process cwd, so the fixture session lives under a redirected
	// store root for this test only.
	dataDir := t.TempDir()
	prev := launch
	launch.SessionDir = dataDir
	t.Cleanup(func() { launch = prev })
	sid := session.NewSessionID()
	// The file's existing helper lays down a valid session (title slot +
	// header + a MarshalEntry-encoded prompt line); hand-rolling the entry
	// line skips the envelope the store expects on reload.
	p := writePickerSession(t, mustGetwd(), sid, "export guard test", "hello")

	// Case 1: the target is an existing JSONL transcript — must be refused
	// and left byte-identical.
	dir := t.TempDir()
	jsonl := filepath.Join(dir, sid+".jsonl")
	original := []byte(`{"type":"header","id":"` + st.ID() + `"}` + "\n")
	if err := os.WriteFile(jsonl, original, 0o600); err != nil {
		t.Fatal(err)
	}
	err := runExport(jsonl, printOptions{ForkID: p})
	if err == nil {
		t.Fatal("runExport over a JSONL transcript must be refused")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("refusal must say what it refused: %v", err)
	}
	if now, rerr := os.ReadFile(jsonl); rerr != nil || string(now) != string(original) {
		t.Fatalf("refused export still modified the target: %v", rerr)
	}

	// Case 2: the target is a previous export (lowercase doctype) — a
	// re-export must be allowed.
	out := filepath.Join(dir, "again.html")
	if _, err := exportSession(st, "", "", out); err != nil {
		t.Fatal(err)
	}
	if err := runExport(out, printOptions{ForkID: p}); err != nil {
		t.Fatalf("re-export over a previous export must be allowed, got %v", err)
	}
}

func TestRunExportNeverCreatesASession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	out := filepath.Join(t.TempDir(), "out.html")
	err := runExport(out, printOptions{})
	if err == nil || !strings.Contains(err.Error(), "no session") {
		t.Fatalf("runExport err = %v, want a no-session error", err)
	}
	if _, serr := os.Stat(out); serr == nil {
		t.Fatal("runExport wrote a file for a session that does not exist")
	}
	if err := runExport("", printOptions{}); err == nil || !strings.Contains(err.Error(), "needs a file path") {
		t.Fatalf("runExport without a path = %v", err)
	}
}

// TestShareLiveServesOneSnapshot: /share hands back a loopback link whose
// fragment key opens the served ciphertext, and a second /share replaces the
// first — one live link per process, never a stale server left running.
func TestShareLiveServesOneSnapshot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	st := exportTestStore(t)

	first, err := shareLive(st, "SYS prompt", "onegw/free")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "http://127.0.0.1:") || !strings.Contains(first, "#") {
		t.Fatalf("first link = %q", first)
	}

	second, err := shareLive(st, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("a second /share reused the first link")
	}
	if _, err := http.Get(strings.SplitN(first, "#", 2)[0]); err == nil {
		t.Fatal("the replaced share server is still serving")
	}

	base, fragment, _ := strings.Cut(second, "#")
	res, err := http.Get(base + "/blob")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	blob, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "SYS prompt") || strings.Contains(string(blob), "<!doctype html>") {
		t.Fatal("the served snapshot is not encrypted")
	}
	key, err := share.DecodeKey(fragment)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := share.Open(blob, key)
	if err != nil {
		t.Fatalf("link key does not open the served snapshot: %v", err)
	}
	if !strings.Contains(string(plain), "please read") {
		t.Fatalf("shared snapshot lost the transcript:\n%.200s", plain)
	}
}

// TestResumeHint: the TUI's exit line is printed only when the session it
// names is reachable by `--resume <id>` from this directory, and never for a
// store with nothing on disk.
func TestResumeHint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := "/tmp/hint-test"
	id := "CCCC3333-0000-0000-0000-000000000000"
	path := writePickerSession(t, cwd, id, "hinted", "hello")
	st, err := session.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if got, want := resumeHint(st, cwd), resumeCommand(id); got != want {
		t.Fatalf("hint = %q, want %q", got, want)
	}
	// Another directory: --resume <id> finds the session across projects
	// (the cross-directory fallback), so the pasted line still lands on this
	// file — the hint is honest there too.
	if got, want := resumeHint(st, "/tmp/hint-elsewhere"), resumeCommand(id); got != want {
		t.Fatalf("hint for another cwd = %q, want %q", got, want)
	}
	// /drop took the file with it.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := resumeHint(st, cwd); got != "" {
		t.Fatalf("hint after the file vanished = %q, want empty", got)
	}
	// A store that never materialized: the fresh session /new or /drop left,
	// or a chat that got no assistant reply.
	if got := resumeHint(session.OpenMem(cwd, "memory-only"), cwd); got != "" {
		t.Fatalf("hint for a memory-only store = %q, want empty", got)
	}
}

// TestResumeProgSpellsTheInvokedName: the hint must paste back into the shell
// the user typed it from, whatever the binary was named.
func TestResumeProgSpellsTheInvokedName(t *testing.T) {
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

// TestResumePickerItemsCarryStatus: the rows /resume draws close on the session
// lifecycle badge, read from each file's tail (#107).
func TestResumePickerItemsCarryStatus(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := config.DataDir()
	cwd := "/tmp/status-picker"
	now := time.Now()
	for i, tail := range []string{"complete", "interrupted"} {
		id := "STATUS" + string(rune('A'+i)) + "-0000-0000-0000-000000000000"
		p := session.SessionFilePath(dir, cwd, now.Add(time.Duration(-i)*time.Minute), id)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		b.Write(session.MarshalTitleSlot("sess "+tail, session.TitleSourceAuto, now))
		b.Write(session.MarshalHeader(session.SessionHeader{
			Version: 3, ID: id, Timestamp: now, CWD: cwd, Title: "sess " + tail, TitleSource: session.TitleSourceAuto,
		}))
		b.WriteString("\n")
		msg := ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop, Content: []ai.Block{ai.TextBlock{Text: "answer"}}}
		if tail == "interrupted" {
			// A tool call that never came back: the turn was cut short.
			msg = ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{
				ai.TextBlock{Text: "unfinished"},
				ai.ToolCallBlock{ID: "call_1", Name: "bash"},
			}}
		}
		line, err := session.MarshalEntry(&session.MessageEntry{Message: msg})
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteString("\n")
		if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	items := resumePickerItems(cwd)
	if len(items) != 2 {
		t.Fatalf("rows = %d, want 2", len(items))
	}
	byID := map[string]string{}
	for _, it := range items {
		byID[it.ID[:7]] = it.Status
	}
	if byID["STATUSA"] != "complete" || byID["STATUSB"] != "interrupted" {
		t.Fatalf("row statuses = %v, want STATUSA complete, STATUSB interrupted", byID)
	}
}

// writeStatusSession lays down one session whose tail classifies as want
// ("complete", "interrupted" or "pending"), so search tests exercise the real
// classifier instead of hand-set struct fields.
func writeStatusSession(t *testing.T, cwd, id, title, want string, age time.Duration) {
	t.Helper()
	now := time.Now().UTC().Add(-age)
	p := session.SessionFilePath(config.DataDir(), cwd, now, id)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.Write(session.MarshalTitleSlot(title, session.TitleSourceAuto, now))
	b.Write(session.MarshalHeader(session.SessionHeader{
		Version: 3, ID: id, Timestamp: now, CWD: cwd, Title: title, TitleSource: session.TitleSourceAuto,
	}))
	b.WriteString("\n")
	msg := ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop, Content: []ai.Block{ai.TextBlock{Text: "answer"}}}
	switch want {
	case "interrupted":
		// A tool call that never came back: the turn was cut short.
		msg = ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{
			ai.TextBlock{Text: "unfinished"},
			ai.ToolCallBlock{ID: "call_1", Name: "bash"},
		}}
	case "pending":
		// The transcript ends on the prompt that is being answered.
		msg = ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "unfinished"}}}
	}
	line, err := session.MarshalEntry(&session.MessageEntry{Message: msg})
	if err != nil {
		t.Fatal(err)
	}
	b.Write(line)
	b.WriteString("\n")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSearchPickerItemsMatchesStatus: "interrupted" is a real filter, not
// just a badge — the picker query narrows on the lifecycle status (#107),
// and status tokens AND with the prompt-text search the way id tokens do.
func TestSearchPickerItemsMatchesStatus(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := "/tmp/picker-status"
	writeStatusSession(t, cwd, "AAAA1111-0000-0000-0000-000000000000", "finished work", "complete", time.Minute)
	writeStatusSession(t, cwd, "BBBB2222-0000-0000-0000-000000000000", "killed work", "interrupted", 2*time.Minute)
	writeStatusSession(t, cwd, "CCCC3333-0000-0000-0000-000000000000", "killed rerun", "interrupted", 3*time.Minute)

	got := searchPickerItems(cwd, "interrupted")
	if len(got) != 2 {
		t.Fatalf("query 'interrupted' matches = %v, want the two interrupted rows", got)
	}
	for _, it := range got {
		if it.Status != "interrupted" {
			t.Fatalf("row %s matched 'interrupted' but carries %q", it.ID, it.Status)
		}
	}

	// A status token mixed with an id token must AND (the badge hit for
	// CCCC3333 must not survive a BBBB2222 id token).
	got = searchPickerItems(cwd, "interrupted bbbb")
	if len(got) != 1 || got[0].ID != "BBBB2222" {
		t.Fatalf("query 'interrupted bbbb' matches = %v, want only BBBB2222", got)
	}

	// The status must not shadow the body search it sits beside.
	got = searchPickerItems(cwd, "interrupted unfinished")
	if len(got) != 2 {
		t.Fatalf("status+body query matches = %v, want both interrupted rows (body carries 'unfinished')", got)
	}
	got = searchPickerItems(cwd, "complete")
	if len(got) != 1 || got[0].ID != "AAAA1111" {
		t.Fatalf("query 'complete' matches = %v, want only AAAA1111", got)
	}
}

// TestRecentResumeOptionsUncapped: /resume with no argument used to hand the
// picker twelve rows and drop the rest; it now hands over every session in
// this folder (the picker windows and scrolls them itself) and still skips
// subagent children and the live session.
func TestRecentResumeOptionsUncapped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := "/tmp/recent-uncapped"
	live := "LIVE0000-0000-0000-0000-000000000000"
	writeStatusSession(t, cwd, live, "the live one", "complete", 0)
	for i := range 20 {
		writeStatusSession(t, cwd,
			fmt.Sprintf("RECENT%02d-0000-0000-0000-000000000000", i),
			fmt.Sprintf("older session %02d", i), "complete", time.Duration(i+1)*time.Minute)
	}
	// A session from another folder must not leak into the picker.
	writeStatusSession(t, "/tmp/recent-elsewhere", "OTHER000-0000-0000-0000-000000000000", "elsewhere", "complete", time.Minute)

	got := recentResumeOptions(cwd, live)
	if len(got) != 20 {
		t.Fatalf("rows = %d, want all 20 (the old code capped at 12)", len(got))
	}
	for _, o := range got {
		if o.ID == live {
			t.Fatal("the live session was offered for resume")
		}
		if !strings.HasPrefix(o.ID, "RECENT") {
			t.Fatalf("foreign row %q in the current-folder picker", o.ID)
		}
		if !strings.Contains(o.Detail, "complete") {
			t.Fatalf("row %q lost its lifecycle status: %q", o.ID, o.Detail)
		}
	}
	// Every session is present exactly once: the cap dropped the tail, not
	// shuffled it. (Row order is session.List's — newest first by file
	// mtime — and is pinned by the listing tests, not here.)
	seen := map[string]bool{}
	for _, o := range got {
		seen[o.ID] = true
	}
	for i := range 20 {
		id := fmt.Sprintf("RECENT%02d-0000-0000-0000-000000000000", i)
		if !seen[id] {
			t.Fatalf("row %s missing from the picker", id)
		}
	}
}

// TestEmptySessionsAreNotPickerRows: an empty session is not a row in either
// picker. /resume drew one, resuming it produced a blank transcript, and a
// newest-empty row is what made --continue and /resume look broken in a
// folder whose last run was a session that never received a prompt.
func TestEmptySessionsAreNotPickerRows(t *testing.T) {
	sandbox(t)
	cwd := "/tmp/empty-picker"
	now := time.Now().UTC()
	withConversation := "REAL0001-0000-0000-0000-000000000000"
	emptyRoot := "EMPTY001-0000-0000-0000-000000000000"
	emptyFork := "EMPTY002-0000-0000-0000-000000000000"

	line, err := session.MarshalEntry(&session.MessageEntry{
		Env:     session.Envelope{ID: "m1", Timestamp: now},
		Message: ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "real work"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	write := func(ts time.Time, id, title string, withMsg bool) {
		t.Helper()
		var b strings.Builder
		b.Write(session.MarshalTitleSlot(title, session.TitleSourceAuto, ts))
		b.Write(session.MarshalHeader(session.SessionHeader{
			Version: 3, ID: id, Timestamp: ts, CWD: cwd, Title: title, TitleSource: session.TitleSourceAuto,
		}))
		if withMsg {
			b.Write(line)
		}
		p := session.SessionFilePath(sessionDataDir(), cwd, ts, id)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(now.Add(-time.Hour), withConversation, "the real one", true)
	write(now.Add(-time.Minute), emptyRoot, "empty root", false)
	write(now, emptyFork, "empty fork", false) // newest, as a fresh /fork leaves it

	for _, it := range resumePickerItems(cwd) {
		if it.ID == emptyRoot[:8] || it.ID == emptyFork[:8] {
			t.Fatalf("empty session %s was offered as a resume row", it.ID)
		}
	}
	rows := recentResumeOptions(cwd, "")
	if len(rows) != 1 || rows[0].ID != withConversation {
		t.Fatalf("recentResumeOptions = %+v, want only the session with a conversation", rows)
	}
}

// TestSessionHasMessagesIsTheForkGuard: /fork refuses a session that never
// received a prompt, because forking it wrote a second empty file — which then
// became the newest file and hijacked --continue. The predicate is MESSAGE
// entries, not entries: a fresh store already carries a goal_updated entry.
func TestSessionHasMessagesIsTheForkGuard(t *testing.T) {
	sandbox(t)
	s := session.OpenMem("/proj", "fresh")
	if sessionHasMessages(s) {
		t.Fatal("a fresh session must not read as forkable")
	}
	if err := s.Append(&session.GoalUpdatedEntry{}); err != nil {
		t.Fatal(err)
	}
	if sessionHasMessages(s) {
		t.Fatal("bookkeeping is not a conversation — /fork would write an empty file")
	}
	if err := s.Append(&session.MessageEntry{Message: ai.Message{
		Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "do the thing"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if !sessionHasMessages(s) {
		t.Fatal("a session with a prompt must read as forkable")
	}
}

// --- resume matching (omp rules 38, 43-47) ---

// writeSessionIn lays down one session file with a real message, in the
// bucket of its own cwd, and returns its path.
func writeSessionIn(t *testing.T, dir, cwd, id, title string, ts time.Time) string {
	t.Helper()
	p := session.SessionFilePath(dir, cwd, ts, id)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.Write(session.MarshalTitleSlot(title, session.TitleSourceAuto, ts))
	b.Write(session.MarshalHeader(session.SessionHeader{
		Version: 3, ID: id, Timestamp: ts, CWD: cwd, Title: title, TitleSource: session.TitleSourceAuto,
	}))
	line, err := session.MarshalEntry(&session.MessageEntry{
		Message: ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "the prompt"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	b.Write(line)
	b.WriteString("\n")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestResolveResumeIDMatchesTheFileName: a query that names the FILE — its
// timestamp prefix — finds the session, the way omp's resolver does.
func TestResolveResumeIDMatchesTheFileName(t *testing.T) {
	sandbox(t)
	dir := config.DataDir()
	cwd := "/tmp/name-match"
	now := time.Now()
	writeSessionIn(t, dir, cwd, "NAME1111-0000-0000-0000-000000000000", "sess", now)

	// The file name is <timestamp>_<id>.jsonl: the timestamp prefix is the
	// query that finds it.
	name := filepath.Base(session.SessionFilePath(dir, cwd, now, "NAME1111-0000-0000-0000-000000000000"))
	stamp := strings.TrimSuffix(name, ".jsonl")
	stamp = stamp[:strings.Index(stamp, "_")]
	got, err := resolveResumeID(cwd, strings.ToLower(stamp))
	if err != nil {
		t.Fatalf("resolve by file name %q: %v", stamp, err)
	}
	if !strings.HasSuffix(got, "NAME1111-0000-0000-0000-000000000000.jsonl") {
		t.Fatalf("resolved %q, want the NAME1111 session", got)
	}
}

// TestResolveResumeIDFallsBackAcrossProjects: a query that names nothing in
// this directory still names a session somewhere else. The user who names an
// id means THAT session.
func TestResolveResumeIDFallsBackAcrossProjects(t *testing.T) {
	sandbox(t)
	dir := config.DataDir()
	here, there := "/tmp/here", "/tmp/there"
	now := time.Now()
	writeSessionIn(t, dir, there, "OTHER2222-0000-0000-0000-000000000000", "sess", now)

	got, err := resolveResumeID(here, "other2222")
	if err != nil {
		t.Fatalf("resolve across projects: %v", err)
	}
	if !strings.HasSuffix(got, "OTHER2222-0000-0000-0000-000000000000.jsonl") {
		t.Fatalf("resolved %q, want the OTHER2222 session", got)
	}
	// The newest-in-cwd form never leaves the directory: --continue must
	// not jump projects.
	if _, err := resolveResumeID(here, ""); err == nil {
		t.Fatal("the empty query must not resolve a session from another project")
	}
}

// TestResumeMatchesPrecedence: the three ways a query can name a session.
func TestResumeMatchesPrecedence(t *testing.T) {
	m := session.SessionMeta{ID: "ABCD1234-0000-0000-0000-000000000000", Path: "/d/2026-01-02T03-04-05.000Z_ABCD1234-0000-0000-0000-000000000000.jsonl"}
	for _, tc := range []struct {
		q    string
		want bool
	}{
		{"abcd", true},    // id prefix
		{"ABCD", true},    // case-insensitive
		{"2026-01", true}, // file name prefix
		{"zzzz", false},   // neither half of the name
	} {
		if got := resumeMatches(m, tc.q); got != tc.want {
			t.Errorf("resumeMatches(%q) = %v, want %v", tc.q, got, tc.want)
		}
	}
	// The id after the last "_" in the file name is a name of its own: a
	// file whose id part differs from the header still answers to it.
	other := session.SessionMeta{ID: "ABCD1234-0000-0000-0000-000000000000", Path: "/d/2026-01-02T03-04-05.000Z_ZZZZ9999.jsonl"}
	if !resumeMatches(other, "zzzz") {
		t.Error("the id after the last _ in the file name must match")
	}
}

// TestResumeCwdFollowsTheSessionHeader: the resumed session's project is
// where the run continues, not the directory the command was typed in.
func TestResumeCwdFollowsTheSessionHeader(t *testing.T) {
	sandbox(t)
	dir := config.DataDir()
	project := t.TempDir()
	now := time.Now()
	p := writeSessionIn(t, dir, project, "CWD11111-0000-0000-0000-000000000000", "sess", now)

	before, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(before) })

	got, err := resumeCwd(p, before)
	if err != nil {
		t.Fatalf("resumeCwd: %v", err)
	}
	if got != project {
		t.Fatalf("cwd = %q, want the session's project %q", got, project)
	}
	now2, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// The temp dir is a symlink on macOS; compare the resolved paths.
	want, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	gotCwd, err := filepath.EvalSymlinks(now2)
	if err != nil {
		t.Fatal(err)
	}
	if gotCwd != want {
		t.Fatalf("process cwd = %q, want %q", gotCwd, want)
	}
}

// TestResumeCwdKeepsTheLaunchDirWhenTheProjectIsGone: a session whose
// project was deleted must not strand the run in a directory that is not
// there.
func TestResumeCwdKeepsTheLaunchDirWhenTheProjectIsGone(t *testing.T) {
	sandbox(t)
	dir := config.DataDir()
	gone := filepath.Join(t.TempDir(), "deleted")
	now := time.Now()
	p := writeSessionIn(t, dir, gone, "GONE1111-0000-0000-0000-000000000000", "sess", now)

	before, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	got, err := resumeCwd(p, before)
	if err == nil {
		t.Fatal("a vanished project must be reported")
	}
	if got != before {
		t.Fatalf("cwd = %q, want the launch dir %q", got, before)
	}
}

// --- the exit marker (omp rule 21) ---

// TestExitMarkerCarriesTheReaderContract: omp's exit diagnostics reader
// needs reason, kind and recordedAt; xdev's own readers still read mode and
// code. The pending tool calls are the REAL count of the calls the session
// left unanswered — not a placeholder.
func TestExitMarkerCarriesTheReaderContract(t *testing.T) {
	sandbox(t)
	st := session.OpenMem(t.TempDir(), "t")
	defer st.Close()
	if err := st.Append(&session.MessageEntry{Message: ai.Message{
		Role:    ai.RoleAssistant,
		Content: []ai.Block{ai.TextBlock{Text: "working"}, ai.ToolCallBlock{ID: "call_1", Name: "bash"}},
	}}); err != nil {
		t.Fatal(err)
	}
	data := exitMarker("print", 0, "", exitReason("print", "normal", ""), pendingToolCalls(st))
	if data["reason"] == "" {
		t.Errorf("marker has no reason: %v", data)
	}
	if data["kind"] != "normal" {
		t.Errorf("kind = %v, want normal", data["kind"])
	}
	if data["mode"] != "print" {
		t.Errorf("mode = %v, want print (the key xdev's readers already had)", data["mode"])
	}
	if data["code"] != 0 {
		t.Errorf("code = %v, want 0", data["code"])
	}
	rec, _ := data["recordedAt"].(string)
	if rec == "" {
		t.Errorf("marker has no recordedAt: %v", data)
	}
	if _, err := time.Parse(time.RFC3339Nano, rec); err != nil {
		t.Errorf("recordedAt %q is not an ISO-8601 timestamp: %v", rec, err)
	}
	if data["pendingToolCalls"] != 1 {
		t.Errorf("pendingToolCalls = %v, want the one unanswered call", data["pendingToolCalls"])
	}
	// A failed run is a fatal exit, and it says so.
	fatal := exitMarker("tui", 1, "", exitReason("tui", "fatal", "the provider died"), nil)
	if fatal["kind"] != "fatal" {
		t.Errorf("kind = %v, want fatal", fatal["kind"])
	}
	if _, ok := fatal["pendingToolCalls"]; ok {
		t.Errorf("an unknown pending count must be omitted, not faked: %v", fatal)
	}
}

// TestExitKindNamesTheDeath: the marker's kind has to name HOW the process
// ended — a normal exit, a death by signal, or a fatal end. A signal death
// writes NO marker at all (a death by signal runs no deferred function), so
// the kind the signal path writes is the only record of it.
func TestExitKindNamesTheDeath(t *testing.T) {
	for _, tc := range []struct {
		code int
		want string
	}{
		{0, "normal"},
		{1, "fatal"},
		{2, "fatal"},   // a panic
		{124, "fatal"}, // a wall-clock deadline
		{130, "signal"},
		{143, "signal"}, // 128 + SIGTERM
	} {
		if got := exitKind(tc.code); got != tc.want {
			t.Errorf("exitKind(%d) = %q, want %q", tc.code, got, tc.want)
		}
	}
	// The reason names the death, not just the code.
	sig := exitMarker("tui", 143, "signal", exitReason("tui", "signal", "terminated"), nil)
	if sig["kind"] != "signal" {
		t.Errorf("kind = %v, want signal", sig["kind"])
	}
	if reason, _ := sig["reason"].(string); !strings.Contains(reason, "terminated") {
		t.Errorf("reason = %q, want it to name the signal", reason)
	}
}

// TestRunExitCodeNamesAnInterrupt: a Ctrl-C is a stop, not a failure — the
// conventional 130, and the marker says "signal" rather than "fatal".
func TestRunExitCodeNamesAnInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := runExitCode(ctx, context.Canceled); got != 130 {
		t.Fatalf("runExitCode(cancelled) = %d, want 130", got)
	}
	if got := runExitCode(context.Background(), errors.New("provider died")); got != 1 {
		t.Fatalf("runExitCode(failure) = %d, want 1", got)
	}
	// A wall-clock deadline is its own code.
	dctx, dcancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer dcancel()
	<-dctx.Done()
	if got := runExitCode(dctx, context.DeadlineExceeded); got != 124 {
		t.Fatalf("runExitCode(deadline) = %d, want 124", got)
	}
}

// TestPendingToolCallCountIsTheRealCount: only the calls that never came
// back count — an answered call is not pending, and a new assistant turn
// clears the previous one's set.
func TestPendingToolCallCountIsTheRealCount(t *testing.T) {
	sandbox(t)
	st := session.OpenMem(t.TempDir(), "t")
	defer st.Close()
	msg := func(role ai.Role, blocks ...ai.Block) *session.MessageEntry {
		return &session.MessageEntry{Message: ai.Message{Role: role, Content: blocks}}
	}
	answer := func(id string) *session.MessageEntry {
		return &session.MessageEntry{Message: ai.Message{Role: ai.RoleToolResult, ToolCallID: id, Content: []ai.Block{ai.TextBlock{Text: "the file"}}}}
	}
	// An answered call: the tool result came back.
	if err := st.Append(msg(ai.RoleAssistant, ai.ToolCallBlock{ID: "call_1", Name: "read"})); err != nil {
		t.Fatal(err)
	}
	if err := st.Append(answer("call_1")); err != nil {
		t.Fatal(err)
	}
	if n := pendingToolCalls(st); n == nil || *n != 0 {
		t.Fatalf("pending = %v, want 0 (the call was answered)", n)
	}
	// A second turn with two calls, one of them never answered.
	if err := st.Append(msg(ai.RoleAssistant,
		ai.ToolCallBlock{ID: "call_2", Name: "read"},
		ai.ToolCallBlock{ID: "call_3", Name: "bash"})); err != nil {
		t.Fatal(err)
	}
	if err := st.Append(answer("call_2")); err != nil {
		t.Fatal(err)
	}
	if n := pendingToolCalls(st); n == nil || *n != 1 {
		t.Fatalf("pending = %v, want 1 (call_3 never came back)", n)
	}
	// No session at all: the count is genuinely unknown, so it is omitted.
	if n := pendingToolCalls(nil); n != nil {
		t.Fatalf("pending = %v, want nil for a missing session", n)
	}
}

// TestResumeFlagClash: --resume names one session, so the flags that name a
// different one cannot ride along with it.
func TestResumeFlagClash(t *testing.T) {
	if got := resumeFlagClash(false, "", false); len(got) != 0 {
		t.Fatalf("clash = %v, want none", got)
	}
	if got := resumeFlagClash(true, "", false); len(got) != 1 || got[0] != "--continue" {
		t.Fatalf("clash = %v, want --continue", got)
	}
	if got := resumeFlagClash(false, "abc", false); len(got) != 1 || got[0] != "--fork" {
		t.Fatalf("clash = %v, want --fork", got)
	}
	if got := resumeFlagClash(false, "", true); len(got) != 1 || got[0] != "--no-session" {
		t.Fatalf("clash = %v, want --no-session", got)
	}
	got := resumeFlagClash(true, "abc", true)
	if len(got) != 3 {
		t.Fatalf("clash = %v, want all three names", got)
	}
}

// TestSignalDeathWritesItsMarker: a death by signal runs no deferred
// function, so the exit marker has to be written by the signal path itself.
// Without it a signalled session has NO exit record at all, and a reader
// cannot tell it from a session that was never closed.
func TestSignalDeathWritesItsMarker(t *testing.T) {
	sandbox(t)
	st := session.OpenMem(t.TempDir(), "t")
	defer st.Close()
	writeSignalExitMarker(st, syscall.SIGTERM, 143)

	entries := st.Entries()
	if len(entries) == 0 {
		t.Fatal("the signal wrote no entry")
	}
	ce, ok := entries[len(entries)-1].(*session.CustomEntry)
	if !ok || ce.CustomType != "session_exit" {
		t.Fatalf("the last entry is not the exit marker: %+v", entries[len(entries)-1])
	}
	if ce.Data["kind"] != "signal" {
		t.Errorf("kind = %v, want signal", ce.Data["kind"])
	}
	if ce.Data["code"] != 143 {
		t.Errorf("code = %v, want 143 (128 + SIGTERM)", ce.Data["code"])
	}
	if reason, _ := ce.Data["reason"].(string); !strings.Contains(reason, "terminated") {
		t.Errorf("reason = %q, want it to name the signal", reason)
	}
	// A nil store is not a crash: there is simply nothing to record.
	writeSignalExitMarker(nil, syscall.SIGTERM, 143)
}

// TestResolveResumeIDPrecedenceIsPerSessionNewestFirst: the rules are
// applied PER SESSION, newest first — the same shape omp's resolver has
// (omp's session-listing.ts: the first session, newest first, that matches
// ANY of the three rules wins). So a NEWER session that matches by
// file-name prefix beats an OLDER one that matches by id prefix, and the
// rules are not tried one after the other across the whole list.
//
// This is the resolver-level counterpart of TestResumeMatchesPrecedence,
// which only exercises the per-session helper.
func TestResolveResumeIDPrecedenceIsPerSessionNewestFirst(t *testing.T) {
	sandbox(t)
	dir := config.DataDir()
	cwd := "/tmp/precedence"
	now := time.Now()
	// The NEWER session matches by FILE NAME (its name starts with the
	// query); the OLDER one matches by ID (its id starts with the same
	// digit). The newer wins.
	newer := writeSessionIn(t, dir, cwd, "OLD1111-0000-0000-0000-000000000000", "newer", now)
	older := writeSessionIn(t, dir, cwd, "2OLD2222-0000-0000-0000-000000000000", "older", now.Add(-time.Hour))
	// The listing sorts by file mtime first, so the fixture's mtimes are
	// set explicitly: the newer session must really be the newer one.
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(newer, future, future); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(older, past, past); err != nil {
		t.Fatal(err)
	}
	// The query is the first character of the file-name timestamp, which is
	// also the first character of the older session's id.
	const query = "2"
	if !strings.Contains(filepath.Base(older), "_2OLD2222") {
		t.Fatalf("the fixture does not exercise the precedence: %q", older)
	}
	if !strings.HasPrefix(strings.ToLower(filepath.Base(newer)), query) {
		t.Fatalf("the fixture does not exercise the precedence: %q", newer)
	}
	got, err := resolveResumeID(cwd, query)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != newer {
		t.Fatalf("resolved %q, want the newer session %q (the rules are per session, newest first)", got, newer)
	}
}

// fakePickerOpener records the rows the picker was opened with, and the
// blocks the command printed.
type fakePickerOpener struct {
	open   func(items []tui.SessionPickerItem)
	blocks []string
}

func (f *fakePickerOpener) OpenSessionPicker(items []tui.SessionPickerItem) { f.open(items) }
func (f *fakePickerOpener) AddSystemBlock(text string)                      { f.blocks = append(f.blocks, text) }

// TestResumeOpensTheRealSessionPicker: the cmd-side /resume wiring must open
// the session picker with the rows cmd builds. The TUI-side test installs a
// fake that opens the picker ITSELF, so the cmd-side wiring was untested —
// this one drives the real closure.
func TestResumeOpensTheRealSessionPicker(t *testing.T) {
	sandbox(t)
	dir := config.DataDir()
	cwd := "/tmp/cmd-resume-picker"
	writeSessionIn(t, dir, cwd, "PICK1111-0000-0000-0000-000000000000", "a session", time.Now())

	var got []tui.SessionPickerItem
	opener := &fakePickerOpener{open: func(items []tui.SessionPickerItem) { got = items }}
	// The REAL command — the one the TUI's session ops call.
	if err := resumeSessionCommand(opener, cwd, "", func(*session.Store) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("the picker was opened with %d rows, want 1", len(got))
	}
	if got[0].ID != "PICK1111" {
		t.Fatalf("row = %+v, want the PICK1111 session", got[0])
	}
	if got[0].Title == "" {
		t.Fatalf("the row carries no title: %+v", got[0])
	}
	// The scope is ALL projects: the same session is a row for another
	// directory too, marked as not-in-cwd (the picker scopes it).
	if err := resumeSessionCommand(opener, "/tmp/nowhere", "", func(*session.Store) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].InCwd {
		t.Fatalf("rows = %+v, want the other project's row, not in cwd", got)
	}
	// A data dir with no sessions at all → no picker (the TUI owns the
	// notice).
	sandbox(t) // a fresh data dir
	var none []tui.SessionPickerItem
	if err := resumeSessionCommand(&fakePickerOpener{open: func(items []tui.SessionPickerItem) { none = items }},
		cwd, "", func(*session.Store) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if none != nil {
		t.Fatalf("the picker opened with %d rows, want none", len(none))
	}
}

// TestResumeOpensThePickerEndToEnd: the TUI's /resume and cmd's own /resume
// command, wired together the way the TUI wires them. A no-argument
// /resume must open the RICH session picker with the rows cmd builds — the
// TUI-side test installs a fake that opens the picker itself, so without
// this one the cmd-side wiring was untested.
func TestResumeOpensThePickerEndToEnd(t *testing.T) {
	sandbox(t)
	dir := config.DataDir()
	cwd := "/tmp/e2e-resume-picker"
	writeSessionIn(t, dir, cwd, "E2E1111-0000-0000-0000-000000000000", "a session", time.Now())

	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	defer scr.Fini()
	scr.SetSize(100, 30)
	app := tui.New(scr, theme.Load("groknight"), "test/free", "sess1234")
	// The TUI's session ops, wired the way runTUI wires them: the command
	// below IS cmd's /resume.
	app.SetSessionOps(&tui.SessionOps{
		Resume: func(q string) error {
			return resumeSessionCommand(app, cwd, q, func(*session.Store) error { return nil })
		},
		// The shipped build wires Recent too: the TUI's /resume falls back
		// to the plain list when the picker cannot open one, so a test that
		// omits it would not exercise the path the user takes.
		Recent: func() []tui.ResumeOption {
			return []tui.ResumeOption{{ID: "E2E1111", Title: "a session"}}
		},
	})
	if err := app.ResumeSession(""); err != nil {
		t.Fatal(err)
	}
	if !app.SessionPickerOpen() {
		t.Fatal("the rich session picker did not open")
	}
	it, ok := app.SessionPickerSelection()
	if !ok {
		t.Fatal("the picker has no selected row")
	}
	if !strings.HasPrefix(it.ID, "E2E1111") {
		t.Fatalf("the highlighted row = %q, want the E2E1111 session", it.ID)
	}
}
