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
	app.FinishTool("read", false, "1:hi\n2:there", ToolOutcome{Dur: "3ms"})

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

// TestToolCallRowShowsNamedArgument pins omp's call row: the tool name and
// the naming argument as a phrase, never the raw JSON the model sent.
func TestToolCallRowShowsNamedArgument(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.AddToolBlock("bash", `{"command":"seq 1 400","timeout":120}`)
	app.mu.Lock()
	lines := app.blockLines(0, app.blocks[0], 80)
	app.mu.Unlock()

	if len(lines) != 1 {
		t.Fatalf("call row = %d lines, want 1", len(lines))
	}
	got := lineText(lines[0])
	if !strings.Contains(got, "bash") || !strings.Contains(got, "seq 1 400") {
		t.Fatalf("call row = %q, want the tool name and its command", got)
	}
	if strings.Contains(got, `"command"`) || strings.Contains(got, "timeout") {
		t.Fatalf("call row leaked the raw arguments: %q", got)
	}
}

// TestToolResultRendersBox pins the frame for a result standing alone (no call
// row above it): a rounded box whose top border names the tool, whose body
// keeps every output line, and whose wall time prints in omp's footer row
// rather than in the border.
func TestToolResultRendersBox(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.FinishTool("bash", false, "line-one\nline-two\nline-three", ToolOutcome{Dur: "5ms"})
	app.mu.Lock()
	i := len(app.blocks) - 1
	lines := app.blockLines(i, app.blocks[i], 80)
	app.mu.Unlock()

	if len(lines) != 6 { // top + 3 body + footer + bottom
		t.Fatalf("boxed render = %d lines, want 6:\n%s", len(lines), joinLines(lines))
	}
	top := lineText(lines[0])
	for _, want := range []string{"╭", "bash"} {
		if !strings.Contains(top, want) {
			t.Fatalf("top border %q missing %q", top, want)
		}
	}
	body := lineText(lines[1]) + lineText(lines[2]) + lineText(lines[3])
	for _, want := range []string{"line-one", "line-two", "line-three"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q; got:\n%s", want, body)
		}
	}
	if footer := lineText(lines[4]); !strings.Contains(footer, "Wall: 5ms") {
		t.Fatalf("footer = %q, want the wall time", footer)
	}
	if bottom := lineText(lines[5]); !strings.Contains(bottom, "╰") || !strings.Contains(bottom, "╯") {
		t.Fatalf("bottom border %q not closed", bottom)
	}
	// Every row shares one right edge: widths equal the requested 80 cells.
	for i, ln := range lines {
		if w := width(lineText(ln)); w != 80 {
			t.Fatalf("row %d width = %d, want 80 (%q)", i, w, lineText(ln))
		}
	}
}

// TestToolResultBoxAlignsTabbedOutput pins the omp sanitize-before-frame
// step: a tab is zero cells to runewidth but paints as an advance to the
// next 8-column stop, so an unsanitized tab drags its row's right border
// off the shared edge. Every framed row must still land on one column.
func TestToolResultBoxAlignsTabbedOutput(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	out := "\tmsg := ai.Message{\n\t\tRole: ai.RoleUser,\n\t}\r\n\x1b[31mred\x1b[0m"
	app.FinishTool("bash", false, out, ToolOutcome{Dur: "5ms"})
	app.mu.Lock()
	i := len(app.blocks) - 1
	lines := app.blockLines(i, app.blocks[i], 80)
	app.mu.Unlock()

	for n, ln := range lines {
		if w := width(lineText(ln)); w != 80 {
			t.Fatalf("row %d width = %d, want 80 (%q)", n, w, lineText(ln))
		}
	}
	body := lineText(lines[1]) + lineText(lines[2]) + lineText(lines[3])
	if !strings.Contains(body, "   msg := ai.Message{") || !strings.Contains(body, "      Role: ai.RoleUser,") {
		t.Fatalf("tab indentation lost, not expanded to the 3-cell stop:\n%s", body)
	}
	if strings.ContainsAny(body, "\x1b[") {
		t.Fatalf("raw escape bytes reached the frame: %q", body)
	}
}

