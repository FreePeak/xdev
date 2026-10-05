package tui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
	"github.com/FreePeak/xdev/internal/tool"
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
// statusLine.segments the HUD renders the two dsh pills — the work timer and
// the session token total — right aligned, and nothing else. The token
// reading is the PILL (dsh's UsagePill: total, with the cache hit rate
// beside it once anything was actually served from the cache), not the old
// ↑⇢↓ split: that is one click away, and one settings entry (`split`) away
// for a session that wants it inline.
func TestHUDDefaultKeepsTokenCounter(t *testing.T) {
	app, scr := drawnApp(t, 100, 24)
	app.AddUsage(1200, 340, 0, 0, 1540)
	app.draw()

	text := screenText(scr)
	if !strings.Contains(text, "1.5k") {
		t.Fatalf("token pill missing:\n%s", text)
	}
	if strings.Contains(text, "↑") || strings.Contains(text, "↓") {
		t.Fatalf("the default row must be the pill, not the glyph split:\n%s", text)
	}
	if !strings.Contains(text, "0s") {
		t.Fatalf("the work timer must read 0s on a session that never ran:\n%s", text)
	}
	if strings.Contains(text, "ctx ") {
		t.Fatalf("the context segment must hide without a known window:\n%s", text)
	}
	if strings.Contains(text, "$0.") {
		t.Fatalf("unconfigured segments must not render:\n%s", text)
	}

	// The context meter is a popup row now, not a segment: the row stays
	// two pills wide whatever the window turns out to be.
	app.SetContextWindow(200000)
	app.draw()
	if row := lastRow(screenText(scr)); strings.Contains(row, "ctx ") {
		t.Fatalf("the context meter left the row: %q", row)
	}
}

// TestHUDContextCountsCachedInput is the regression: a cached turn re-read 93k
// of prompt and answered with 362 tokens, so the whole request costs 101.8k
// even though only 8272 input tokens were billed at the full rate. The ctx
// number is the provider's request total — an input+output sum of 8.6k read 90%
// low, the bug this segment had. Both readings are opt-in segments now (the
// default row is the two dsh pills), so the case configures them by name; the
// numbers they compute are what the token pill's popup breaks out.
func TestHUDContextCountsCachedInput(t *testing.T) {
	app, scr := drawnApp(t, 100, 24)
	app.SetStatusSegments([]string{"context", "split"})
	app.AddUsage(101818-93546, 362, 93546, 118, 101818)
	app.SetContextWindow(200000)
	app.draw()

	row := lastRow(screenText(scr))
	if !strings.Contains(row, "ctx 101.8k/200k") {
		t.Fatalf("cached prompt tokens missing from the context total: %q", row)
	}
	if strings.Contains(row, "ctx 8.6k") {
		t.Fatalf("the context total must not be the uncached input+output sum: %q", row)
	}
	if !strings.Contains(row, "↑8.3k ⇢93.5k │ ↓362 ˟118") {
		t.Fatalf("the counters must show the whole billed split, not input+output alone: %q", row)
	}
}

// TestHUDContextTracksTheSession pins the number's meaning across the
// transcript resets that end or move a session: /new, /resume and tree
// navigation empty the live context (so it must not keep showing the old
// session's), and a replayed transcript measures its rebuilt history back in
// with the agent's own context count.
func TestHUDContextTracksTheSession(t *testing.T) {
	app, scr := drawnApp(t, 100, 24)
	app.SetStatusSegments([]string{"context"})
	app.AddUsage(50000, 50000, 0, 0, 100000)
	app.SetContextWindow(200000)
	app.draw()
	if row := lastRow(screenText(scr)); !strings.Contains(row, "ctx 100k/200k") {
		t.Fatalf("context total missing after a turn: %q", row)
	}

	// A reset transcript occupies nothing: the segment hides rather than
	// keep claiming the previous session's 100k.
	app.Reset()
	app.draw()
	if row := lastRow(screenText(scr)); strings.Contains(row, "ctx ") {
		t.Fatalf("context total must not survive a session reset: %q", row)
	}

	// Resume rebuilds the history without sending a request; the replay
	// measurement brings the number back (and a replayed 40k never overrides
	// a live 60k that already answered).
	app.SetContextReplay(40000)
	app.draw()
	if row := lastRow(screenText(scr)); !strings.Contains(row, "ctx 40k/200k") {
		t.Fatalf("replayed history must report its context: %q", row)
	}
	app.AddUsage(30000, 30000, 0, 0, 60000)
	app.SetContextReplay(40000)
	app.draw()
	if row := lastRow(screenText(scr)); !strings.Contains(row, "ctx 60k/200k") {
		t.Fatalf("a replay must not stale a live measurement: %q", row)
	}
}

