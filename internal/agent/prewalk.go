package agent

import (
	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/tool"
)

// Prewalk is the one-shot model handoff (M11, research §5): the session
// starts on the (big) planning model; after the first successful edit or
// write the active model switches to Target for the rest of the run.
// One-shot and self-disarming.
//
// Gate (omp parity): the switch additionally requires a plan — the
// session's todo tool must hold at least one phase with at least one task —
// so planning happens on the big model and execution on Target. A registry
// without a todo tool (reduced modes, tests) keeps the pre-todo open gate
// and logs why; see prewalkGate.
type Prewalk struct {
	// Target is the resolved provider+model the run hands off to. Built
	// by cmd from the --prewalk-into ref (default: the session model); an
	// unresolved target leaves the agent unarmed (nil Prewalk).
	Target FailoverTarget
}

// prewalkState is the live state machine (single-goroutine Run, no lock).
type prewalkState struct {
	done bool // switched already
	held bool // already logged that a plan list is missing
}

// prewalkNote inspects one tool-results batch and fires the handoff on the
// first successful edit/write once the gate is open. Caller: the Run turn
// loop after runTools. Tool results carry the tool name on the Message
// itself (ai.RoleToolResult entries), so no call-id map is needed.
//
// Every batch is re-checked rather than latching the gate shut: a plan
// created mid-run arms a later edit/write (the todo tool and the edit can
// even share one batch — the snapshot is read after the batch completed).
func (a *Agent) prewalkNote(results []ai.Message) {
	if a.Prewalk == nil || a.prewalk.done {
		return
	}
	for _, rm := range results {
		if rm.Role != ai.RoleToolResult || rm.IsError {
			continue
		}
		if rm.ToolName != "edit" && rm.ToolName != "write" {
			continue
		}
		planned, gated := a.prewalkGate()
		switch {
		case !gated:
			// Documented fallback: no todo tool in this mode, so there is
			// nothing to plan with and the pre-todo behavior stands.
			logx.Warnf("prewalk: no todo tool in the registry — firing ungated after %s", rm.ToolName)
			a.prewalkSwitch()
		case planned:
			a.prewalkSwitch()
		case !a.prewalk.held:
			a.prewalk.held = true
			logx.Warnf("prewalk: %s arrived before any plan todo list exists — holding until one does", rm.ToolName)
		}
		return
	}
}

// prewalkGate reports whether a plan todo list exists. gated=false means the
// registry holds no *tool.TodoTool (absent, or a foreign tool under the
// name): the caller keeps the open pre-todo gate. planned=true means at
// least one phase carries at least one task — statuses are irrelevant, a
// fully finished list is still a plan and a cleared list (phases survive
// `rm none` with no tasks) is not.
func (a *Agent) prewalkGate() (planned, gated bool) {
	if a.Tools == nil {
		return false, false
	}
	t, ok := a.Tools.Get("todo")
	if !ok {
		return false, false
	}
	tt, ok := t.(*tool.TodoTool)
	if !ok {
		return false, false
	}
	for _, p := range tt.Snapshot() {
		if len(p.Tasks) > 0 {
			return true, true
		}
	}
	return false, true
}

// prewalkSwitch performs the one-shot handoff by reusing the failover
// machinery: the target joins the failover chain as the new current
// target, so provider/model swap, compaction-window bookkeeping, and the
// persisted ModelChangeEntry all come from switchTarget — prewalk and
// failover compose instead of fighting over a.Model.
func (a *Agent) prewalkSwitch() {
	if a.Prewalk == nil || a.prewalk.done {
		return
	}
	a.Failovers = append(a.Failovers, a.Prewalk.Target)
	a.switchTarget(len(a.Failovers), "prewalk")
	a.prewalk.done = true
	logx.Errorf("prewalk: handed off to %s/%s", a.Prewalk.Target.Provider.Name(), a.Prewalk.Target.Model)
}
