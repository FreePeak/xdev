package tui

import "testing"

// The reported break: a batch of two `bash` calls (the model emits them in one
// message; agent.runTools executes them concurrently, MaxToolWorkers=6)
// finishes in whatever order the shells finish. Before the fix FinishTool
// closed "the newest running row with this name", so whichever result arrived
// second-in-creation-order was painted under the wrong command, and the row
// still spinning over the other command's output read as "it never ran".
func TestConcurrentSameNamedCallsPairByCallID(t *testing.T) {
	a, _ := newTestApp(t, 100, 40)
	a.AddToolBlock("call_a", "bash", `{"command":"git clone --depth 1 opencode"}`)
	a.AddToolBlock("call_b", "bash", `{"command":"ls packages"}`)

	// The first call to finish is the first one to be created — the ordering
	// that transposed the two boxes.
	a.FinishTool("call_a", "bash", false, "CLONE-OUTPUT", ToolOutcome{Dur: "9s"})
	if got := rowStatus(a, "call_a"); got == "running" {
		t.Fatalf("call_a's result arrived but its row is still running — the result closed another call's row")
	}
	if got := rowStatus(a, "call_b"); got != "running" {
		t.Fatalf("call_b row = %q, want it untouched while its own call is still in flight", got)
	}

	a.FinishTool("call_b", "bash", false, "LS-OUTPUT", ToolOutcome{Dur: "1s"})
	if got := rowStatus(a, "call_b"); got == "running" {
		t.Errorf("call_b never closed — a running row spins forever")
	}
	if got := doneTexts(a); len(got) != 2 || got[0] != "CLONE-OUTPUT" || got[1] != "LS-OUTPUT" {
		t.Errorf("result boxes = %q, want each call's own output in finish order", got)
	}
}

// A finish carrying no call id (replayTranscript's shape — resumed history has
// no ids) must not steal the row of a call that is still in flight.
func TestIdlessFinishDoesNotStealALiveRow(t *testing.T) {
	a, _ := newTestApp(t, 80, 24)
	a.AddToolBlock("live", "bash", `{"command":"make build"}`)
	a.AddToolBlock("", "bash", "") // the replayed pair's own call row
	a.FinishTool("", "bash", false, "replayed", ToolOutcome{Dur: "1ms"})

	if got := rowStatus(a, "live"); got != "running" {
		t.Errorf("the live call's row = %q, want the idless finish to leave it running", got)
	}
	if got := doneTexts(a); len(got) != 1 || got[0] != "replayed" {
		t.Errorf("result boxes = %q, want just the replayed output", got)
	}
}

// The status row's "● bash" indicator is a depth, not a flag: the first of two
// concurrent calls to finish must not clear it while the other is in flight.
func TestActiveCommandIsACounter(t *testing.T) {
	a, _ := newTestApp(t, 80, 24)
	a.BeginActiveCommand("bash")
	a.BeginActiveCommand("bash")
	a.EndActiveCommand()
	if got := a.hudCommand(); got == "" {
		t.Errorf("hudCommand cleared while a second bash call is still running: %q", got)
	}
	a.EndActiveCommand()
	if got := a.hudCommand(); got != "" {
		t.Errorf("hudCommand = %q, want empty once every call finished", got)
	}
}

func rowStatus(a *App, callID string) string {
	for _, b := range a.Blocks() {
		if b.Kind == KindTool && b.CallID == callID {
			return b.Status
		}
	}
	return "absent"
}

func doneTexts(a *App) []string {
	var out []string
	for _, b := range a.Blocks() {
		if b.Kind == KindToolDone {
			out = append(out, b.Text)
		}
	}
	return out
}
