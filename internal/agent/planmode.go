package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// PlanMode is the read-only sub-state (M11, opencode plan-mode parity
// [CORE]): while active, the agent explores and plans but cannot mutate.
// Mutating tools (TierWrite/TierExec) are denied with a pointer to the
// propose tool; read-only tools work. Exiting requires an explicit
// propose call the host can gate on a user decision (omp xd://propose,
// Claude ExitPlanMode, opencode plan_exit — same contract, one surface).
//
// The state sits behind mu because #291's context dock reads it from the
// paint goroutine while the agent loop writes it from a tool goroutine.
// Every access goes through an accessor, and mu covers a whole transition
// (read + flip + consume), not one field at a time — a surface that reads
// Pending then Active as two separate steps can approve a proposal that was
// already consumed. mu is never held across a callback: OnAccept switches
// models, and a switch that re-enters plan state must not deadlock.
type PlanMode struct {
	mu sync.RWMutex
	// active is the read-only guard.
	active bool
	// note (set by the host) appears in the system reminder that teaches the
	// shape: research first, then propose.
	note string
	// propose is the host-built exit tool, set by SetPropose. The agent
	// exposes it in toolDefs and routes calls to it ONLY while plan mode is
	// a live sub-state — it never appears in the normal-mode registry.
	propose tool.Tool
	// pending is the latest submitted plan while the decision is open: the
	// xd://propose device serves it, the dock shows it as the pending-plan
	// card, and acceptance (reviewer, device, or the user) consumes it.
	pending string
	// yolo auto-approves the FIRST proposal (--plan-yolo): the reviewer
	// callback still fires for the host to observe, but its answer cannot
	// block the first acceptance.
	yolo bool
	// onAccept (optional) fires once when a proposal is accepted —
	// --plan-yolo-into uses it to hand the run to the execution model.
	// It runs on the tool-execution goroutine, like the propose call.
	onAccept func()
	// planOnly makes a proposal END the run instead of opening
	// implementation: the headless `-plan` (without --plan-yolo) semantic,
	// where nobody can approve and the plan itself is the deliverable.
	// Auto-accepting there would void the read-only guarantee the flag
	// advertises (parity finding T3 #27).
	planOnly bool
	// proposed marks that a planOnly run has submitted its plan; the loop
	// stops at the turn boundary and the host prints the document.
	proposed bool
	// yoloUsed marks the auto-approval as spent: later proposals take the
	// reviewer path.
	yoloUsed bool
	// todo is the session's task list, registered by the host for the plan
	// surfaces (#291 §2). Guarded by the same lock as everything above.
	todo TodoLister
	// invalidate (set by the host) fires after a pending proposal is published
	// or consumed. #291's context dock is the only reason it exists: the panel
	// rebuilds on the event instead of being polled, which is what keeps a frame
	// cost-free. It is fired under no lock and must be cheap and non-blocking.
	invalidate func()
}

// SetInvalidate wires the host's "the pending proposal moved" callback (the TUI
// stamps its dock version from it). A host with no such surface leaves it nil.
func (pm *PlanMode) SetInvalidate(fn func()) {
	if pm == nil {
		return
	}
	pm.mu.Lock()
	pm.invalidate = fn
	pm.mu.Unlock()
}

// moved fires the invalidation. Call it after the lock step that changed
// `pending`, never while holding it: the callback belongs to the display layer.
func (pm *PlanMode) moved() {
	if pm != nil && pm.invalidate != nil {
		pm.invalidate()
	}
}

// Active reports whether the read-only plan-mode guard is in force.
func (pm *PlanMode) Active() bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.active
}

// SetActive turns the read-only guard on or off — the /plan transition.
func (pm *PlanMode) SetActive(on bool) {
	pm.mu.Lock()
	pm.active = on
	pm.mu.Unlock()
}

// Note returns the host's plan-mode note.
func (pm *PlanMode) Note() string {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.note
}

// SetNote records the host's plan-mode note.
func (pm *PlanMode) SetNote(s string) {
	pm.mu.Lock()
	pm.note = s
	pm.mu.Unlock()
}

// Propose returns the host-built exit tool (nil until SetPropose).
func (pm *PlanMode) Propose() tool.Tool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.propose
}

// SetPropose registers the exit tool.
func (pm *PlanMode) SetPropose(t tool.Tool) {
	pm.mu.Lock()
	pm.propose = t
	pm.mu.Unlock()
}

