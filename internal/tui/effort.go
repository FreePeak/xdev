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
// silent flip (same rule /thinking follows).
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

// CycleEffort is the Shift-Tab chord: the next rung in SessionEffortCycle.
// An unknown current value enters the cycle at lean (the lightest rung).
func (a *App) CycleEffort() {
	if a.effortOps == nil || a.effortOps.Current == nil || a.effortOps.Set == nil {
		return
	}
	cur := a.effortOps.Current()
	next := config.SessionEffortLean
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
// unwired — an invented "standard" would claim a posture nobody chose.
func (a *App) effortLabel() string {
	if a.effortOps == nil || a.effortOps.Current == nil {
		return ""
	}
	return strings.TrimSpace(a.effortOps.Current())
}

// effortToken is the ink for the effort rung: lean is muted, standard is
// the status-line grey, full wears the success accent so complex work is visible.
func (a *App) effortToken() string {
	switch a.effortLabel() {
	case config.SessionEffortLean:
		return theme.Gray
	case config.SessionEffortFull:
		return theme.AccentSuccess
	case config.SessionEffortStandard:
		return theme.StatusLineMode
	default:
		return theme.StatusLineMode
	}
}
