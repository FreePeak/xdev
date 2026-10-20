package session

import (
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

// TestBuildContextDropsToolResultThatAnswersNoLiveCall pins the wire
// invariant every strict tool-calling upstream validates: a tool result
// answers a call made by the assistant turn it follows, and every call that
// turn made is answered exactly once.
//
// A stored session can hold a result whose id belongs to an EARLIER turn —
// the agent's per-run dedup cache returns a cached result verbatim, so the
// id is the first call's, not the current one (agent.runOneTool). Replaying
// that history as-is reaches the provider with a tool message that no
// assistant turn asked for, and MiniMax answers "invalid params, tool call
// id is invalid" (2013) — on every retry, because the turn is history.
func TestBuildContextDropsToolResultThatAnswersNoLiveCall(t *testing.T) {
	entries := []Entry{
		userMsg("11111111", "", "hi"),
		asstMsg("22222222", "11111111", "",
			ai.ToolCallBlock{ID: "call_A", Name: "leankg_status", Arguments: []byte(`{"project":"x"}`)}),
		toolResultMsg("33333333", "22222222", "call_A", "first"),
		asstMsg("44444444", "33333333", "",
			ai.ToolCallBlock{ID: "call_B", Name: "leankg_status", Arguments: []byte(`{"project":"x"}`)},
			ai.ToolCallBlock{ID: "call_C", Name: "bash", Arguments: []byte(`{"command":"ls"}`)}),
		// The stale one: the id of the FIRST turn's call, replayed here.
		toolResultMsg("55555555", "44444444", "call_A", "second"),
		toolResultMsg("66666666", "55555555", "call_C", "ls output"),
	}

	r := ctx(t, entries, "66666666")

	// Walk the rebuilt conversation the way the upstream validator does.
	var open []string
	answered := map[string]int{}
	for i, m := range r.Messages {
		switch m.Role {
		case ai.RoleAssistant:
			open = nil
			for _, b := range m.Content {
				if tc, ok := b.(ai.ToolCallBlock); ok {
					open = append(open, tc.ID)
				}
			}
		case ai.RoleToolResult:
			known := false
			for _, id := range open {
				if id == m.ToolCallID {
					known = true
				}
			}
			if !known {
				t.Fatalf("messages[%d]: tool result answers %q, which the assistant turn it follows never asked for (it asked for %v)",
					i, m.ToolCallID, open)
			}
			answered[m.ToolCallID]++
			if answered[m.ToolCallID] > 1 {
				t.Fatalf("messages[%d]: call %q answered %d times", i, m.ToolCallID, answered[m.ToolCallID])
			}
		}
	}
}
