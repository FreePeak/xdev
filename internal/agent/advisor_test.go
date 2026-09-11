package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// adviseEvents scripts a reviewer turn that calls advise then stops.
func adviseEvents(sev, text string) []ai.Event {
	args := `{"severity":"` + sev + `","text":"` + text + `"}`
	return []ai.Event{
		{Type: ai.EventStart},
		{Type: ai.EventToolcallStart, ToolCallID: "a1", ToolName: AdviseToolName, StreamIndex: 0},
		{Type: ai.EventToolcallDelta, StreamIndex: 0, PartialJSON: args},
		{Type: ai.EventToolcallEnd, StreamIndex: 0, PartialJSON: args},
		ai.Donef(ai.StopReasonStop, nil, &ai.Message{
			Role: ai.RoleAssistant,
			Content: []ai.Block{
				ai.ToolCallBlock{ID: "a1", Name: AdviseToolName, Arguments: json.RawMessage(args)},
			},
			StopReason: ai.StopReasonStop,
		}),
	}
}

func newAdvisorFixture(t *testing.T, scripts []fakeScript) (*Advisor, *Agent, *[]string) {
	t.Helper()
	reg := tool.NewRegistry()
	reg.Register(tool.NewReadTool())
	RegisterAdviseTool(reg)
	rev := &fakeProvider{calls: scripts}
	var steered []string
	primary := &Agent{
		Provider: &fakeProvider{}, Model: "m",
		Tools: tool.NewRegistry(),
	}
	primary.steerMu.Lock()
	primary.steering = nil
	// capture steering by wrapping Run? No: steer() appends to the queue —
	// read the queue after Feed instead.
	primary.steerMu.Unlock()
	_ = steered
	adv := NewAdvisor(rev, "m", reg)
	adv.Primary = primary
	return adv, primary, &steered
}

func TestAdvisorRoutesAdviceBySeverity(t *testing.T) {
	adv, primary, _ := newAdvisorFixture(t, []fakeScript{
		{events: adviseEvents(AdviseBlocker, "rm -rf would delete the repo")},
		{events: doneEvents("reviewed")},
	})
	hist := []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "clean up"}}},
		{Role: ai.RoleAssistant, Content: []ai.Block{ai.ToolCallBlock{ID: "t1", Name: "bash", Arguments: json.RawMessage(`{"command":"rm -rf /tmp/x"}`)}}},
	}
	adv.Feed(context.Background(), hist)
	primary.steerMu.Lock()
	n := len(primary.steering)
	primary.steerMu.Unlock()
	if n != 1 {
		t.Fatalf("steering queue = %d, want the blocker", n)
	}
	primary.steerMu.Lock()
	txt := primary.steering[0].Text
	primary.steerMu.Unlock()
	if !strings.Contains(txt, "blocker") || !strings.Contains(txt, "rm -rf") {
		t.Fatalf("steer text = %q", txt)
	}
}

func TestAdvisorNothingNewSkipsFeed(t *testing.T) {
	rev := &fakeProvider{}
	reg := tool.NewRegistry()
	RegisterAdviseTool(reg)
	adv := NewAdvisor(rev, "m", reg)
	adv.Primary = &Agent{}
	hist := []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}}
	adv.Feed(context.Background(), hist) // sets cursor
	before := rev.i
	adv.Feed(context.Background(), hist) // nothing new
	if rev.i != before {
		t.Fatal("feed ran with no delta")
	}
}

func TestAdvisorGuardFiltersNoiseAndDupes(t *testing.T) {
	g := newEmissionGuard()
	if !g.allow(AdviseConcern, "Check the API key") {
		t.Fatal("first note must pass")
	}
	if g.allow(AdviseConcern, "check the API key") {
		t.Fatal("normalized dupe must be filtered")
	}
	if g.allow(AdviseConcern, "lgtm") {
		t.Fatal("content-free phrase must be filtered")
	}
	if !g.allow(AdviseBlocker, "lgtm") {
		t.Fatal("blockers bypass the phrase filter")
	}
	if g.allow(AdviseNit, "") {
		t.Fatal("empty text must be filtered")
	}
}

