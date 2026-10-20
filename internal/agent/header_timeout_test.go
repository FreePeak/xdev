package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// headerTimeoutErr is the transport's own verdict when the response
// headers never arrive: "net/http: timeout awaiting response headers".
// The provider wraps it ("openai-completions: ..."), so the phrase is what
// the classifier reads.
func headerTimeoutErr() error {
	return errors.New("openai-completions: net/http: timeout awaiting response headers")
}

// TestHeaderTimeoutEndsTheTurnInsteadOfReplayingIt is the regression for the
// live 2026-10-20 failure: a provider that stays silent about a request, a
// ladder that replayed the SAME body for hours, and a turn that never ended.
//
// The request was 663 KB (322 tool results) and every attempt waited out the
// transport's 5-minute header budget. Nothing shrank the body, so every
// attempt was the same request, and the same request was silent every time.
// The turn must surface that instead of replaying it.
func TestHeaderTimeoutEndsTheTurnInsteadOfReplayingIt(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{err: headerTimeoutErr()},
		{err: headerTimeoutErr()},
		{err: headerTimeoutErr()},
	}}
	a, s, p := storeAgent(t, p, CompactionConfig{})
	// The shipped default: retry.infinite is on, so the transient ladder
	// would otherwise replay this body for as long as the run lasts.
	a.Retry = RetryPolicy{MaxRetries: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Infinite: true}
	submitHistory(t, s, "hi")

	_, err := a.Run(context.Background(), "sys", []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}},
	})
	if err == nil {
		t.Fatal("a provider that never answers must end the turn with its verdict")
	}
	if !strings.Contains(err.Error(), "timeout awaiting response headers") {
		t.Fatalf("err = %v, want the provider's own verdict", err)
	}
	// One repair attempt at most: the request is sent, found wanting, and
	// the turn ends. A replay loop is what this test exists to stop.
	if len(p.gotReqs) > 3 {
		t.Fatalf("stream calls = %d, want at most 3 (the same body must not be replayed)", len(p.gotReqs))
	}
}

// TestHeaderTimeoutRepairsTheRequestWhenCompactionShrinksIt is the other
// half: the same silence, but this time the history CAN be shrunk, so the
// repair is a smaller request and the turn continues.
//
// A compaction that replaces a hundred tool results with a summary is the
// move the overflow path already makes; here the provider proved by staying
// silent that the request did not fit the way it was.
func TestHeaderTimeoutRepairsTheRequestWhenCompactionShrinksIt(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{err: headerTimeoutErr()},
		{events: []ai.Event{textEvent("ok"), doneEvent("ok")}},
	}}
	// A small window: the threshold method compacts, so the forced
	// compaction the repair calls has something to do.
	a, s, p := storeAgent(t, p, CompactionConfig{ContextWindow: 1000, KeepRecentTokens: 5})
	a.Retry = RetryPolicy{MaxRetries: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Infinite: true}
	// A history big enough to be worth compacting — the shape the live
	// failure had: a long run of tool results, then the user's question.
	big := strings.Repeat("x", 4000)
	for _, m := range []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "first question"}}},
		{Role: ai.RoleAssistant, Content: []ai.Block{ai.ToolCallBlock{ID: "call_1", Name: "read", Arguments: []byte(`{"path":"a.go"}`)}}},
		{Role: ai.RoleToolResult, ToolCallID: "call_1", ToolName: "read", Content: []ai.Block{ai.TextBlock{Text: big}}},
		{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "answer"}}},
	} {
		if err := s.Append(&session.MessageEntry{Message: m}); err != nil {
			t.Fatal(err)
		}
	}
	submitHistory(t, s, big)

	final, err := a.Run(context.Background(), "sys", []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: big}}},
	})
	if err != nil {
		t.Fatalf("Run: the repaired request must be tried, got %v", err)
	}
	if final == nil || final.Text() != "ok" {
		t.Fatalf("final = %#v", final)
	}
	if len(p.gotReqs) != 2 {
		t.Fatalf("stream calls = %d, want 2 (the silent one, then the repaired one)", len(p.gotReqs))
	}
	// The repair must have SHRUNK the request: the second attempt carries
	// fewer bytes than the first.
	first, second := requestBytes(p.gotReqs[0].Messages), requestBytes(p.gotReqs[1].Messages)
	if second >= first {
		t.Fatalf("the repaired request is not smaller: %d bytes then %d", first, second)
	}
}

// TestHeaderTimeoutNoticeIsPersisted: the record of what happened has to
// outlive the turn, because a turn that ends this way writes no assistant
// message — the session file would otherwise end at the user's question.
func TestHeaderTimeoutNoticeIsPersisted(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{{err: headerTimeoutErr()}}}
	a, s, _ := storeAgent(t, p, CompactionConfig{})
	a.Retry = RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Infinite: true}
	submitHistory(t, s, "hi")
	_, _ = a.Run(context.Background(), "sys", []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}},
	})
	found := false
	for _, e := range s.Entries() {
		ce, ok := e.(*session.CustomEntry)
		if ok && ce.CustomType == "recovery_header_timeout" {
			found = true
		}
	}
	if !found {
		t.Fatal("no recovery_header_timeout notice in the session file")
	}
}
