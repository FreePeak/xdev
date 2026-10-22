package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

// Live 2026-10-21 (session 8efefc9e): the compaction summary's "## Original
// Request" listed FIVE requests the user had made in OTHER sessions —
// merge the open PRs, stop a session in the sibling `agentloop` repo, list
// every branch and worktree, write a blog, deep-dive a testnet PnL — and
// none of them was the question the user had actually asked ("tell me where
// we are from this xdev repos features, issues").
//
// The cause was the memory backend. `wireAgentMode` hands the agent the
// backend's recall (cmd/xdev/print.go), the recall rides ahead of the
// transcript in the summarize request, and the summarizer was told to write
// "## Original Request — what the user asked for in this turn". A global
// memory scope makes the recall reach every session the bank has ever seen,
// so the summarizer found five other requests and wrote them down as this
// one's.
//
// The model then read the summary as its instructions: it planned the PR
// merges, the monitoring, the blog and the testnet dive, and it left the
// working directory to look for a sibling repo.
//
// The transcript is the only source of what THIS user asked for. The
// memories are background.
func TestSummarizeRequestKeepsRecalledMemoriesOutOfTheRequest(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{textEvent("SUMMARY"), doneEvent("summary")}},
	}}
	memories := "- Review all the open PR in my xdev and tell me which one can merge?\n" +
		"- check the current xdev session in this agentloop repo, does it trigger to call claude code"
	if _, _, err := summarizeWith(context.Background(), p, "m",
		[]ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "tell me where we are"}}}},
		memories, true, ""); err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if len(p.gotReqs) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(p.gotReqs))
	}
	req := p.gotReqs[0]
	prompt := req.System + "\n" + req.Messages[0].Text()
	// The block must say what it is: background from other sessions, not the
	// request of this conversation.
	if !strings.Contains(prompt, "EARLIER, UNRELATED sessions") {
		t.Fatalf("the memory block is not labelled for what it is:\n%s", prompt)
	}
	if !strings.Contains(prompt, "NOT the request of this conversation") {
		t.Fatalf("the memory block does not rule itself out as the request:\n%s", prompt)
	}
	// The prompt must tell the summarizer where a memory belongs and that the
	// transcript alone decides the request.
	if !strings.Contains(prompt, "## Recalled Context") {
		t.Fatalf("the prompt has no section for recalled memories:\n%s", req.System)
	}
	if !strings.Contains(req.System, "ONLY from the conversation transcript") {
		t.Fatalf("the prompt does not bind the request slot to the transcript:\n%s", req.System)
	}
}

// The main compaction prompt carries the same rule: the Goal is the goal of
// THIS conversation, and a memory is never it.
func TestCompactionPromptBindsTheGoalToTheTranscript(t *testing.T) {
	if !strings.Contains(compactionPrompt, "ONLY from the conversation transcript") {
		t.Fatalf("the goal slot is not bound to the transcript:\n%s", compactionPrompt)
	}
	if !strings.Contains(compactionPrompt, "## Recalled Context") {
		t.Fatalf("the prompt has no section for recalled memories:\n%s", compactionPrompt)
	}
	if !strings.Contains(compactionTurnPrefixPrompt, "ONLY from the conversation transcript") {
		t.Fatalf("the original-request slot is not bound to the transcript:\n%s", compactionTurnPrefixPrompt)
	}
	if !strings.Contains(compactionTurnPrefixPrompt, "## Recalled Context") {
		t.Fatalf("the split-turn prompt has no section for recalled memories:\n%s", compactionTurnPrefixPrompt)
	}
}

// A summary with no request in its transcript must say so rather than invent
// one. The instruction has to survive the summarizer's own discretion.
func TestCompactionPromptsForbidInventingARequest(t *testing.T) {
	for name, prompt := range map[string]string{
		"main":       compactionPrompt,
		"split-turn": compactionTurnPrefixPrompt,
	} {
		flat := strings.Join(strings.Fields(prompt), " ")
		if !strings.Contains(flat, "write that instead of inventing one") {
			t.Fatalf("%s prompt does not forbid inventing a request:\n%s", name, prompt)
		}
	}
}
