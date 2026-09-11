package agent

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

// doneEvents streams an assistant text and stops — no tool calls.
func doneEvents(text string) []ai.Event {
	return []ai.Event{
		{Type: ai.EventStart, Provider: "fake", API: "fake-api", Model: "m"},
		{Type: ai.EventTextDelta, Delta: text},
		ai.Donef(ai.StopReasonStop, nil, &ai.Message{Role: ai.RoleAssistant,
			Content: []ai.Block{ai.TextBlock{Text: text}}, StopReason: ai.StopReasonStop}),
	}
}

func TestHubBackgroundJobLifecycle(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: yieldEvents(`{"result":"bg-done"}`)},
	}}
	h := NewHub()
	id, err := h.Start(context.Background(), SubagentSpec{
		Name: "bg", Prompt: "work", Provider: p, Model: "m",
		Tools: []tool.Tool{}, MaxTurns: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "hub-1" {
		t.Fatalf("id = %q", id)
	}
	settled := h.Wait(context.Background(), []string{id}, 10*time.Second)
	if len(settled) != 1 {
		t.Fatalf("job did not settle: %v", h.Jobs())
	}
	res, ok := h.Result(id)
	if !ok || res == nil {
		t.Fatal("result missing after settle")
	}
	if !strings.Contains(res.Text, "bg-done") {
		t.Fatalf("result text = %q", res.Text)
	}
	if info, _ := h.Status(id); info.Status != "yielded" {
		t.Fatalf("status = %q, want yielded", info.Status)
	}
}

// Send steers a still-running child: a blocking tool holds the child
// mid-run; the steer must land in its queue.
type blockingTool struct {
	release chan struct{}
	once    sync.Once
}

func (b *blockingTool) unblock() { b.once.Do(func() { close(b.release) }) }

func (b *blockingTool) Name() string { return "block" }
func (b *blockingTool) Description() string {
	return "blocks until released (hub test fixture)"
}
func (b *blockingTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (b *blockingTool) Execute(_ context.Context, _ json.RawMessage) (tool.Result, error) {
	<-b.release
	return tool.Result{Text: "released"}, nil
}

func TestHubSendSteersRunningChild(t *testing.T) {
	blocker := &blockingTool{release: make(chan struct{})}
	defer blocker.unblock() // unstick the child if the test fails early
	p := &fakeProvider{calls: []fakeScript{
		{events: toolCallEvents("block", `{}`)},
		{events: yieldEvents(`{"result":"steered-done"}`)},
	}}
	h := NewHub()
	var child *Agent
	id, err := h.Start(context.Background(), SubagentSpec{
		Name: "s", Prompt: "go", Provider: p, Model: "m",
		Tools: []tool.Tool{blocker}, MaxTurns: 3,
		OnRun: func(a *Agent) { child = a },
	})
	if err != nil {
		t.Fatal(err)
	}
	// Wait for the child's run to start (OnRun fires before turn 1).
	deadline := time.Now().Add(5 * time.Second)
	for child == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if child == nil {
		t.Fatal("child agent never started")
	}
	if err := h.Send(id, "focus on the database part"); err != nil {
		t.Fatalf("send: %v", err)
	}
	// The child's steering queue now holds the message (drained at the
	// next step boundary, which is blocked behind the tool).
	child.steerMu.Lock()
	n := len(child.steering)
	child.steerMu.Unlock()
	if n != 1 {
		t.Fatalf("child steering queue = %d, want 1", n)
	}
	blocker.unblock()
	if len(h.Wait(context.Background(), []string{id}, 10*time.Second)) == 0 {
		t.Fatal("job did not settle")
	}
}

func TestHubCancelRunningJob(t *testing.T) {
	// The child would block forever on an exhausted script; cancel wins.
	p := &fakeProvider{calls: []fakeScript{
		{err: &ai.HTTPError{API: "a", Status: 500, Body: "boom"}},
	}}
	h := NewHub()
	id, _ := h.Start(context.Background(), SubagentSpec{
		Name: "c", Prompt: "go", Provider: p, Model: "m", MaxTurns: 30,
	})
	if !h.Cancel(id) {
		t.Fatal("cancel returned false for a running job")
	}
	if len(h.Wait(context.Background(), []string{id}, 10*time.Second)) == 0 {
		t.Fatal("canceled job never settled")
	}
	if h.Cancel(id) {
		t.Fatal("cancel after settle must be a no-op")
	}
}

func TestHubUnknownJobAndSendAfterFinish(t *testing.T) {
	h := NewHub()
	if err := h.Send("hub-99", "x"); err == nil {
		t.Fatal("send to unknown job must error")
	}
	p := &fakeProvider{calls: []fakeScript{{events: doneEvents("ok")}}}
	id, _ := h.Start(context.Background(), SubagentSpec{
		Name: "f", Prompt: "go", Provider: p, Model: "m", MaxTurns: 2,
	})
	h.Wait(context.Background(), []string{id}, 10*time.Second)
	if err := h.Send(id, "too late"); err == nil {
		t.Fatal("send to finished job must error")
	}
}

// TaskTool background mode registers a hub job and returns its id.
func TestTaskToolBackgroundDispatch(t *testing.T) {
	h := NewHub()
	tt := &TaskTool{
		Provider:   &fakeProvider{calls: []fakeScript{{events: yieldEvents(`{"result":"async"}`)}}},
		Model:      "m",
		ChildTools: []tool.Tool{},
		Hub:        h,
		MaxTurns:   2,
	}
	res, err := tt.Execute(context.Background(), json.RawMessage(`{"prompt":"x","background":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("background dispatch errored: %q", res.Text)
	}
	if !strings.Contains(res.Text, "hub-1") {
		t.Fatalf("result text missing job id: %q", res.Text)
	}
	// The job settles and its result is readable through the hub.
	if len(h.Wait(context.Background(), nil, 10*time.Second)) == 0 {
		t.Fatal("background job never settled")
	}
	res2, ok := h.Result("hub-1")
	if !ok || !strings.Contains(res2.Text, "async") {
		t.Fatalf("hub result = %v ok=%v", res2, ok)
	}
	// Background without a hub falls back to an error result.
	tt2 := &TaskTool{Provider: &fakeProvider{}, Model: "m", MaxTurns: 1}
	res3, _ := tt2.Execute(context.Background(), json.RawMessage(`{"prompt":"x","background":true}`))
	if !res3.IsError || !strings.Contains(res3.Text, "no hub") {
		t.Fatalf("background without hub: %q", res3.Text)
	}
}

// HubTool ops walk the JSON surface end to end.
func TestHubToolOps(t *testing.T) {
	h := NewHub()
	ht := &HubTool{Hub: h}
	// jobs: empty
	res, _ := ht.Execute(context.Background(), json.RawMessage(`{"op":"jobs"}`))
	if !strings.Contains(res.Text, "no background jobs") {
		t.Fatalf("empty jobs: %q", res.Text)
	}
	// start one job via the task tool, then drive it through hub ops.
	tt := &TaskTool{
		Provider: &fakeProvider{calls: []fakeScript{{events: yieldEvents(`{"result":"via-ops"}`)}}},
		Model:    "m", ChildTools: []tool.Tool{}, Hub: h, MaxTurns: 2,
	}
	if _, err := tt.Execute(context.Background(), json.RawMessage(`{"prompt":"x","name":"ops","background":true}`)); err != nil {
		t.Fatal(err)
	}
	res, _ = ht.Execute(context.Background(), json.RawMessage(`{"op":"jobs"}`))
	if !strings.Contains(res.Text, "hub-1") {
		t.Fatalf("jobs listing: %q", res.Text)
	}
	res, _ = ht.Execute(context.Background(), json.RawMessage(`{"op":"wait","ids":["hub-1"],"timeout":10}`))
	if !strings.Contains(res.Text, "hub-1") {
		t.Fatalf("wait: %q", res.Text)
	}
	res, _ = ht.Execute(context.Background(), json.RawMessage(`{"op":"result","id":"hub-1"}`))
	if !strings.Contains(res.Text, "via-ops") {
		t.Fatalf("result op: %q", res.Text)
	}
	res, _ = ht.Execute(context.Background(), json.RawMessage(`{"op":"bogus"}`))
	if !res.IsError {
		t.Fatal("unknown op must error")
	}
}