// Pending returns the plan awaiting user review ("" = none).
func (pm *PlanMode) Pending() string {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.pending
}

// View is #291's one read of the review state: the pending plan plus
// whether the guard is live. Both come out of a single lock, so the
// pending-plan card cannot show a proposal under a mode the user already
// switched off.
func (pm *PlanMode) View() (pending string, active bool) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.pending, pm.active
}

// TodoLister is the task list behind the plan: the phases the plan was written
// against, so a document shown without the work it implies is half a review.
// tool.TodoTool satisfies it; a host with no todo tool leaves it unset.
type TodoLister interface {
	View() string
}

// SetTodo registers the session's task list for the plan surfaces. The dock
// reads it beside Pending; it is display state, and approval never consults it.
func (pm *PlanMode) SetTodo(t TodoLister) {
	if pm == nil {
		return
	}
	pm.mu.Lock()
	pm.todo = t
	pm.mu.Unlock()
}

// Todo returns the task list's rendered view ("" with no list or no tasks).
func (pm *PlanMode) Todo() string {
	if pm == nil {
		return ""
	}
	pm.mu.RLock()
	t := pm.todo
	pm.mu.RUnlock()
	if t == nil {
		return ""
	}
	return t.View()
}

// Yolo reports whether a first proposal is pre-approved (--plan-yolo).
func (pm *PlanMode) Yolo() bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.yolo
}

// SetYolo arms --plan-yolo.
func (pm *PlanMode) SetYolo(on bool) {
	pm.mu.Lock()
	pm.yolo = on
	pm.mu.Unlock()
}

// PlanOnly reports whether proposals end the run (headless -plan).
func (pm *PlanMode) PlanOnly() bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.planOnly
}

// SetPlanOnly arms the headless -plan posture.
func (pm *PlanMode) SetPlanOnly(on bool) {
	pm.mu.Lock()
	pm.planOnly = on
	pm.mu.Unlock()
}

// SetOnAccept wires the one-shot acceptance hook (--plan-yolo-into).
func (pm *PlanMode) SetOnAccept(fn func()) {
	pm.mu.Lock()
	pm.onAccept = fn
	pm.mu.Unlock()
}

