package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// namedProvider is a fakeProvider with its own provider key, so a chain
// switch or a credential rotation can be told apart in the assertions.
type namedProvider struct {
	name string
	*fakeProvider
}

func (p *namedProvider) Name() string { return p.name }

func named(name string, p *fakeProvider) *namedProvider {
	return &namedProvider{name: name, fakeProvider: p}
}

func okScript(text string) fakeScript {
	return fakeScript{events: []ai.Event{{Type: ai.EventStart}, textEvent(text), doneEvent(text)}}
}

// usageLimitErr is a quota verdict: it must be classified as a usage limit,
// not as a per-minute rate blip.
func usageLimitErr() error {
	return &ai.HTTPError{API: "openai-completions", Status: 429,
		Body: `{"error":{"type":"usage_limit","message":"You have exceeded your current quota, retry after 2 hours"}}`}
}

func userHistory() []ai.Message {
	return []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}}
}

// newFallbackAgent wires a minimal but realistic agent: one primary
// provider, a scripted fallback chain, and the live fallback state.
func newFallbackAgent(primary ai.Provider, model string, fus []FailoverTarget) *Agent {
	return &Agent{
		Provider:  primary,
		Tools:     tool.NewRegistry(),
		Retry:     fastRetry(),
		Model:     model,
		Failovers: fus,
	}
}

func TestDetectUsageLimit(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		want  bool
		reset bool
	}{
		{"typed", &UsageLimitError{Message: "spent"}, true, false},
		{"typed banked", &UsageLimitError{Message: "spent", Banked: true}, true, false},
		{"quota body", &ai.HTTPError{API: "x", Status: 429, Body: `{"message":"quota exceeded"}`}, true, false},
		{"plan denial", &ai.HTTPError{API: "x", Status: 403, Body: "account policy denial: cyber_policy"}, true, false},
		{"credits", &ai.HTTPError{API: "x", Status: 402, Body: "insufficient credits"}, true, false},
		{"retry hint", &ai.HTTPError{API: "x", Status: 429, Body: "usage limit reached, retry after 30s"}, true, true},
		{"hour hint", &ai.HTTPError{API: "x", Status: 429, Body: "usage limit reached, resets in 2 hours"}, true, true},
		{"plain rate limit stays transient", &ai.HTTPError{API: "x", Status: 429, Body: "rate limit exceeded, slow down"}, false, false},
		{"5xx is not a quota verdict", &ai.HTTPError{API: "x", Status: 503, Body: "quota exceeded"}, false, false},
		{"non-http", context.DeadlineExceeded, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ul, got := detectUsageLimit(tc.err, "onegw", "free")
			if got != tc.want {
				t.Fatalf("detectUsageLimit = %v, want %v", got, tc.want)
			}
			if !got {
				return
			}
			if ul.Provider != "onegw" || ul.Model != "free" {
				t.Fatalf("attribution = %s/%s", ul.Provider, ul.Model)
			}
			if tc.reset && ul.ResetAt.Before(time.Now()) {
				t.Fatalf("reset hint not parsed: %v", ul.ResetAt)
			}
		})
	}
}

// TestUsageLimitRotatesCredentialAcrossKeys: a spent quota on one models.yml
// key rotates to the sibling and keeps serving the SAME model, without
// spending a chain step.
func TestUsageLimitRotatesCredentialAcrossKeys(t *testing.T) {
	first := &fakeProvider{calls: []fakeScript{{err: usageLimitErr()}}}
	second := &fakeProvider{calls: []fakeScript{okScript("rotated")}}
	a := newFallbackAgent(named("onegw", first), "free", nil)
	a.Fallback = &FallbackState{}
	rotations := 0
	a.Fallback.Rotate = func(provider string) (ai.Provider, bool) {
		rotations++
		if provider != "onegw" {
			t.Fatalf("rotate(%q), want the failing provider", provider)
		}
		if rotations > 1 {
			return nil, false
		}
		return named("onegw", second), true
	}

	msg, err := a.Run(context.Background(), "sys", userHistory())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if msg.Text() != "rotated" {
		t.Fatalf("final = %q", msg.Text())
	}
	if len(second.gotReqs) != 1 || second.gotReqs[0].Model != "free" {
		t.Fatalf("rotated requests = %+v, want one request for model free", second.gotReqs)
	}
	if a.curTarget != 0 {
		t.Fatalf("curTarget = %d, want 0 (rotation must not spend a chain step)", a.curTarget)
	}
	if first.i != 1 {
		t.Fatalf("primary stream calls = %d, want 1", first.i)
	}
}

