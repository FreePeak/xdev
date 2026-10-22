package agent

import (
	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// ToolLessonObserver is the tool-usage learning seam: it sees every tool
// outcome, and the backend turns a failure plus the agent's own recovery
// into a durable recipe (internal/memory's tool-use store).
//
// The loop reports to it on the two paths where the agent got the call
// wrong: a tool that ran and failed (runOneTool, after the extension
// interceptors have finalized the text and before the rulebook note is
// appended — a rule note is guidance for the model, not the tool's own
// error, and it must not end up in the symptom), and a call to a tool that
// does not exist.
//
// A backend that learns nothing (ag.ToolLessons nil) keeps the exact
// behavior it had before: the call runs, the result goes to the model, and
// nothing is remembered.
type ToolLessonObserver interface {
	// ObserveTool reports one finished tool call. A result with
	// IsError set is a failure; anything else is the agent getting the
	// call right, which is what closes a pending failure.
	ObserveTool(call ai.ToolCallBlock, res tool.Result)
}

// observeTool hands one tool outcome to the tool-usage learner.
//
// It fires on the two paths where the agent got the CALL wrong: a tool
// that ran and failed, and a call to a tool that does not exist. The
// paths where the harness refuses — denied, blocked, canceled — are not
// the agent's mistake, so they are not reported: there is no form of
// those calls that would have worked.
func (a *Agent) observeTool(call ai.ToolCallBlock, res tool.Result) {
	if a == nil || a.ToolLessons == nil {
		return
	}
	a.ToolLessons.ObserveTool(call, res)
}