func TestAdvisorOneNotePerReview(t *testing.T) {
	reg := tool.NewRegistry()
	RegisterAdviseTool(reg)
	// Script: the reviewer calls advise TWICE in one turn.
	args1 := `{"severity":"nit","text":"consider caching"}`
	args2 := `{"severity":"nit","text":"also rename the variable"}`
	events := []ai.Event{
		{Type: ai.EventStart},
		{Type: ai.EventToolcallStart, ToolCallID: "a1", ToolName: AdviseToolName, StreamIndex: 0},
		{Type: ai.EventToolcallDelta, StreamIndex: 0, PartialJSON: args1},
		{Type: ai.EventToolcallEnd, StreamIndex: 0, PartialJSON: args1},
		{Type: ai.EventToolcallStart, ToolCallID: "a2", ToolName: AdviseToolName, StreamIndex: 1},
		{Type: ai.EventToolcallDelta, StreamIndex: 1, PartialJSON: args2},
		{Type: ai.EventToolcallEnd, StreamIndex: 1, PartialJSON: args2},
		ai.Donef(ai.StopReasonStop, nil, &ai.Message{
			Role: ai.RoleAssistant,
			Content: []ai.Block{
				ai.ToolCallBlock{ID: "a1", Name: AdviseToolName, Arguments: json.RawMessage(args1)},
				ai.ToolCallBlock{ID: "a2", Name: AdviseToolName, Arguments: json.RawMessage(args2)},
			},
			StopReason: ai.StopReasonStop,
		}),
	}
	rev := &fakeProvider{calls: []fakeScript{
		{events: events},
		{events: doneEvents("reviewed")},
	}}
	adv := NewAdvisor(rev, "m", reg)
	primary := &Agent{}
	adv.Primary = primary
	adv.Feed(context.Background(), []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "work"}}},
	})
	primary.steerMu.Lock()
	n := len(primary.steering)
	primary.steerMu.Unlock()
	if n != 1 {
		t.Fatalf("deliveries = %d, want exactly one note per review", n)
	}
}

func TestAdvisorHaltsAfterThreeFailures(t *testing.T) {
	reg := tool.NewRegistry()
	RegisterAdviseTool(reg)
	rev := &fakeProvider{calls: []fakeScript{
		{err: &ai.HTTPError{API: "a", Status: 500, Body: "boom"}},
		{err: &ai.HTTPError{API: "a", Status: 500, Body: "boom"}},
		{err: &ai.HTTPError{API: "a", Status: 500, Body: "boom"}},
	}}
	adv := NewAdvisor(rev, "m", reg)
	adv.Primary = &Agent{}
	hist := []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "x"}}}}
	adv.Feed(context.Background(), hist)
	adv.Feed(context.Background(), append(hist, hist[0]))
	if adv.Halted() {
		t.Fatal("halted before 3 failures")
	}
	adv.Feed(context.Background(), append(hist, hist[0], hist[0]))
	if !adv.Halted() {
		t.Fatal("3 consecutive failures must halt the advisor")
	}
	// A fourth feed does nothing (no provider call).
	before := rev.i
	adv.Feed(context.Background(), append(hist, hist[0], hist[0], hist[0]))
	if rev.i != before {
		t.Fatal("halted advisor still consumed provider calls")
	}
}

func TestAdvisorResetClearsState(t *testing.T) {
	reg := tool.NewRegistry()
	RegisterAdviseTool(reg)
	rev := &fakeProvider{calls: []fakeScript{
		{events: adviseEvents(AdviseConcern, "watch the lock ordering")},
		{events: doneEvents("reviewed")},
		{events: adviseEvents(AdviseConcern, "watch the lock ordering")},
		{events: doneEvents("reviewed")},
	}}
	adv := NewAdvisor(rev, "m", reg)
	primary := &Agent{}
	adv.Primary = primary
	hist := []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "x"}}}}
	adv.Feed(context.Background(), hist)
	// Same note again after a reset must pass the guard (state cleared).
	adv.Reset(hist)
	primary.steerMu.Lock()
	n := len(primary.steering)
	primary.steerMu.Unlock()
	if n != 1 {
		t.Fatalf("notes delivered = %d", n)
	}
	adv.Feed(context.Background(), append(hist, hist[0]))
	primary.steerMu.Lock()
	n2 := len(primary.steering)
	primary.steerMu.Unlock()
	if n2 != 2 {
		t.Fatalf("after reset the same note must deliver again: %d", n2)
	}
}

var _ sync.Locker = (*sync.Mutex)(nil)
