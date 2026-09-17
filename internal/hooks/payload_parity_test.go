package hooks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

// Payload-shape parity (docs/parity/agent-system.md T3 #14): a hook ported
// from the baseline reads event.toolName / event.content / event.isError.
// These tests pin what actually reaches the hook's stdin, and — the part a
// rename can silently break — that the stage-2 matcher and the `if`
// prefilter still resolve the tool name from the new key.

func TestToolCallPayloadUsesOmpKeyNames(t *testing.T) {
	dir := t.TempDir()
	sink := filepath.Join(dir, "call.jsonl")
	b := FromSettings(map[string]any{
		"tool_call": "cat >> " + sink,
	})
	args := json.RawMessage(`{"command":"echo hi"}`)
	if _, err := b.ToolCall(context.Background(), hookCall("bash", args)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(sink)
	if err != nil {
		t.Fatalf("hook never ran: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("payload is not one JSON object: %q", raw)
	}
	if got["toolName"] != "bash" {
		t.Fatalf(`payload must carry toolName (omp's key): %s`, raw)
	}
	// The call id lets a hook pair its tool_call and tool_result events.
	if got["toolCallId"] != "tc-bash" {
		t.Fatalf("payload must carry toolCallId: %s", raw)
	}
	if _, present := got["tool"]; present {
		t.Fatalf("the legacy `tool` key must be gone: %s", raw)
	}
	in, ok := got["input"].(map[string]any)
	if !ok || in["command"] != "echo hi" {
		t.Fatalf(`input must carry the call arguments: %s`, raw)
	}
}

func TestToolResultPayloadCarriesContentAndIsError(t *testing.T) {
	dir := t.TempDir()
	sink := filepath.Join(dir, "result.jsonl")
	b := FromSettings(map[string]any{
		"tool_result": "cat >> " + sink,
	})
	rendered := json.RawMessage(`{"text":"file body","isError":true}`)
	// A hook that only observes (no mutation keys) leaves the result as-is.
	if out := b.ToolResult(context.Background(), hookCall("read", json.RawMessage(`{"path":"x"}`)), rendered, true); string(out) != string(rendered) {
		t.Fatalf("an observing hook must leave the result untouched: %s", out)
	}
	raw, err := os.ReadFile(sink)
	if err != nil {
		t.Fatalf("hook never ran: %v", err)
	}
	var got struct {
		ToolName   string `json:"toolName"`
		ToolCallID string `json:"toolCallId"`
		IsError    bool   `json:"isError"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("payload is not the expected object: %q", raw)
	}
	if got.ToolCallID != "tc-read" {
		t.Fatalf("tool_result must carry toolCallId: %s", raw)
	}
	if got.ToolName != "read" || !got.IsError {
		t.Fatalf("toolName/isError wrong: %s", raw)
	}
	if len(got.Content) != 1 || got.Content[0].Text != "file body" {
		t.Fatalf("content must be omp's block list: %s", raw)
	}
}

// A hook that replaces the result via omp's `content` key must land in the
// rendered envelope the agent consumes (text + preserved isError).
func TestToolResultContentMutationApplies(t *testing.T) {
	b := FromSettings(map[string]any{
		"tool_result": `echo '{"content":"REDACTED BY HOOK"}'`,
	})
	out := b.ToolResult(context.Background(), hookCall("read", json.RawMessage(`{"path":"x"}`)),
		json.RawMessage(`{"text":"secret","isError":false}`), false)
	var p struct {
		Text    string `json:"text"`
		IsError bool   `json:"isError"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatalf("patched result must stay the rendered envelope: %s", out)
	}
	if p.Text != "REDACTED BY HOOK" {
		t.Fatalf("content mutation not applied: %q", p.Text)
	}
}

// The matcher resolves the tool name from the payload, so gating a hook on
// `Bash(...)` keeps working after the key rename.
func TestMatcherAndIfPrefilterReadToolName(t *testing.T) {
	payload := map[string]any{
		"toolName": "bash",
		"input":    json.RawMessage(`{"command":"rm -rf /tmp/x"}`),
	}
	if got := matchSubject(payload); got != "bash" {
		t.Fatalf("matchSubject = %q, want bash", got)
	}
	if !ifMatch("Bash(rm *)", payload) {
		t.Fatal("if prefilter must match under the toolName key")
	}
	// The legacy spelling keeps working for other emitters.
	legacy := map[string]any{"tool": "bash", "input": json.RawMessage(`{"command":"ls"}`)}
	if !ifMatch("Bash(ls *)", legacy) {
		t.Fatal("the legacy `tool` key must still resolve")
	}
}

// A multi-line content block list joins in order (an omp hook returning
// several text blocks replaces the whole result).
func TestMutationTextJoinsContentBlocks(t *testing.T) {
	res := map[string]any{"content": []any{
		map[string]any{"type": "text", "text": "one\n"},
		map[string]any{"type": "text", "text": "two"},
	}}
	got, ok := mutationText(res, "content")
	if !ok || !strings.Contains(got, "one") || !strings.Contains(got, "two") {
		t.Fatalf("blocks not joined: %q ok=%v", got, ok)
	}
}

// hookCall builds the call block the widened interceptor seam carries; the id
// is the field a hook correlates pre/post events on.
func hookCall(name string, args json.RawMessage) ai.ToolCallBlock {
	return ai.ToolCallBlock{ID: "tc-" + name, Name: name, Arguments: args}
}
