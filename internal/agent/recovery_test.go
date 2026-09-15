package agent

import (
	"context"
	"errors"
	"fmt"
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

// ladderAgent wires an agent with a primary provider plus backup targets,
func ladderAgent(t *testing.T, primary *fakeProvider, backups ...*fakeProvider) (*Agent, *session.Store) {
	t.Helper()
	reg := tool.NewRegistry()
	reg.Register(echoTool{})
	s := session.OpenMem("test", "t")
	hooks := TurnHooksFunc{
		OnMessageEndF:    func(m *ai.Message) { _ = s.Append(&session.MessageEntry{Message: *m}) },
		OnToolResultMsgF: func(m *ai.Message) { _ = s.Append(&session.MessageEntry{Message: *m}) },
	}
	a := &Agent{Provider: primary, Tools: reg, Hooks: hooks, Store: s, Retry: RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond}}
	for i, b := range backups {
		a.Failovers = append(a.Failovers, FailoverTarget{Provider: b, Model: fmt.Sprintf("backup%d", i+1), ContextWindow: 1_000_000})
	}
	return a, s
}

func TestOverflowPromotesBeforeCompaction(t *testing.T) {
	// Overflow on the primary promotes to the bigger-window target; the
	// turn succeeds WITHOUT a compaction entry — promotion owns recovery
	// while a bigger window exists (PRD M5: promotion before compaction).
	primary := &fakeProvider{calls: []fakeScript{
		{err: &ai.HTTPError{API: "a", Status: 400, Body: "maximum context length exceeded"}},
	}}
	backup := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{textEvent("done on big model"), doneEvent("done on big model")}},
	}}
	a, s := ladderAgent(t, primary, backup)
	final, err := a.Run(context.Background(), "sys", submitHistory(t, s, "hi"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Text() != "done on big model" {
		t.Fatalf("final = %q", final.Text())
	}
	if a.Model != "backup1" {
		t.Fatalf("model after promotion = %q, want backup1", a.Model)
	}
	if len(backup.gotReqs) != 1 {
		t.Fatalf("backup stream calls = %d, want 1", len(backup.gotReqs))
	}
	for _, e := range s.Entries() {
		if _, ok := e.(*session.CompactionEntry); ok {
			t.Fatal("compaction must not run while promotion is available")
		}
	}
	// The switch is mirrored into the session store.
	changed := false
	for _, e := range s.Entries() {
		if mc, ok := e.(*session.ModelChangeEntry); ok && mc.Model == "fake/backup1" {
			changed = true
		}
	}
	if !changed {
		t.Fatal("no model_change entry persisted on promotion")
	}
}

func TestPromotionLadderExhaustsThenCompacts(t *testing.T) {
	// Overflow on the primary AND on the promoted model: the ladder
	// tops out and compaction owns recovery, on the promoted target.
	primary := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{textEvent("first reply"), doneEvent("first reply")}},
		{events: []ai.Event{textEvent("second reply"), doneEvent("second reply")}},
		{err: &ai.HTTPError{API: "a", Status: 400, Body: "maximum context length exceeded"}},
	}}
	backup := &fakeProvider{calls: []fakeScript{
		{err: &ai.HTTPError{API: "b", Status: 400, Body: "prompt is too long"}},
		{events: []ai.Event{textEvent("summary: did things"), doneEvent("summary: did things")}},
		{events: []ai.Event{textEvent("done after compaction"), doneEvent("done after compaction")}},
	}}
	a, s := ladderAgent(t, primary, backup)
	a.Compaction = CompactionConfig{ContextWindow: 0, KeepRecentTokens: 2}
	for _, turn := range []string{"first", "second"} {
		if _, err := a.Run(context.Background(), "sys", submitHistory(t, s, turn)); err != nil {
			t.Fatalf("run %s: %v", turn, err)
		}
	}
	final, err := a.Run(context.Background(), "sys", submitHistory(t, s, "third"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Text() != "done after compaction" {
		t.Fatalf("final = %q", final.Text())
	}
	compacted := false
	for _, e := range s.Entries() {
		if _, ok := e.(*session.CompactionEntry); ok {
			compacted = true
		}
	}
	if !compacted {
		t.Fatal("no compaction entry after the ladder topped out")
	}
}

func TestFailoverAfterRetryLadderDrains(t *testing.T) {
	// Pre-content 500s drain the retry ladder, then the run fails over
	// to the next chain target instead of surfacing.
	primary := &fakeProvider{calls: []fakeScript{
		{err: &ai.HTTPError{API: "a", Status: 500, Body: "boom"}},
		{err: &ai.HTTPError{API: "a", Status: 500, Body: "boom"}},
	}}
	backup := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{textEvent("ok on backup"), doneEvent("ok on backup")}},
	}}
	a, s := ladderAgent(t, primary, backup)
	final, err := a.Run(context.Background(), "sys", submitHistory(t, s, "hi"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Text() != "ok on backup" {
		t.Fatalf("final = %q", final.Text())
	}
	if len(primary.gotReqs) != 2 {
		t.Fatalf("primary calls = %d, want 2 (first + 1 retry)", len(primary.gotReqs))
	}
	if len(backup.gotReqs) != 1 {
		t.Fatalf("backup calls = %d, want 1", len(backup.gotReqs))
	}
}

func TestFailoverChainExhaustedSurfaces(t *testing.T) {
	// Every target drains its ladder. The primary gets exactly one ladder
	// (the chain never bounces back mid-turn); the last target then runs
	// the bounded escalation rounds before the error surfaces.
	boom := func(api string) fakeScript {
		return fakeScript{err: &ai.HTTPError{API: api, Status: 500, Body: "boom"}}
	}
	primary := &fakeProvider{calls: []fakeScript{boom("a"), boom("a")}}
	backupCalls := 2 * (maxEscalationRounds + 1) // one ladder per round (MaxRetries 1)
	var backupScripts []fakeScript
	for i := 0; i < backupCalls; i++ {
		backupScripts = append(backupScripts, boom("b"))
	}
	backup := &fakeProvider{calls: backupScripts}
	a, s := ladderAgent(t, primary, backup)
	_, err := a.Run(context.Background(), "sys", submitHistory(t, s, "hi"))
	if err == nil {
		t.Fatal("expected error after the chain exhausted")
	}
	if len(primary.gotReqs) != 2 {
		t.Fatalf("primary calls = %d, want 2 (one ladder, then fail over)", len(primary.gotReqs))
	}
	if len(backup.gotReqs) != backupCalls {
		t.Fatalf("backup calls = %d, want %d (bounded escalation rounds)", len(backup.gotReqs), backupCalls)
	}
}

// TestStreamEndedWithoutFinishReasonRetriesAndResumes is the end-to-end pin
// for the session-killing bug: the adapters' "clean EOF, no terminal event"
// error — the exact string openai_completions.go emits — must classify as
// transient so the ladder retries and the run resumes instead of ending.
func TestStreamEndedWithoutFinishReasonRetriesAndResumes(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{err: errors.New("openai-completions: stream ended without finish_reason")},
		{events: []ai.Event{ai.Event{Type: ai.EventStart}, textEvent("recovered"), doneEvent("recovered")}},
	}}
	a, _, p := storeAgent(t, p, CompactionConfig{})
	a.Retry = fastRetry()
	final, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}})
	if err != nil {
		t.Fatalf("a stream-ended error must not end the session: %v", err)
	}
	if final.Text() != "recovered" {
		t.Fatalf("final = %q", final.Text())
	}
	if len(p.gotReqs) != 2 {
		t.Fatalf("stream calls = %d, want 2 (retry + success)", len(p.gotReqs))
	}
}

