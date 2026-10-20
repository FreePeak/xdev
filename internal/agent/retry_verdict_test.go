package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// The ladder's rule: a verdict that cannot be repaired by re-sending the
// same request (a credential denial, a client-posture gate) ends the turn —
// UNLESS the session still has work that has not been answered. Then the
// turn is not finished, so the ladder keeps going: the user can fix the key,
// switch client or start a paid tier while the turn waits, and the next
// attempt picks it up. The store is what says so — the user message is
// persisted before the run (submitHistory mirrors the CLI/TUI flow).

// TestAuthRetriesWhileSessionUnfinished: a 401 with an unanswered user
// message in the store is retried, and the turn completes when the provider
// starts serving.
func TestAuthRetriesWhileSessionUnfinished(t *testing.T) {
	auth := &ai.HTTPError{API: "openai-completions", Status: 401, Body: `{"error":{"type":"authentication_error","message":"invalid api key"}}`}
	p := &fakeProvider{calls: []fakeScript{
		{err: auth},
		{err: auth},
		{events: []ai.Event{textEvent("ok"), doneEvent("ok")}},
	}}
	a, s, p := storeAgent(t, p, CompactionConfig{})
	hist := submitHistory(t, s, "hi")
	a.Retry = RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
	final, err := a.Run(context.Background(), "sys", hist)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Text() != "ok" {
		t.Fatalf("final = %q, want %q", final.Text(), "ok")
	}
	if len(p.gotReqs) != 3 {
		t.Fatalf("stream calls = %d, want 3 (the 401 is retried while the session is unfinished)", len(p.gotReqs))
	}
}

// TestAuthFailsFastWhenSessionAnswered: the same 401, but the store's last
// message is the assistant's answer — the session has nothing pending, so
// there is no work to protect and the verdict surfaces on the first request.
func TestAuthFailsFastWhenSessionAnswered(t *testing.T) {
	auth := &ai.HTTPError{API: "openai-completions", Status: 401, Body: `{"error":{"type":"authentication_error","message":"invalid api key"}}`}
	p := &fakeProvider{calls: []fakeScript{
		{err: auth},
		{err: auth},
		{err: auth},
	}}
	a, s, p := storeAgent(t, p, CompactionConfig{})
	submitHistory(t, s, "hi")
	if err := s.Append(&session.MessageEntry{Message: ai.Message{
		Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "answered"}},
	}}); err != nil {
		t.Fatal(err)
	}
	a.Retry = RetryPolicy{MaxRetries: 5, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
	_, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}})
	if err == nil {
		t.Fatal("Run: expected the 401 surfaced")
	}
	if !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("err = %v", err)
	}
	if len(p.gotReqs) != 1 {
		t.Fatalf("stream calls = %d, want 1 (nothing pending — the verdict ends the turn)", len(p.gotReqs))
	}
}

// TestVendorGateRetriesWhileSessionUnfinished: the client-posture gate
// (free tier) is the same shape of verdict — the provider refused THIS
// client — so an unanswered session keeps the turn alive.
func TestVendorGateRetriesWhileSessionUnfinished(t *testing.T) {
	gate := &ai.HTTPError{API: ai.APIOpenAICompletions, Status: 403, Body: freeTierBody}
	p := &fakeProvider{calls: []fakeScript{
		{err: gate},
		{events: []ai.Event{textEvent("ok"), doneEvent("ok")}},
	}}
	a, s, p := storeAgent(t, p, CompactionConfig{})
	hist := submitHistory(t, s, "hi")
	a.Retry = RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
	final, err := a.Run(context.Background(), "sys", hist)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Text() != "ok" {
		t.Fatalf("final = %q, want %q", final.Text(), "ok")
	}
	if len(p.gotReqs) != 2 {
		t.Fatalf("stream calls = %d, want 2 (the gate is retried while the session is unfinished)", len(p.gotReqs))
	}
}

// TestVendorGateFailsFastWhenSessionAnswered: the gate's fail-fast
// invariant is unchanged for a session with nothing pending.
func TestVendorGateFailsFastWhenSessionAnswered(t *testing.T) {
	gate := &ai.HTTPError{API: ai.APIOpenAICompletions, Status: 403, Body: freeTierBody}
	p := &fakeProvider{calls: []fakeScript{{err: gate}, {err: gate}, {err: gate}}}
	a, s, p := storeAgent(t, p, CompactionConfig{})
	submitHistory(t, s, "hi")
	if err := s.Append(&session.MessageEntry{Message: ai.Message{
		Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "answered"}},
	}}); err != nil {
		t.Fatal(err)
	}
	a.Retry = RetryPolicy{MaxRetries: 4, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Infinite: true, RetryAllErrors: true}
	_, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}})
	if err == nil {
		t.Fatal("Run: expected the free-tier 403 surfaced")
	}
	if len(p.gotReqs) != 1 {
		t.Fatalf("stream calls = %d, want 1 (nothing pending — the gate ends the turn)", len(p.gotReqs))
	}
}

// TestVerdictRetryAnnouncesWait pins the visible-wait contract: a verdict
// the ladder is riding out raises one notice per round on the event stream,
// so a turn that waits on a credential reads as waiting, not as hung.
func TestVerdictRetryAnnouncesWait(t *testing.T) {
	auth := &ai.HTTPError{API: "openai-completions", Status: 401, Body: `{"error":{"type":"authentication_error","message":"invalid api key"}}`}
	p := &fakeProvider{calls: []fakeScript{
		{err: auth},
		{events: []ai.Event{textEvent("ok"), doneEvent("ok")}},
	}}
	a, s, p := storeAgent(t, p, CompactionConfig{})
	hist := submitHistory(t, s, "hi")
	var mu sync.Mutex
	var notices []*VerdictRetryError
	a.Hooks = TurnHooksFunc{
		OnEventF: func(ev ai.Event) {
			if ev.Type != ai.EventError {
				return
			}
			var vr *VerdictRetryError
			if !errors.As(ev.Err, &vr) {
				return // the provider's own error event is not a recovery notice
			}
			mu.Lock()
			notices = append(notices, vr)
			mu.Unlock()
		},
		OnMessageEndF:    func(m *ai.Message) { _ = s.Append(&session.MessageEntry{Message: *m}) },
		OnToolResultMsgF: func(m *ai.Message) { _ = s.Append(&session.MessageEntry{Message: *m}) },
	}
	a.Retry = RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
	if _, err := a.Run(context.Background(), "sys", hist); err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(notices) != 1 {
		t.Fatalf("verdict notices = %d, want 1", len(notices))
	}
	vr := notices[0]
	if vr.Round != 1 {
		t.Fatalf("notice round = %d, want 1", vr.Round)
	}
	if !strings.Contains(vr.Error(), "401") {
		t.Fatalf("notice = %v, want the provider's verdict in the text", vr)
	}
}
