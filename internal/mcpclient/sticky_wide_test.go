package mcpclient

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/FreePeak/xdev/internal/tool"
)

// The ordering bug this file exists for.
//
// cmd/xdev registers the MCP tools (mcpclient.Register) and then applies the
// session effort (tool.ApplySessionEffort). ApplySessionEffort calls
// ResetDeferred — it rebuilds the deferred set from its own tables, because a
// rung change must not leave names from the previous rung behind.
//
// A plain Defer was therefore WIPED by that rebuild, and every tool of a wide
// remote server landed back in the EAGER schema: the full JSON schema of all
// 27 database tools, re-sent on every request, in a session working on an
// unrelated Go repository. The code even documents the hazard for the
// code-graph tools ("a Defer here would be wiped by its ResetDeferred rebuild
// and the tools would land in the eager schema") — but the MCP path was
// ordered the wrong way round.
//
// The deferral is now sticky: a remote surface that is too wide to advertise
// at one rung is too wide at every rung, and the ladder does not re-open it.
func TestWideRemoteSurfaceStaysDeferredAfterTheEffortLadder(t *testing.T) {
	for _, rung := range []string{"low", "medium", "high", "xhigh", "max"} {
		reg := tool.NewRegistry()
		Register(reg, stubTools(DeferThreshold+15))

		if got := len(reg.Defs()); got != 0 {
			t.Fatalf("rung %q: eager defs before the ladder = %d, want 0 (a wide surface starts deferred)", rung, got)
		}

		tool.ApplySessionEffort(reg, rung)

		if got := len(reg.Defs()); got != 0 {
			t.Fatalf("rung %q: eager defs after the ladder = %d, want 0 — the ladder re-opened the remote surface", rung, got)
		}
		if d := reg.Deferred(); len(d) != DeferThreshold+15 {
			t.Fatalf("rung %q: deferred = %d, want %d", rung, len(d), DeferThreshold+15)
		}
	}
}

// The counterpart: a NARROW remote surface stays eager. Hiding three tools
// would be a pure usability regression for no measurable gain, and the
// threshold exists precisely so the small case is not treated like the wide
// one.
func TestNarrowRemoteSurfaceStaysEagerAfterTheEffortLadder(t *testing.T) {
	reg := tool.NewRegistry()
	Register(reg, stubTools(3))

	tool.ApplySessionEffort(reg, "high")

	if got := len(reg.Defs()); got != 3 {
		t.Fatalf("eager defs = %d, want 3 (a narrow surface must stay direct)", got)
	}
	if d := reg.Deferred(); len(d) != 0 {
		t.Fatalf("deferred = %v, want none", d)
	}
}

// The wide surface stays RUNNABLE through the bridge: a deferral is
// discovery, not a boundary. The approval policy judges the real tool name.
func TestWideRemoteSurfaceStaysRunnableAfterTheEffortLadder(t *testing.T) {
	reg := tool.NewRegistry()
	Register(reg, stubTools(DeferThreshold+1))
	tool.ApplySessionEffort(reg, "high")

	remote, ok := reg.Get("srv_call0")
	if !ok {
		t.Fatal("a deferred remote tool left the registry")
	}
	if _, err := remote.Execute(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}
