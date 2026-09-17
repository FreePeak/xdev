package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/agent"
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
		Content:     []ai.Block{ai.TextBlock{Text: agent.TurnBudgetPrompt}},
		Attribution: agent.TurnBudgetAttribution,
	}})
	blocks := app.Blocks()
	if len(blocks) != 1 || blocks[0].Kind != tui.KindSystem {
		t.Fatalf("wrap-up replayed as %v, want one system block", blocks)
	}
	if strings.Contains(blocks[0].Text, agent.TurnBudgetPrompt) {
		t.Fatalf("the raw harness prompt leaked into the transcript: %q", blocks[0].Text)
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

// TestWorkOfSumsAssistantSpans: the HUD's re-based total is the provider
// request time the replayed path carries — assistant spans only, missing
// timings add nothing (an old or imported message must not invent seconds).
func TestWorkOfSumsAssistantSpans(t *testing.T) {
	msgs := []ai.Message{
		{Role: ai.RoleUser},
		{Role: ai.RoleAssistant, DurationMS: 4000},
		{Role: ai.RoleToolResult, DurationMS: 9000},
		{Role: ai.RoleAssistant, DurationMS: 2500},
		{Role: ai.RoleAssistant}, // pre-timing history: contributes 0
	}
	if got := workOf(msgs); got != 6500*time.Millisecond {
		t.Fatalf("workOf = %v, want 6.5s", got)
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
	// Another directory: --resume <id> would error there, so no line.
	if got := resumeHint(st, "/tmp/hint-elsewhere"); got != "" {
		t.Fatalf("hint for another cwd = %q, want empty", got)
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
	for i, tail := range []string{"done", "interrupted"} {
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
		role := ai.RoleAssistant
		msg := ai.Message{Role: role, Content: []ai.Block{ai.TextBlock{Text: "answer"}}}
		if tail == "done" {
			msg.StopReason = ai.StopReasonStop
		} else {
			// An aborted turn persists the prompt and nothing past it.
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

	items := resumePickerItems(cwd)
	if len(items) != 2 {
		t.Fatalf("rows = %d, want 2", len(items))
	}
	byID := map[string]string{}
	for _, it := range items {
		byID[it.ID[:7]] = it.Status
	}
	if byID["STATUSA"] != "done" || byID["STATUSB"] != "interrupted" {
		t.Fatalf("row statuses = %v, want STATUSA done, STATUSB interrupted", byID)
	}
}

// writeStatusSession lays down one session whose tail classifies as want
// ("done" or "interrupted"), so search tests exercise the real classifier
// instead of hand-set struct fields.
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
	if want == "interrupted" {
		// An aborted turn persists the prompt and nothing past it.
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
	writeStatusSession(t, cwd, "AAAA1111-0000-0000-0000-000000000000", "finished work", "done", time.Minute)
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
	got = searchPickerItems(cwd, "done")
	if len(got) != 1 || got[0].ID != "AAAA1111" {
		t.Fatalf("query 'done' matches = %v, want only AAAA1111", got)
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
	writeStatusSession(t, cwd, live, "the live one", "done", 0)
	for i := range 20 {
		writeStatusSession(t, cwd,
			fmt.Sprintf("RECENT%02d-0000-0000-0000-000000000000", i),
			fmt.Sprintf("older session %02d", i), "done", time.Duration(i+1)*time.Minute)
	}
	// A session from another folder must not leak into the picker.
	writeStatusSession(t, "/tmp/recent-elsewhere", "OTHER000-0000-0000-0000-000000000000", "elsewhere", "done", time.Minute)

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
		if !strings.Contains(o.Detail, "done") {
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
