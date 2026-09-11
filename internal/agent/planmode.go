package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// PlanMode is the read-only sub-state (M11, opencode plan-mode parity
// [CORE]): while active, the agent explores and plans but cannot mutate.
// Mutating tools (TierWrite/TierExec) are denied with a pointer to the
// propose tool; read-only tools work. Exiting requires an explicit
// propose call the host can gate on a user decision (omp xd://propose,
// Claude ExitPlanMode, opencode plan_exit — same contract, one surface).
type PlanMode struct {
	Active bool
	// Note (optional, set by the host) appears in the system reminder that
	// teaches the model the shape: research first, then propose.
	Note string
	// Propose is the host-built exit tool. The agent exposes it in
	// toolDefs and routes calls to it ONLY while plan mode is a live
	// sub-state — it never appears in the normal-mode registry.
	Propose tool.Tool
}

// proposeTool ends the plan phase: the model submits its plan and the
// tool result tells the loop the agent is ready to leave plan mode. The
// HOST decides whether to accept (switch modes) — the tool only hands
// the proposal up, so approval UIs stay outside the loop.
type proposeTool struct {
	pm *PlanMode
	// OnPropose (host callback) receives the plan text; returns the
	// answer shown to the model: accept (leave plan mode, proceed to
	// build) or revise (stay).
	OnPropose func(ctx context.Context, plan string) (accept bool, note string)
}

const ProposeToolName = "propose"

func (p *proposeTool) Name() string { return ProposeToolName }

func (p *proposeTool) Description() string {
	return "submit your implementation plan and request to leave plan mode (read-only research); the user reviews it — revise and re-propose if rejected"
}

func (p *proposeTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "plan": {"type": "string", "description": "the implementation plan: findings, files to touch, step-by-step changes, and the verification you will run"}
  },
  "required": ["plan"]
}`)
}

func (p *proposeTool) Execute(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var a struct {
		Plan string `json:"plan"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return tool.Result{Text: "propose: malformed arguments: " + err.Error(), IsError: true}, nil
	}
	if p.OnPropose == nil {
		// No host decision wired (headless/print): accept — a plan nobody
		// can review must not trap the run in read-only forever.
		p.pm.Active = false
		return tool.Result{Text: "plan accepted (no reviewer wired) — plan mode off; implement it now"}, nil
	}
	accept, note := p.OnPropose(ctx, a.Plan)
	if accept {
		p.pm.Active = false
		if note == "" {
			note = "plan approved — plan mode off; implement it now"
		}
		return tool.Result{Text: note}, nil
	}
	if note == "" {
		note = "the user asked for revisions"
	}
	return tool.Result{Text: "plan rejected — " + note + ". Revise the plan and propose again.", IsError: false}, nil
}

// planModeSystemReminder is appended to the system prompt while plan
// mode is active (opencode system-reminder shape).
func planModeSystemReminder(note string) string {
	s := "Plan mode is ACTIVE. You are the plan agent: explore with read-only tools (read, grep, glob), " +
		"map the change surface, and verify feasibility — but do NOT edit, write, or run state-changing commands. " +
		"When the design is ready, call propose with the full implementation plan. " +
		"If a tool call is denied with a plan-mode message, that is the read-only boundary working."
	if note != "" {
		s += "\n" + note
	}
	return s
}

// planReadOnlyTools are the tools a planning agent may use. Explicit
// names, not Classify: the approval tier table covers only the core four
// and errors on the rest, which would deny grep/glob — the exact tools
// planning needs.
var planReadOnlyTools = map[string]bool{
	"read": true, "grep": true, "glob": true, "ast_grep": true,
}

// planDenyKinds gives friendlier denial text for the common mutators.
var planDenyKinds = map[string]string{
	"write":    "a mutating tool",
	"edit":     "a mutating tool",
	"bash":     "a state-changing command",
	"ast_edit": "a mutating tool",
}

// applyPlanMode wraps one tool call: read-only tools pass, everything
// else is denied with a pointer to propose. propose itself is the exit.
func applyPlanMode(pm *PlanMode, call ai.ToolCallBlock) (tool.Result, bool) {
	if pm == nil || !pm.Active {
		return tool.Result{}, false
	}
	if call.Name == ProposeToolName {
		return tool.Result{}, false
	}
	if planReadOnlyTools[call.Name] {
		return tool.Result{}, false
	}
	what, ok := planDenyKinds[call.Name]
	if !ok {
		what = "not read-only"
	}
	return tool.Result{Text: planDenyMessage(call.Name, what), IsError: true}, true
}

func planDenyMessage(name, what string) string {
	return fmt.Sprintf("plan mode: %s is %s and not allowed while planning. Research with read-only tools (read, grep, glob), then call propose with the plan.", name, what)
}

// NewProposeTool builds the propose tool bound to a plan-mode state and
// the host's reviewer callback (nil reviewer = auto-accept, for headless
// runs that would otherwise trap in read-only).
func NewProposeTool(pm *PlanMode, onPropose func(ctx context.Context, plan string) (accept bool, note string)) tool.Tool {
	return &proposeTool{pm: pm, OnPropose: onPropose}
}