// TestUsageLimitStepsChainWhenPoolExhausted: with no sibling credential the
// ladder steps the chain, and every target failing surfaces the error
// instead of looping.
func TestUsageLimitStepsChainWhenPoolExhausted(t *testing.T) {
	first := &fakeProvider{calls: []fakeScript{{err: usageLimitErr()}}}
	// The last target also runs the bounded escalation rounds (loop.go):
	// (1+MaxRetries) calls per round × (1+maxEscalationRounds) rounds.
	second := &fakeProvider{calls: func() []fakeScript {
		n := (fastRetry().MaxRetries + 1) * (maxEscalationRounds + 1)
		scripts := make([]fakeScript, n)
		for i := range scripts {
			scripts[i] = fakeScript{err: usageLimitErr()}
		}
		return scripts
	}()}
	a := newFallbackAgent(named("onegw", first), "free", []FailoverTarget{
		{Provider: named("other", second), Model: "dev", ContextWindow: 64000},
	})
	a.Fallback = &FallbackState{}

	_, err := a.Run(context.Background(), "sys", userHistory())
	if err == nil {
		t.Fatal("Run: expected the usage limit to surface once every target failed")
	}
	if !strings.Contains(err.Error(), "quota") {
		t.Fatalf("err = %v, want the provider's usage-limit body", err)
	}
	if a.curTarget != 1 {
		t.Fatalf("curTarget = %d, want 1", a.curTarget)
	}
	if want := (fastRetry().MaxRetries + 1) * (maxEscalationRounds + 1); second.i != want {
		t.Fatalf("fallback stream calls = %d, want %d (drained ladder + bounded escalation rounds; no infinite loop)", second.i, want)
	}
}

// TestUsageLimitSkipsProviderWithBankedReset: a provider whose quota resets
// in two hours must not be re-selected, even on a different model id.
func TestUsageLimitSkipsProviderWithBankedReset(t *testing.T) {
	first := &fakeProvider{calls: []fakeScript{{err: usageLimitErr()}}}
	sibling := &fakeProvider{calls: []fakeScript{okScript("sibling")}}
	other := &fakeProvider{calls: []fakeScript{okScript("other")}}
	a := newFallbackAgent(named("onegw", first), "free", []FailoverTarget{
		{Provider: named("onegw", sibling), Model: "dev", ContextWindow: 64000},
		{Provider: named("other", other), Model: "dev", ContextWindow: 64000},
	})
	a.Fallback = &FallbackState{}

	msg, err := a.Run(context.Background(), "sys", userHistory())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if msg.Text() != "other" {
		t.Fatalf("final = %q, want the provider without a banked reset", msg.Text())
	}
	if sibling.i != 0 {
		t.Fatal("the same provider was re-selected while its reset was pending")
	}
	if until := a.Fallback.banked["onegw"]; !until.After(time.Now()) {
		t.Fatalf("banked reset = %v, want a future timestamp", until)
	}
	if a.curTarget != 2 {
		t.Fatalf("curTarget = %d, want 2", a.curTarget)
	}
}

