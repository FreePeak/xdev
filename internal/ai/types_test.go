package ai

import (
	"encoding/json"
	"testing"
)

// omp wire shapes verified against real session files (2026-09-09).
const ompAssistantJSON = `{"role":"assistant","content":[{"type":"thinking","thinking":"plan","thinkingSignature":"reasoning_content"},{"type":"text","text":"doing"},{"type":"toolCall","id":"call_1","name":"read","arguments":{"path":"/tmp/x"},"partialArgs":"{\"path\":\"/tmp/x\"}","streamIndex":0,"intent":"Reading"}],"provider":"router","api":"openai-completions","model":"free","stopReason":"stop","usage":{"input":2132,"output":165,"cacheRead":30784,"totalTokens":33081,"reasoningTokens":100},"completedAt":"2026-09-07T03:21:00.570Z","duration":5400,"ttft":410}`

const ompToolResultJSON = `{"role":"toolResult","toolCallId":"call_1","toolName":"read","content":[{"type":"text","text":"1:x"}],"isError":false,"details":{"isDirectory":false},"timestamp":1788751260587}`

const ompUserJSON = `{"role":"user","content":[{"type":"text","text":"config my herdr"}],"attribution":"user","timestamp":1788751254141}`

func TestMessageRoundTripAssistant(t *testing.T) {
	var in Message
	if err := json.Unmarshal([]byte(ompAssistantJSON), &in); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if in.Role != RoleAssistant || in.StopReason != StopReasonStop {
		t.Fatalf("role/stopReason = %v/%v", in.Role, in.StopReason)
	}
	if len(in.Content) != 3 {
		t.Fatalf("content blocks = %d, want 3", len(in.Content))
	}
	th, ok := in.Content[0].(ThinkingBlock)
	if !ok || th.Thinking != "plan" || th.ThinkingSignature != "reasoning_content" {
		t.Fatalf("block 0 = %#v", in.Content[0])
	}
	tc, ok := in.Content[2].(ToolCallBlock)
	if !ok || tc.ID != "call_1" || tc.Name != "read" || tc.StreamIndex != 0 {
		t.Fatalf("block 2 = %#v", in.Content[2])
	}
	if string(tc.Arguments) != `{"path":"/tmp/x"}` {
		t.Fatalf("arguments = %s", tc.Arguments)
	}
	if in.Usage == nil || in.Usage.CacheRead != 30784 || in.Usage.TotalTokens != 33081 {
		t.Fatalf("usage = %#v", in.Usage)
	}

	out, err := json.Marshal(&in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Message
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("re-unmarshal: %v (%s)", err, out)
	}
	if back.Text() != "doing" || len(back.ToolCalls()) != 1 || back.ToolCalls()[0].ID != "call_1" {
		t.Fatalf("round trip lost data: %+v", back)
	}
	// Key fields survive byte-for-byte.
	var orig, redo map[string]any
	_ = json.Unmarshal([]byte(ompAssistantJSON), &orig)
	_ = json.Unmarshal(out, &redo)
	for _, k := range []string{"stopReason", "provider", "api", "model", "completedAt"} {
		if orig[k] != redo[k] {
			t.Fatalf("field %q drifted: %v -> %v", k, orig[k], redo[k])
		}
	}
}

func TestMessageRoundTripUserAndToolResult(t *testing.T) {
	for name, src := range map[string]string{"user": ompUserJSON, "toolResult": ompToolResultJSON} {
		var in Message
		if err := json.Unmarshal([]byte(src), &in); err != nil {
			t.Fatalf("%s: unmarshal: %v", name, err)
		}
		out, err := json.Marshal(&in)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		var back Message
		if err := json.Unmarshal(out, &back); err != nil {
			t.Fatalf("%s: re-unmarshal: %v", name, err)
		}
		if back.Role != in.Role || len(back.Content) != len(in.Content) {
			t.Fatalf("%s: round trip drift: %+v", name, back)
		}
	}
	if tr := ompToolResult(t); tr.ToolCallID != "call_1" || tr.ToolName != "read" || tr.IsError {
		t.Fatalf("toolResult fields = %#v", tr)
	}
}

func ompToolResult(t *testing.T) Message {
	t.Helper()
	var m Message
	if err := json.Unmarshal([]byte(ompToolResultJSON), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func TestParseBlockRejectsUnknown(t *testing.T) {
	if _, err := ParseBlock([]byte(`{"type":"quantum","x":1}`)); err == nil {
		t.Fatal("expected error for unknown block type")
	}
	if _, err := ParseBlock([]byte(`{"x":1}`)); err == nil {
		t.Fatal("expected error for missing type")
	}
}
