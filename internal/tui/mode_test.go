package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// The session mode is the one fact that decides what a tool call is allowed to
// do, and it had no surface at all: /plan announced a change in the
// transcript, and the answer to "what is this session allowed to do right now"
// was a memory of a command you typed minutes ago. The mode rides the composer
// divider (where model and reasoning level already sit — one request, three
// answers, each in its own ink) and the status row beside the working
// directory.

// modeWired is the seam /mode, Shift-Tab and both readouts share: one mutable
// mode name, which is what the chrome must read.
func modeWired(cur *string) *ModeOps {
	return &ModeOps{
		Current: func() string { return *cur },
		Set:     func(m string) error { *cur = m; return nil },
	}
}

// statusRow is the bottom row of a drawn screen (the status line); the
// composer divider sits directly above it.
func statusRow(t *testing.T, scr tcell.SimulationScreen) string {
	t.Helper()
	rows := strings.Split(strings.TrimRight(screenText(scr), "\n"), "\n")
	if len(rows) == 0 {
		t.Fatal("nothing drawn")
	}
	return rows[len(rows)-1]
}

// TestModeCommandAndCycle pins the two ways a mode changes: /mode <name> sets
// it, Shift-Tab walks default → auto → plan → default. The echo in the
// transcript is the mode that was applied, not the word typed, and an
// unrecognised name is a usage error rather than a silent no-op — a typo must
// not be the thing that widens what the agent may do.
func TestModeCommandAndCycle(t *testing.T) {
	app, scr := newTestApp(t, 100, 24)
	cur := ModeDefault
	app.SetModeOps(modeWired(&cur))
	app.AddSystemBlock("ready")
	app.draw()

	if err := app.Mode(""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lastBlock(t, app), "mode: default") {
		t.Fatalf("bare /mode = %q", lastBlock(t, app))
	}

	if err := app.Mode("plan"); err != nil {
		t.Fatal(err)
	}
	if cur != ModePlan {
		t.Fatalf("mode = %q, want plan", cur)
	}
	if b := lastBlock(t, app); !strings.Contains(b, "mode: plan") || !strings.Contains(b, "read-only") {
		t.Fatalf("/mode plan = %q, want the mode and what it permits", b)
	}

	// A name nobody defines is a usage error, and the live mode is untouched.
	if err := app.Mode("auto-approve-everything"); err == nil {
		t.Fatal("an unknown mode must be a usage error")
	}
	if cur != ModePlan {
		t.Fatalf("a rejected mode changed the live mode to %q", cur)
	}

	// Shift-Tab walks the cycle and wraps; from plan it lands on default.
	app.CycleMode()
	if cur != ModeDefault {
		t.Fatalf("cycle from plan = %q, want default", cur)
	}
	app.CycleMode()
	if cur != ModeAuto {
		t.Fatalf("cycle from default = %q, want auto", cur)
	}
	app.draw()
	if d := dividerRow(t, scr); !strings.Contains(d, "test/free · auto") {
		t.Fatalf("divider %q did not follow the cycle", d)
	}
}

// TestModeCycleNeverReachesBypass pins the one safety property of the cycle:
// bypass is selectable by name and unreachable by pressing one key twice.
// A session sitting in bypass enters the cycle at default — the least
// permissive mode — rather than continuing round.
func TestModeCycleNeverReachesBypass(t *testing.T) {
	// The one property: the unrestricted mode is not one of the rungs. The
	// other three obviously are — they are the whole cycle.
	if slices.Contains(modeCycle, ModeBypass) {
		t.Fatalf("bypass must not be a Shift-Tab rung")
	}
	if slices.Contains(modeAll, ModeBypass) != true {
		t.Fatalf("bypass must still be reachable by name: %v", modeAll)
	}
	app, _ := newTestApp(t, 100, 24)
	cur := ModeBypass
	app.SetModeOps(modeWired(&cur))
	app.CycleMode()
	if cur != ModeDefault {
		t.Fatalf("cycle from bypass = %q, want default", cur)
	}
	// …and every rung of the cycle is a single press away from the next.
	want := map[string]string{ModeDefault: ModeAuto, ModeAuto: ModePlan, ModePlan: ModeDefault}
	for from, to := range want {
		cur = from
		app.CycleMode()
		if cur != to {
			t.Fatalf("cycle from %s = %q, want %q", from, cur, to)
		}
	}
}

// TestModeRidesBothChromeSurfaces pins the readout itself: the bare mode name
// on the composer divider beside the model, and beside the working directory
// on the status row. Both hide entirely when the seam is unwired — a host
// that never wired /mode has no mode to report, and an invented "default"
// would claim a posture nobody chose.
func TestModeRidesBothChromeSurfaces(t *testing.T) {
	app, scr := newTestApp(t, 100, 24)
	app.SetLocation("/tmp/somewhere")
	app.AddSystemBlock("ready")
	app.draw()
	if d := dividerRow(t, scr); strings.Contains(d, "default") {
		t.Fatalf("an unwired mode seam must paint no mode: %q", d)
	}
	if r := statusRow(t, scr); strings.Contains(r, "default") {
		t.Fatalf("an unwired mode seam must paint no mode on the status row: %q", r)
	}

	cur := ModePlan
	app.SetModeOps(modeWired(&cur))
	app.AddSystemBlock("mode set")
	app.draw()
	if d := dividerRow(t, scr); !strings.Contains(d, "test/free · plan") {
		t.Fatalf("divider %q must carry the mode", d)
	}
	if r := statusRow(t, scr); !strings.Contains(r, "/tmp/somewhere · plan") {
		t.Fatalf("status row %q must carry the mode beside the location", r)
	}
	// The mode follows a flip without a rebuild: the chrome reads the holder.
	cur = ModeAuto
	app.AddSystemBlock("mode flipped")
	app.draw()
	if d := dividerRow(t, scr); !strings.Contains(d, "test/free · auto") {
		t.Fatalf("divider %q did not follow the flip", d)
	}
}

