package agent

import (
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

func TestIsEmptyAssistantReasoningOnly(t *testing.T) {
	thinkingOnly := ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{
		ai.ThinkingBlock{Thinking: "think"}}}
	if !isEmptyAssistant(thinkingOnly) {
		t.Fatal("A")
	}
	thinkingAndText := ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{
		ai.ThinkingBlock{Thinking: "think"},
		ai.TextBlock{Text: "answer"}}}
	if isEmptyAssistant(thinkingAndText) {
		t.Fatal("B")
	}
	empty := ai.Message{Role: ai.RoleAssistant}
	if !isEmptyAssistant(empty) {
		t.Fatal("D")
	}
}
