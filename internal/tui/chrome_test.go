package tui

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// screenText flattens a simulation screen into rows of text.
func screenText(scr tcell.SimulationScreen) string {
	prim, w, _ := scr.GetContents()
	var b strings.Builder
	for y := 0; y*w < len(prim); y++ {
		for x := 0; x < w && y*w+x < len(prim); x++ {
			if r := prim[y*w+x].Runes; len(r) > 0 {
				b.WriteString(string(r))
			} else {
				b.WriteByte(' ')
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// drawnApp is a test app with a non-empty transcript, so draw() takes the
// transcript path instead of the welcome screen.
func drawnApp(t *testing.T, w, h int) (*App, tcell.SimulationScreen) {
	t.Helper()
	app, scr := newTestApp(t, w, h)
	app.AddSystemBlock("ready")
	return app, scr
}

// lastRow is the shortcuts/status row (the last line of the dump).
func lastRow(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	return lines[len(lines)-1]
}

// TestHUDDefaultKeepsTokenCounter pins the shipped layout: with no
// statusLine.segments the HUD renders the session clock and the token
// counters, right aligned, and nothing else.
func TestHUDDefaultKeepsTokenCounter(t *testing.T) {
	app, scr := drawnApp(t, 100, 24)
	app.AddUsage(1200, 340)
	app.draw()

	text := screenText(scr)
	if !strings.Contains(text, "↑1.2k │ ↓340") {
		t.Fatalf("token counter missing:\n%s", text)
	}
	if !strings.Contains(text, "0s") {
		t.Fatalf("session clock missing from the default layout:\n%s", text)
	}
	if strings.Contains(text, "ctx ") || strings.Contains(text, "$0.") {
		t.Fatalf("unconfigured segments must not render:\n%s", text)
	}
}

// TestHUDConfiguredSegments: settings statusLine.segments picks which
// segments render, in order, from the statusLine* theme tokens. The row is
// wide enough for the path plus every segment; a narrow row drops segments by
// keep-rank so the clock and the decode rate stay readable (see
// TestStatusRowShowsPathAndMetrics).
func TestHUDConfiguredSegments(t *testing.T) {
	app, scr := drawnApp(t, 200, 24)
	app.AddUsage(50000, 50000)
	app.AddCost(0.0123)
	app.SetContextWindow(200000)
	app.SetStatusSegments([]string{"theme", "model", "context", "tokens", "cost"})
	app.draw()

	text := screenText(scr)
	for _, want := range []string{"groknight", "test/free", "ctx 100k/200k", "↑50k │ ↓50k", "$0.0123"} {
		if !strings.Contains(text, want) {
			t.Fatalf("configured segment %q missing:\n%s", want, text)
		}
	}
	// Order is the configured order, left to right on the row.
	row := lastRow(text)
	at := -1
	for _, seg := range []string{"groknight", "test/free", "ctx 100k/200k", "↑50k", "$0.0123"} {
		i := strings.Index(row, seg)
		if i < 0 || i < at {
			t.Fatalf("segment %q out of order in %q", seg, row)
		}
		at = i
	}

	// The meter reads the LAST request, not the session total: a second turn
	// of 1.5k in + 500 out re-bases it to 2k/200k, while the cumulative token
	// counters keep counting up.
	app.AddUsage(1500, 500)
	app.draw()
	row = lastRow(screenText(scr))
	if !strings.Contains(row, "ctx 2k/200k") {
		t.Fatalf("context meter must track the latest request, not the sum of all turns: %q", row)
	}
	if !strings.Contains(row, "↑51.5k │ ↓50.5k") {
		t.Fatalf("token counters must stay cumulative: %q", row)
	}

	// An unknown name is skipped, not rendered, and the rest still draw.
	app.SetStatusSegments([]string{"model", "hologram"})
	app.draw()
	text = screenText(scr)
	if strings.Contains(text, "hologram") {
		t.Fatalf("unknown segment rendered:\n%s", text)
	}
	if !strings.Contains(text, "test/free") {
		t.Fatalf("known segment dropped with the unknown one:\n%s", text)
	}
	// A segment whose data is not wired hides instead of drawing an empty
	// cell, while the segments that do have data keep rendering.
	app.SetStatusSegments([]string{"context", "cost"})
	app.SetContextWindow(0)
	app.draw()
	got := lastRow(screenText(scr))
	if strings.Contains(got, "ctx ") {
		t.Fatalf("context segment without a window must hide: %q", got)
	}
	if !strings.Contains(got, "$0.0123") {
		t.Fatalf("cost segment must survive an unwired context window: %q", got)
	}
}

// TestHUDTimeSegment counts total session time: the segment renders the
// elapsed clock, re-bases on SetSessionStart, and hides when unanchored.
func TestHUDTimeSegment(t *testing.T) {
	app, scr := drawnApp(t, 200, 24)
	app.SetStatusSegments([]string{"time", "tokens"})
	app.AddUsage(1200, 340)

	// Fresh app: the clock anchors at New (process start), so the first
	// draw shows a live (sub-minute) session time, not an empty cell.
	app.draw()
	row := lastRow(screenText(scr))
	if !strings.Contains(row, "s │ ↑") {
		t.Fatalf("session clock missing from the row: %q", row)
	}

	// A re-based clock (session swap) shows the carried-over span.
	app.SetSessionStart(time.Now().Add(-2*time.Hour - 5*time.Minute))
	app.draw()
	row = lastRow(screenText(scr))
	if !strings.Contains(row, "2h05m") {
		t.Fatalf("re-based session clock missing: %q", row)
	}

	// An unanchored clock hides instead of drawing an empty cell; the
	// segments that do have data keep rendering.
	app.SetSessionStart(time.Time{})
	app.draw()
	row = lastRow(screenText(scr))
	if strings.Contains(row, " │ ↑") || !strings.Contains(row, "↑1.2k │ ↓340") {
		t.Fatalf("unanchored clock must hide, tokens must survive: %q", row)
	}
}

// statusCell reads one cell from the shortcuts/status row.
func statusCell(scr tcell.SimulationScreen, x int) tcell.SimCell {
	prim, w, _ := scr.GetContents()
	return prim[(len(prim)/w-1)*w+x]
}

// TestHUDStatusLineBackground proves the statusLineBg token fills the status
// row, and that a theme leaving it at the terminal default does not.
func TestHUDStatusLineBackground(t *testing.T) {
	app, scr := drawnApp(t, 100, 24)
	app.AddUsage(10, 20)
	app.draw()
	if _, bg, _ := statusCell(scr, 96).Style.Decompose(); bg != tcell.ColorDefault {
		t.Fatalf("default theme must leave the status row transparent, got %v", bg)
	}

	custom := &theme.Theme{
		Name:  "banded",
		Dark:  true,
		Slots: map[string]theme.Color{theme.StatusLineBg: theme.Hex("#102030")},
	}
	app.SetTheme(custom)
	app.draw()
	if _, bg, _ := statusCell(scr, 96).Style.Decompose(); bg != tcell.NewRGBColor(0x10, 0x20, 0x30) {
		t.Fatalf("statusLineBg must fill the status row, got %v", bg)
	}
}

// TestSpinnerFramesFromTheme: the running indicator cycles the frames the
// theme names, not the built-in braille list.
func TestSpinnerFramesFromTheme(t *testing.T) {
	app, scr := drawnApp(t, 100, 24)
	th := theme.Load("groknight")
	th.Symbols = theme.Symbols{Status: []string{"X", "Y"}}
	app.SetTheme(th)
	app.SetRunning(true)

	app.mu.Lock()
	app.st.spinnerIdx = 0
	app.mu.Unlock()
	app.draw()
	if text := screenText(scr); !strings.Contains(text, "test/free · X") {
		t.Fatalf("theme frame 0 not drawn:\n%s", text)
	}

	app.mu.Lock()
	app.st.spinnerIdx = 1
	app.mu.Unlock()
	app.draw()
	if text := screenText(scr); !strings.Contains(text, "test/free · Y") {
		t.Fatalf("theme frame 1 not drawn:\n%s", text)
	}
}

// TestStatusRowShowsPathAndMetrics pins the bottom row's contract: the
// working directory on the left, the session clock and the decode rate
// right-aligned, and no keyboard chords anywhere (they live in /hotkeys and
// the welcome menu now). The metrics own the width: the path tail-truncates
// and the optional segments drop before the clock or the rate is touched.
func TestStatusRowShowsPathAndMetrics(t *testing.T) {
	const deep = "/Volumes/work/example/freepeak/checkout/xdev-feature"

	// seededStatusRow draws the row for a deep path with a 2h05m clock and a
	// measured 42.5 t/s.
	seededStatusRow := func(t *testing.T, w int) string {
		t.Helper()
		app, scr := drawnApp(t, w, 4)
		app.AddUsage(50000, 50000)
		app.SetSessionStart(time.Now().Add(-2*time.Hour - 5*time.Minute))
		app.SetLocation(deep)
		app.mu.Lock()
		app.st.Rate = 42.5
		app.mu.Unlock()
		app.draw()
		return lastRow(screenText(scr))
	}

	wide := seededStatusRow(t, 160)
	for _, want := range []string{deep, "2h05m", "↑50k │ ↓50k", "42.5 t/s"} {
		if !strings.Contains(wide, want) {
			t.Fatalf("wide row missing %q: %q", want, wide)
		}
	}

	narrow := seededStatusRow(t, 60)
	if !strings.Contains(narrow, "2h05m") || !strings.Contains(narrow, "42.5 t/s") {
		t.Fatalf("rate and total time must share a narrow row, got %q", narrow)
	}
	if strings.Contains(narrow, "↑50k") {
		t.Fatalf("the token counter must drop before the metrics do: %q", narrow)
	}
	// A path too long for the row keeps the components that identify the
	// project and marks the cut, instead of clipping at the screen edge.
	if !strings.Contains(narrow, "…/freepeak/checkout/xdev-feature") {
		t.Fatalf("60-column row must tail-keep the path: %q", narrow)
	}
	for _, gone := range []string{"send", "newline", "cancel", "quit", "⏎", "^J"} {
		if strings.Contains(narrow, gone) {
			t.Fatalf("keyboard chords no longer belong on the row: %q", narrow)
		}
	}

	tiny := seededStatusRow(t, 40)
	if !strings.Contains(tiny, "2h05m") || !strings.Contains(tiny, "42.5 t/s") {
		t.Fatalf("40-column row lost the metrics: %q", tiny)
	}
	if !strings.Contains(tiny, "…/xdev-feature") {
		t.Fatalf("40-column row must shorten the path, not the metrics: %q", tiny)
	}

	// The home directory abbreviates rather than eating the row.
	if home, err := os.UserHomeDir(); err == nil {
		if got := pathDisplay(home+"/work/proj", 40); got != "~/work/proj" {
			t.Fatalf("home must abbreviate, got %q", got)
		}
	}
}

// TestBoxStyleFromTheme: boxRound keeps today's glyphs, boxSharp swaps the
// outline, and the ascii preset degenerates it.
func TestBoxStyleFromTheme(t *testing.T) {
	app, scr := drawnApp(t, 60, 20)
	app.draw()
	if text := screenText(scr); !strings.Contains(text, "╰ test/free") {
		t.Fatalf("default round composer missing:\n%s", text)
	}

	sharp := &theme.Theme{Name: "sharp", Dark: true, Slots: map[string]theme.Color{}, Symbols: theme.Symbols{Box: "sharp"}}
	app.SetTheme(sharp)
	app.draw()
	if text := screenText(scr); !strings.Contains(text, "┌") || strings.Contains(text, "╭") {
		t.Fatalf("sharp composer missing:\n%s", text)
	}

	ascii := &theme.Theme{Name: "ascii", Dark: true, Slots: map[string]theme.Color{}, Symbols: theme.Symbols{Preset: "ascii"}}
	app.SetTheme(ascii)
	app.draw()
	if text := screenText(scr); !strings.Contains(text, "+-") || strings.Contains(text, "╭") {
		t.Fatalf("ascii composer missing:\n%s", text)
	}
}

// askResultOf drives one card to completion and returns its answer.
func askResultOf(t *testing.T, app *App, req AskRequest, timeout time.Duration, keys ...any) (AskAnswer, bool) {
	t.Helper()
	type outcome struct {
		ans AskAnswer
		ok  bool
	}
	done := make(chan outcome, 1)
	go func() {
		ans, ok := app.AskCard(context.Background(), req, timeout)
		done <- outcome{ans, ok}
	}()
	waitAsk(t, app, true)
	for _, k := range keys {
		switch v := k.(type) {
		case tcell.Key:
			pressKey(app, v)
		case rune:
			pressRune(app, v)
		}
	}
	select {
	case got := <-done:
		return got.ans, got.ok
	case <-time.After(3 * time.Second):
		t.Fatal("ask card did not resolve")
		return AskAnswer{}, false
	}
}

// waitAsk waits for the card to open (or close) without racing the UI thread.
func waitAsk(t *testing.T, app *App, want bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if app.AskPending() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("ask card pending = %v", !want)
}

func askOptions() AskRequest {
	return AskRequest{
		Question: "Which storage backend should the fix target?",
		Options: []AskOption{
			{Label: "sqlite", Description: "local file"},
			{Label: "postgres"},
			{Label: "mysql"},
		},
	}
}

// TestAskCardRendersAndConfirms drives the blocking card end to end through
// the real key path: Enter answers with the highlighted option.
func TestAskCardRendersAndConfirms(t *testing.T) {
	app, _ := drawnApp(t, 100, 30)
	ans, ok := askResultOf(t, app, askOptions(), 5*time.Second, tcell.KeyDown, tcell.KeyEnter)
	if !ok || len(ans.Labels) != 1 || ans.Labels[0] != "postgres" {
		t.Fatalf("answer = %+v ok=%v", ans, ok)
	}
	if app.AskPending() {
		t.Fatal("card must close after Enter")
	}
}

// TestAskCardDrawsQuestionAndOptions: the card is visible with its question,
// every option, the recommended marker, and the key hint.
func TestAskCardDrawsQuestionAndOptions(t *testing.T) {
	app, scr := drawnApp(t, 100, 30)
	req := askOptions()
	req.Recommended = []string{"postgres"}
	done := make(chan struct{})
	go func() {
		_, _ = app.AskCard(context.Background(), req, 5*time.Second)
		close(done)
	}()
	waitAsk(t, app, true)
	app.draw()
	text := screenText(scr)
	for _, want := range []string{"storage backend", "sqlite", "postgres", "(recommended)", "1-9 quick pick", "Esc skip"} {
		if !strings.Contains(text, want) {
			t.Fatalf("card missing %q:\n%s", want, text)
		}
	}
	if sel, n := app.AskSelection(); sel != 1 || n != 3 {
		t.Fatalf("selection = %d/%d", sel, n)
	}
	// A modal card swallows typing: nothing reaches the composer.
	typeRunes(app, "hello")
	if app.ed.Text() != "" {
		t.Fatalf("composer received %q while the card was open", app.ed.Text())
	}
	pressKey(app, tcell.KeyEsc)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Esc must close the card")
	}
	if app.AskPending() {
		t.Fatal("card must close after Esc")
	}
}

// TestAskCardQuickPickAndSkip: digits confirm, Esc skips with no labels, and
// a timeout resolves as a skip so nothing can hang on an unattended card.
func TestAskCardQuickPickAndSkip(t *testing.T) {
	app, _ := drawnApp(t, 100, 30)
	if ans, ok := askResultOf(t, app, askOptions(), 5*time.Second, '3'); !ok || ans.Labels[0] != "mysql" {
		t.Fatalf("quick pick = %+v ok=%v", ans, ok)
	}
	ans, ok := askResultOf(t, app, askOptions(), 5*time.Second, tcell.KeyEsc)
	if ok || len(ans.Labels) != 0 {
		t.Fatalf("skip = %+v ok=%v", ans, ok)
	}
	ans, ok = askResultOf(t, app, askOptions(), 40*time.Millisecond)
	if ok || len(ans.Labels) != 0 {
		t.Fatalf("timeout = %+v ok=%v", ans, ok)
	}
}

// TestAskCardMultiSelect: Space toggles, Enter returns every toggled label.
func TestAskCardMultiSelect(t *testing.T) {
	app, _ := drawnApp(t, 100, 30)
	req := askOptions()
	req.Multi = true
	ans, ok := askResultOf(t, app, req, 5*time.Second, tcell.KeyDown, ' ', tcell.KeyUp, ' ', tcell.KeyEnter)
	if !ok || len(ans.Labels) != 2 || ans.Labels[0] != "sqlite" || ans.Labels[1] != "postgres" {
		t.Fatalf("multi answer = %+v ok=%v", ans, ok)
	}
}

// TestAskCardNilSafe: an empty option list and a canceled context both
// resolve as a skip instead of parking a card nobody can answer.
func TestAskCardNilSafe(t *testing.T) {
	app, _ := drawnApp(t, 100, 30)
	if ans, ok := app.AskCard(context.Background(), AskRequest{Question: "no options"}, time.Second); ok || len(ans.Labels) != 0 {
		t.Fatalf("empty options = %+v ok=%v", ans, ok)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := app.AskCard(ctx, askOptions(), time.Second); ok {
		t.Fatal("canceled context must skip")
	}
	if app.AskPending() {
		t.Fatal("no card may stay on screen")
	}
}

// TestAskOpsSeam: the seam cmd hands to the ask tool resolves through the
// card with the timeout the host configured.
func TestAskOpsSeam(t *testing.T) {
	app, _ := drawnApp(t, 100, 30)
	ops := app.NewAskOps(30 * time.Millisecond)
	if ops == nil || ops.Show == nil {
		t.Fatal("seam must carry Show")
	}
	if ans, ok := ops.Show(context.Background(), askOptions(), time.Second); ok || len(ans.Labels) != 0 {
		t.Fatalf("unattended seam call = %+v ok=%v", ans, ok)
	}
	done := make(chan AskAnswer, 1)
	go func() {
		ans, _ := ops.Show(context.Background(), askOptions(), time.Second)
		done <- ans
	}()
	waitAsk(t, app, true)
	pressRune(app, '2')
	select {
	case ans := <-done:
		if len(ans.Labels) != 1 || ans.Labels[0] != "postgres" {
			t.Fatalf("seam answer = %+v", ans)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("seam did not resolve")
	}
}

// TestAskCardNarrowWindowNotice: with no room to draw the card the question
// still surfaces as a transcript notice, and the call resolves as a skip so
// the tool's headless policy answers instead of the question vanishing.
func TestAskCardNarrowWindowNotice(t *testing.T) {
	app, _ := newTestApp(t, 8, 12)
	if _, ok := app.AskCard(context.Background(), askOptions(), time.Second); ok {
		t.Fatal("a window too narrow to draw the card must take the skip path")
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.ask != nil {
		t.Fatal("no card may stay on screen")
	}
	if len(app.blocks) == 0 || !strings.Contains(app.blocks[len(app.blocks)-1].Text, "storage backend") {
		t.Fatalf("question must surface as a notice: %+v", app.blocks)
	}
}

// TestTopBarCarriesBranchAndLastPrompt pins the persistent header: once a
// transcript is on screen, row 0 keeps the git branch AND the newest user
// prompt collapsed to one line — so the request being answered stays visible
// even while its transcript band scrolls away. Neither the directory path nor
// the model name is on the bar (the status row and the composer's info divider
// carry them; both removals were user-requested).
func TestTopBarCarriesBranchAndLastPrompt(t *testing.T) {
	app, scr := newTestApp(t, 100, 24)
	dir := t.TempDir()
	app.SetLocation(dir)
	app.mu.Lock()
	app.branch = "fix/boxes"
	app.mu.Unlock()
	app.AddUserBlock("fix the   tool\noutput box please")
	app.AddSystemBlock(strings.Repeat("line\n", 60)) // guarantees hidden rows
	app.draw()

	rows := strings.Split(strings.TrimRight(screenText(scr), "\n"), "\n")
	bar := rows[0]
	for _, want := range []string{"❯ fix/boxes", "· fix the tool"} {
		if !strings.Contains(bar, want) {
			t.Fatalf("top bar %q missing %q", bar, want)
		}
	}
	if dirName := dir[strings.LastIndex(dir, "/")+1:]; strings.Contains(bar, dirName) {
		t.Fatalf("top bar still carries the directory path: %q", bar)
	}
	app.mu.Lock()
	model := app.st.Model
	app.mu.Unlock()
	if model == "" || strings.Contains(bar, model) {
		t.Fatalf("top bar %q must not carry the model name %q", bar, model)
	}
	// The transcript starts below the bar: the bar is chrome, content rows
	// belong to the scrollback — and at the tail of a 60-row block the bar
	// still names the last prompt.
	if !strings.Contains(rows[1], "line") {
		t.Fatalf("first transcript row lost to the top bar: %q", rows[1])
	}
}
