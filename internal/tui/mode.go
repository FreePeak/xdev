package tui

import (
	"fmt"
	"strings"

	"github.com/FreePeak/xdev/internal/theme"
)

// The session MODE: one vocabulary over the two states it folds — plan mode
// (read-only, propose to exit) and the approval policy (what a write costs
// you). Claude Code's permission modes, named for what they do rather than
// for the layer that implements them, so /mode and Shift-Tab can drive the
// planMode + policy seam cmd already owns without a second gate to keep in
// step with the first.
//
// bypass is reachable BY NAME only, never by the cycle — the one mode whose
// mistakes cannot be undone by answering a card.
const (
	// ModeDefault prompts before a write or a command.
	ModeDefault = "default"
	// ModeAuto approves writes and commands without asking.
	ModeAuto = "auto"
	// ModePlan is read-only research; propose exits it.
	ModePlan = "plan"
	// ModeBypass never asks at all (xdev's approvalMode: yolo).
	ModeBypass = "bypass"
)

// modeCycle is the Shift-Tab order: default → auto → plan → default. The
// three safe modes, in that order, so a cycle can only ever reach the
// permissive one on purpose.
var modeCycle = []string{ModeDefault, ModeAuto, ModePlan}

// modeAll is every mode /mode accepts: the cycle plus bypass.
var modeAll = []string{ModeDefault, ModeAuto, ModePlan, ModeBypass}

// modeBlurb is the one line a /mode or Shift-Tab transition announces. It
// earns its place in the transcript because the mode changes what a tool
// call is allowed to do, and the user should not have to remember which.
var modeBlurb = map[string]string{
	ModeDefault: "asks before a write or a command",
	ModeAuto:    "writes and commands run without asking",
	ModePlan:    "read-only research; propose with the plan to exit",
	ModeBypass:  "never asks — every tool call runs",
}

// ModeOps wires /mode and the Shift-Tab cycle to the live mode (cmd owns the
// plan state and the approval policy). Current names the mode in force; Set
// applies one and returns an error rather than a half-applied mode. nil ops
// degrade both surfaces to a notice.
type ModeOps struct {
	Current func() string
	Set     func(mode string) error
}

// SetModeOps wires /mode and the Shift-Tab cycle (Claude Code's shift+tab).
func (a *App) SetModeOps(ops *ModeOps) { a.modeOps = ops }

// Mode implements CommandAPI /mode: a bare /mode reports the mode in force,
// /mode <name> sets it, and anything unrecognised is a usage error — never a
// silent flip, the same rule /auto-answer and /plan already follow.
func (a *App) Mode(args string) error {
	if a.modeOps == nil || a.modeOps.Current == nil || a.modeOps.Set == nil {
		return fmt.Errorf("mode not wired")
	}
	name := strings.ToLower(strings.TrimSpace(args))
	if name == "" {
		a.AddSystemBlock("mode: " + a.modeOps.Current())
		return nil
	}
	return a.setMode(name)
}

// setMode applies one mode and announces what it now permits. The echo is the
// mode Set reported, never the word typed: "/mode default" that the session
// cannot reach must not claim the session is in it.
func (a *App) setMode(name string) error {
	known := false
	for _, m := range modeAll {
		known = known || m == name
	}
	if !known {
		return fmt.Errorf("mode: want %s", strings.Join(modeAll, "|"))
	}
	if err := a.modeOps.Set(name); err != nil {
		return err
	}
	a.AddSystemBlock("mode: " + name + " — " + modeBlurb[name])
	return nil
}

// CycleMode is the Shift-Tab chord: the next mode in modeCycle. A session
// whose mode is outside the cycle (bypass, or an unwired/unknown name) enters
// it at default, which is the least permissive mode — the cycle never walks
// itself into bypass.
func (a *App) CycleMode() {
	if a.modeOps == nil || a.modeOps.Current == nil || a.modeOps.Set == nil {
		return
	}
	cur := a.modeOps.Current()
	next := ModeDefault
	for i, m := range modeCycle {
		if m == cur {
			next = modeCycle[(i+1)%len(modeCycle)]
			break
		}
	}
	if err := a.setMode(next); err != nil {
		a.AddSystemBlock("mode: " + err.Error())
	}
}

// modeLabel is the mode as the chrome shows it: the bare name, no icon and
// no word, because the composer divider carries it and has no room to spell
// it. Empty when the seam is unwired — a host that never wired /mode has no
// mode to report, and an invented "default" would claim a posture nobody
// chose.
func (a *App) modeLabel() string {
	if a.modeOps == nil || a.modeOps.Current == nil {
		return ""
	}
	return strings.TrimSpace(a.modeOps.Current())
}

// modeToken is the ink for the mode in force. One colour for all four modes
// said nothing about the one fact they differ ON — how much they let the
// agent do without asking — so the map follows Claude Code's mode inks
// (default → inactive, plan → planMode, acceptEdits → autoAccept,
// bypassPermissions → error) on top of the theme slots that mean the same
// thing. Two deliberate departures:
//
//   - auto wears SUCCESS, not autoAccept. Claude's autoAccept is a violet
//     one cell from the accent, and the model name on this same divider now
//     wears the accent (omp's rule, and 43 of its 98 themes agree): auto in
//     the same ink would collapse "model · auto" back into one flat phrase,
//     which is the split this map exists to win.
//   - plan wears the theme's HEADING teal rather than a new token. It is a
//     role a palette already has and omp already requires, so an imported
//     theme paints it without knowing xdev has a mode surface at all.
//
// The slot NAMES are the legacy xdev spellings on purpose: the built-in
// palettes carry those, and ParseTheme mirrors legacy onto canonical on the
// way in, so one name resolves in a built-in and in an imported theme alike.
// Asking for the canonical Error/Success/Muted would resolve in neither — no
// built-in defines them, and Get would hand all four the same body grey.
//
// An unwired seam and a name nobody defines both fall back to
// StatusLineMode, so a mode is never invisible and never borrows another
// mode's ink.
func (a *App) modeToken() string {
	switch a.modeLabel() {
	case ModeDefault:
		return theme.Gray
	case ModeAuto:
		return theme.AccentSuccess
	case ModePlan:
		return theme.MdHeading1
	case ModeBypass:
		return theme.AccentError
	default:
		return theme.StatusLineMode
	}
}
