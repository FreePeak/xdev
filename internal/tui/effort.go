package tui

import (
	"fmt"
	"strings"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/theme"
)

// EffortOps wires /effort and the Shift-Tab cycle to the live session
// effort rung (cmd owns the settings file and the tool registry re-defer).
// Current names the rung in force; Set applies one and returns an error
// rather than a half-applied state. nil ops degrade both surfaces to a notice.
type EffortOps struct {
	Current func() string
	Set     func(effort string) error
}

// SetEffortOps wires /effort and the Shift-Tab cycle.
func (a *App) SetEffortOps(ops *EffortOps) { a.effortOps = ops }

// Effort implements CommandAPI /effort: bare reports the rung, /effort
// <name> sets it, and anything unrecognised is a usage error — never a
// silent flip (same rule /thinking follows). The vocabulary is the same
// upward ladder /thinking and --thinking share (low|medium|high|xhigh|max),
// so the rung a user types is the rung the request carries.
func (a *App) Effort(args string) error {
	if a.effortOps == nil || a.effortOps.Current == nil || a.effortOps.Set == nil {
		return fmt.Errorf("effort not wired")
	}
	name := strings.ToLower(strings.TrimSpace(args))
	if name == "" {
		cur := a.effortOps.Current()
		a.AddSystemBlock("effort: " + cur + " — " + config.SessionEffortBlurb(cur))
		return nil
	}
	return a.setEffort(name)
}

// setEffort applies one rung and announces what it now permits.
func (a *App) setEffort(name string) error {
	if !config.IsSessionEffort(name) {
		return fmt.Errorf("effort: want %s", strings.Join(config.SessionEffortLevels, "|"))
	}
	if err := a.effortOps.Set(name); err != nil {
		return err
	}
	a.AddSystemBlock("effort: " + name + " — " + config.SessionEffortBlurb(name))
	return nil
}

// CycleEffort is the Shift-Tab chord: the next rung in SessionEffortCycle
// (low → medium → high → xhigh → max → low). An unknown current value enters
// the cycle at low (the lightest rung).
func (a *App) CycleEffort() {
	if a.effortOps == nil || a.effortOps.Current == nil || a.effortOps.Set == nil {
		return
	}
	cur := a.effortOps.Current()
	next := config.SessionEffortLevels[0]
	for i, e := range config.SessionEffortCycle {
		if e == cur {
			next = config.SessionEffortCycle[(i+1)%len(config.SessionEffortCycle)]
			break
		}
	}
	if err := a.setEffort(next); err != nil {
		a.AddSystemBlock("effort: " + err.Error())
	}
}

// effortLabel is the bare rung the chrome shows. Empty when the seam is
// unwired — an invented "medium" would claim a posture nobody chose.
func (a *App) effortLabel() string {
	if a.effortOps == nil || a.effortOps.Current == nil {
		return ""
	}
	return strings.TrimSpace(a.effortOps.Current())
}

// effortToken is the ink for the effort rung: the rung itself is named on the
// same rail /thinking uses, so the ladder reads as one amount of effort
// rather than as two unrelated labels. low is the muted end, medium the
// status-line grey, and high and above wear the theme's own reasoning ink for
// their rung (a theme that names none falls back to the accent).
func (a *App) effortToken() string {
	switch a.effortLabel() {
	case config.SessionEffortLow:
		return theme.Gray
	case config.SessionEffortHigh:
		return theme.ThinkingHigh
	case config.SessionEffortXHigh:
		return theme.ThinkingXhigh
	case config.SessionEffortMax:
		return theme.ThinkingMax
	case config.SessionEffortMedium:
		return theme.StatusLineMode
	default:
		return theme.StatusLineMode
	}
}
