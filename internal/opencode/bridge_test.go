package opencode

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// A Bridge is one opencode session bound to one xdev session.Store.
// tabset.open() takes a *session.Store, so a Bridge's store IS a tab: there is
// no new tab type and no change to tabset.go.

func TestBridgeStoreIsUsableAsATab(t *testing.T) {
	b := newBridge(New(), SessionInfo{
		ID: "ses_a", Title: "one", Location: Location{Directory: "/tmp"},
	})
	if b.Store == nil {
		t.Fatal("bridge has no store")
	}
	if got := b.Store.ID(); got == "" {
		t.Error("store has no id; tabset keys tabs by it")
	}
	if got := b.Store.Title(); got != "one" {
		t.Errorf("store title = %q, want %q", got, "one")
	}
	if got := b.Store.CWD(); got != "/tmp" {
		t.Errorf("store cwd = %q, want /tmp", got)
	}
	t.Cleanup(func() { b.Store.Close() })
}

// A reasoning delta becomes a ThinkingBlock. opencode sends `text` on the
// delta; xdev's block calls it `thinking`.
func TestBridgeReasoningDeltaAppendsThinking(t *testing.T) {
	b := testBridge(t)
	b.Handle(eventOf(t, "session.reasoning.delta", map[string]any{
		"sessionID": "ses_a", "assistantMessageID": "msg_1", "ordinal": 0,
		"delta": "let me think",
	}))
	ents := b.Store.Entries()
	last := messageOf(t, ents[len(ents)-1])
	tb, ok := last.Content[0].(ai.ThinkingBlock)
	if !ok {
		t.Fatalf("block is %T, want ai.ThinkingBlock", last.Content[0])
	}
	if tb.Thinking != "let me think" {
		t.Errorf("thinking = %q", tb.Thinking)
	}
}

func TestBridgeTextDeltaAppendsText(t *testing.T) {
	b := testBridge(t)
	b.Handle(eventOf(t, "session.text.delta", map[string]any{
		"sessionID": "ses_a", "assistantMessageID": "msg_1", "delta": "hello ",
	}))
	ents := b.Store.Entries()
	last := messageOf(t, ents[len(ents)-1])
	if tb := last.Content[0].(ai.TextBlock); tb.Text != "hello " {
		t.Errorf("text = %q", tb.Text)
	}
}

// The store is append-only, so a streaming turn arrives as a sequence of
// blocks rather than one rewritten entry. Chunks must stay in order.
func TestBridgePreservesDeltaOrder(t *testing.T) {
	b := testBridge(t)
	for _, chunk := range []string{"one ", "two ", "three"} {
		b.Handle(eventOf(t, "session.text.delta", map[string]any{
			"sessionID": "ses_a", "assistantMessageID": "msg_1", "delta": chunk,
		}))
	}
	var got strings.Builder
	for _, e := range b.Store.Entries() {
		m := messageOf(t, e)
		for _, blk := range m.Content {
			if tb, ok := blk.(ai.TextBlock); ok {
				got.WriteString(tb.Text)
			}
		}
	}
	if got.String() != "one two three" {
		t.Errorf("streamed text = %q, want %q", got.String(), "one two three")
	}
}

func TestBridgeToolCalledBecomesAToolCallBlock(t *testing.T) {
	b := testBridge(t)
	b.Handle(eventOf(t, "session.tool.called", map[string]any{
		"sessionID": "ses_a", "assistantMessageID": "msg_1",
		"id": "call_1", "name": "read", "input": map[string]any{"path": "/tmp"},
	}))
	ents := b.Store.Entries()
	tb, ok := messageOf(t, ents[len(ents)-1]).Content[0].(ai.ToolCallBlock)
	if !ok {
		t.Fatalf("block is %T, want ai.ToolCallBlock", messageOf(t, ents[len(ents)-1]).Content[0])
	}
	if tb.ID != "call_1" || tb.Name != "read" {
		t.Errorf("id/name = %q/%q", tb.ID, tb.Name)
	}
	var args map[string]any
	if err := json.Unmarshal(tb.Arguments, &args); err != nil {
		t.Fatalf("arguments: %v", err)
	}
	if args["path"] != "/tmp" {
		t.Errorf("args = %s", tb.Arguments)
	}
}

func TestBridgeToolSuccessBecomesAToolResult(t *testing.T) {
	b := testBridge(t)
	b.Handle(eventOf(t, "session.tool.success", map[string]any{
		"sessionID": "ses_a", "assistantMessageID": "msg_1", "id": "call_1",
		"content": []any{map[string]any{"type": "text", "text": "file body"}},
	}))
	ents := b.Store.Entries()
	m := messageOf(t, ents[len(ents)-1])
	if m.Role != ai.RoleToolResult {
		t.Fatalf("role = %q, want toolResult", m.Role)
	}
	if tb := m.Content[0].(ai.TextBlock); tb.Text != "file body" {
		t.Errorf("result = %q", tb.Text)
	}
}

