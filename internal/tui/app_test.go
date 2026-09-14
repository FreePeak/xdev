package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

func TestWrap(t *testing.T) {
	got := wrap("hello wonderful world", 10)
	if strings.Join(got, "|") != "hello|wonderful|world" {
		t.Fatalf("wrap = %q", got)
	}
	got = wrap("a\n\nb", 10)
	if strings.Join(got, "|") != "a||b" {
		t.Fatalf("empty lines must survive: %q", got)
	}
	// Long word hard-breaks.
	got = wrap("abcdefghijklmnopqrstuvwxyz", 10)
	for _, l := range got {
		if width(l) > 10 {
			t.Fatalf("line exceeds maxW: %q", l)
		}
	}
	if strings.Join(got, "") != "abcdefghij"+"klmnopqrst"+"uvwxyz" {
		t.Fatalf("hard break lost content: %q", got)
	}
}

func TestEditorSendAndHistory(t *testing.T) {
	var e Editor
	typeIn := func(s string) {
		for _, r := range s {
			e.HandleKey(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
		}
	}
	typeIn("hello")
	if e.Text() != "hello" {
		t.Fatalf("text = %q", e.Text())
	}
	if !e.HandleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone)) {
		t.Fatal("Enter with content must send")
	}
	if e.Text() != "" {
		t.Fatal("editor must reset after send")
	}
	// Empty Enter does not send.
	if e.HandleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone)) {
		t.Fatal("empty Enter must not send")
	}
	// History recall.
	typeIn("second")
	e.HandleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	e.PushHistory("second")
	e.PushHistory("hello")
	e.HandleKey(tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone))
	if e.Text() != "hello" {
		t.Fatalf("Up should recall most recent: %q", e.Text())
	}
	e.HandleKey(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	if e.Text() != "" {
		t.Fatalf("Down past newest should clear: %q", e.Text())
	}
}

func TestEditorBackspaceAndWordDelete(t *testing.T) {
	var e Editor
	for _, r := range "one two three" {
		e.HandleKey(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
	}
	e.HandleKey(tcell.NewEventKey(tcell.KeyCtrlW, 0, tcell.ModNone))
	if got := e.Text(); got != "one two " {
		t.Fatalf("CtrlW = %q", got)
	}
	e.HandleKey(tcell.NewEventKey(tcell.KeyBackspace, 0, tcell.ModNone))
	if got := e.Text(); got != "one two" {
		t.Fatalf("backspace = %q", got)
	}
}

func newTestApp(t *testing.T, w, h int) (*App, tcell.SimulationScreen) {
	t.Helper()
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	scr.SetSize(w, h)
	th := theme.Load("groknight")
	app := New(scr, th, "test/free", "sess1234")
	t.Cleanup(func() { scr.Fini() })
	return app, scr
}

func TestAppSendFlow(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	var sent []string
	app.SetHandlers(func(text string) { sent = append(sent, text) }, func() {}, func() {})

	for _, r := range "make a file" {
		app.handleKey(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
	}
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))

	if len(sent) != 1 || sent[0] != "make a file" {
		t.Fatalf("sent = %v", sent)
	}
	app.mu.Lock()
	n := len(app.blocks)
	kind := app.blocks[0].Kind
	edEmpty := app.ed.Text() == ""
	app.mu.Unlock()
	if n != 1 || kind != KindUser || !edEmpty {
		t.Fatalf("blocks=%d kind=%v editorEmpty=%v", n, kind, edEmpty)
	}
}