func oneShotRetry() RetryPolicy {
	return RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
}

func TestEscalationRoundsRecoverAfterChainDrains(t *testing.T) {
	// No failover chain: the ladder drains, and the FIRST escalation round
	// must be allowed to succeed — an outage slightly longer than the
	// ladder no longer ends the session.
	p := &fakeProvider{calls: []fakeScript{
		{err: &ai.HTTPError{API: "a", Status: 500, Body: "boom"}},
		{err: errors.New("openai-completions: stream ended without finish_reason")},
		{events: []ai.Event{textEvent("ok after round 1"), doneEvent("ok after round 1")}},
	}}
	a, _, p := storeAgent(t, p, CompactionConfig{})
	a.Retry = oneShotRetry()
	final, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}})
	if err != nil {
		t.Fatalf("escalation round 1 must get a chance: %v", err)
	}
	if final.Text() != "ok after round 1" {
		t.Fatalf("final = %q", final.Text())
	}
	if len(p.gotReqs) != 3 {
		t.Fatalf("stream calls = %d, want 3 (ladder 2 + round 1)", len(p.gotReqs))
	}
}

func TestEscalationRoundsAreBounded(t *testing.T) {
	// A hard failure the classifier believes is transient must still end:
	// (MaxRetries+1) calls per round × (1 + maxEscalationRounds) rounds.
	var calls []fakeScript
	want := (oneShotRetry().MaxRetries + 1) * (maxEscalationRounds + 1)
	for i := 0; i < want; i++ {
		calls = append(calls, fakeScript{err: &ai.HTTPError{API: "a", Status: 500, Body: "boom"}})
	}
	p := &fakeProvider{calls: calls}
	a, _, p := storeAgent(t, p, CompactionConfig{})
	a.Retry = oneShotRetry()
	_, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}})
	if err == nil {
		t.Fatal("an always-failing provider must eventually surface")
	}
	if len(p.gotReqs) != want {
		t.Fatalf("stream calls = %d, want %d (bounded escalation)", len(p.gotReqs), want)
	}
}

