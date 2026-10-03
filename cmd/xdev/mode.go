package main

import (
	"fmt"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/tui"
)

// The session mode (/mode, the Shift-Tab cycle) is one vocabulary over the two
// knobs the TUI already turns: the plan guard (agent.PlanMode) and the
// approval policy (tools.approvalMode). Nothing here is a new gate — a mode
// only names a posture the agent already enforces, so the name a user picks
// cannot disagree with what the next turn actually does.
//
// The live mode is DERIVED on every read, never cached: plan wins when the
// guard is on (a read-only run is a stricter posture than any approval mode),
// and the approval vocabulary maps straight across otherwise. Deriving is what
// keeps /plan and --approval-mode — both of which still exist and must keep
// working — in step with the readout instead of beside it.

// sessionModeOf is the derivation: plan guard → plan; else the approval mode
// that will actually apply this turn (a CLI override wins over the settings
// file, exactly as it does for the per-turn policy). Anything unrecognised
// reads as bypass, which is what parseApprovalMode does with it.
func sessionModeOf(planActive bool, override, settingsApproval string) string {
	if planActive {
		return tui.ModePlan
	}
	mode := override
	if mode == "" {
		mode = settingsApproval
	}
	switch mode {
	case "always-ask":
		return tui.ModeDefault
	case "write":
		return tui.ModeAuto
	default: // "yolo", and the unset default, which parses as yolo
		return tui.ModeBypass
	}
}

// setSessionMode is the write half: plan is session state (never persisted — a
// file that opened every session read-only would be a trap), and the other
// three are the approval vocabulary written through the one config.Set path
// `xdev config set approvalMode` uses, with the in-memory layer following it
// because every later read goes through lastSettings(), not through a reloaded
// file.
//
// current is the same derivation as Current, handed back so an error can say
// which mode is actually in force — a half-applied mode reported as the one
// the user asked for would be worse than the error.
func setSessionMode(planMode *agent.PlanMode, vibeActive func() bool, apply func(string) error, current func() string) func(string) error {
	return func(name string) error {
		if name == tui.ModePlan {
			if vibeActive() {
				return fmt.Errorf("vibe mode is active — /vibe off first")
			}
			planMode.SetActive(true)
			return nil
		}
		// Leaving plan mode is the other half of the transition: a mode the
		// user can name must be reachable from plan without first typing
		// /plan off, or the two surfaces disagree about the same state.
		planMode.SetActive(false)
		var mode string
		switch name {
		case tui.ModeDefault:
			mode = "always-ask"
		case tui.ModeAuto:
			mode = "write"
		case tui.ModeBypass:
			mode = "yolo"
		default:
			return fmt.Errorf("mode: unknown %q (mode is still %s)", name, current())
		}
		return apply(mode)
	}
}

// persistApprovalMode is the single write path the mode switch and
// `xdev config set approvalMode` share, so the settings file and the live
// per-turn policy cannot drift: config.Set writes the file, and the
// in-memory layer is folded in afterwards because the rest of this process
// reads lastSettings() rather than reloading.
func persistApprovalMode(mode string) error {
	// The vocabulary check happens HERE, before the write. config.Set
	// round-trips the file for YAML shape, not for the settings vocabulary, so
	// a typo would otherwise land and only be rejected at the next start — the
	// user's session would keep its old posture with no error now. Parsing
	// through the same helper the load path uses keeps one vocabulary.
	if _, err := config.ParseApprovalMode(mode); err != nil {
		return err
	}
	if err := config.Set(config.GlobalSettingsPath(), "approvalMode", mode); err != nil {
		return err
	}
	lastSettings().ApprovalMode = mode
	return nil
}
