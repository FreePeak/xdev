package agent

import (
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

// The keep-window is a TOKEN budget, not a count of results. The count alone
// cannot say what the kept window is worth: five 50 KB results are 250 KB of
// context and five 200-byte results are 1 KB, so a fixed count either throws a
// survey's findings away or keeps a window no provider will serve.
func TestMicrocompactToolResultsKeepsWhatTheBudgetAffords(t *testing.T) {
	// 8 results of 2000 chars ≈ 500 tokens each. A 2000-token budget keeps
	// about four of them; a 5-result count would have kept five regardless
	// of what they weighed.
	msgs := make([]ai.Message, 0, 8)
	for i := 0; i < 8; i++ {
		msgs = append(msgs, ai.Message{
			Role:     ai.RoleToolResult,
			ToolName: "bash",
			Content:  []ai.Block{ai.TextBlock{Text: strings.Repeat("x", 2000)}},
		})
	}
	cleared, changed := microcompactToolResults(msgs, 2000)
	if !changed {
		t.Fatal("expected change")
	}
	intact := 0
	for _, m := range cleared {
		if m.Text() != strings.Repeat("x", 2000) {
			continue
		}
		intact++
	}
	if intact == 0 {
		t.Fatal("no result survived: the newest finding must always be kept")
	}
	if intact >= 8 {
		t.Fatalf("intact = %d, want the budget to have cleared some", intact)
	}
	// The kept window must be inside the budget (plus the one result that is
	// kept whatever it weighs).
	kept := int64(0)
	for _, m := range cleared {
		if m.Text() == strings.Repeat("x", 2000) {
			kept += messageTokens(m)
		}
	}
	if kept > 2000+messageTokens(msgs[0]) {
		t.Fatalf("kept %d tokens, over the 2000 budget", kept)
	}
}

// A result that fits is never touched. The old shape cleared everything
// older than the newest five results on every boundary, whatever the window
// had room for.
func TestMicrocompactToolResultsKeepsWhatFits(t *testing.T) {
	msgs := make([]ai.Message, 0, 8)
	for i := 0; i < 8; i++ {
		msgs = append(msgs, ai.Message{
			Role:     ai.RoleToolResult,
			ToolName: "read",
			Content:  []ai.Block{ai.TextBlock{Text: strings.Repeat("x", 200)}},
		})
	}
	cleared, changed := microcompactToolResults(msgs, 20000)
	if changed {
		t.Fatal("expected no change: everything fits the budget")
	}
	for i, m := range cleared {
		if m.Text() != strings.Repeat("x", 200) {
			t.Fatalf("result %d was cleared", i)
		}
	}
}

func TestMicrocompactToolResultsNoopWhenNone(t *testing.T) {
	msgs := []ai.Message{
		{Role: ai.RoleToolResult, Content: []ai.Block{ai.TextBlock{Text: "hi"}}},
	}
	_, changed := microcompactToolResults(msgs, 20000)
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
	cleared, changed := microcompactToolResults(msgs, 1000)
	if !changed {
		t.Fatal("expected change")
	}
	if estimateTokens(cleared) >= estimateTokens(msgs) {
		t.Fatal("expected token reduction")
	}
}

// The placeholder is the model's own record of what the call produced. The
// first shape was a flat "[Old tool result content cleared]", and the model
// read that as "this call produced nothing" — so it ran the command again to
// find out. It must name the tool and say the call can be repeated.
func TestMicrocompactPlaceholderNamesTheTool(t *testing.T) {
	msgs := []ai.Message{
		{Role: ai.RoleToolResult, ToolName: "bash", Content: []ai.Block{ai.TextBlock{Text: strings.Repeat("z", 4000)}}},
		{Role: ai.RoleToolResult, ToolName: "bash", Content: []ai.Block{ai.TextBlock{Text: strings.Repeat("z", 4000)}}},
	}
	cleared, changed := microcompactToolResults(msgs, 100)
	if !changed {
		t.Fatal("expected change")
	}
	found := ""
	for _, m := range cleared {
		if strings.Contains(m.Text(), "cleared from the context") {
			found = m.Text()
		}
	}
	if found == "" {
		t.Fatalf("no placeholder found in %+v", cleared)
	}
	if !strings.Contains(found, "bash") {
		t.Fatalf("placeholder does not name the tool: %q", found)
	}
	if !strings.Contains(found, "re-run") {
		t.Fatalf("placeholder does not say the call can be repeated: %q", found)
	}
}
