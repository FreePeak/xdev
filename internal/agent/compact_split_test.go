package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// scriptedTranscriptProvider answers the summarize call with a fixed text and
// records the System prompt each call carried, so a test can assert which
// prompt (history vs turn prefix vs update) the summarizer received.
type scriptedTranscriptProvider struct {
	text     string
	system   []string
	requests []ai.StreamRequest
}

func (p *scriptedTranscriptProvider) Stream(_ context.Context, req ai.StreamRequest) (<-chan ai.Event, error) {
	p.system = append(p.system, req.System)
	p.requests = append(p.requests, req)
	ch := make(chan ai.Event, 2)
	ch <- ai.Event{Type: ai.EventTextDelta, Delta: p.text}
	ch <- ai.Donef(ai.StopReasonStop, nil, &ai.Message{
		Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: p.text}}, StopReason: ai.StopReasonStop,
	})
	close(ch)
	return ch, nil
}

func (p *scriptedTranscriptProvider) Name() string { return "scripted" }
func (p *scriptedTranscriptProvider) API() string  { return "scripted-api" }

// splitAgent wires a store-backed agent over prov.
func splitAgent(t *testing.T, prov ai.Provider, keepRecent int64) (*Agent, *session.Store) {
	t.Helper()
	s := session.OpenMem("test", "split")
	hooks := TurnHooksFunc{
		OnMessageEndF:    func(m *ai.Message) { _ = s.Append(&session.MessageEntry{Message: *m}) },
		OnToolResultMsgF: func(m *ai.Message) { _ = s.Append(&session.MessageEntry{Message: *m}) },
	}
	return &Agent{
		Provider: prov, Tools: tool.NewRegistry(), Hooks: hooks, Store: s,
		Compaction: CompactionConfig{ContextWindow: 200_000, KeepRecentTokens: keepRecent, Methods: []string{MethodHandoff}},
	}, s
}

// appendBig persists a big user message followed by a big assistant reply: one
// turn the kept tail cannot swallow.
func appendBig(t *testing.T, s *session.Store, seed string) {
	t.Helper()
	appendMsg(t, s, userText(seed+": "+strings.Repeat("x", 4000)))
	appendMsg(t, s, ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: seed + " reply: " + strings.Repeat("y", 4000)}}})
}

// TestHandoffSummarizesTheTurnPrefixOfASplitCut pins the split-turn summarizer
// input: a cut that lands inside a turn summarizes that turn's prefix with the
// turn-prefix prompt — NOT the older history — and the retained suffix stays
// live. Before the split arm the whole turn crossed the boundary, so the
// summary described work the model was still reading verbatim.
func TestHandoffSummarizesTheTurnPrefixOfASplitCut(t *testing.T) {
	stubPressure(t, 0)
	// keepRecent 1002 keeps the 1002-token second reply, so the accumulated
	// allowance crosses the threshold while walking back over turn two: the cut
	// is that reply — inside turn two — and the span the summarizer sees is
	// everything up to turn two's own user message.
	p := &scriptedTranscriptProvider{text: "SUMMARY-TEXT"}
	a, s := splitAgent(t, p, 1002)
	appendBig(t, s, "first")
	appendBig(t, s, "second")

	if err := a.compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	entries := compactionEntries(s)
	if len(entries) != 1 {
		t.Fatalf("compaction entries = %d, want 1", len(entries))
	}
	if entries[0].Summary.Text() != "SUMMARY-TEXT" {
		t.Fatalf("summary = %q, want the provider's answer", entries[0].Summary.Text())
	}
	// The summarizer saw the PREFIX prompt, not the history prompt.
	if len(p.system) != 1 {
		t.Fatalf("summarize calls = %d, want 1", len(p.system))
	}
	if !strings.Contains(p.system[0], "This is the PREFIX of a turn") {
		t.Fatalf("summarize system = %q, want the turn-prefix prompt", p.system[0])
	}
	// The split arm decides what the SUMMARIZER sees, not what survives: the
	// input is the history up to the turn start (turn one plus turn two's user
	// message), so it never contains the retained reply the anchor keeps.
	body := p.requests[0].Messages[0].Text()
	if !strings.Contains(body, "first: ") {
		t.Errorf("the summarize input must carry the history up to the turn start, got:\n%s", cutFirstLine(body))
	}
	if strings.Contains(body, "second reply") {
		t.Errorf("the summarize input must not carry the retained reply, got:\n%s", cutFirstLine(body))
	}
	// The rebuilt context is the summary plus the retained tail: the anchor
	// keeps everything from the cut onward, so turn two's reply is still live.
	res, err := session.BuildContext(s.Entries(), s.LeafID(), session.SystemPrompt{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 2 {
		t.Fatalf("rebuilt context = %d messages, want 2 (summary + retained reply)", len(res.Messages))
	}
	if !strings.Contains(res.Messages[1].Text(), "second reply") {
		t.Errorf("the retained reply must survive, got %q", res.Messages[1].Text())
	}
}

// TestHandoffUsesTheHistoryPromptOffASplitTurn is the counterpart: a cut on a
// user boundary summarizes the older history with the history prompt.
func TestHandoffUsesTheHistoryPromptOffASplitTurn(t *testing.T) {
	stubPressure(t, 0)
	p := &scriptedTranscriptProvider{text: "SUMMARY-TEXT"}
	// keepRecent 5 keeps only one message, so the cut lands on the tail user
	// message: a clean turn boundary.
	a, s := splitAgent(t, p, 5)
	appendBig(t, s, "first")
	appendBig(t, s, "second")
	appendMsg(t, s, userText("tail question"))

	if err := a.compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.system) != 1 || !strings.Contains(p.system[0], "## Goal") {
		t.Fatalf("summarize system = %v, want the structured history prompt", p.system)
	}
	body := p.requests[0].Messages[0].Text()
	if !strings.Contains(body, "first: ") {
		t.Errorf("the summarize input must carry the older history, got:\n%s", cutFirstLine(body))
	}
}

// TestSecondCompactionUpdatesThePreviousSummary pins the iterative-update arm:
// a compaction when a summary already exists feeds the previous summary back,
// so the second one preserves it instead of re-describing a window the model
// can no longer see.
func TestSecondCompactionUpdatesThePreviousSummary(t *testing.T) {
	stubPressure(t, 0)
	p := &scriptedTranscriptProvider{text: "SECOND-SUMMARY"}
	a, s := splitAgent(t, p, 5)
	// Seed the earlier compaction the way the store would have it: a summary
	// whose anchor is the first later entry, so the rebuild emits the summary
	// and only the history after that anchor.
	appendBig(t, s, "earlier work")
	anchorID := s.Entries()[0].Envelope().ID
	first := "FIRST-SUMMARY"
	if err := s.Append(&session.CompactionEntry{
		Summary:          textSummary(first),
		FirstKeptEntryID: &anchorID,
		Method:           MethodHandoff,
	}); err != nil {
		t.Fatal(err)
	}
	appendBig(t, s, "later work")
	appendMsg(t, s, userText("tail question"))

	if err := a.compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.system) != 1 {
		t.Fatalf("summarize calls = %d, want 1", len(p.system))
	}
	body := p.requests[0].Messages[0].Text()
	if !strings.Contains(body, "FIRST-SUMMARY") {
		t.Errorf("the previous summary must ride the summarize input, got:\n%s", cutFirstLine(body))
	}
	// The request must ALSO ask for the update, or a summarizer that sees both
	// blocks has no instruction on which one wins.
	if !strings.Contains(body, "PRESERVE everything it already records") {
		t.Errorf("the request must instruct the update, got:\n%s", cutFirstLine(body))
	}
	entries := compactionEntries(s)
	if len(entries) != 2 {
		t.Fatalf("compaction entries = %d, want 2 (the seeded one and the new one)", len(entries))
	}
	if entries[1].Summary.Text() != "SECOND-SUMMARY" {
		t.Fatalf("summary = %q, want the second provider answer", entries[1].Summary.Text())
	}
}

