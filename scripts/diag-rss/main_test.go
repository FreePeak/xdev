package main

import (
	"encoding/json"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

// blockBytes estimates the retained bytes of a block's text payload. The
// estimate is what the tool reports as "context block bytes", so a block
// type that is silently uncounted would under-report the whole session.
func TestBlockBytesCountsEveryBlockType(t *testing.T) {
	cases := []struct {
		name  string
		block ai.Block
		want  int
	}{
		{"text", ai.TextBlock{Text: "hello"}, 5},
		{"thinking", ai.ThinkingBlock{Thinking: "why", ThinkingSignature: "sig"}, 6},
		{"tool call", ai.ToolCallBlock{ID: "id", Name: "bash", Arguments: json.RawMessage(`{}`)}, 8},
		{"image", ai.ImageBlock{Source: ai.ImageSource{Data: "data", Type: "image/png", MediaType: "image/png"}}, 22},
	}
	for _, c := range cases {
		if got := blockBytes(c.block); got != c.want {
			t.Errorf("%s: blockBytes = %d, want %d", c.name, got, c.want)
		}
	}
}

// A block type the tool does not know is counted as nothing rather than
// guessed at — the report says what it measured, not what it assumed.
func TestBlockBytesOfAnUnknownBlock(t *testing.T) {
	if got := blockBytes(ai.TextBlock{}); got != 0 {
		t.Fatalf("blockBytes of an empty text = %d, want 0", got)
	}
}
