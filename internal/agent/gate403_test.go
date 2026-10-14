package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
)

// freeTierBody is OpenCode's own 403 for a request from a client that is not
// OpenCode (live 2026-10-13 through onegw, which relays the upstream status
// verbatim).
const freeTierBody = `{"error":{"code":"403","message":"OpenCode's free tier can only be used from within OpenCode. Download it at https://opencode.ai and keep building.","type":"FreeTierError"}}`

// TestFreeTierGateFailsFast pins the user-reported shape: a 403 vendor-tier
// gate ends the turn on ONE request. It is a posture verdict about the client,
// not a credential one, so neither the credential ladder (which would rotate a
// key), the failover chain (which would switch models) nor the bounded
// bad-request escalation rounds (which rebuild history and re-send) can change
// the answer. At the shipped retry.infinite default the old path slept through
// maxSilentRecoveryRounds before surfacing the provider's own message.
func TestFreeTierGateFailsFast(t *testing.T) {
	gate := &ai.HTTPError{API: ai.APIOpenAICompletions, Status: 403, Body: freeTierBody}
	p := &fakeProvider{calls: []fakeScript{{err: gate}, {err: gate}, {err: gate}, {err: gate}}}
	a, _, p := storeAgent(t, p, CompactionConfig{})
	a.Retry = RetryPolicy{MaxRetries: 4, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Infinite: true, RetryAllErrors: true}
	start := time.Now()
	_, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}})
	if err == nil {
		t.Fatal("Run: expected the free-tier 403 surfaced")
	}
	if !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "FreeTierError") {
		t.Fatalf("err = %v", err)
	}
	if len(p.gotReqs) != 1 {
		t.Fatalf("stream calls = %d, want 1 (a client-posture verdict is not retried)", len(p.gotReqs))
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("turn took %s: the ladder slept on a verdict no retry can change", elapsed)
	}
}

// TestNeighbouring403VerdictsAreUnchanged: one status carries three verdicts
// behind a gateway, and only the body tells them apart. The gate must not have
// swallowed the two neighbours — a credential denial stays fail-fast, and an
// upstream hiccup relay keeps its retry even when the gate's own words appear
// in it.
func TestNeighbouring403VerdictsAreUnchanged(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		failFirst int // how many leading requests fail before one is served
		wantCalls int
	}{
		{"client-posture gate", freeTierBody, 4, 1},
		{"credential denial", `{"error":{"code":"403","message":"Forbidden","type":"auth_error"}}`, 4, 1},
		{"upstream hiccup relay", `{"error":{"code":"403","message":"Upstream request failed: [server_error] Upstream response was not valid JSON","type":"server_error"}}`, 2, 3},
		{"hiccup relay that names the free tier", `{"error":{"code":"403","message":"free tier: upstream response was not valid JSON","type":"server_error"}}`, 2, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err403 := &ai.HTTPError{API: ai.APIOpenAICompletions, Status: 403, Body: c.body}
			scripts := make([]fakeScript, 0, c.failFirst)
			for range c.failFirst {
				scripts = append(scripts, fakeScript{err: err403})
			}
			scripts = append(scripts, okScript("ok"))
			p := &fakeProvider{calls: scripts}
			a, _, p := storeAgent(t, p, CompactionConfig{})
			a.Retry = RetryPolicy{MaxRetries: 4, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Infinite: true}
			start := time.Now()
			_, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}})
			if len(p.gotReqs) != c.wantCalls {
				t.Fatalf("stream calls = %d, want %d (elapsed %s, err %v)", len(p.gotReqs), c.wantCalls, time.Since(start), err)
			}
		})
	}
}