// TestSummarizeCarriesRecalledMemory pins the memory-preservation contract: the
// backend's recalled block rides the summarize request, so a compaction cannot
// silently drop what recall just put in front of the model.
//
// The contract is the prompt's own too: the structured template carries a
// "Critical Context" section, so a summarizer that honors the format records
// the recalled facts in the durable artifact rather than only in the request.
func TestSummarizeCarriesRecalledMemory(t *testing.T) {
	stubPressure(t, 0)
	p := &scriptedTranscriptProvider{text: "SUMMARY-TEXT"}
	a, s := splitAgent(t, p, 5)
	a.MemoryContext = func() string { return "MEMORY-ONE\nMEMORY-TWO" }
	appendBig(t, s, "first")
	appendMsg(t, s, userText("tail question"))

	if err := a.compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	body := p.requests[0].Messages[0].Text()
	for _, want := range []string{"MEMORY-ONE", "MEMORY-TWO"} {
		if !strings.Contains(body, want) {
			t.Errorf("the summarize input must preserve %q, got:\n%s", want, cutFirstLine(body))
		}
	}
}

// cutFirstLine is the first 200 chars of a body, for readable failure output.
func cutFirstLine(s string) string {
	if len(s) > 200 {
		return strings.ReplaceAll(s[:200], "\n", "\\n") + "…"
	}
	return strings.ReplaceAll(s, "\n", "\\n")
}

// TestCompactionPromptNamesTheSectionsAContinuationNeeds pins the template
// itself: the structured headings a continuation reads (goal, progress, next
// steps) must be in the system prompt, so a free-form summary cannot quietly
// drop them — and the turn-prefix prompt must stay distinct from it, because
// the two spans summarize different things.
func TestCompactionPromptNamesTheSectionsAContinuationNeeds(t *testing.T) {
	for _, want := range []string{"## Goal", "## Constraints & Preferences", "### Done", "### In Progress", "### Blocked", "## Key Decisions", "## Next Steps", "## Critical Context"} {
		if !strings.Contains(compactionPrompt, want) {
			t.Errorf("compactionPrompt is missing %q", want)
		}
	}
	if compactionTurnPrefixPrompt == compactionPrompt {
		t.Fatal("the turn-prefix prompt must differ from the history prompt")
	}
	if !strings.Contains(compactionTurnPrefixPrompt, "PREFIX") {
		t.Error("the turn-prefix prompt must say the span is a prefix")
	}
}
