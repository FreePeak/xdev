package agent

import (
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

func TestMicrocompactToolResultsKeepsNewest5(t *testing.T) {
	msgs := make([]ai.Message, 0, 8)
	for i := 0; i < 8; i++ {
		msgs = append(msgs, ai.Message{
			Role:    ai.RoleToolResult,
			ToolName: "read",
			Content: []ai.Block{ai.TextBlock{Text: strings.Repeat("x", 2000)}},
		})
	}
	cleared, changed := microcompactToolResults(msgs, 5)
	if !changed {
		t.Fatal("expected change")
	}
	clearedCount := 0
	for _, m := range cleared {
		if m.Text() == microcompactPlaceholder {
			clearedCount++
		}
	}
	if clearedCount != 3 {
		t.Fatalf("cleared = %d, want 3", clearedCount)
	}
	// newest 5 intact (text not placeholder)
	intact := 0
	for _, m := range cleared {
		if m.Text() != microcompactPlaceholder {
			intact++
		}
	}
	if intact != 5 {
		t.Fatalf("intact = %d, want 5", intact)
	}
}

func TestMicrocompactToolResultsNoopWhenFew(t *testing.T) {
	msgs := []ai.Message{
		{Role: ai.RoleToolResult, Content: []ai.Block{ai.TextBlock{Text: "hi"}}},
	}
	_, changed := microcompactToolResults(msgs, 5)
	if changed {
		t.Fatal("expected no change")
	}
}

func TestMicrocompactToolResultsTokenReduction(t *testing.T) {
	msgs := make([]ai.Message, 0, 7)
	for i := 0; i < 7; i++ {
		msgs = append(msgs, ai.Message{
			Role:    ai.RoleToolResult,
			Content: []ai.Block{ai.TextBlock{Text: strings.Repeat("y", 2000)}},
		})
	}
	cleared, changed := microcompactToolResults(msgs, 5)
	if !changed {
		t.Fatal("expected change")
	}
	if estimateTokens(cleared) >= estimateTokens(msgs) {
		t.Fatal("expected token reduction")
	}
}