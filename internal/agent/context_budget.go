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