// TestContinuationInjectionAttributedAndHooked pins #283: the injected
// cut-off recovery turn must carry the harness attribution (never
// masquerade as user input in the store or in stats) and must fire
// OnContinuation so the live transcript can render it as a harness event.
func TestContinuationInjectionAttributedAndHooked(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{
			ai.Event{Type: ai.EventTextStart}, textEvent("partial "),
			ai.Errorf(&ai.HTTPError{API: "a", Status: 500, Body: "stall"}),
		}},
		{events: []ai.Event{textEvent("continued"), doneEvent("continued")}},
	}}
	var injected []string
	reg := tool.NewRegistry()
	reg.Register(echoTool{})
	st := session.OpenMem("test", "t")
	hooks := TurnHooksFunc{
		OnMessageEndF:    func(m *ai.Message) { _ = st.Append(&session.MessageEntry{Message: *m}) },
		OnToolResultMsgF: func(m *ai.Message) { _ = st.Append(&session.MessageEntry{Message: *m}) },
		OnContinuationF:  func(text string) { injected = append(injected, text) },
	}
	a := &Agent{Provider: p, Tools: reg, Hooks: hooks, Store: st}
	a.Retry = fastRetry()
	if _, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}}); err != nil {
		t.Fatalf("retain-and-continue must recover: %v", err)
	}
	if len(injected) != 1 || injected[0] != ContinuationPrompt {
		t.Fatalf("OnContinuation fired %v", injected)
	}
	var found bool
	for _, e := range st.Entries() {
		me, ok := e.(*session.MessageEntry)
		if !ok {
			continue
		}
		if me.Message.Text() == ContinuationPrompt {
			found = true
			if me.Message.Attribution != ContinuationAttribution {
				t.Fatalf("persisted continuation attribution = %q, want %q", me.Message.Attribution, ContinuationAttribution)
			}
		}
	}
	if !found {
		t.Fatal("continuation turn was not persisted at all")
	}
}
