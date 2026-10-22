package theme

// Agent status glyphs — omp's agent-hub glyph set (agent-hub-renderer.ts:
// the four shapes a reader scans an agent row's left column for).
//
// A reader looks at the SHAPE first and the hue second, so the glyph is
// the information and the ink only says which state it is. The shapes are
// deliberately distinct from the transcript's own status set (theme's
// success/error/pending/enabled/shadowed): a running AGENT is a spinner
// arrow, not a pending clock, because the two never appear in the same
// column.
//
// The unicode forms are one cell each. The ascii preset's substitutes are
// not offered here: an agent row is a fixed-cell grid, and a bracketed
// form would be three cells where the grid has one.
const (
	// AgentStatusRunning: the agent is live.
	AgentStatusRunning = "running"
	// AgentStatusIdle: the agent is settled and at rest.
	AgentStatusIdle = "idle"
	// AgentStatusParked: the agent is settled and parked, waiting for a revive.
	AgentStatusParked = "parked"
	// AgentStatusAborted: the agent was canceled before it could finish.
	AgentStatusAborted = "aborted"
)

// StatusGlyphs is the agent-status glyph table: the shape a status is
// drawn with, keyed by the status name. It is exported so a theme's
// symbol preset can override a single shape later, without the table
// having to be rebuilt.
//
// U+27F3 is the running arrow — NOT U+23F3, which is a double-width clock
// face that would spill a second column over the name beside it.
var StatusGlyphs = map[string]string{
	AgentStatusRunning: "⟳",
	AgentStatusIdle:    "●",
	AgentStatusParked:  "○",
	AgentStatusAborted: "⏹",
}

// StatusGlyph returns the glyph an agent status is drawn with.
//
// The status names are the hub's own vocabulary (agent.AgentStatus):
// running, idle, parked, aborted. An unknown status draws nothing, so a
// caller that gets a status it does not know leaves the cell blank rather
// than painting a wrong shape.
func StatusGlyph(status string) string {
	return StatusGlyphs[status]
}
