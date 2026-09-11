package agent

import (
	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/logx"
)

// Prewalk is the one-shot model handoff (M11, research §5): the session
// starts on the (big) planning model; after the first successful edit or
// write the active model switches to Target for the rest of the run.
// One-shot and self-disarming.
//
// Gate: omp opens the gate on a successful todo call (plan-before-execute
// discipline). xdev has no todo tool yet, so the gate is open from run
// start and the first successful edit/write switches. When a todo tool
// lands, tighten the gate here — one place.
type Prewalk struct {
	// Target is the resolved provider+model the run hands off to. Built
	// by cmd from the --prewalk-into ref (default @smol); an unresolved
	// target leaves the agent unarmed (nil Prewalk).
	Target FailoverTarget
}

// prewalkState is the live state machine (single-goroutine Run, no lock).
type prewalkState struct {
	done bool // switched already
}

// prewalkNote inspects one tool-results batch and fires the handoff on
// the first successful edit/write. Caller: the Run turn loop after
// runTools. Tool results carry the tool name on the Message itself
// (ai.RoleToolResult entries), so no call-id map is needed.
func (a *Agent) prewalkNote(results []ai.Message) {
	if a.Prewalk == nil || a.prewalk.done {
		return
	}
	for _, rm := range results {
		if rm.Role != ai.RoleToolResult || rm.IsError {
			continue
		}
		if rm.ToolName == "edit" || rm.ToolName == "write" {
			a.prewalkSwitch()
			return
		}
	}
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
	a.switchTarget(len(a.Failovers))
	a.prewalk.done = true
	logx.Errorf("prewalk: handed off to %s/%s", a.Prewalk.Target.Provider.Name(), a.Prewalk.Target.Model)
}