// TestBankedResetRedeemedBeforeRotating: when a stored reset can unblock the
// pool, it is spent before rotating away (Codex auto-reset).
func TestBankedResetRedeemedBeforeRotating(t *testing.T) {
	first := &fakeProvider{calls: []fakeScript{{err: usageLimitErr()}, okScript("after-reset")}}
	a := newFallbackAgent(named("onegw", first), "free", nil)
	a.Fallback = &FallbackState{Reserve: ReserveConfirm}
	redeems := 0
	a.Fallback.RedeemReset = func(provider string) bool { redeems++; return true }
	rotates := 0
	a.Fallback.Rotate = func(string) (ai.Provider, bool) { rotates++; return nil, false }

	msg, err := a.Run(context.Background(), "sys", userHistory())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if msg.Text() != "after-reset" {
		t.Fatalf("final = %q", msg.Text())
	}
	if redeems != 1 || rotates != 0 {
		t.Fatalf("redeems = %d, rotates = %d; want the reset spent before any rotation", redeems, rotates)
	}
	if a.curTarget != 0 || !a.Fallback.redeemed {
		t.Fatalf("curTarget = %d, redeemed = %v; want the same provider retried", a.curTarget, a.Fallback.redeemed)
	}
}

// TestRotationIsBoundedPerTurn: a Rotate seam that always claims success
// must not spin the turn forever.
func TestRotationIsBoundedPerTurn(t *testing.T) {
	calls := 0
	failing := func() ai.Provider {
		calls++
		return named("onegw", &fakeProvider{calls: []fakeScript{{err: usageLimitErr()}, {err: usageLimitErr()}, {err: usageLimitErr()}, {err: usageLimitErr()}, {err: usageLimitErr()}, {err: usageLimitErr()}}})
	}
	a := newFallbackAgent(failing(), "free", nil)
	a.Fallback = &FallbackState{}
	rotations := 0
	a.Fallback.Rotate = func(string) (ai.Provider, bool) { rotations++; return failing(), true }

	if _, err := a.Run(context.Background(), "sys", userHistory()); err == nil {
		t.Fatal("Run: expected the usage limit to surface")
	}
	if rotations != maxRotationsPerProvider {
		t.Fatalf("rotations = %d, want the cap %d", rotations, maxRotationsPerProvider)
	}
	if calls > maxRotationsPerProvider+6 {
		t.Fatalf("stream calls = %d, want a bounded ladder", calls)
	}
}

// TestReserveThresholdAutoSwitchesSilently: near-quota usage moves to the
// next chain entry before a turn is spent on the spent target.
func TestReserveThresholdAutoSwitchesSilently(t *testing.T) {
	primary := &fakeProvider{calls: []fakeScript{okScript("primary")}}
	fallback := &fakeProvider{calls: []fakeScript{okScript("fallback")}}
	a := newFallbackAgent(named("onegw", primary), "free", []FailoverTarget{
		{Provider: named("other", fallback), Model: "dev", ContextWindow: 64000},
	})
	a.Fallback = &FallbackState{Reserve: ReserveAuto, ReserveFraction: 0.1, UsageProbe: func(string, string) (float64, bool) { return 0.02, true }}
	notices := 0
	a.Fallback.Notify = func(string) { notices++ }

	msg, err := a.Run(context.Background(), "sys", userHistory())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if msg.Text() != "fallback" {
		t.Fatalf("final = %q, want the fallback target to serve", msg.Text())
	}
	if primary.i != 0 {
		t.Fatal("the near-quota target was streamed anyway")
	}
	if notices != 0 {
		t.Fatalf("notices = %d, want silent switching in auto mode", notices)
	}
}

// TestReserveThresholdConfirmNotifies: confirm mode surfaces the notice
// before moving (the TUI path; print mode logs it).
func TestReserveThresholdConfirmNotifies(t *testing.T) {
	primary := &fakeProvider{calls: []fakeScript{okScript("primary")}}
	fallback := &fakeProvider{calls: []fakeScript{okScript("fallback")}}
	a := newFallbackAgent(named("onegw", primary), "free", []FailoverTarget{
		{Provider: named("other", fallback), Model: "dev", ContextWindow: 64000},
	})
	a.Fallback = &FallbackState{Reserve: ReserveConfirm, ReserveFraction: 0.1, UsageProbe: func(string, string) (float64, bool) { return 0.05, true }}
	var notices []string
	a.Fallback.Notify = func(msg string) { notices = append(notices, msg) }

	msg, err := a.Run(context.Background(), "sys", userHistory())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if msg.Text() != "fallback" {
		t.Fatalf("final = %q", msg.Text())
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "near its quota") {
		t.Fatalf("notices = %v, want one near-quota notice", notices)
	}
}

