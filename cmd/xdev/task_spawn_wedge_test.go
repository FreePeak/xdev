package main

import (
	"context"
	"encoding/json"
	"runtime"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/ai"
)

// Drives the REAL foreground spawn path end to end: a TaskTool wired the way
// print.go registers it (own Hub, OnEvent through the live taskChildSink)
// executing against a REAL tui.App — only the provider is stubbed. The bare
// path is pinned without a TUI in internal/agent (hub_foreground_test.go);
// this is the host-integrated twin. If any step blocks — spawn, child loop,
// sink fan-out into App methods, hub settle + notify — the watchdog fires and
// dumps every goroutine so the guilty stack is on record.
func TestForegroundTaskSpawnDoesNotWedge(t *testing.T) {
	app := newSinkApp(t)
	// The parent loop registers the call row before executing the tool;
	// the sink binds child events to the newest running row.
	app.AddToolBlock("c1", agent.TaskToolName, `{"prompt":"probe","name":"scout"}`)

	hub := agent.NewHub()
	hub.SetNotify(app.DockBump)
	sink := &taskChildSink{app: app}

	// Child script: one assistant turn that yields immediately (no
	// child-tool round trip needed to exercise the spawn seam).
	events := []ai.Event{
		{Type: ai.EventStart, Provider: "stub", API: "stub", Model: "m"},
		{Type: ai.EventToolcallStart, ToolCallID: "y1", ToolName: "yield", StreamIndex: 0},
		{Type: ai.EventToolcallDelta, StreamIndex: 0, PartialJSON: `{"result":"probe done"}`},
		{Type: ai.EventToolcallEnd, StreamIndex: 0, PartialJSON: `{"result":"probe done"}`},
		ai.Donef(ai.StopReasonStop, nil, &ai.Message{
			Role: ai.RoleAssistant,
			Content: []ai.Block{
				ai.ToolCallBlock{ID: "y1", Name: "yield", Arguments: json.RawMessage(`{"result":"probe done"}`), StreamIndex: 0},
			},
			StopReason: ai.StopReasonStop,
		}),
	}

	tt := &agent.TaskTool{
		Hub:      hub,
		Provider: &stubProvider{scripts: [][]ai.Event{events}},
		Model:    "m",
		// Children live in their own subtree; never the real one.
		DataDir: t.TempDir(),
		System:  agent.SubagentSystemPromptBase,
		OnEvent: sink.onEvent,
	}

	type outcome struct {
		text string
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := tt.Execute(context.Background(), json.RawMessage(`{"prompt":"probe","name":"scout"}`))
		if err != nil {
			done <- outcome{err: err}
			return
		}
		done <- outcome{text: res.Text}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("foreground spawn failed: %v", got.err)
		}
		if got.text == "" {
			t.Fatal("yield text empty")
		}
	case <-time.After(20 * time.Second):
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		t.Fatalf("foreground task spawn wedged; all goroutines:\n%s", buf)
	}
}
