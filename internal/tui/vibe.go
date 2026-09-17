package tui

import (
	"fmt"
	"strings"
)

// VibeOps wires /vibe to the live director mode (the state lives in cmd).
//   - Active reports the mode: the composer's status line renders the Vibe
//     indicator from it, so the indicator follows every transition.
//   - Set enters or exits; an error (plan/goal conflict, no hub) is shown to
//     the user and leaves the mode unchanged.
//   - Status renders the worker registry for `/vibe status`.
//
// nil ops degrade the command to a notice.
type VibeOps struct {
	Active func() bool
	Set    func(on bool) error
	Status func() string
}

// SetVibeOps wires the /vibe command (M14 #58).
func (a *App) SetVibeOps(ops *VibeOps) { a.vibeOps = ops }

// Vibe implements CommandAPI /vibe:
//
//	/vibe                 toggle the mode
//	/vibe on | off        set it explicitly
//	/vibe <prompt>        enter and submit that prompt as the first directive
//	/vibe status          show the worker registry
func (a *App) Vibe(args string) error {
	if a.vibeOps == nil || a.vibeOps.Active == nil || a.vibeOps.Set == nil {
		return fmt.Errorf("vibe mode not wired")
	}
	arg := strings.TrimSpace(args)
	if arg == "status" {
		if a.vibeOps.Status == nil {
			return fmt.Errorf("vibe status not wired")
		}
		a.AddSystemBlock(a.vibeOps.Status())
		return nil
	}
	directive := ""
	var on bool
	switch arg {
	case "":
		on = !a.vibeOps.Active()
	case "on":
		on = true
	case "off":
		on = false
	default:
		on, directive = true, arg
	}
	if err := a.vibeOps.Set(on); err != nil {
		return err
	}
	if on {
		a.AddSystemBlock("vibe mode ON — director: read + todo + vibe_* worker tools; workers do the work (/vibe off to exit)")
	} else {
		a.AddSystemBlock("vibe mode OFF — toolset restored, workers killed")
	}
	if directive != "" {
		a.SendPrompt(directive)
	}
	return nil
}