// TestReserveThresholdOffIgnoresUsage: off (the default) never consults the
// probe, and an unknown probe never moves either.
func TestReserveThresholdOffIgnoresUsage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state *FallbackState
	}{
		{"off", &FallbackState{Reserve: ReserveOff, ReserveFraction: 0.1, UsageProbe: func(string, string) (float64, bool) { return 0.0, true }}},
		{"unknown usage", &FallbackState{Reserve: ReserveAuto, ReserveFraction: 0.1, UsageProbe: func(string, string) (float64, bool) { return 0, false }}},
		{"nil state", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			primary := &fakeProvider{calls: []fakeScript{okScript("primary")}}
			fallback := &fakeProvider{calls: []fakeScript{okScript("fallback")}}
			a := newFallbackAgent(named("onegw", primary), "free", []FailoverTarget{
				{Provider: named("other", fallback), Model: "dev", ContextWindow: 64000},
			})
			a.Fallback = tc.state
			msg, err := a.Run(context.Background(), "sys", userHistory())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if msg.Text() != "primary" || a.curTarget != 0 {
				t.Fatalf("final = %q, curTarget = %d; want the primary to serve", msg.Text(), a.curTarget)
			}
		})
	}
}

// TestCooldownRevertRestoresPrimaryWithModelChange: once the fallback
// cooldown expires the primary comes back, and both the fallback and the
// restore are recorded as model_change entries.
func TestCooldownRevertRestoresPrimaryWithModelChange(t *testing.T) {
	store := session.OpenMem("t", "t")
	primary := named("onegw", &fakeProvider{})
	fallback := named("other", &fakeProvider{})
	a := &Agent{Provider: primary, Model: "free", Store: store,
		Compaction: CompactionConfig{ContextWindow: 200000},
		Failovers:  []FailoverTarget{{Provider: fallback, Model: "dev", ContextWindow: 1000000}},
	}
	a.ArmFallback(&config.Settings{})
	a.Fallback.Cooldown = time.Minute

	a.switchTarget(1, "recovery")
	if a.Provider != ai.Provider(fallback) || a.curTarget != 1 {
		t.Fatalf("switchTarget did not activate the fallback: curTarget = %d", a.curTarget)
	}
	if a.Compaction.ContextWindow != 1000000 {
		t.Fatalf("window = %d, want the fallback's 1000000", a.Compaction.ContextWindow)
	}

	// The cooldown expires (and with it the primary's pin).
	a.Fallback.revertAt = time.Now().Add(-time.Second)
	a.Fallback.cooldowns = map[string]time.Time{}
	a.fallbackPreTurn()

	if a.Provider != ai.Provider(primary) || a.Model != "free" || a.curTarget != 0 {
		t.Fatalf("revert failed: provider %v model %s curTarget %d", a.Provider.Name(), a.Model, a.curTarget)
	}
	if a.Compaction.ContextWindow != 200000 {
		t.Fatalf("window = %d, want the primary's 200000 restored", a.Compaction.ContextWindow)
	}
	if a.Fallback.switched {
		t.Fatal("state still marked as switched after the revert")
	}

	var changes []*session.ModelChangeEntry
	for _, e := range store.Entries() {
		if mc, ok := e.(*session.ModelChangeEntry); ok {
			changes = append(changes, mc)
		}
	}
	if len(changes) != 2 {
		t.Fatalf("model_change entries = %d, want 2 (fallback + restore)", len(changes))
	}
	if changes[0].Model != "other/dev" || !changes[0].ResolvedModelIsFallback {
		t.Fatalf("fallback entry = %+v, want other/dev marked as a fallback", changes[0])
	}
	if changes[1].Model != "onegw/free" || changes[1].ResolvedModelIsFallback {
		t.Fatalf("restore entry = %+v, want onegw/free unmarked", changes[1])
	}
}