// TestToolResultExitCodeSitsInTheFooter pins omp's division of labour: the
// frame under its own call row repeats no name, the body keeps the output
// without the marker the footer now reports, and the exit code appears exactly
// once — as a status, not as prose inside the result.
func TestToolResultExitCodeSitsInTheFooter(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.AddToolBlock("bash", `{"command":"sh -c 'exit 9'"}`)
	app.FinishTool("bash", true, "boom\n[exit code 9]", ToolOutcome{Dur: "80ms", Exit: 9, HasExit: true})
	app.mu.Lock()
	i := len(app.blocks) - 1
	lines := app.blockLines(i, app.blocks[i], 80)
	app.mu.Unlock()

	if len(lines) != 4 { // top + "boom" + footer + bottom
		t.Fatalf("frame = %d lines, want 4:\n%s", len(lines), joinLines(lines))
	}
	if top := lineText(lines[0]); strings.Contains(top, "bash") {
		t.Fatalf("top border repeats what the call row above already said: %q", top)
	}
	body := lineText(lines[1])
	if !strings.Contains(body, "boom") || strings.Contains(body, "exit code") {
		t.Fatalf("body = %q, want the output without the exit marker", body)
	}
	footer := lineText(lines[2])
	for _, want := range []string{"Wall: 80ms", "Exit: 9"} {
		if !strings.Contains(footer, want) {
			t.Fatalf("footer %q missing %q", footer, want)
		}
	}
}

// TestToolResultRowWindow pins the bounded render window and the Ctrl+O
// affordance that closes it: head rows, the hidden-line notice at the hole it
// describes, tail rows — and every row once expanded, with no notice.
func TestToolResultRowWindow(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	var b strings.Builder
	for i := 1; i <= 400; i++ {
		fmt.Fprintf(&b, "row-%03d\n", i)
	}
	app.FinishTool("bash", false, strings.TrimSuffix(b.String(), "\n"), ToolOutcome{Dur: "9ms"})
	render := func() []line {
		app.mu.Lock()
		defer app.mu.Unlock()
		i := len(app.blocks) - 1
		return app.blockLines(i, app.blocks[i], 80)
	}

	lines := render()
	if len(lines) != 200+50+1+3 { // top + head + notice + tail + footer + bottom
		t.Fatalf("windowed render = %d lines, want 254", len(lines))
	}
	if !strings.Contains(lineText(lines[1]), "row-001") {
		t.Fatalf("first head row = %q", lineText(lines[1]))
	}
	if notice := lineText(lines[201]); !strings.Contains(notice, "150 lines hidden") || !strings.Contains(notice, "Ctrl+O") {
		t.Fatalf("hidden notice = %q", notice)
	}
	if !strings.Contains(lineText(lines[251]), "row-400") {
		t.Fatalf("last tail row = %q", lineText(lines[251]))
	}

	if !app.ToggleToolExpand() {
		t.Fatal("Ctrl+O found no tool result to expand")
	}
	lines = render()
	if len(lines) != 400+3 { // top + every row + footer + bottom
		t.Fatalf("expanded render = %d lines, want 403", len(lines))
	}
	if strings.Contains(lineText(lines[201]), "hidden") {
		t.Fatalf("expanded frame still hides rows: %q", lineText(lines[201]))
	}
	if !strings.Contains(lineText(lines[400]), "row-400") {
		t.Fatalf("last row after expand = %q", lineText(lines[400]))
	}
}

// joinLines renders a line slice as text for failure messages.
func joinLines(lines []line) string {
	var b strings.Builder
	for _, ln := range lines {
		b.WriteString(lineText(ln))
		b.WriteString("\n")
	}
	return b.String()
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
