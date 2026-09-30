package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/theme"
	"github.com/FreePeak/xdev/internal/tool"
	"github.com/FreePeak/xdev/internal/tui"
)

// The host's half of live tool output, end to end: a real `bash` call through
// the real registry, wired exactly as runTUI wires it, and the transcript
// asked to paint while the command is still running. The tool's half is pinned
// in internal/tool, the transcript's in internal/tui; this is the wire between
// them, which no test on either side covers.
func TestWiredTUIHostPaintsARunningBashCall(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(scr.Fini)
	scr.SetSize(80, 24)
	app := tui.New(scr, theme.Load("groknight"), "test/free", "sess")
	// An in-memory store so the hooks' OnMessageEnd persists (a nil store
	// panics) — this test is about the output wire, and the store is not it.
	hooks := &tuiHooks{ts: &tuiSession{app: app, store: session.OpenMem(t.TempDir(), "live")}}

	reg := tool.NewRegistry()
	reg.Register(tool.NewBashTool(t.TempDir()))
	ag := &agent.Agent{
		Provider: &stubProvider{scripts: [][]ai.Event{{
			{Type: ai.EventStart, Provider: "stub", Model: "m"},
			{Type: ai.EventToolcallStart, ToolCallID: "call-1", ToolName: "bash", StreamIndex: 0},
			{Type: ai.EventToolcallEnd, StreamIndex: 0,
				PartialJSON: `{"command":"printf live-first\\n; sleep 0.5; printf live-second\\n"}`},
			ai.Donef(ai.StopReasonStop, nil, &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop}),
		}}},
		Tools: reg, Model: "m", Hooks: hooks,
		// The TUI's live-output wire, verbatim from runTUI.
		OnOutput: app.AppendToolOutput,
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go ag.Run(ctx, "", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "go"}}}})

	// The command sleeps 0.5s after its first line, so seeing the first line
	// can only have happened while the call was still running.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, b := range app.Blocks() {
			if b.Live && strings.Contains(b.Text, "live-first") {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("a running bash call never painted its output")
}