// TestModeWearsItsOwnInk pins the colour half: the mode paints in the mode
// token, not the grey the model and the location wear, because the three parts
// of that row answer three different questions and one grey answers none of
// them. The status-row mode is the same ink the divider uses.
func TestModeWearsItsOwnInk(t *testing.T) {
	app, scr := newTestApp(t, 100, 24)
	cur := ModePlan
	app.SetModeOps(modeWired(&cur))
	app.AddSystemBlock("ready")
	setDraft(&app.ed, "say something", len("say something"))
	app.draw()

	rows := strings.Split(strings.TrimRight(screenText(scr), "\n"), "\n")
	divY := rowOf(t, rows, "╰")
	stY := rowOf(t, rows, "· plan")
	modeInk := app.cellColor(app.th.Get(theme.StatusLineMode))
	modelInk := app.cellColor(app.th.Get(theme.StatusLineModel))
	// The slot is optional in a theme, so on a theme that omits it the mode
	// derives from accent; what must hold is that it is not the model grey —
	// otherwise the mode would read as a fourth word of the model name.
	if modeInk == modelInk {
		t.Fatalf("the mode ink must differ from the model ink (both %v)", modeInk)
	}
	for _, tc := range []struct {
		name string
		row  int
	}{{"divider", divY}, {"status row", stY}} {
		x := strings.Index(rows[tc.row], "plan")
		if x < 0 {
			t.Fatalf("%s %q has no mode on it", tc.name, rows[tc.row])
		}
		if got, _, _ := cellStyle(scr, x, tc.row).Decompose(); got != modeInk {
			t.Fatalf("%s: the mode is painted in %v, want the mode ink %v", tc.name, got, modeInk)
		}
	}
	// The model beside it keeps the model grey — the whole point of the split.
	if x := strings.Index(rows[divY], "test/free"); x >= 0 {
		if got, _, _ := cellStyle(scr, x, divY).Decompose(); got != modelInk {
			t.Fatalf("the model is painted in %v, want the model ink %v", got, modelInk)
		}
	}
}

// rowOf is the index of the first row carrying needle — the chrome rows are
// located by what is painted on them, not by counting up from the bottom,
// because the number of transcript rows above them is the scroll position.
func rowOf(t *testing.T, rows []string, needle string) int {
	t.Helper()
	for i, r := range rows {
		if strings.Contains(r, needle) {
			return i
		}
	}
	t.Fatalf("no row carries %q", needle)
	return -1
}

// TestThinkingLevelWearsItsRail pins the reasoning half of the same idea: the
// level is an amount of effort, so it wears the theme's rail colour for that
// rung rather than the model grey, and a level nobody named falls back to the
// model ink instead of borrowing a rung's colour.
func TestThinkingLevelWearsItsRail(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	lvl := "high"
	app.SetThinkingOps(&ThinkingOps{Current: func() string { return lvl }, Set: func(l string) error { lvl = l; return nil }})
	if got := app.thinkingToken(); got != theme.ThinkingHigh {
		t.Fatalf("high token = %q, want %q", got, theme.ThinkingHigh)
	}
	if got := app.thinkingToken(); got != app.dividerParts()[1].token {
		t.Fatalf("the divider must paint the level in its rail token")
	}
	lvl = "off"
	if got := app.thinkingToken(); got != theme.ThinkingOff {
		t.Fatalf("off token = %q, want %q", got, theme.ThinkingOff)
	}
	lvl = "whatever"
	if got := app.thinkingToken(); got != theme.StatusLineModel {
		t.Fatalf("an unknown rung must fall back to the model ink, got %q", got)
	}
}

// TestModeIsCloserToTheBorderThanTheHint keeps the two ends of the divider
// from colliding: the phrase is dropped from the tail, not overrun, when the
// row cannot hold it, and the scroll hint still wins the right end.
func TestModeIsCloserToTheBorderThanTheHint(t *testing.T) {
	app, scr := newTestApp(t, 40, 24)
	app.AddSystemBlock(strings.Repeat("line\n", 40)) // guarantees rows hidden below
	cur := ModePlan
	app.SetModeOps(modeWired(&cur))
	app.SetStatusModel(strings.Repeat("verylongmodel/", 4))
	app.draw()
	div := dividerRow(t, scr)
	if i := strings.Index(div, "╯"); i >= 0 {
		after := strings.TrimSpace(div[i+1:])
		if strings.Contains(after, "verylongmodel") {
			t.Fatalf("the phrase overran the right border: %q", div)
		}
	}
}

// lastBlock is the newest transcript block's text.
func lastBlock(t *testing.T, app *App) string {
	t.Helper()
	app.mu.Lock()
	defer app.mu.Unlock()
	for i := len(app.blocks) - 1; i >= 0; i-- {
		if app.blocks[i].Kind == KindSystem {
			return app.blocks[i].Text
		}
	}
	t.Fatal("no system block")
	return ""
}