// Reset leaves plan mode and drops the pending proposal. The handoff calls
// it: a handed-off context must not keep a proposal the document already
// captured, or the next session's propose would gate on a review round the
// user never saw.
func (pm *PlanMode) Reset() {
	if pm == nil {
		return
	}
	pm.mu.Lock()
	pm.active, pm.pending = false, ""
	pm.mu.Unlock()
	pm.moved()
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
	pm := p.pm
	// Publish the proposal FIRST, in the same lock step that decides what
	// kind of run this is: the xd://propose device serves the text, and
	// #291's dock card is the only place a user can actually read it — the
	// reviewer callback owns the decision, not the display. Reading the
	// flags here (rather than after the callback returns) is also what keeps
	// a reviewer that flips plan mode from changing the run's kind mid-call.
	pm.mu.Lock()
	pm.pending = a.Plan
	planOnly, yolo, reviewer, yoloSpent := pm.planOnly, pm.yolo, p.OnPropose, pm.yoloUsed
	if planOnly {
		pm.proposed = true
	}
	pm.mu.Unlock()
	// The proposal is out, and the human can read it before anything decides it:
	// the bump fires here rather than on return, because the reviewer callback
	// below is what blocks on the answer. (accept() and Resolve bump again when
	// the state moves a second time, which is a second fact worth repainting.)
	pm.moved()
	switch {
	case planOnly:
		// A plan-only run stops at the proposal: the model is told the run
		// ends here, so it cannot drift into mutating work the session was
		// never permitted to start.
		return tool.Result{Text: "plan submitted — this run ends here (headless plan mode). Re-run with --plan-yolo to auto-approve and implement."}, nil
	case reviewer == nil:
		// No host decision wired and the run is not plan-only: accept — a
		// plan nobody can review must not trap the run in read-only forever.
		pm.accept()
		return tool.Result{Text: "plan accepted (no reviewer wired) — plan mode off; implement it now"}, nil
	case yolo && !yoloSpent:
		// --plan-yolo: the first proposal is pre-approved. The reviewer
		// still fires (hosts observe/display the submission), but its
		// answer cannot block the run.
		pm.mu.Lock()
		pm.yoloUsed = true
		pm.mu.Unlock()
		reviewer(ctx, a.Plan)
		pm.accept()
		return tool.Result{Text: "plan approved automatically (--plan-yolo) — plan mode off; implement it now"}, nil
	}
	accept, note := reviewer(ctx, a.Plan)
	if accept {
		pm.accept()
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

// Proposed reports whether a PlanOnly run has submitted its plan (the loop
// stop signal; see PlanMode.PlanOnly).
func (pm *PlanMode) Proposed() bool {
	if pm == nil {
		return false
	}
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.proposed
}

// accept leaves plan mode, consumes the pending proposal, and fires the
// one-shot onAccept hook (the --plan-yolo execution-model handoff). It is
// the single acceptance transition, so no reviewer path can diverge from
// another. The hook runs unlocked — see the type comment.
func (pm *PlanMode) accept() {
	pm.mu.Lock()
	pm.active, pm.pending = false, ""
	sw := pm.onAccept
	pm.onAccept = nil // one-shot: only the first acceptance switches
	pm.mu.Unlock()
	if sw != nil {
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
		if pending := pm.Pending(); pending != "" {
			return pending, nil
		}
		return "no pending proposal — the propose tool submits one", nil
	default:
		return "", fmt.Errorf("unknown xd:// read device %q (xd://propose returns the pending plan; finalize with a write to xd://resolve or xd://reject)", uri)
	}
}

// ResolveDevice approves the pending proposal — the write-device form of
// the reviewer's accept (write xd://resolve "<one sentence>").
func (pm *PlanMode) ResolveDevice(_, content string) (string, error) {
	return pm.Resolve(true, content)
}

// RejectDevice declines it, leaving plan mode on so the model revises
// (write xd://reject "<one sentence>").
func (pm *PlanMode) RejectDevice(_, content string) (string, error) {
	return pm.Resolve(false, content)
}

// Resolve is the ONE review transition every surface shares: the xd://
// devices, the /plan commands, and #291's dock card. It consumes the
// pending proposal, approves through the accept transition, or rejects with
// the revision note — so a second approval surface cannot drift from the
// first. content is the reviewer's one-sentence reason.
func (pm *PlanMode) Resolve(accept bool, content string) (string, error) {
	pm.mu.Lock()
	if pm.pending == "" {
		pm.mu.Unlock()
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
		pm.pending = ""
		pm.mu.Unlock()
		pm.moved()
		return "plan rejected — " + reason + ". Revise the plan and propose again.", nil
	}
	pm.active, pm.pending = false, ""
	sw := pm.onAccept
	pm.onAccept = nil // one-shot, as in accept()
	pm.mu.Unlock()
	pm.moved()
	if sw != nil {
		sw()
	}
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

// applyPlanMode wraps one tool call: capability-declared read-only (or
// session-scoped) tools pass; everything else is denied with a pointer to
// propose. propose itself is the exit. Undeclared tools fail closed (#420).
func applyPlanMode(pm *PlanMode, call ai.ToolCallBlock, reg *tool.Registry) (tool.Result, bool) {
	if pm == nil || !pm.Active() {
		return tool.Result{}, false
	}
	if call.Name == ProposeToolName {
		return tool.Result{}, false
	}
	c, declared := capsForCall(reg, call.Name)
	if tool.AllowedInPlan(c, declared) {
		return tool.Result{}, false
	}
	what := "not read-only"
	if declared && c.Destructive {
		what = "a mutating tool"
	}
	if call.Name == "bash" {
		what = "a state-changing command"
	}
	return tool.Result{Text: planDenyMessage(call.Name, what), IsError: true}, true
}

// capsForCall resolves Caps from a registered Capser, else the builtin table.
func capsForCall(reg *tool.Registry, name string) (tool.Caps, bool) {
	if reg != nil {
		if t, ok := reg.Get(name); ok {
			return tool.CapsOf(t)
		}
	}
	return tool.CapsByName(name)
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

// Publish parks a finished plan out for review without a propose call. The
// host's own resume path needs it — a plan the model wrote before a restart
// must come back readable — and it is the seam the device tests set up
// through. The read-only guard is untouched: publishing is not approving.
func (pm *PlanMode) Publish(plan string) {
	pm.mu.Lock()
	pm.pending = plan
	pm.mu.Unlock()
	pm.moved()
}
