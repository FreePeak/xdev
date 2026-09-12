package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
	// Pending is the latest submitted plan while the decision is open:
	// the xd://propose device serves it, and acceptance (reviewer, device,
	// or a new proposal replacing it) consumes it.
	Pending string
	// Yolo auto-approves the FIRST proposal (--plan-yolo): the reviewer
	// callback still fires for the host to observe, but its answer cannot
	// block the first acceptance.
	Yolo bool
	// OnAccept (optional) fires once when a proposal is accepted —
	// --plan-yolo-into uses it to hand the run to the execution model.
	// It runs on the tool-execution goroutine, like the propose call.
	OnAccept func()
	// yoloUsed marks the auto-approval as spent: later proposals take the
	// reviewer path.
	yoloUsed bool
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
	// Publish the proposal while the decision is open: the xd://propose
	// device serves this text, and acceptance consumes it.
	p.pm.Pending = a.Plan
	switch {
	case p.OnPropose == nil:
		// No host decision wired (headless/print): accept — a plan nobody
		// can review must not trap the run in read-only forever.
		p.pm.accept()
		return tool.Result{Text: "plan accepted (no reviewer wired) — plan mode off; implement it now"}, nil
	case p.pm.Yolo && !p.pm.yoloUsed:
		// --plan-yolo: the first proposal is pre-approved. The reviewer
		// still fires (hosts observe/display the submission), but its
		// answer cannot block the run.
		p.pm.yoloUsed = true
		p.OnPropose(ctx, a.Plan)
		p.pm.accept()
		return tool.Result{Text: "plan approved automatically (--plan-yolo) — plan mode off; implement it now"}, nil
	}
	accept, note := p.OnPropose(ctx, a.Plan)
	if accept {
		p.pm.accept()
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

// accept leaves plan mode, consumes the pending proposal, and fires the
// one-shot OnAccept hook (the --plan-yolo execution-model handoff). It is
// the single acceptance transition for both the propose tool and the
// xd://resolve device, so the two cannot diverge.
func (pm *PlanMode) accept() {
	pm.Active = false
	pm.Pending = ""
	if pm.OnAccept != nil {
		sw := pm.OnAccept
		pm.OnAccept = nil // one-shot: only the first acceptance switches
		sw()
	}
}

// --- xd:// proposal devices (M11 #36, omp naming parity) ---
//
// omp exposes plan finalization as URI devices. xdev's propose tool stays
// the primary submission path; these are the additional access path: read
// xd://propose returns the pending plan, and writing xd://resolve /
// xd://reject with a one-sentence reason finalizes it. cmd registers all
// three on the URI seam (tool.RegisterURIScheme / RegisterWriteDevice).

// DeviceRead serves the xd:// read device (read xd://propose).
func (pm *PlanMode) DeviceRead(uri string) (string, error) {
	switch xdDevice(uri) {
	case "propose":
		if pm.Pending == "" {
			return "no pending proposal — the propose tool submits one", nil
		}
		return pm.Pending, nil
	default:
		return "", fmt.Errorf("unknown xd:// read device %q (xd://propose returns the pending plan; finalize with a write to xd://resolve or xd://reject)", uri)
	}
}

// ResolveDevice approves the pending proposal — the write-device form of
// the reviewer's accept (write xd://resolve "<one sentence>").
func (pm *PlanMode) ResolveDevice(_, content string) (string, error) {
	return pm.finalize(true, content)
}

// RejectDevice declines it, leaving plan mode on so the model revises
// (write xd://reject "<one sentence>").
func (pm *PlanMode) RejectDevice(_, content string) (string, error) {
	return pm.finalize(false, content)
}

// finalize resolves the pending proposal through the accept transition
// (or the reject revision note) and consumes the pending text.
func (pm *PlanMode) finalize(accept bool, content string) (string, error) {
	if pm.Pending == "" {
		verb := "reject"
		if accept {
			verb = "resolve"
		}
		return "", fmt.Errorf("no pending proposal to %s", verb)
	}
	reason := firstLine(content)
	if !accept {
		if reason == "" {
			reason = "the host asked for revisions"
		}
		pm.Pending = ""
		return "plan rejected — " + reason + ". Revise the plan and propose again.", nil
	}
	pm.accept()
	if reason == "" {
		return "plan approved — plan mode off; implement it now", nil
	}
	return "plan approved (" + reason + ") — plan mode off; implement it now", nil
}

// xdDevice returns the device name of an xd:// URI ("propose", "resolve").
func xdDevice(uri string) string {
	if i := strings.Index(uri, "://"); i >= 0 {
		return strings.ToLower(strings.TrimSpace(uri[i+3:]))
	}
	return strings.ToLower(strings.TrimSpace(uri))
}

// firstLine narrows a resolution reason to the one sentence the device
// contract asks for.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// SwitchToModel hands the run to target (the --plan-yolo execution-model
// handoff). The target joins the failover chain as the current one, so the
// provider/model swap, the compaction window, and the persisted
// ModelChangeEntry all come from switchTarget — the same machinery
// prewalkSwitch uses, so prewalk and plan-yolo compose instead of fighting
// over a.Model.
func (a *Agent) SwitchToModel(t FailoverTarget, reason string) {
	a.Failovers = append(a.Failovers, t)
	a.switchTarget(len(a.Failovers), reason)
}

// planModeSystemReminder is appended to the system prompt while plan
// mode is active (opencode system-reminder shape).
func planModeSystemReminder(note string) string {
	s := "Plan mode is ACTIVE. You are the plan agent: explore with read-only tools (read, grep, glob), " +
		"map the change surface, and verify feasibility — but do NOT edit, write, or run state-changing commands. " +
		"When the design is ready, call propose with the full implementation plan. " +
		"If a tool call is denied with a plan-mode message, that is the read-only boundary working. " +
		"While a proposal awaits review, read xd://propose to re-read it."
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
	"web_search": true, "lsp": true, // read-only research tools (M13)
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