// usage.updated is the live cost/token roll-up. It updates the roster row's
// numbers without touching the transcript.
func TestBridgeUsageUpdatedRefreshesCounters(t *testing.T) {
	b := testBridge(t)
	b.Handle(eventOf(t, "session.usage.updated", map[string]any{
		"sessionID": "ses_a", "cost": 0.5,
		"tokens": map[string]any{
			"input": 10, "output": 2, "reasoning": 1,
			"cache": map[string]any{"read": 100, "write": 0},
		},
	}))
	if b.Cost() != 0.5 {
		t.Errorf("cost = %v", b.Cost())
	}
	if b.Tokens().Input != 10 || b.Tokens().Cache.Read != 100 {
		t.Errorf("tokens = %+v", b.Tokens())
	}
	// A usage event is not transcript content.
	if n := len(b.Store.Entries()); n != 0 {
		t.Errorf("usage event wrote %d entries, want 0", n)
	}
}

// One stream serves every session; the bridge must ignore what is not its own
// or a parked tab would show another session's text.
func TestBridgeIgnoresOtherSessions(t *testing.T) {
	b := testBridge(t)
	b.Handle(eventOf(t, "session.text.delta", map[string]any{
		"sessionID": "ses_OTHER", "delta": "not mine",
	}))
	if n := len(b.Store.Entries()); n != 0 {
		t.Fatalf("bridge wrote %d entries for another session, want 0", n)
	}
}

func TestBridgeIgnoresUnrelatedEventTypes(t *testing.T) {
	b := testBridge(t)
	b.Handle(eventOf(t, "server.connected", map[string]any{}))
	b.Handle(eventOf(t, "project.updated", map[string]any{}))
	b.Handle(eventOf(t, "shell.exited", map[string]any{"sessionID": "ses_a"}))
	if n := len(b.Store.Entries()); n != 0 {
		t.Fatalf("wrote %d entries for unrelated events, want 0", n)
	}
}

// F2: after a gap, the caller reconciles from the message list rather than
// replaying deltas (which are gone). Reconcile appends what the store missed.
func TestBridgeReconcileFillsTheGap(t *testing.T) {
	b, c := testBridgeWithClient(t)
	c.routes["api session.message.list"] = reply{stdout: `{"data":[
	  {"id":"msg_1","type":"user","time":{"created":1},"text":"do it"},
	  {"id":"msg_2","type":"assistant","time":{"created":2},
	   "content":[{"type":"text","text":"done"}]}]}`}
	c.routes["api session.get"] = reply{stdout: `{"data":{"id":"ses_a","title":"t",
	  "cost":0,"tokens":{"input":1,"output":1,"reasoning":0,"cache":{"read":0,"write":0}},
	  "time":{"created":1},"location":{"directory":"/tmp"}}}`}

	if err := b.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var text strings.Builder
	for _, e := range b.Store.Entries() {
		m := messageOf(t, e)
		for _, blk := range m.Content {
			if tb, ok := blk.(ai.TextBlock); ok {
				text.WriteString(tb.Text)
			}
		}
	}
	if !strings.Contains(text.String(), "do it") || !strings.Contains(text.String(), "done") {
		t.Errorf("reconciled text = %q, want both messages", text.String())
	}
}

func TestBridgePromptGoesThroughTheClient(t *testing.T) {
	b, c := testBridgeWithClient(t)
	c.routes["api session.prompt"] = reply{stdout: `{}`}
	if err := b.Prompt(context.Background(), "hello"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := strings.Join(c.calls[0], " "); !strings.Contains(got, "ses_a") {
		t.Errorf("argv = %q, want the session id", got)
	}
}

// --- helpers ---------------------------------------------------------------

func testBridge(t *testing.T) *Bridge {
	t.Helper()
	b, _ := testBridgeWithClient(t)
	t.Cleanup(func() { b.Store.Close() })
	return b
}

func testBridgeWithClient(t *testing.T) (*Bridge, *fake) {
	t.Helper()
	f := &fake{routes: map[string]reply{}}
	c := &Client{Exec: f.exec}
	b := newBridge(c, SessionInfo{
		ID: "ses_a", Title: "t", Location: Location{Directory: "/tmp"},
	})
	return b, f
}

func eventOf(t *testing.T, typ string, data map[string]any) Event {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return Event{ID: "evt_x", Type: typ, Data: raw}
}

func messageOf(t *testing.T, e session.Entry) ai.Message {
	t.Helper()
	me, ok := e.(*session.MessageEntry)
	if !ok {
		t.Fatalf("entry is %T, want *session.MessageEntry", e)
	}
	return me.Message
}
