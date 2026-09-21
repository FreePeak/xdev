package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

// TestTrajectoryMetaSplitsUsage is the one runnable check for the richer
// ledger facts: input/cache/output/think/total, duration, and ttft all land
// on the machine-fact line the inspector (and row tail) shows. A blank meta
// for a message that carries none stays blank.
func TestTrajectoryMetaSplitsUsage(t *testing.T) {
	m := &ai.Message{
		Role: ai.RoleAssistant,
		Usage: &ai.Usage{
			Input:           3500,
			CacheRead:       30000,
			CacheWrite:      1200,
			Output:          319,
			ReasoningTokens: 41,
			TotalTokens:     35060,
			Cost:            &ai.UsageCost{Total: 0.0123},
		},
		DurationMS: 2424,
		TTFTMS:     4429,
	}
	got := trajectoryMeta(m)
	for _, want := range []string{
		"↑3.5k new",
		"⇢30k cache",
		"⇢1.2k cache+",
		"↓319",
		"think 41",
		"total 35.1k",
		"$0.0123",
		"2.424s",
		"ttft 4.429s",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("meta missing %q in %q", want, got)
		}
	}

	if empty := trajectoryMeta(&ai.Message{Role: ai.RoleUser}); empty != "" {
		t.Fatalf("empty message meta = %q, want \"\"", empty)
	}
}

// TestTrajectoryDetailShowsToolPayloads pins that an assistant turn which only
// called tools still opens a full payload body in the inspector — the gap
// MessageLabel's one-line name left behind.
func TestTrajectoryDetailShowsToolPayloads(t *testing.T) {
	m := &ai.Message{
		Role: ai.RoleAssistant,
		Content: []ai.Block{
			ai.ToolCallBlock{
				ID:        "c1",
				Name:      "bash",
				Arguments: json.RawMessage(`{"command":"echo NAVIGATION_OK","description":"Print NAVIGATION_OK"}`),
			},
			ai.ToolCallBlock{
				ID:        "c2",
				Name:      "read",
				Arguments: json.RawMessage(`{"path":"nav-a.md"}`),
			},
			ai.ThinkingBlock{Thinking: "plan the two reads"},
		},
	}
	got := trajectoryDetail(m)
	for _, want := range []string{
		"tool calls:",
		"bash(",
		"NAVIGATION_OK",
		"read(",
		"nav-a.md",
		"reasoning:",
		"plan the two reads",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("detail missing %q in %q", want, got)
		}
	}
}