func TestStreamingBlocksAndTools(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.BeginAssistant()
	app.AppendAssistant("Hello ")
	app.AppendAssistant("world")
	app.EndAssistant()
	app.BeginThinking()
	app.AppendThinking("pondering")
	app.EndThinking()
	app.AddToolBlock("read", `{"path":"a.txt"}`)
	app.FinishTool("read", false, "1:hi\n2:there", "3ms")

	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.blocks) != 4 {
		t.Fatalf("blocks = %d", len(app.blocks))
	}
	if app.blocks[0].Text != "Hello world" || app.blocks[0].stream {
		t.Fatalf("assistant = %+v", app.blocks[0])
	}
	if app.blocks[2].Status != "ok" {
		t.Fatalf("tool status = %q", app.blocks[2].Status)
	}
	if app.blocks[3].Kind != KindToolDone || app.blocks[3].Text != "1:hi\n2:there" {
		t.Fatalf("result block = %+v", app.blocks[3])
	}
}

func TestThinkingDisplayToggle(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.BeginThinking()
	app.AppendThinking("first line of reasoning\nsecond line")
	app.EndThinking()
	app.mu.Lock()
	if len(app.blocks) != 1 || app.blocks[0].Kind != KindThinking {
		t.Fatalf("thinking block missing: blocks=%d", len(app.blocks))
	}
	lines := app.blockLines(0, app.blocks[0], 80)
	app.mu.Unlock()
	var got []string
	for _, ln := range lines {
		if len(ln.runs) == 1 {
			got = append(got, ln.runs[0].text)
		}
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{"Thought for", "first line of reasoning", "second line"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("thinking render missing %q:\n%s", want, joined)
		}
	}
	// Turning it off drops existing thinking blocks and suppresses new ones.
	app.SetShowThinking(false)
	app.mu.Lock()
	if len(app.blocks) != 0 {
		t.Fatalf("thinking blocks must be dropped when off: %d", len(app.blocks))
	}
	app.mu.Unlock()
	app.BeginThinking()
	app.mu.Lock()
	if len(app.blocks) != 0 {
		t.Fatalf("thinking block created while off: %d", len(app.blocks))
	}
	app.mu.Unlock()
	// Turning it back on renders the body again.
	app.SetShowThinking(true)
	app.BeginThinking()
	app.AppendThinking("visible again")
	app.EndThinking()
	app.mu.Lock()
	lines = app.blockLines(len(app.blocks)-1, app.blocks[len(app.blocks)-1], 80)
	app.mu.Unlock()
	got = got[:0]
	for _, ln := range lines {
		if len(ln.runs) == 1 {
			got = append(got, ln.runs[0].text)
		}
	}
	if !strings.Contains(strings.Join(got, "\n"), "visible again") {
		t.Fatalf("thinking body missing after re-enable:\n%s", strings.Join(got, "\n"))
	}
}

// TestSystemBlockRendersEveryLine pins the multi-line fix: a system notice
// with embedded newlines (from /help, /model, /settings) must render one
// row per line, not collapse into a single unreadable row.
func TestSystemBlockRendersEveryLine(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.AddSystemBlock("commands:\n  /new   start a fresh session\n  /help  show commands")
	app.mu.Lock()
	lines := app.blockLines(len(app.blocks)-1, app.blocks[len(app.blocks)-1], 80)
	app.mu.Unlock()
	if len(lines) != 3 {
		t.Fatalf("system block rows = %d, want 3", len(lines))
	}
	if lines[1].runs[0].text != "  /new   start a fresh session" {
		t.Fatalf("row 1 = %q", lines[1].runs[0].text)
	}
}

// lineText concatenates every run of a rendered line, so assertions read the
// whole visual row (the tool box splits each row into border/content/border
// runs) instead of one run.
func lineText(ln line) string {
	var b strings.Builder
	for _, r := range ln.runs {
		b.WriteString(r.text)
	}
	return b.String()
}

