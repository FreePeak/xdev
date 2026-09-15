package agent

import (
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/logx"
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

// ContinuationAttribution tags the injected user turn that follows a
// retained partial: harness text the user never typed. The transcript
// renders it as a harness event and the stats keep it out of the user
// turn count (#283: 27 such turns were masquerading as user input).
const ContinuationAttribution = "provider-continuation"

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
// skipping targets still inside a cooldown window or whose provider has a
// banked quota reset pending (M5 #25: the ladder must not bounce straight
// back onto the provider that just refused). -1 when the chain is
// exhausted.
func (a *Agent) nextFailoverTarget() int {
	if a.curTarget >= len(a.Failovers) {
		return -1
	}
	now := time.Now()
	for i := a.curTarget + 1; i <= len(a.Failovers); i++ {
		t := a.Failovers[i-1]
		if t.Provider == nil {
			continue
		}
		if a.Fallback.suppressed(t.Provider.Name()+"/"+t.Model, now) {
			continue
		}
		return i
	}
	return -1
}

// switchTarget activates chain index i (1-based, Failovers[i-1]), raises
// the compaction window when the target is bigger, and mirrors the change
// into the session store. Run is single-goroutine, so no lock is needed.
func (a *Agent) switchTarget(i int, reason string) {
	t := a.Failovers[i-1]
	prev := ""
	if a.Provider != nil {
		prev = a.Provider.Name() + "/" + a.Model
	}
	a.curTarget = i
	a.Provider = t.Provider
	a.Model = t.Model
	if t.ContextWindow > a.Compaction.ContextWindow {
		a.Compaction.ContextWindow = t.ContextWindow
	}
	logx.Errorf("%s: switched to %s/%s (window %d)", reason, t.Provider.Name(), t.Model, t.ContextWindow)
	sel := t.Provider.Name() + "/" + t.Model
	a.Fallback.onSwitch(prev, reason, time.Now())
	a.recordModelChange(sel, fallbackReasons[reason])
}
