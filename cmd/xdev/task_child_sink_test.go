package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/theme"
	"github.com/FreePeak/xdev/internal/tui"
)

// The host's half of the feature: an agent child event has no call id, so the
// sink decides which `task` row a child belongs to. Two `task` calls can be
// in flight in the same turn (agent.MaxToolWorkers), so a new child belongs
// to the newest running row and a child the sink already knows keeps its own
// row for the rest of its life. The transcript's side of that binding is
// pinned in internal/tui (TestTaskChildRowsFollowTheirOwnCall); this is the
// host's.
func TestTaskChildSinkBindsEachChildToItsOwnCall(t *testing.T) {
	app := newSinkApp(t)
	app.AddToolBlock("c1", agent.TaskToolName, `{"prompt":"first"}`)
	app.AddToolBlock("c2", agent.TaskToolName, `{"prompt":"second"}`)

	sink := &taskChildSink{app: app}
	// c2 is the newest running row, so the first child joins c2.
	sink.onEvent(agent.SubagentEvent{Kind: agent.SubagentStart, Label: "second-child", Model: "m"})
	if got := sink.callID("second-child"); got != "c2" {
		t.Fatalf("child bound to %q, want the running call c2", got)
	}

	// A child of the FIRST call arrives late; the sink must not re-bind it
	// to c2 just because c2 is still spinning. Reaching in is the shape of
	// the race: the agent reports the start on whichever goroutine the
	// batch ran it, and only the host knows which call asked for it.
	sink.mu.Lock()
	sink.ids["first-child"] = "c1"
	sink.mu.Unlock()
	sink.onEvent(agent.SubagentEvent{Kind: agent.SubagentStart, Label: "first-child", Model: "m"})
	if got := sink.callID("first-child"); got != "c1" {
		t.Fatalf("child re-bound to %q, want its own call c1", got)
	}

	// A settled child does not disturb the binding, and its events keep
	// flowing to the same row.
	sink.onEvent(agent.SubagentEvent{Kind: agent.SubagentEnd, Label: "second-child", Status: "yielded", Dur: time.Second})
	sink.onEvent(agent.SubagentEvent{
		Kind: agent.SubagentTool, Label: "second-child", Tool: "read",
		Args: json.RawMessage(`{"path":"a.go"}`), Status: "ok",
	})
	if got := sink.callID("second-child"); got != "c2" {
		t.Fatalf("a settled child lost its call: %q", got)
	}
}

// A child event that arrives after its call settled has no row to join: the
// sink must not hang it off the next call. The transcript drops an event
// with an unknown call id, and the transcript's own test pins that half.
func TestTaskChildSinkDropsLateEvents(t *testing.T) {
	app := newSinkApp(t)
	app.AddToolBlock("c1", agent.TaskToolName, `{"prompt":"x"}`)
	app.FinishTool("c1", agent.TaskToolName, false, "yielded", tui.ToolOutcome{Dur: "1s"})

	sink := &taskChildSink{app: app}
	sink.onEvent(agent.SubagentEvent{Kind: agent.SubagentStart, Label: "late", Model: "m"})

	if got := sink.callID("late"); got != "" {
		t.Fatalf("a late child bound to %q, want nothing", got)
	}
}

func newSinkApp(t *testing.T) *tui.App {
	t.Helper()
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	scr.SetSize(100, 30)
	t.Cleanup(func() { scr.Fini() })
	return tui.New(scr, theme.Load("groknight"), "test/free", "sess1234")
}
