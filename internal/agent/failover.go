package agent

import (
	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/session"
)

// FailoverTarget is one backup model in the resilience chain (M5 #6).
// Context overflow promotes to a larger ContextWindow; a drained transient
// retry ladder fails over to the next target (omp model-host failover).
type FailoverTarget struct {
	Provider      ai.Provider
	Model         string
	ContextWindow int // 0 = unknown: usable for outage failover, never promotion
}

// ContinuationPrompt asks the model to resume after a mid-stream failure
// whose partial output was retained (omp turn-recovery continuation).
const ContinuationPrompt = "your previous message was cut off by a provider error — continue exactly where it stopped"

// currentWindow returns the active target's context window (the promotion
// ladder compares against it).
func (a *Agent) currentWindow() int {
	if a.curTarget > 0 {
		return a.Failovers[a.curTarget-1].ContextWindow
	}
	return a.Compaction.ContextWindow
}

// promotionTarget returns the chain index of the smallest-window target
// strictly larger than the active window — one ladder step, so an upgrade
// that still overflows promotes again before compaction is considered.
// -1 when no bigger window exists.
func (a *Agent) promotionTarget() int {
	cur := a.currentWindow()
	best, bestW := -1, 0
	for i, t := range a.Failovers {
		idx := i + 1
		if t.ContextWindow <= cur || idx == a.curTarget {
			continue
		}
		if best == -1 || t.ContextWindow < bestW {
			best, bestW = idx, t.ContextWindow
		}
	}
	return best
}

// nextFailoverTarget returns the next chain index for outage failover,
// -1 when the chain is exhausted.
func (a *Agent) nextFailoverTarget() int {
	if a.curTarget < len(a.Failovers) {
		return a.curTarget + 1
	}
	return -1
}

// switchTarget activates chain index i (1-based, Failovers[i-1]), raises
// the compaction window when the target is bigger, and mirrors the change
// into the session store. Run is single-goroutine, so no lock is needed.
func (a *Agent) switchTarget(i int) {
	t := a.Failovers[i-1]
	a.curTarget = i
	a.Provider = t.Provider
	a.Model = t.Model
	if t.ContextWindow > a.Compaction.ContextWindow {
		a.Compaction.ContextWindow = t.ContextWindow
	}
	logx.Errorf("recovery: switched to %s/%s (window %d)", t.Provider.Name(), t.Model, t.ContextWindow)
	if a.Store != nil {
		_ = a.Store.Append(&session.ModelChangeEntry{Model: t.Provider.Name() + "/" + t.Model})
	}
}
