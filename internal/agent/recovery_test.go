package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// doneEvent builds a plain terminal assistant event.
func doneEvent(text string) ai.Event {
	return ai.Event{
		Type: ai.EventDone, StopReason: ai.StopReasonStop,
		Message: &ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: text}}, StopReason: ai.StopReasonStop},
	}
}

func textEvent(txt string) ai.Event {
	return ai.Event{Type: ai.EventTextDelta, Delta: txt}
}

// storeAgent wires hooks that persist to a real store, mirroring print mode.
func storeAgent(t *testing.T, p *fakeProvider, cfg CompactionConfig) (*Agent, *session.Store, *fakeProvider) {
	t.Helper()
	reg := tool.NewRegistry()
	reg.Register(echoTool{})
	s := session.OpenMem("test", "t")
	hooks := TurnHooksFunc{
		OnMessageEndF:    func(m *ai.Message) { _ = s.Append(&session.MessageEntry{Message: *m}) },
		OnToolResultMsgF: func(m *ai.Message) { _ = s.Append(&session.MessageEntry{Message: *m}) },
	}
	a := &Agent{Provider: p, Tools: reg, Hooks: hooks, Store: s, Compaction: cfg}
	return a, s, p
}

// submitHistory mirrors the CLI/TUI flow: the user message lands in the
// store BEFORE Run sees it, so compaction (which walks the store) can
// always see the full conversation.
func submitHistory(t *testing.T, s *session.Store, text string) []ai.Message {
	t.Helper()
	m := ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: text}}}
	if err := s.Append(&session.MessageEntry{Message: m}); err != nil {
		t.Fatal(err)
	}
	return []ai.Message{m}
}

func fastRetry() RetryPolicy {
	return RetryPolicy{MaxRetries: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
}

func TestRetryTransientPreContentSucceeds(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{err: &ai.HTTPError{API: "openai-completions", Status: 500, Body: "boom"}},
		{events: []ai.Event{ai.Event{Type: ai.EventStart}, textEvent("ok"), doneEvent("ok")}},
	}}
	a, _, p := storeAgent(t, p, CompactionConfig{})
	a.Retry = fastRetry()
	final, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Text() != "ok" {
		t.Fatalf("final = %q", final.Text())
	}
	if len(p.gotReqs) != 2 {
		t.Fatalf("stream calls = %d, want 2", len(p.gotReqs))
	}
}

func TestRetryAuthFailsFast(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{err: &ai.HTTPError{API: "openai-completions", Status: 401, Body: `{"error":{"type":"authentication_error","message":"invalid api key"}}`}},
	}}
	a, _, p := storeAgent(t, p, CompactionConfig{})
	a.Retry = RetryPolicy{MaxRetries: 5, BaseDelay: time.Millisecond}
	_, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}})
	if err == nil {
		t.Fatal("Run: expected 401 error")
	}
	if !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("err = %v", err)
	}
	if len(p.gotReqs) != 1 {
		t.Fatalf("stream calls = %d, want 1 (auth must fail fast)", len(p.gotReqs))
	}
}

func TestRetryPostContentRetainsAndContinues(t *testing.T) {
	// Text deltas streamed, then a transient error: replaying would
	// double-emit the visible text, so the partial is retained and the
	// turn resumes ONCE (M5 tail retain-and-continue). A second failure
	// surfaces instead of looping.
	p := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{
			ai.Event{Type: ai.EventTextStart}, textEvent("partial "),
			ai.Errorf(&ai.HTTPError{API: "a", Status: 500, Body: "stall"}),
		}},
		{events: []ai.Event{
			textEvent("continued"),
			doneEvent("continued"),
		}},
	}}
	a, st, p := storeAgent(t, p, CompactionConfig{})
	a.Retry = fastRetry()
	msg, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}})
	if err != nil {
		t.Fatalf("retain-and-continue must recover: %v", err)
	}
	if msg == nil || !strings.Contains(msg.Text(), "continued") {
		t.Fatalf("final message must be the continuation: %q", msg.Text())
	}
	if len(p.gotReqs) != 2 {
		t.Fatalf("stream calls = %d, want 2 (initial + one continuation)", len(p.gotReqs))
	}
	// The partial and the continuation prompt are persisted, so a rebuild
	// keeps the visible pre-failure text in context.
	res, err := session.BuildContext(st.Entries(), st.LeafID(), session.SystemPrompt{})
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, m := range res.Messages {
		joined += m.Text() + "\n"
	}
	if !strings.Contains(joined, "partial ") || !strings.Contains(joined, "continued") {
		t.Fatalf("rebuild must keep partial + continuation:\n%s", joined)
	}
}

