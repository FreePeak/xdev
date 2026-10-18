package opencode

import (
	"encoding/json"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// The adapter maps opencode's v2 message union onto xdev's existing block
// model. It is a rename and a re-wrap on the assistant side and a one-line
// fold on the user side — the point of the whole slice is that no second
// renderer is written (R1).

func blocksOf(t *testing.T, e session.Entry) []ai.Block {
	t.Helper()
	me, ok := e.(*session.MessageEntry)
	if !ok {
		t.Fatalf("entry is %T, want *session.MessageEntry", e)
	}
	return me.Message.Content
}

func TestAdapterUserMessageFoldsToOneTextBlock(t *testing.T) {
	// opencode v2 user messages carry a flat string, not a content list.
	raw := `{"id":"msg_1","type":"user","time":{"created":1000},"text":"do the thing"}`
	got, err := adaptMessage(raw)
	if err != nil {
		t.Fatalf("adaptMessage: %v", err)
	}
	if got.Role != ai.RoleUser {
		t.Fatalf("role = %q, want user", got.Role)
	}
	bs := blocksOf(t, &session.MessageEntry{Message: *got})
	if len(bs) != 1 {
		t.Fatalf("blocks = %d, want 1", len(bs))
	}
	tb, ok := bs[0].(ai.TextBlock)
	if !ok {
		t.Fatalf("block is %T, want ai.TextBlock", bs[0])
	}
	if tb.Text != "do the thing" {
		t.Errorf("text = %q", tb.Text)
	}
}

func TestAdapterAssistantTextIsAStraightCarry(t *testing.T) {
	raw := `{"id":"msg_2","type":"assistant","time":{"created":2000},
	  "agent":"build","model":{"id":"m","providerID":"opencode"},
	  "content":[{"type":"text","text":"hello"}]}`
	got, err := adaptMessage(raw)
	if err != nil {
		t.Fatalf("adaptMessage: %v", err)
	}
	if got.Role != ai.RoleAssistant {
		t.Fatalf("role = %q", got.Role)
	}
	bs := blocksOf(t, &session.MessageEntry{Message: *got})
	if len(bs) != 1 {
		t.Fatalf("blocks = %d, want 1", len(bs))
	}
	if tb := bs[0].(ai.TextBlock); tb.Text != "hello" {
		t.Errorf("text = %q", tb.Text)
	}
}

// opencode's reasoning item carries its payload under "text"; xdev's
// ThinkingBlock calls the same field "thinking". One rename.
func TestAdapterReasoningRenamesTextToThinking(t *testing.T) {
	raw := `{"id":"msg_3","type":"assistant","time":{"created":3000},
	  "content":[{"type":"reasoning","text":"let me think"}]}`
	got, err := adaptMessage(raw)
	if err != nil {
		t.Fatalf("adaptMessage: %v", err)
	}
	bs := blocksOf(t, &session.MessageEntry{Message: *got})
	tb, ok := bs[0].(ai.ThinkingBlock)
	if !ok {
		t.Fatalf("block is %T, want ai.ThinkingBlock", bs[0])
	}
	if tb.Thinking != "let me think" {
		t.Errorf("thinking = %q", tb.Thinking)
	}
}

// The streaming case: input is a partial JSON *string*, not an object. It must
// land in PartialArgs, never in Arguments — a half-parsed object there would
// be sent back to a model as if it were complete.
func TestAdapterToolStreamingInputGoesToPartialArgs(t *testing.T) {
	raw := `{"id":"msg_4","type":"assistant","time":{"created":4000},
	  "content":[{"type":"tool","id":"call_1","name":"read",
	    "state":{"status":"streaming","input":"{\"path\":\"/tm"}}]}`
	got, err := adaptMessage(raw)
	if err != nil {
		t.Fatalf("adaptMessage: %v", err)
	}
	tb, ok := blocksOf(t, &session.MessageEntry{Message: *got})[0].(ai.ToolCallBlock)
	if !ok {
		t.Fatal("first block is not a ToolCallBlock")
	}
	if tb.ID != "call_1" || tb.Name != "read" {
		t.Errorf("id/name = %q/%q", tb.ID, tb.Name)
	}
	if tb.PartialArgs != `{"path":"/tm` {
		t.Errorf("partialArgs = %q", tb.PartialArgs)
	}
	if len(tb.Arguments) != 0 {
		t.Errorf("streaming input must not populate Arguments, got %s", tb.Arguments)
	}
}

func TestAdapterToolCompletedInputBecomesArguments(t *testing.T) {
	raw := `{"id":"msg_5","type":"assistant","time":{"created":5000},
	  "content":[{"type":"tool","id":"call_2","name":"bash",
	    "state":{"status":"completed","input":{"command":"ls"},
	    "content":[{"type":"text","text":"a.txt"}]}}]}`
	got, err := adaptMessage(raw)
	if err != nil {
		t.Fatalf("adaptMessage: %v", err)
	}
	tb := blocksOf(t, &session.MessageEntry{Message: *got})[0].(ai.ToolCallBlock)
	var args map[string]any
	if err := json.Unmarshal(tb.Arguments, &args); err != nil {
		t.Fatalf("arguments is not an object: %s", tb.Arguments)
	}
	if args["command"] != "ls" {
		t.Errorf("arguments = %s", tb.Arguments)
	}
	if tb.PartialArgs != "" {
		t.Errorf("completed input must not set partialArgs, got %q", tb.PartialArgs)
	}
}

func TestAdapterToolResultBecomesAToolResultMessage(t *testing.T) {
	raw := `{"id":"msg_6","type":"assistant","time":{"created":6000},
	  "content":[{"type":"tool","id":"call_3","name":"read",
	    "state":{"status":"completed","input":{"path":"/tmp"},
	    "content":[{"type":"text","text":"file body"}]}}]}`
	entries, err := AdaptMessage(raw)
	if err != nil {
		t.Fatalf("AdaptMessage: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 (the call, then its result)", len(entries))
	}
	me := entries[1].(*session.MessageEntry)
	if me.Message.Role != ai.RoleToolResult {
		t.Fatalf("second entry role = %q, want toolResult", me.Message.Role)
	}
	tb := me.Message.Content[0].(ai.TextBlock)
	if tb.Text != "file body" {
		t.Errorf("result text = %q", tb.Text)
	}
}

// F3: an unknown part type must survive as a CustomEntry carrying the raw
// JSON. Dropping it loses information; rendering it as text would fabricate
// content opencode never sent.
func TestAdapterUnknownContentBecomesCustomEntry(t *testing.T) {
	raw := `{"id":"msg_7","type":"assistant","time":{"created":7000},
	  "content":[{"type":"hologram","payload":{"a":1}}]}`
	entries, err := AdaptMessage(raw)
	if err != nil {
		t.Fatalf("AdaptMessage: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	ce, ok := entries[0].(*session.CustomEntry)
	if !ok {
		t.Fatalf("entry is %T, want *session.CustomEntry", entries[0])
	}
	if ce.CustomType != "opencode_part" {
		t.Errorf("customType = %q", ce.CustomType)
	}
	if _, ok := ce.Data["raw"]; !ok {
		t.Errorf("custom entry dropped the raw payload: %+v", ce.Data)
	}
}

// An unknown message type is the same problem one level up, and it is the
// likely one: opencode adds message types over time.
func TestAdapterUnknownMessageTypeBecomesCustomEntry(t *testing.T) {
	raw := `{"id":"msg_8","type":"telepathy","time":{"created":8000},"freq":42}`
	entries, err := AdaptMessage(raw)
	if err != nil {
		t.Fatalf("AdaptMessage: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if _, ok := entries[0].(*session.CustomEntry); !ok {
		t.Fatalf("entry is %T, want *session.CustomEntry", entries[0])
	}
}

// Compaction maps onto xdev's own compaction entry rather than inventing one.
func TestAdapterCompactionBecomesCompactionEntry(t *testing.T) {
	raw := `{"id":"msg_9","type":"compaction","time":{"created":9000},
	  "state":{"status":"completed"}}`
	entries, err := AdaptMessage(raw)
	if err != nil {
		t.Fatalf("AdaptMessage: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if _, ok := entries[0].(*session.CompactionEntry); !ok {
		t.Fatalf("entry is %T, want *session.CompactionEntry", entries[0])
	}
}

func TestAdapterRejectsMalformedMessage(t *testing.T) {
	if _, err := AdaptMessage("{not json"); err == nil {
		t.Fatal("want an error for malformed JSON")
	}
}

// AdaptMessages folds a message list into entries, preserving order and
// skipping nothing without saying so.
func TestAdaptMessagesPreservesOrder(t *testing.T) {
	msgs := []string{
		`{"id":"m1","type":"user","time":{"created":1},"text":"one"}`,
		`{"id":"m2","type":"assistant","time":{"created":2},"content":[{"type":"text","text":"two"}]}`,
	}
	entries, err := AdaptMessages(msgs)
	if err != nil {
		t.Fatalf("AdaptMessages: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	first := entries[0].(*session.MessageEntry).Message
	second := entries[1].(*session.MessageEntry).Message
	if first.Content[0].(ai.TextBlock).Text != "one" {
		t.Errorf("entry 0 = %+v", first.Content)
	}
	if second.Content[0].(ai.TextBlock).Text != "two" {
		t.Errorf("entry 1 = %+v", second.Content)
	}
}