// TestToolResultRendersBox pins the omp-parity frame: a finished tool result
// renders as a rounded box whose top border carries "name · state (dur)", the
// body keeps every output line (not a flattened preview), and the frame
// closes on its own bottom border.
func TestToolResultRendersBox(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.FinishTool("bash", false, "line-one\nline-two\nline-three", "5ms")
	app.mu.Lock()
	lines := app.blockLines(len(app.blocks)-1, app.blocks[len(app.blocks)-1], 80)
	app.mu.Unlock()

	if len(lines) != 5 { // top + 3 body + bottom
		t.Fatalf("boxed render = %d lines, want 5", len(lines))
	}
	top := lineText(lines[0])
	for _, want := range []string{"╭", "bash", "ok", "5ms"} {
		if !strings.Contains(top, want) {
			t.Fatalf("top border %q missing %q", top, want)
		}
	}
	bottom := lineText(lines[4])
	if !strings.Contains(bottom, "╰") || !strings.Contains(bottom, "╯") {
		t.Fatalf("bottom border %q not closed", bottom)
	}
	body := lineText(lines[1]) + lineText(lines[2]) + lineText(lines[3])
	for _, want := range []string{"line-one", "line-two", "line-three"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q; got:\n%s", want, body)
		}
	}
	// Every row shares one right edge: widths equal the requested 80 cells.
	for i, ln := range lines {
		if w := width(lineText(ln)); w != 80 {
			t.Fatalf("row %d width = %d, want 80 (%q)", i, w, lineText(ln))
		}
	}
}

// TestToolResultRowWindow pins the bounded-render ceiling inside the box:
// outputs wider than the window keep the first 200 + last 50 rows and
// announce the elided middle, all framed by the top and bottom borders.
func TestToolResultRowWindow(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	var b strings.Builder
	for i := 1; i <= 400; i++ {
		fmt.Fprintf(&b, "row-%03d\n", i)
	}
	app.FinishTool("bash", false, strings.TrimSuffix(b.String(), "\n"), "9ms")
	app.mu.Lock()
	lines := app.blockLines(len(app.blocks)-1, app.blocks[len(app.blocks)-1], 80)
	app.mu.Unlock()
	// top + head(200) + elision + tail(50) + bottom
	if len(lines) != 200+50+1+2 {
		t.Fatalf("windowed render = %d lines, want 253", len(lines))
	}
	if !strings.Contains(lineText(lines[1]), "row-001") {
		t.Fatalf("first head row = %q", lineText(lines[1]))
	}
	if !strings.Contains(lineText(lines[201]), "rows elided") {
		t.Fatalf("elision marker missing: %q", lineText(lines[201]))
	}
	if !strings.Contains(lineText(lines[251]), "row-400") {
		t.Fatalf("last tail row = %q", lineText(lines[251]))
	}
}

func TestScrollClamps(t *testing.T) {
	app, _ := newTestApp(t, 80, 10)
	for i := range 50 {
		app.AddSystemBlock(strings.Repeat("line ", 2) + string(rune('a'+i%26)))
	}
	app.mu.Lock()
	total := a_totalLines(app)
	app.mu.Unlock()
	if total < 20 {
		t.Fatalf("expected many lines, got %d", total)
	}
	app.scroll(10000, false)
	app.mu.Lock()
	off := app.sm.offset
	maxOK := app.sm.offset <= app.totalLinesLocked()
	app.mu.Unlock()
	if off == 0 || !maxOK {
		t.Fatalf("scroll-up clamp broken: off=%d", off)
	}
	if app.sm.Following() {
		t.Fatalf("scroll-up must clear follow")
	}
	app.scroll(10000, true)
	app.mu.Lock()
	off = app.sm.offset
	app.mu.Unlock()
	if off != 0 {
		t.Fatalf("scroll-down must return to follow: %d", off)
	}
}

// a_totalLines is a test helper around the lock-held method.
func a_totalLines(app *App) int { return app.totalLinesLocked() }

func TestHumanTokens(t *testing.T) {
	cases := map[int64]string{0: "0", 999: "999", 1500: "1.5k", 2_500_000: "2.5M"}
	for in, want := range cases {
		if got := humanTokens(in); got != want {
			t.Fatalf("humanTokens(%d) = %q, want %q", in, got, want)
		}
	}
}