// TestRetryPostContentSecondFailureSurfaces pins the runaway guard: the
// retain-and-continue path fires at most once per turn.
func TestRetryPostContentSecondFailureSurfaces(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{
			ai.Event{Type: ai.EventTextStart}, textEvent("partial "),
			ai.Errorf(&ai.HTTPError{API: "a", Status: 500, Body: "stall"}),
		}},
		{events: []ai.Event{
			ai.Event{Type: ai.EventTextStart}, textEvent("more "),
			ai.Errorf(&ai.HTTPError{API: "a", Status: 500, Body: "stall again"}),
		}},
	}}
	a, _, p := storeAgent(t, p, CompactionConfig{})
	a.Retry = fastRetry()
	_, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}})
	if err == nil {
		t.Fatal("second post-content failure must surface")
	}
	if len(p.gotReqs) != 2 {
		t.Fatalf("stream calls = %d, want 2 (no second continuation)", len(p.gotReqs))
	}
}

func TestOverflowTriggersCompactionAndRetries(t *testing.T) {
	// Two normal turns populate the store, then the third call overflows
	// (fake), a summary call compacts it, and the retried turn succeeds.
	// Threshold compaction is off (ContextWindow 0) so this exercises the
	// overflow path alone.
	p := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{textEvent("first reply"), doneEvent("first reply")}},
		{events: []ai.Event{textEvent("second reply"), doneEvent("second reply")}},
		{err: &ai.HTTPError{API: "a", Status: 400, Body: "maximum context length exceeded (128000 tokens)"}},
		{events: []ai.Event{textEvent("summary: did things"), doneEvent("summary: did things")}},
		{events: []ai.Event{textEvent("done on compacted history"), doneEvent("done on compacted history")}},
	}}
	cfg := CompactionConfig{ContextWindow: 0, KeepRecentTokens: 2}
	a, s, p := storeAgent(t, p, cfg)
	a.Retry = RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond}
	if _, err := a.Run(context.Background(), "sys", submitHistory(t, s, "first")); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if _, err := a.Run(context.Background(), "sys", submitHistory(t, s, "second")); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	final, err := a.Run(context.Background(), "sys", submitHistory(t, s, "third"))
	if err != nil {
		t.Fatalf("run 3: %v", err)
	}
	if final.Text() != "done on compacted history" {
		t.Fatalf("final = %q", final.Text())
	}
	if len(p.gotReqs) != 5 {
		t.Fatalf("stream calls = %d, want 5 (2 turns + overflow + summary + retry)", len(p.gotReqs))
	}
	// A compaction entry must have landed in the store.
	compacted := false
	for _, e := range s.Entries() {
		if _, ok := e.(*session.CompactionEntry); ok {
			compacted = true
		}
	}
	if !compacted {
		t.Fatal("no compaction entry persisted after overflow")
	}
}

func TestThresholdCompactionBetweenTurns(t *testing.T) {
	// TUI shape: two submits against one store, history rebuilt from the
	// store before each Run. The oversized first turn pushes the context
	// over the tiny threshold, so run 2 compacts before streaming and its
	// request carries the summary.
	seed := strings.Repeat("x", 4000) // ~1000 tokens by chars/4 estimate
	p := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{textEvent("first"), doneEvent("first")}},
		{events: []ai.Event{textEvent("summary of past"), doneEvent("summary of past")}},
		{events: []ai.Event{textEvent("second"), doneEvent("second")}},
	}}
	cfg := CompactionConfig{ContextWindow: 1000, KeepRecentTokens: 5}
	a, s, _ := storeAgent(t, p, cfg)
	a.Retry = RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond}

	if _, err := a.Run(context.Background(), "sys", submitHistory(t, s, seed)); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if _, err := a.Run(context.Background(), "sys", submitHistory(t, s, "second user")); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if len(p.gotReqs) != 3 {
		t.Fatalf("stream calls = %d, want 3 (turn, summary, turn)", len(p.gotReqs))
	}
	// The third request (run 2's turn) must have been built from the
	// compacted context: the store must hold a compaction entry and the
	// rebuilt context must start with its summary.
	compacted := false
	var summary string
	for _, e := range s.Entries() {
		if c, ok := e.(*session.CompactionEntry); ok {
			compacted = true
			summary = c.Summary.Text()
		}
	}
	if !compacted {
		t.Fatal("no compaction entry after threshold exceeded")
	}
	if !strings.Contains(summary, "summary of past") {
		t.Fatalf("summary = %q", summary)
	}
	res, err := session.BuildContext(s.Entries(), s.LeafID(), session.SystemPrompt{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) == 0 || !strings.Contains(res.Messages[0].Text(), "summary of past") {
		t.Fatalf("rebuilt context does not start with the summary: %+v", res.Messages)
	}
}
