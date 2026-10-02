package agent

// The steering-delivery announcement (#157). The loop's steering drain IS the
// delivery point for a message a host queued mid-turn — the TUI's pending-row
// list retires its rows from here — so the callback's contract has to be
// exact: called once per delivery, in delivery order, and never for a message
// the run did not inject.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// deliveredBatch records the texts a run announced, in order.
type deliveredBatch struct {
	mu      sync.Mutex
	batches [][]string
}

func (d *deliveredBatch) record(texts []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.batches = append(d.batches, append([]string(nil), texts...))
}

func (d *deliveredBatch) flat() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, b := range d.batches {
		out = append(out, b...)
	}
	return out
}

// announceAgent builds an agent whose run ends in two turns — the first calls
// the echo tool, the second answers in text. That is the shape a mid-turn
// prompt is steered into: a run still working, with a step boundary to land on.
func announceAgent(p *fakeProvider) *Agent {
	reg := tool.NewRegistry()
	reg.Register(echoTool{})
	return &Agent{Provider: p, Tools: reg}
}

// toolCallScript is the two-turn script: a tool call, then prose. Built from
// the Done event's message rather than toolcall deltas, which is the shape
// loop_test.go's own tool-call tests use.
func toolCallScript() *fakeProvider {
	return &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{echoToolCallEvent()}},
		{events: []ai.Event{textEvent("done"), doneEvent("done")}},
	}}
}

// echoToolCallEvent is a completed assistant turn carrying one echo call.
func echoToolCallEvent() ai.Event {
	return ai.Donef(ai.StopReasonStop, nil, &ai.Message{
		Role:    ai.RoleAssistant,
		Content: []ai.Block{ai.ToolCallBlock{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{"text":"hi"}`)}},
	})
}

// sawUserMessage reports whether any request the provider received carried
// this text as a user message. An announcement without the injection behind it
// would be the worst kind of lie, so every delivery test checks both.
func sawUserMessage(p *fakeProvider, text string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, req := range p.gotReqs {
		for _, m := range req.Messages {
			if m.Role == ai.RoleUser && m.Text() == text {
				return true
			}
		}
	}
	return false
}

// TestSteeringDeliveredFiresAtTheStepBoundary: a message steered into a run
// that is still calling tools is injected at the next turn, and the host hears
// about it there. Before this callback existed the TUI could only know a queued
// prompt reached a channel, not that it had become part of the conversation.
func TestSteeringDeliveredFiresAtTheStepBoundary(t *testing.T) {
	p := toolCallScript()
	var got deliveredBatch
	a := announceAgent(p)
	a.SteeringDelivered = got.record
	a.Steer("actually, use make")

	if _, err := a.Run(context.Background(), "sys", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if flat := got.flat(); len(flat) != 1 || flat[0] != "actually, use make" {
		t.Fatalf("announced deliveries = %v, want the steered message once", flat)
	}
	if !sawUserMessage(p, "actually, use make") {
		t.Fatal("the steered message was announced but never reached the provider request")
	}
}

// TestSteeringDeliveredKeepsQueueOrder: two messages steered in a row are
// announced oldest-first, matching the order they were injected. A host that
// retires rows by announcement order must see the same order the conversation
// did, or a row disappears for the wrong message.
func TestSteeringDeliveredKeepsQueueOrder(t *testing.T) {
	p := toolCallScript()
	var got deliveredBatch
	a := announceAgent(p)
	a.SteeringDelivered = got.record
	a.Steer("first correction")
	a.Steer("second correction")

	if _, err := a.Run(context.Background(), "sys", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	flat := got.flat()
	if len(flat) != 2 || flat[0] != "first correction" || flat[1] != "second correction" {
		t.Fatalf("announced %v, want [first correction second correction]", flat)
	}
}

// TestSteeringDeliveredIsSilentWithoutSteering: an ordinary run with nothing
// queued announces nothing. A callback that fired with an empty batch would
// make a host retire rows for messages the model never saw.
func TestSteeringDeliveredIsSilentWithoutSteering(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{textEvent("hello"), doneEvent("hello")}},
	}}
	var got deliveredBatch
	a := announceAgent(p)
	a.SteeringDelivered = got.record

	if _, err := a.Run(context.Background(), "sys", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := len(got.batches); n != 0 {
		t.Fatalf("announced %d batches for a run with nothing queued", n)
	}
}

// TestSteeringDeliveredSurvivesUnwiredAgents: the callback is optional. An
// unwired agent must not panic at the drain point — the same discipline the
// rest of the loop follows with Hooks.
func TestSteeringDeliveredSurvivesUnwiredAgents(t *testing.T) {
	p := toolCallScript()
	a := announceAgent(p) // SteeringDelivered left nil
	a.Steer("no host listening")

	if _, err := a.Run(context.Background(), "sys", nil); err != nil {
		t.Fatalf("Run with no SteeringDelivered: %v", err)
	}
	if !sawUserMessage(p, "no host listening") {
		t.Fatal("the message was dropped instead of injected")
	}
}

// TestSteeringDeliveredAnnouncesWhatTheRunInjected: a message that arrives
// after the model's LAST tool call continues the same run through the second
// drain point, and is announced from there too. Without this, a prompt typed
// during a run's final turn would stay "pending" forever even though the
// conversation had it.
func TestSteeringDeliveredAnnouncesWhatTheRunInjected(t *testing.T) {
	// One turn only, so the post-final-tool-call drain is the only boundary.
	p := toolCallScript()
	var got deliveredBatch
	a := announceAgent(p)
	a.SteeringDelivered = got.record
	// Steer from the tool-end hook: the run has finished its tool calls, so
	// this lands on the second drain rather than a step boundary.
	steerAt := true
	a.Hooks = TurnHooksFunc{OnToolEndF: func(ai.ToolCallBlock, tool.Result, time.Duration) {
		if steerAt {
			steerAt = false
			a.Steer("late correction")
		}
	}}

	if _, err := a.Run(context.Background(), "sys", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	flat := got.flat()
	if len(flat) != 1 || flat[0] != "late correction" {
		t.Fatalf("announced %v, want the late steer once", flat)
	}
	if !sawUserMessage(p, "late correction") {
		t.Fatalf("the late steer never reached the provider:\n%s", strings.Join(got.flat(), ","))
	}
}