// TestHUDConfiguredSegments: settings statusLine.segments picks which
// segments render, in order, from the statusLine* theme tokens. The row is
// wide enough for the path plus every segment; a narrow row drops segments by
// keep-rank so the clock and the decode rate stay readable (see
// TestStatusRowShowsPathAndMetrics).
func TestHUDConfiguredSegments(t *testing.T) {
	app, scr := drawnApp(t, 200, 24)
	app.AddUsage(50000, 50000, 0, 0, 100000)
	app.AddCost(0.0123)
	app.SetContextWindow(200000)
	app.SetStatusSegments([]string{"theme", "model", "context", "split", "cost"})
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
	// costing 2k re-bases it to 2k/200k, while the cumulative token counters
	// keep counting up.
	app.AddUsage(1500, 500, 0, 0, 2000)
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

// TestHUDTimeSegment counts WORK time only: the segment renders the banked
// active spans plus the live span of a run in flight, re-bases on SetWork (the
// work a replayed history carried), and freezes — it does not tick — while the
// agent is idle or parked on a question card.
func TestHUDTimeSegment(t *testing.T) {
	app, scr := drawnApp(t, 200, 24)
	app.SetStatusSegments([]string{"time", "split"})
	app.AddUsage(1200, 340, 0, 0, 1540)

	// Fresh app, nothing has run: the honest reading is 0s, not a clock
	// inherited from process start. "0s" is a reading, so it renders.
	app.draw()
	row := lastRow(screenText(scr))
	if !strings.Contains(row, "0s │ ▤↑1.2k │ ↓340") {
		t.Fatalf("zero work must render beside the tokens: %q", row)
	}

	// A re-based total (resume/fork replay) shows the carried-over work.
	app.SetWork(2*time.Hour + 5*time.Minute)
	app.draw()
	row = lastRow(screenText(scr))
	if !strings.Contains(row, "2h05m") {
		t.Fatalf("carried work missing from the row: %q", row)
	}

	// Idle, the number does not move: a second draw reads the same.
	app.draw()
	if r2 := lastRow(screenText(scr)); r2 != row {
		t.Fatalf("idle HUD time must not tick: %q then %q", row, r2)
	}

	// A live run counts: an open span adds to the banked total.
	app.SetRunning(true)
	app.mu.Lock()
	app.st.runStart = time.Now().Add(-90 * time.Second)
	app.mu.Unlock()
	app.draw()
	if row = lastRow(screenText(scr)); !strings.Contains(row, "2h06m") {
		t.Fatalf("open run span must add to the total: %q", row)
	}

	// Ending the run folds the span in and stops the count.
	app.SetRunning(false)
	app.draw()
	frozen := lastRow(screenText(scr))
	if !strings.Contains(frozen, "2h06m") {
		t.Fatalf("the finished span must be banked, not lost: %q", frozen)
	}
	app.draw()
	if r2 := lastRow(screenText(scr)); r2 != frozen {
		t.Fatalf("a finished run must not keep counting: %q then %q", frozen, r2)
	}
}

// TestHUDTimeFreezesOnAskCard: a question card waiting for the human is not
// work the session did. The number stops mid-run while the card is up, and
// starts again the moment it is answered.
func TestHUDTimeFreezesOnAskCard(t *testing.T) {
	app, _ := drawnApp(t, 100, 30)
	app.SetRunning(true)

	app.mu.Lock()
	before := app.activeWork()
	app.mu.Unlock()
	_, _ = askResultOf(t, app, AskRequest{Question: "which?", Options: []AskOption{{Label: "a"}}}, 5*time.Second, tcell.KeyEnter)

	app.mu.Lock()
	after := app.activeWork()
	askWaits := app.st.askWaits
	app.mu.Unlock()
	if askWaits != 0 {
		t.Fatalf("the card left a wait claimed: %d", askWaits)
	}
	// The answer is instantaneous, so the run only accrued the few ms the
	// card spent opening; a wall-clock wait would add whole seconds.
	if after-before > 500*time.Millisecond {
		t.Fatalf("the clock ran while the card waited: %v → %v", before, after)
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
	app.AddUsage(10, 20, 0, 0, 30)
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
// theme names, not the built-in braille list, and it LEADS the divider's
// model pair (it used to trail "model · level", where the moving glyph read
// as part of the static reasoning label).
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
	if text := screenText(scr); !strings.Contains(text, "X · test/free") {
		t.Fatalf("theme frame 0 not drawn ahead of the model:\n%s", text)
	}

	app.mu.Lock()
	app.st.spinnerIdx = 1
	app.mu.Unlock()
	app.draw()
	if text := screenText(scr); !strings.Contains(text, "Y · test/free") {
		t.Fatalf("theme frame 1 not drawn ahead of the model:\n%s", text)
	}
}

// TestStatusRowShowsPathAndMetrics pins the bottom row's contract: the
// working directory on the left, the active-work timer and the decode rate
// right-aligned, and no keyboard chords anywhere (they live in /hotkeys and
// the welcome menu now). The metrics own the width: the path tail-truncates
// and the optional segments drop before the timer or the rate is touched.
func TestStatusRowShowsPathAndMetrics(t *testing.T) {
	const deep = "/Volumes/work/harvey/freepeak/checkout/xdev-feature"

	// seededStatusRow draws the row for a deep path with 2h05m of banked work
	// and a measured 42.5 t/s.
	seededStatusRow := func(t *testing.T, w int) string {
		app, scr := drawnApp(t, w, 20)
		app.AddSystemBlock("ready")
		app.AddUsage(50000, 50000, 0, 0, 100000)
		app.SetWork(2*time.Hour + 5*time.Minute)
		app.SetLocation(deep)
		app.mu.Lock()
		app.st.Rate = 42.5
		app.mu.Unlock()
		app.draw()
		return lastRow(screenText(scr))
	}

	wide := seededStatusRow(t, 160)
	for _, want := range []string{deep, "2h05m", "100k", "42.5 t/s"} {
		if !strings.Contains(wide, want) {
			t.Fatalf("wide row missing %q: %q", want, wide)
		}
	}

	narrow := seededStatusRow(t, 60)
	if !strings.Contains(narrow, "2h05m") || !strings.Contains(narrow, "42.5 t/s") {
		t.Fatalf("rate and total time must share a narrow row, got %q", narrow)
	}
	// Both pills are the headline now, so a narrow row gives way on the PATH
	// rather than on a pill: the drop loop sheds theme, model and the
	// opt-in refinements first (see statusKeepRank).
	if !strings.Contains(narrow, "100k") {
		t.Fatalf("both pills must survive a narrow row: %q", narrow)
	}
	// A path too long for the row keeps the one component that identifies the
	// project — the folder name — and drops the parents, instead of clipping at
	// the screen edge or spending the same cells on "…/freepeak/xdev". What
	// must not happen is the path disappearing.
	if strings.Contains(narrow, "…") {
		t.Fatalf("60-column row must show the folder name whole: %q", narrow)
	}
	if !strings.Contains(narrow, "xdev-feature") {
		t.Fatalf("60-column row must keep the project name: %q", narrow)
	}
	if home, err := os.UserHomeDir(); err == nil {
		if got := pathDisplay(home+"/work/proj", 40); got != "~/work/proj" {
			t.Fatalf("home must abbreviate, got %q", got)
		}
		if got := pathDisplay(home+"/work/harvey/proj", 8); got != "proj" {
			t.Fatalf("a narrow row must show the folder name alone, got %q", got)
		}
		if got := pathDisplay("/", 40); got != "/" {
			t.Fatalf("the root must not degrade to an empty name, got %q", got)
		}
	}
}

// TestBoxStyleFromTheme: with the composer frame gone, theme.Box shows
// through the tool-result box — round keeps today's glyphs, boxSharp swaps
// the outline, and the ascii preset degenerates it to +-| (BRO-614).
func TestBoxStyleFromTheme(t *testing.T) {
	app, scr := drawnApp(t, 60, 20)
	app.AddToolBlock("", "bash", `{"command":"echo hi"}`)
	app.FinishTool("", "bash", false, "done", ToolOutcome{Dur: "1ms"})
	app.draw()
	if text := screenText(scr); !strings.Contains(text, "╭") || !strings.Contains(text, "╰") {
		t.Fatalf("default round tool box missing:\n%s", text)
	}

	sharp := &theme.Theme{Name: "sharp", Dark: true, Slots: map[string]theme.Color{}, Symbols: theme.Symbols{Box: "sharp"}}
	app.SetTheme(sharp)
	app.draw()
	if text := screenText(scr); !strings.Contains(text, "┌") || !strings.Contains(text, "└") || strings.Contains(text, "╭") {
		t.Fatalf("sharp tool box missing:\n%s", text)
	}

	ascii := &theme.Theme{Name: "ascii", Dark: true, Slots: map[string]theme.Color{}, Symbols: theme.Symbols{Preset: "ascii"}}
	app.SetTheme(ascii)
	app.draw()
	if text := screenText(scr); !strings.Contains(text, "+") || !strings.Contains(text, "|") || strings.Contains(text, "╭") || strings.Contains(text, "┌") {
		t.Fatalf("ascii tool box missing:\n%s", text)
	}
}

// TestComposerIsBoxed pins the composer's border box (grok's full TUI —
// PromptStyle::default carries show_borders: true, chrome_pad_left: 2,
// chrome_pad_right: 1): ╭─╮ on top, │ on both sides of every draft row,
// ╰─ model ─╯ underneath. d589778 copied minimal mode's borderless fill band
// into the interactive TUI, where a text field without an outline reads as a
// dropped frame.
func TestComposerIsBoxed(t *testing.T) {
	app, scr := drawnApp(t, 60, 14)
	setDraft(&app.ed, "hello there", 11)
	app.draw()
	if got := app.composerRows(); got != 3 { // one draft row + two borders
		t.Fatalf("one-row composer = %d rows, want 3", got)
	}

	text := screenText(scr)
	if !strings.Contains(text, "╭") || !strings.Contains(text, "╰") {
		t.Fatalf("composer lost its box:\n%s", text)
	}
	if !strings.Contains(text, "│ ❯ hello there") {
		t.Fatalf("draft row is not framed by side borders:\n%s", text)
	}
	if !strings.Contains(text, "╰ test/free") {
		t.Fatalf("model name left the bottom border:\n%s", text)
	}
}

// TestUserBandPaintsUnderGlyphs pins the sent-message band as one surface:
// every cell of a banded transcript row that carries the prompt's own runes
// shows the band background, not the terminal default. A run painted with a
// foreground-only style resets its cells (tcell's zero background is
// ColorDefault), so the row fill survives only in the gaps and the user
// input reads as black behind the glyphs inside the band.
//
// The ❯ sits one cell in (userBandInset), not flush against the edge: the
// prompt is a card with a margin, so the scan starts where the glyph is.
func TestUserBandPaintsUnderGlyphs(t *testing.T) {
	app, scr := drawnApp(t, 80, 20)
	app.AddUserBlock("hello band")
	app.draw()

	band, ok := app.th.Slot(theme.BgHighlight)
	if !ok {
		t.Fatal("built-in theme must carry a user band")
	}
	want := app.cellColor(band)
	prim, w, _ := scr.GetContents()
	x0, y0 := -1, -1
	for i := range prim {
		// The band row is a transcript row, and with no top bar it can be
		// row 0; what identifies it is the leading ❯ at the band's margin.
		if i%w == userBandInset && len(prim[i].Runes) > 0 && prim[i].Runes[0] == '❯' {
			x0, y0 = i%w, i/w
			break
		}
	}
	if x0 < 0 {
		t.Fatal("no user band row painted")
	}
	// Cells of "❯ hello band": gutter, space, and the ten prompt runes.
	for x := userBandInset; x < userBandInset+12; x++ {
		_, bg, _ := prim[y0*w+x].Style.Decompose()
		if bg != want {
			t.Fatalf("band row cell %d (rune %q) bg = %s, want the band %v under the glyphs", x, string(prim[y0*w+x].Runes), bg, band)
		}
	}
}

// TestUserBandHasItsOwnAir pins the padding on all four sides of a sent
// message: a blank banded row above AND below the prompt, so it hangs off the
// middle of its own card, and a userBandMargin of pane background at each side,
// so the card floats instead of running off both edges. The row COUNT alone
// would pass with the padding in the wrong place, and a prompt flush against
// the screen edge is a geometry bug, not a style one.
func TestUserBandHasItsOwnAir(t *testing.T) {
	app, scr := drawnApp(t, 80, 20)
	app.AddUserBlock("a prompt with air around it")
	app.draw()

	band, ok := app.th.Slot(theme.BgHighlight)
	if !ok {
		t.Fatal("built-in theme must carry a user band")
	}
	want := app.cellColor(band)
	prim, w, _ := scr.GetContents()
	runeAt := func(row, x int) rune {
		if c := prim[row*w+x]; len(c.Runes) > 0 {
			return c.Runes[0]
		}
		return ' '
	}
	bgAt := func(row, x int) tcell.Color {
		_, bg, _ := prim[row*w+x].Style.Decompose()
		return bg
	}
	y := -1
	for row := range 20 {
		if runeAt(row, userBandInset) == '❯' {
			y = row
			break
		}
	}
	if y < 1 || y >= 19 {
		t.Fatalf("no prompt row with a row above and below to pad: %d", y)
	}
	// The rows above and below the prompt carry the band and nothing else.
	// The card's right end is not among them: the timestamp rides it, which is
	// where it has always ridden, and the row it paints on is the card's own
	// first row.
	for _, row := range []int{y - 1, y + 1} {
		for _, x := range []int{userBandMargin, userBandInset, w / 2} {
			if bg := bgAt(row, x); bg != want || runeAt(row, x) != ' ' {
				t.Fatalf("pad row %d cell %d is %q on %s, want a blank banded cell", row, x, runeAt(row, x), bg)
			}
		}
	}
	// The margins: the band stops userBandMargin short of each edge, on every
	// row of the card, so it floats with the pane's own background at both sides.
	// What the margin must NOT be is the band — an unpainted cell is the
	// terminal's own background, which is the whole contract here.
	for _, row := range []int{y - 1, y, y + 1} {
		for _, x := range []int{0, userBandMargin - 1, w - 1} {
			if bg := bgAt(row, x); bg == want {
				t.Fatalf("the card runs off the pane: row %d column %d is still banded", row, x)
			}
		}
	}
	if bg := bgAt(y, w-1-userBandMargin); bg != want {
		t.Fatalf("the card's right edge is %s one column short of its margin, want the band %v", bg, band)
	}
	// One cell of air inside the card before the ❯, then the ❯ itself.
	if r := runeAt(y, userBandInset-1); r != ' ' {
		t.Fatalf("the cell inside the card is %q, want a blank cell before the ❯", r)
	}
	if r := runeAt(y, userBandInset); r != '❯' {
		t.Fatalf("the prompt lost its ❯: %q", r)
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

// askBatchResultOf drives one tabbed card of several questions to completion.
func askBatchResultOf(t *testing.T, app *App, reqs []AskRequest, timeout time.Duration, keys ...any) ([]AskAnswer, bool) {
	t.Helper()
	done := make(chan struct {
		ans []AskAnswer
		ok  bool
	}, 1)
	go func() {
		ans, ok := app.AskCardBatch(context.Background(), reqs, timeout)
		done <- struct {
			ans []AskAnswer
			ok  bool
		}{ans, ok}
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
		return nil, false
	}
}

// TestAskCardBatchOneInterruption: two questions answer in ONE card — Enter on
// the first steps to the next, and the last answer submits both.
func TestAskCardBatchOneInterruption(t *testing.T) {
	app, _ := drawnApp(t, 100, 30)
	q1, q2 := askOptions(), askOptions()
	q1.ID, q2.ID = "backend", "cache"
	q2.Question = "Turn the cache on?"
	q2.Options = []AskOption{{Label: "yes"}, {Label: "no"}}
	// '1' takes sqlite and steps to the cache question, '2' takes "no" and
	// lands on the review, Enter submits both.
	answers, ok := askBatchResultOf(t, app, []AskRequest{q1, q2}, 5*time.Second, '1', '2', tcell.KeyEnter)
	if !ok || len(answers) != 2 {
		t.Fatalf("batch answers = %+v ok=%v", answers, ok)
	}
	if answers[0].Labels[0] != "sqlite" || answers[1].Labels[0] != "no" {
		t.Fatalf("answers out of order or wrong: %+v", answers)
	}
	if app.AskPending() {
		t.Fatal("card must close after the last answer")
	}
}

// TestAskCardChatEscape: the escape hatch closes the card with the note set and
// NO labels — a host must not read it as a chosen option, nor as a skip.
func TestAskCardChatEscape(t *testing.T) {
	app, _ := drawnApp(t, 100, 30)
	ans, ok := askResultOf(t, app, askOptions(), 5*time.Second, 'x')
	if !ok {
		t.Fatal("the chat escape is an answer, not a skip")
	}
	if len(ans.Labels) != 0 || ans.Note != AskChatLabel {
		t.Fatalf("escape answer = %+v", ans)
	}
}

// TestAskCardTypedAnswer: typing at the card writes the free-text row, and
// Enter answers with prose instead of an option.
func TestAskCardTypedAnswer(t *testing.T) {
	app, _ := drawnApp(t, 100, 30)
	done := make(chan AskAnswer, 1)
	go func() {
		ans, _ := app.AskCard(context.Background(), askOptions(), 5*time.Second)
		done <- ans
	}()
	waitAsk(t, app, true)
	typeRunes(app, "postgres")
	pressKey(app, tcell.KeyEnter)
	select {
	case ans := <-done:
		if len(ans.Labels) != 0 || ans.Note != "postgres" {
			t.Fatalf("typed answer = %+v", ans)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("typed answer did not resolve the card")
	}
}

// TestAskChatLabelMatchesTheTool: the card and the tool name the escape hatch
// in different packages, and the value has to be the same string on both sides
// or a chat answer arrives as an unknown label.
func TestAskChatLabelMatchesTheTool(t *testing.T) {
	if AskChatLabel != tool.AskChatNote {
		t.Fatalf("tui.AskChatLabel = %q, tool.AskChatNote = %q", AskChatLabel, tool.AskChatNote)
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
	// opencode's shape: the mark and title on the header row, the ordinal + box
	// on every option row, the free-text row under them, and the footer naming
	// the keys in opencode's wording (lowercase key, then the verb).
	for _, want := range []string{"◆ ask", "storage backend", "1. [ ] sqlite", "2. [ ] postgres", "3. [ ] mysql", "(recommended)", "4. Type your own answer", "5. Chat about this", "↑↓ select", "enter confirm", "esc skip"} {
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

// TestAskCardIsTheMainPaneNotAFullWidthOverlay: "the ask box has the 100%
// width overlay to the sidebar". The card is a surface of the main pane in
// opencode's shape, and three things say so at once: it ends at the pane's
// right edge instead of the terminal's, it carries one ┃ rail down its left
// edge rather than a ╭─╮ box, and that rail wears the human's accent, not the
// dim border slot the framed transcript blocks wear. The box frame is the
// defect: a thinking block and a tool result are exactly that shape, so a
// boxed card read as one more of them as well as covering the panel.
func TestAskCardIsTheMainPaneNotAFullWidthOverlay(t *testing.T) {
	app, scr := drawnApp(t, 100, 30)
	app.SetDockMode(DockShow)
	app.SetDockOps(DockOps{Session: func() (string, string) { return "pane", "sess1234" }})
	app.AddUserBlock("a prompt so the panel is on screen")
	done := make(chan struct{})
	go func() {
		_, _ = app.AskCard(context.Background(), askOptions(), 5*time.Second)
		close(done)
	}()
	waitAsk(t, app, true)
	app.draw()
	rows := strings.Split(screenText(scr), "\n")
	box := app.th.Box()
	app.mu.Lock()
	edge := app.rightEdge()
	app.mu.Unlock()
	if edge >= 100 {
		t.Fatalf("the panel is not reserving columns (rightEdge=%d), nothing to prove", edge)
	}
	// The rail: one ┃ on the pane's gutter column, on every row the card
	// paints, in the human's accent. Its first row is the header — the ◆ mark
	// and the title — and its last is the key footer.
	top := -1
	for y, ln := range rows {
		if strings.Contains(ln, "◆ ask") {
			top = y
			break
		}
	}
	if top < 0 {
		t.Fatalf("the card is not on screen:\n%s", screenText(scr))
	}
	accent := app.cellColor(app.th.Get(theme.AccentUser))
	bottom := top
	for y := top; y < len(rows); y++ {
		got, _, st, _ := scr.GetContent(1, y)
		if got != '┃' {
			break
		}
		if fg, _, _ := st.Decompose(); fg != accent {
			t.Fatalf("row %d rail is %v, want the human's accent %v", y, fg, accent)
		}
		// No frame anywhere in the card: a ╭ or ╰ in a row is the old box
		// back, and the rail is the whole of the card's chrome.
		if strings.ContainsAny(rows[y], box.TopLeft+box.TopRight+box.BottomLeft+box.BottomRight) {
			t.Fatalf("row %d paints a box corner: %q", y, rows[y])
		}
		bottom = y
	}
	if bottom <= top {
		t.Fatalf("the rail spans one row only (top=%d bottom=%d):\n%s", top, bottom, screenText(scr))
	}
	// And the card stops at the pane's edge: nothing it paints reaches the
	// panel's columns, which is the whole of the report.
	for y := top; y <= bottom; y++ {
		for x := edge; x < 100; x++ {
			if ch, _, _, _ := scr.GetContent(x, y); ch != ' ' {
				t.Fatalf("the card painted %q at x=%d y=%d, inside the panel", string(ch), x, y)
			}
		}
	}
	pressKey(app, tcell.KeyEsc)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Esc must close the card")
	}
}

// TestAskCardPaintsTheOpencodeLayout: the report this restyle answers is "make
// the ask box look exactly like opencode's", so the card's own shape is pinned
// here, on a live frame, one assertion per thing opencode's form does that the
// card could drift on: the header's ◆ mark and title, the ordinal + checkbox on
// every row, the free-text and chat rows under the options, the cursor's band
// as a background (never a ❯ in the layout), and the footer in opencode's
// "key + verb" wording. A regression in any of them fails here, and no other
// test in the package reads these glyphs.
func TestAskCardPaintsTheOpencodeLayout(t *testing.T) {
	app, scr := drawnApp(t, 90, 26)
	req := askOptions()
	req.Multi = true
	req.Recommended = []string{"postgres"}
	done := make(chan struct{})
	go func() {
		_, _ = app.AskCard(context.Background(), req, 5*time.Second)
		close(done)
	}()
	waitAsk(t, app, true)
	app.draw()
	text := screenText(scr)
	for _, want := range []string{
		"◆ ask",                          // the header: mark, then title
		"Which storage backend",          // the question, plain, no bullet
		"1. [ ] sqlite",                  // ordinal + checkbox, then the label
		"2. [✓] postgres  (recommended)", // a pre-toggled box on a multi card
		"3. [ ] mysql",
		"4. Type your own answer", // opencode's custom row, last option + 1
		"5. Chat about this",      // xdev's prose escape, on the row after it
		"↑↓ select   space toggle   enter done   1-9 pick", // the footer, in order
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the card is not opencode's shape, missing %q:\n%s", want, text)
		}
	}
	// The cursor is a background band on the row, not a ❯ glyph: opencode's
	// option box has no cursor column, and a card that grows one shifts every
	// label a cell right of where opencode puts it.
	if strings.Contains(text, "❯ 1. ") {
		t.Fatalf("the rows carry a cursor column again:\n%s", text)
	}
	band := app.cellColor(app.th.Get(theme.BgHighlight))
	// The band covers the row it marks: a cursor the card paints only behind the
	// glyphs it happens to write stops mid-row and reads as a stray highlight.
	cells := 0
	for y := 0; y < app.height; y++ {
		for x := 0; x < app.width; x++ {
			_, _, st, _ := scr.GetContent(x, y)
			if _, bg, _ := st.Decompose(); bg == band {
				cells++
			}
		}
	}
	if cells < 20 {
		t.Fatalf("the selected row has no band across the pane (%d cells):\n%s", cells, text)
	}
	pressKey(app, tcell.KeyEsc)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Esc must close the card")
	}
}

// TestAskCardWrapsLongOptionText: a label or a description longer than the card
// wraps on screen instead of running under the right border, and the
// description keeps its own indented lines (omp's shape). The words asserted
// here sit past the old single-line cut, so this fails if the truncation comes
// back.
func TestAskCardWrapsLongOptionText(t *testing.T) {
	app, scr := drawnApp(t, 100, 30)
	req := AskRequest{
		Question: "Which storage backend should the fix target?",
		Options: []AskOption{
			{
				Label: "postgres, the shared instance every integration test in the repo already points at, which nobody on the team currently owns or patches",
				Description: "a local file with no server to run, nothing to page anyone about, and no connection string " +
					"to put in the environment or rotate on the schedule the platform team agreed to",
			},
			{Label: "mysql"},
		},
	}
	// The recommended option is the OTHER one, so the cursor starts away from the
	// wrapped row: a click on its continuation line proving the hit map works is
	// then a real move, not the row the cursor was already on.
	req.Recommended = []string{"mysql"}

	done := make(chan struct{})
	go func() {
		_, _ = app.AskCard(context.Background(), req, 5*time.Second)
		close(done)
	}()
	waitAsk(t, app, true)
	app.draw()
	text := screenText(scr)
	// Both wraps have to be on screen: the tail of the label, and the tail of
	// the description's second line. The old single-line row cut them off at the
	// border, so neither could appear.
	for _, want := range []string{
		"which nobody on the team currently owns or patches",
		"rotate on the schedule the platform team agreed to",
		"1-9 pick", // and the footer still fits beside them
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("card missing %q:\n%s", want, text)
		}
	}
	var row, cont, desc1, desc2 string
	for _, ln := range strings.Split(text, "\n") {
		switch {
		case strings.Contains(ln, "postgres, the shared instance"):
			row = ln
		case strings.Contains(ln, "which nobody on the team"):
			cont = ln
		case strings.Contains(ln, "a local file with no server"):
			desc1 = ln
		case strings.Contains(ln, "rotate on the schedule"):
			desc2 = ln
		}
	}
	if row == "" || cont == "" || desc1 == "" || desc2 == "" {
		t.Fatalf("row=%q cont=%q desc1=%q desc2=%q\n%s", row, cont, desc1, desc2, text)
	}
	// The description is its own block, not a tail on the label's line: every
	// wrapped line — the label's continuation and the description's lines alike —
	// starts under the label's first character, which is what makes a wrapped row
	// read as one option (omp's shape). Column of a byte offset, hence width().
	at := strings.Index(row, "postgres, the shared instance")
	if at < 0 {
		t.Fatalf("row line lost its label: %q", row)
	}
	col := width(row[:at])
	for _, ln := range []string{cont, desc1, desc2} {
		// The rail is frame, not indentation, so a wrapped line's start is the
		// first cell after it — trimming spaces alone would stop at the rail and
		// measure the frame as content.
		ind := width(ln) - width(strings.TrimLeft(ln, " ┃"))
		if ind != col {
			t.Fatalf("wrapped line %.40q starts at cell %d, want %d:\n%s", ln, ind, col, text)
		}
	}
	// A click on the description's line selects that row: the wrapped lines are
	// part of the row, not dead space between two of them.
	if sel, _ := app.AskSelection(); sel != 1 {
		t.Fatalf("cursor should start on the recommended row, got %d", sel)
	}
	clickAskRow(t, app, text, "rotate on the schedule")
	if sel, _ := app.AskSelection(); sel != 0 {
		t.Fatalf("clicking a row's description line must select that row, got %d\n%s", sel, text)
	}
	// Enter then answers with that row, proving the click landed on the row the
	// user aimed at and not on its first line only.
	pressKey(app, tcell.KeyEnter)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Enter must close the card")
	}
}

// clickAskRow presses on the card line containing want, at the column its text
// starts on (the byte offset a mouse report wants is a cell one).
func clickAskRow(t *testing.T, app *App, text, want string) {
	t.Helper()
	for y, line := range strings.Split(text, "\n") {
		if at := strings.Index(line, want); at >= 0 {
			app.handleAskMouse(tcell.NewEventMouse(width(line[:at]), y, tcell.Button1, tcell.ModNone), true)
			return
		}
	}
	t.Fatalf("no card line contains %q:\n%s", want, text)
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
	ops := app.NewAskOps(func() time.Duration { return 30 * time.Millisecond })
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

// TestAskOpsSeamWithoutTimeout: with ask.autoAnswer off cmd hands the seam a
// zero timeout, and the card must WAIT for the human — no timer resolves it
// and answers for them. Only a pick or a canceled turn may end it.
func TestAskOpsSeamWithoutTimeout(t *testing.T) {
	app, _ := drawnApp(t, 100, 30)
	ops := app.NewAskOps(func() time.Duration { return 0 })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct {
		ans AskAnswer
		ok  bool
	}, 1)
	go func() {
		ans, ok := ops.Show(ctx, askOptions(), 0)
		done <- struct {
			ans AskAnswer
			ok  bool
		}{ans, ok}
	}()
	waitAsk(t, app, true)
	pressRune(app, '2')
	select {
	case got := <-done:
		if !got.ok || len(got.ans.Labels) != 1 || got.ans.Labels[0] != "postgres" {
			t.Fatalf("waiting card answer = %+v ok=%v", got.ans, got.ok)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a zero-timeout card must still resolve on a pick")
	}

	// Left alone it must not resolve on its own.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	pending := make(chan struct{}, 1)
	go func() {
		ops.Show(ctx2, askOptions(), 0)
		pending <- struct{}{}
	}()
	waitAsk(t, app, true)
	select {
	case <-pending:
		t.Fatal("a card with no timeout must not answer by itself")
	case <-time.After(200 * time.Millisecond):
	}
	cancel2()
	select {
	case <-pending:
	case <-time.After(2 * time.Second):
		t.Fatal("a canceled turn must release the waiting card")
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

// TestAskCardNoRoomClosesTheCard pins the 5b999f0 invariant for ask: a card
// that cannot paint must close on the next frame instead of parking a modal
// that owns the keyboard while drawing nothing — the "invisible lock" that
// read as a dead TUI. The question survives as a transcript notice.
func TestAskCardNoRoomClosesTheCard(t *testing.T) {
	// Roomy width, but one row short: the card needs 7 rows above the
	// composer (border + question + 3 options + footer + border).
	app, _ := drawnApp(t, 100, 11)
	req := askOptions()
	done := make(chan AskAnswer, 1)
	go func() {
		// A long timeout: only the draw path may end the wait.
		ans, _ := app.AskCard(context.Background(), req, time.Minute)
		done <- ans
	}()
	waitAsk(t, app, true)

	app.mu.Lock()
	yTop := app.height - 1 - app.composerRows()
	app.mu.Unlock()
	app.drawAskCard(yTop) // what draw() does each frame, sans the full paint

	waitAsk(t, app, false)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("an unpaintable card must release its caller")
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.blocks) < 2 || !strings.Contains(app.blocks[len(app.blocks)-1].Text, "storage backend") {
		t.Fatalf("question must surface as a notice: %+v", app.blocks)
	}
}

// TestAskCardTimeoutNotices: when nobody answers, the card closes and the
// transcript records the question with the reason — the wait the caller spent
// is never invisible.
func TestAskCardTimeoutNotices(t *testing.T) {
	app, _ := drawnApp(t, 100, 30)
	before := len(app.Blocks())
	if _, ok := app.AskCard(context.Background(), askOptions(), 20*time.Millisecond); ok {
		t.Fatal("a timeout is a skip")
	}
	if app.AskPending() {
		t.Fatal("the card must close on timeout")
	}
	blocks := app.Blocks()
	if len(blocks) != before+1 || !strings.Contains(blocks[len(blocks)-1].Text, "no answer within") {
		t.Fatalf("timeout must leave a notice: %+v", blocks)
	}
}

// TestNoTopBarAndBranchOnTheStatusRow pins the chrome contract after the top
// bar was dropped: row 0 belongs to the transcript, and the git branch rides
// the status row beside the working directory — one surface that reads as a
// single location. The running spinner never comes back to that row either;
// it lives in the composer's divider (TestSpinnerFramesFromTheme).
func TestNoTopBarAndBranchOnTheStatusRow(t *testing.T) {
	app, scr := newTestApp(t, 100, 24)
	app.SetLocation(t.TempDir())
	app.mu.Lock()
	app.branch = "fix/boxes"
	app.st.Model = "some-model"
	app.mu.Unlock()
	app.AddUserBlock("fix the   tool\noutput box please")
	app.AddSystemBlock(strings.Repeat("line\n", 60)) // guarantees hidden rows
	app.draw()

	rows := strings.Split(strings.TrimRight(screenText(scr), "\n"), "\n")
	// Row 0 is the session's own first prompt, not a header: no branch on it,
	// and the prompt is where it has always been — under the card's own blank
	// padding row, which is air rather than content.
	first := app.transcriptTop() + stickyPad // under the card's blank padding row
	if first >= len(rows) || strings.Contains(rows[first], "fix/boxes") || !strings.Contains(rows[first], "fix the") {
		t.Fatalf("row %d %q must be transcript content, not a header", first, rows[first])
	}

	// The branch is on the status row, next to the path, in that order.
	status := lastRow(screenText(scr))
	pathAt := strings.Index(status, "·")
	branchAt := strings.Index(status, "fix/boxes")
	if branchAt < 0 || pathAt < 0 || pathAt > branchAt {
		t.Fatalf("status row %q must carry the directory then the branch", status)
	}

	// Running: the spinner belongs to the divider, not to row 0 or the
	// status row.
	th := theme.Load("groknight")
	th.Symbols = theme.Symbols{Status: []string{"X", "Y"}}
	app.SetTheme(th)
	app.SetRunning(true)
	app.mu.Lock()
	app.st.spinnerIdx = 0
	app.mu.Unlock()
	app.draw()
	text := screenText(scr)
	row0 := strings.Split(strings.TrimRight(text, "\n"), "\n")[0]
	for _, where := range []struct {
		what string
		ln   string
	}{{"row 0", row0}, {"the status row", lastRow(text)}} {
		if strings.Contains(where.ln, "X ") || strings.Contains(where.ln, "Y ") {
			t.Fatalf("a spinner frame leaked onto %s: %q", where.what, where.ln)
		}
	}
	if !strings.Contains(text, "X · some-model") {
		t.Fatalf("the spinner belongs in the divider, not on a chrome row:\n%s", text)
	}
}

// TestStatusRowKeepsBranchAndTruncatesThePath pins the two halves of the
// location's width argument: the branch claims cells first and a long path
// gives way down to its folder name around it, while a branch too wide for the
// row leaves whole rather than printing half of itself.
func TestStatusRowKeepsBranchAndTruncatesThePath(t *testing.T) {
	app, scr := newTestApp(t, 100, 20)
	app.AddSystemBlock("ready")
	app.SetLocation("/Volumes/work/harvey/freepeak/checkout/xdev-feature/deep/nested/subdir")
	app.mu.Lock()
	app.branch = "feat/branch-in-status-row"
	app.mu.Unlock()
	app.draw()

	// A path longer than the row gives way down to the folder it lives in and
	// the branch survives beside it: the branch claims its cells first, the
	// path spends what is left, and neither is clipped mid-string.
	row := lastRow(screenText(scr))
	if !strings.Contains(row, "subdir · feat/branch-in-status-row") {
		t.Fatalf("a long path must yield its parents to the branch, not swallow it: %q", row)
	}

	// A branch with no room at all leaves the row entirely — clipped in the
	// middle it would read as a longer branch name than it is.
	app.mu.Lock()
	app.branch = strings.Repeat("long-branch-name-", 6)
	app.mu.Unlock()
	app.draw()
	row = lastRow(screenText(scr))
	if strings.Contains(row, "long-branch-name") {
		t.Fatalf("a branch with no room must be dropped whole, not clipped: %q", row)
	}
	if !strings.Contains(row, "subdir") {
		t.Fatalf("the path is what the row keeps: %q", row)
	}
}

// TestStatusRowWearsItsOwnInks pins the colour half of the location: the
// path wears status_line_path, the branch wears the tree's clean/dirty ink,
// and the two are not the same grey. One string said "cwd · branch" as a single
// fact; the split inks say where you are and what state the tree is in, which
// is the reading the model id already gets on the divider above.
func TestStatusRowWearsItsOwnInks(t *testing.T) {
	app, scr := newTestApp(t, 120, 24)
	app.AddSystemBlock("ready")
	app.SetLocation("/Volumes/work/harvey/freepeak/checkout/xdev-feature")
	app.mu.Lock()
	app.branch, app.branchDirty = "feat/branch-ink", false
	app.mu.Unlock()
	app.draw()

	rows := strings.Split(strings.TrimRight(screenText(scr), "\n"), "\n")
	row := rowOf(t, rows, "feat/branch-ink")
	pathX, branchX := strings.Index(rows[row], "xdev-feature"), strings.Index(rows[row], "feat/branch-ink")
	pathInk := app.cellColor(app.th.Get(theme.StatusLinePath))
	cleanInk := app.cellColor(app.th.Get(theme.StatusLineGitClean))
	dirtyInk := app.cellColor(app.th.Get(theme.StatusLineGitDirty))
	if got, _, _ := cellStyle(scr, pathX, row).Decompose(); got != pathInk {
		t.Fatalf("the path is painted in %v, want statusLinePath %v", got, pathInk)
	}
	if got, _, _ := cellStyle(scr, branchX, row).Decompose(); got != cleanInk {
		t.Fatalf("a clean branch is painted in %v, want statusLineGitClean %v", got, cleanInk)
	}

	// The same branch with an edit in the tree flips to the dirty ink — the
	// green/red pair the theme already spent on this row's git state.
	app.mu.Lock()
	app.branchDirty = true
	app.mu.Unlock()
	app.draw()
	if got, _, _ := cellStyle(scr, branchX, row).Decompose(); got != dirtyInk {
		t.Fatalf("a dirty branch is painted in %v, want statusLineGitDirty %v", got, dirtyInk)
	}
}

// TestGitDirtySamplesTheTree pins the half that cannot be asserted from a
// hand-set flag: gitDirty is what makes the ink honest, so it must answer for
// a real repo — dirty with an untracked file, clean without one — and false
// for a directory that is not a repository at all (no branch, no ink).
func TestGitDirtySamplesTheTree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if gitDirty(dir) {
		t.Fatalf("a directory that is not a repo must not claim a dirty tree: %s", dir)
	}
	run("init")
	if gitDirty(dir) {
		t.Fatal("a fresh repo has nothing uncommitted")
	}
	if err := os.WriteFile(filepath.Join(dir, "scratch.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !gitDirty(dir) {
		t.Fatal("an untracked file must read as a dirty tree")
	}
	if err := os.Remove(filepath.Join(dir, "scratch.txt")); err != nil {
		t.Fatal(err)
	}
	if gitDirty(dir) {
		t.Fatal("the tree is clean again once the file is gone")
	}
}
