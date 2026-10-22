package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

// The field report this exists for (2026-10-22, session adf5029b): a model
// answering "list me all pr urls opening xdev" asked the same question in ten
// spellings and never repeated an exact call, so the repeat guard — which
// keys on the tool name plus the exact arguments — saw 28 tool calls and 28
// first attempts. The model's own reasoning had the answer at turn 5:
//
//	"So: the answer is clear. There are ZERO open PRs in FreePeak/xdev."
//
// and it went on for 15 more turns. The notice below is what tells a model
// that re-derivation is what it is doing, and it says the one thing that
// ends it: REPORT WHAT YOU HAVE.
func TestRepeatedWorkNoticeFiresOnARederivation(t *testing.T) {
	a := &Agent{}
	// Five spellings of one question, interleaved with other work so the
	// EXACT chain never sees two in a row.
	spellings := []string{
		`gh pr list --repo FreePeak/xdev --state open --limit 100 --json number,url`,
		`gh pr list --repo FreePeak/xdev --state open --limit 2000 --json number,title,url`,
		`gh api "repos/FreePeak/xdev/pulls?state=open&per_page=100" --jq 'length'`,
		`gh api "repos/FreePeak/xdev/pulls?state=open&per_page=100" --jq '.[] | .url'`,
		`gh pr list --repo FreePeak/xdev --state open --limit 50 --json url`,
	}
	for i, cmd := range spellings {
		if i > 0 {
			// Other work in between: the exact chain resets on it.
			other := &ai.Message{Role: ai.RoleToolResult, Content: []ai.Block{ai.TextBlock{Text: "ok"}}}
			a.repeatNotice(other, bashCall("git status --short"))
		}
		msg := &ai.Message{Role: ai.RoleToolResult, Content: []ai.Block{ai.TextBlock{Text: "result"}}}
		a.repeatNotice(msg, bashCall(cmd))
		if i == len(spellings)-1 {
			text := msg.Text()
			if !strings.Contains(text, "Repeated work detected") {
				t.Fatalf("the last of five re-derivations carried no notice: %q", text)
			}
			if !strings.Contains(text, "report it now") {
				t.Fatalf("the notice does not tell the model to report what it has: %q", text)
			}
		}
	}
}

// The other direction: genuinely different work must NOT be called a
// repetition. A hard task runs many different commands, and a notice that
// fired there would be the nag the guard is supposed to avoid.
func TestRepeatedWorkNoticeStaysOffForDifferentWork(t *testing.T) {
	a := &Agent{}
	work := []string{
		"gh pr list --repo FreePeak/xdev --state open",
		"gh pr list --repo FreePeak/xdev-private2 --state open",
		"gh issue list --repo FreePeak/xdev --state open",
		"gh api repos/FreePeak/agentloop --jq .full_name",
		"gh repo list FreePeak --limit 200 --json name",
	}
	for _, cmd := range work {
		msg := &ai.Message{Role: ai.RoleToolResult, Content: []ai.Block{ai.TextBlock{Text: "result"}}}
		a.repeatNotice(msg, bashCall(cmd))
		if strings.Contains(msg.Text(), "Repeated work detected") {
			t.Fatalf("different work was called a repetition: %q -> %q", cmd, msg.Text())
		}
	}
}

// The hard break stays on the EXACT chain. A coarse count of eight can be a
// model reading a file in chunks, and breaking the run for that would cost
// the user their work.
func TestTheHardBreakStaysOnTheExactChain(t *testing.T) {
	a := &Agent{}
	// Eight differently-spelled reads of the same file: one coarse work,
	// never eight identical calls.
	for i := 0; i < 8; i++ {
		msg := &ai.Message{Role: ai.RoleToolResult, Content: []ai.Block{ai.TextBlock{Text: "result"}}}
		a.repeatNotice(msg, bashCall("sed -n '"+string(rune('1'+i))+"0,50p' internal/agent/loop.go"))
	}
	if a.repeatBreak {
		t.Fatal("the run was broken on a coarse count — the break must stay on the exact chain")
	}
}

func bashCall(cmd string) ai.ToolCallBlock {
	return ai.ToolCallBlock{ID: "c", Name: "bash", Arguments: json.RawMessage(`{"command":` + quoteJSON(cmd) + `}`)}
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