// TestRevertNeverKeepsFallback: fallbackRevertPolicy=never keeps the
// fallback for the session.
func TestRevertNeverKeepsFallback(t *testing.T) {
	fallback := named("other", &fakeProvider{})
	primary := named("onegw", &fakeProvider{})
	a := &Agent{Provider: primary, Model: "free",
		Failovers: []FailoverTarget{{Provider: fallback, Model: "dev"}},
	}
	a.ArmFallback(&config.Settings{Retry: config.RetrySettings{FallbackRevertPolicy: config.RevertNever}})
	a.Fallback.Cooldown = time.Millisecond
	a.switchTarget(1, "recovery")
	a.Fallback.revertAt = time.Now().Add(-time.Second)
	a.Fallback.cooldowns = map[string]time.Time{}
	a.fallbackPreTurn()
	if a.curTarget != 1 || a.Provider != ai.Provider(fallback) {
		t.Fatalf("policy never must keep the fallback: curTarget = %d", a.curTarget)
	}
}

// TestPrewalkSwitchIsNotAutoReverted: a deliberate model handoff (prewalk,
// plan-yolo) is not a fallback and must survive the cooldown.
func TestPrewalkSwitchIsNotAutoReverted(t *testing.T) {
	target := named("plan", &fakeProvider{})
	primary := named("onegw", &fakeProvider{})
	a := &Agent{Provider: primary, Model: "free",
		Failovers: []FailoverTarget{{Provider: target, Model: "big"}},
	}
	a.ArmFallback(&config.Settings{})
	a.Fallback.Cooldown = time.Millisecond
	a.switchTarget(1, "prewalk")
	if a.Fallback.switched {
		t.Fatal("a prewalk handoff must not arm the revert policy")
	}
	a.Fallback.revertAt = time.Time{}
	a.fallbackPreTurn()
	if a.curTarget != 1 || a.Provider != ai.Provider(target) {
		t.Fatal("the prewalk target was reverted")
	}
}

// TestBankedResetFlagConsultedWithoutWindow: an adapter that knows a stored
// reset exists (Banked) is asked to spend it even when the error carried no
// reset window.
func TestBankedResetFlagConsultedWithoutWindow(t *testing.T) {
	first := &fakeProvider{calls: []fakeScript{{err: &UsageLimitError{Message: "spent", Banked: true}}, okScript("after-reset")}}
	a := newFallbackAgent(named("onegw", first), "free", nil)
	a.Fallback = &FallbackState{}
	redeems := 0
	a.Fallback.RedeemReset = func(string) bool { redeems++; return true }

	msg, err := a.Run(context.Background(), "sys", userHistory())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if msg.Text() != "after-reset" || redeems != 1 || a.curTarget != 0 {
		t.Fatalf("msg = %q, redeems = %d, curTarget = %d", msg.Text(), redeems, a.curTarget)
	}
}

// TestParseResetHintWindows pins the reset-window parsing: the provider's
// own window is what suppresses the target, so a mis-parsed one either
// retries too early or hides a usable provider.
func TestParseResetHintWindows(t *testing.T) {
	now := time.Now()
	for _, body := range []string{"retry after 30s", "retry after 90 seconds", "resets in 2 hours", "try again in 45m"} {
		if got := parseResetHint(body); !got.After(now) {
			t.Fatalf("parseResetHint(%q) = %v, want a future window", body, got)
		}
	}
	for _, body := range []string{"no window here", "retry after 3 fortnights", "retry after 0s"} {
		if got := parseResetHint(body); !got.IsZero() {
			t.Fatalf("parseResetHint(%q) = %v, want no window", body, got)
		}
	}
}
