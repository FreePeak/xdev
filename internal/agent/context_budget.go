package agent

import (
	"fmt"
	"os"
)

// MaxContextTokensDefault mirrors Claude Code's
// CLAUDE_CODE_MAX_CONTEXT_TOKENS default (200000 — CC's
// documented default for 200K-window models). Override with
// the XDEV_MAX_CONTEXT_TOKENS environment variable.
const MaxContextTokensDefault = 200000

// MaxContextTokensEnv names the env var to override the
// context token budget. CC ships CLAUDE_CODE_MAX_CONTEXT_TOKENS;
// xdev uses XDEV_MAX_CONTEXT_TOKENS (same purpose, different name).
const MaxContextTokensEnv = "XDEV_MAX_CONTEXT_TOKENS"

// ResolveMaxContextTokens returns the context token budget:
// XDEV_MAX_CONTEXT_TOKENS env var, or MaxContextTokensDefault.
// A bare env var ("") falls back to the default — a zero budget
// would silently disable compaction and every model it touches
// would be wrong.
func ResolveMaxContextTokens() int {
	if raw := os.Getenv(MaxContextTokensEnv); raw != "" {
		var n int
		if _, err := fmt.Sscanf(raw, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return MaxContextTokensDefault
}

// SetContextWindow re-bases the context window of a live agent, so a pin
// reaches the turn already running and not only the next one. w <= 0 clears
// the pin, handing the agent back the window it was constructed with.
func (a *Agent) SetContextWindow(w int) {
	if w <= 0 {
		a.windowPin.Store(0)
		return
	}
	a.windowPin.Store(int64(w))
}

// compaction is the effective config for every window decision: the
// Compaction literal as constructed, with a live pin layered over it. With
// no pin set (the shipped default) it is exactly a.Compaction, so every
// reader routed through here is unchanged for every session that never
// touches /context.
func (a *Agent) compaction() CompactionConfig {
	c := a.Compaction
	if w := a.windowPin.Load(); w > 0 {
		c.ContextWindow = int(w)
	}
	return c
}
