package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// syncProvider is a race-safe fakeProvider for tests that observe the
// reviewer from another goroutine (the roster fan-out and the child advisor
// both feed asynchronously, exactly like the session host does).
type syncProvider struct {
	mu      sync.Mutex
	scripts []fakeScript
	i       int
	reqs    []ai.StreamRequest
}

func (p *syncProvider) Stream(_ context.Context, req ai.StreamRequest) (<-chan ai.Event, error) {
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	if p.i >= len(p.scripts) {
		p.mu.Unlock()
		return nil, errors.New("script exhausted")
	}
	sc := p.scripts[p.i]
	p.i++
	p.mu.Unlock()
	ch := make(chan ai.Event, 32)
	go func() {
		defer close(ch)
		if sc.err != nil {
			ch <- ai.Errorf(sc.err)
			return
		}
		for _, ev := range sc.events {
			ch <- ev
		}
	}()
	return ch, nil
}

func (p *syncProvider) Name() string { return "syncfake" }
func (p *syncProvider) API() string  { return "fake-api" }

func (p *syncProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.i
}

func (p *syncProvider) request(n int) ai.StreamRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= len(p.reqs) {
		return ai.StreamRequest{}
	}
	return p.reqs[n]
}

// deltaPrompt is the reviewer's rendered delta as the model first saw it —
// the review's opening turn (one message, unlike the tool-result wrap-up).
func (p *syncProvider) deltaPrompt() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, req := range p.reqs {
		if len(req.Messages) == 1 && strings.Contains(messageText(req.Messages[0]), "delta to review") {
			return messageText(req.Messages[0])
		}
	}
	return ""
}

// reviews counts review openings (each opening is a single-message
// request; the wrap-up turn replays the transcript plus the tool result).
func (p *syncProvider) reviews() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, req := range p.reqs {
		if len(req.Messages) == 1 && strings.Contains(messageText(req.Messages[0]), "delta to review") {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// steeringTexts snapshots the primary's queued steering in order.
func steeringTexts(a *Agent) []string {
	a.steerMu.Lock()
	defer a.steerMu.Unlock()
	out := make([]string, 0, len(a.steering))
	for _, s := range a.steering {
		out = append(out, s.Text)
	}
	return out
}

// reviewScripts builds the provider script for whole reviews: each advise
// call is followed by the reviewer's wrap-up turn, so one Feed consumes two
// scripts.
func reviewScripts(calls ...[]ai.Event) []fakeScript {
	out := make([]fakeScript, 0, 2*len(calls))
	for _, c := range calls {
		out = append(out, fakeScript{events: c}, fakeScript{events: doneEvents("reviewed")})
	}
	return out
}

func newReviewer(t *testing.T, scripts []fakeScript) (*Advisor, *syncProvider, *tool.Registry) {
	t.Helper()
	rev := &syncProvider{scripts: scripts}
	reg := tool.NewRegistry()
	reg.Register(tool.NewReadTool())
	RegisterAdviseTool(reg)
	return NewAdvisor(rev, "m", reg), rev, reg
}

// TestAdvisorImmuneTurnsRoutesInterruptsAsAsides: the first concern
// interrupts, the next advisor.immuneTurns primary turns ride as asides,
// then the interrupt is available again. Reset clears the window.
func TestAdvisorImmuneTurnsRoutesInterruptsAsAsides(t *testing.T) {
	adv, primary, _ := newAdvisorFixture(t, reviewScripts(
		adviseEvents(AdviseConcern, "first concern"),
		adviseEvents(AdviseConcern, "second concern"),
		adviseEvents(AdviseConcern, "third concern"),
		adviseEvents(AdviseConcern, "fourth concern"),
		adviseEvents(AdviseConcern, "fifth concern"),
	))
	adv.ImmuneTurns = 2
	hist := []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "t0"}}}}
	for i := 1; i <= 4; i++ {
		adv.Feed(context.Background(), hist)
		hist = append(hist, ai.Message{Role: ai.RoleAssistant,
			Content: []ai.Block{ai.TextBlock{Text: fmt.Sprintf("turn %d", i)}}})
	}
	// Reset drops the immune window: the very next concern interrupts.
	adv.Reset(hist)
	hist = append(hist, ai.Message{Role: ai.RoleAssistant,
		Content: []ai.Block{ai.TextBlock{Text: "turn 5"}}})
	adv.Feed(context.Background(), hist)

	got := steeringTexts(primary)
	want := []string{
		"advisor (concern): first concern",
		"advisor aside (concern): second concern",
		"advisor aside (concern): third concern",
		"advisor (concern): fourth concern",
		"advisor (concern): fifth concern",
	}
	if len(got) != len(want) {
		t.Fatalf("steering = %q, want %d notes", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("steering[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestAdvisorSyncBacklogBoundsCatchUp: with a backlog, one review covers
// only the most recent N turns; off, it covers the whole delta.
func TestAdvisorSyncBacklogBoundsCatchUp(t *testing.T) {
	fiveTurns := func() []ai.Message {
		var hist []ai.Message
		for i := 1; i <= 5; i++ {
			hist = append(hist,
				ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: fmt.Sprintf("user-%d", i)}}},
				ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: fmt.Sprintf("assistant-%d", i)}}})
		}
		return hist
	}

	t.Run("bounded", func(t *testing.T) {
		adv, rev, _ := newReviewer(t, reviewScripts(adviseEvents(AdviseNit, "ok")))
		adv.Primary = &Agent{}
		adv.SyncBacklog = 2
		adv.Feed(context.Background(), fiveTurns())
		prompt := rev.deltaPrompt()
		for _, want := range []string{"user-4", "assistant-4", "user-5", "assistant-5"} {
			if !strings.Contains(prompt, want) {
				t.Errorf("catch-up review lost %q:\n%s", want, prompt)
			}
		}
		for _, gone := range []string{"assistant-1", "assistant-2", "assistant-3"} {
			if strings.Contains(prompt, gone) {
				t.Errorf("catch-up review still carries skipped %q:\n%s", gone, prompt)
			}
		}
	})

	t.Run("off feeds the whole delta", func(t *testing.T) {
		adv, rev, _ := newReviewer(t, reviewScripts(adviseEvents(AdviseNit, "ok")))
		adv.Primary = &Agent{}
		adv.Feed(context.Background(), fiveTurns())
		if prompt := rev.deltaPrompt(); !strings.Contains(prompt, "assistant-1") {
			t.Errorf("unbounded review dropped the oldest turn:\n%s", prompt)
		}
	})
}

// TestAdvisorInjectsWatchdogGuidance: WATCHDOG.md content reaches the
// reviewer as its attention block, after the base contract.
func TestAdvisorInjectsWatchdogGuidance(t *testing.T) {
	adv, rev, _ := newReviewer(t, reviewScripts(adviseEvents(AdviseNit, "ok")))
	adv.Primary = &Agent{}
	adv.Guidance = "watch the durable queue in src/jobs"
	adv.Feed(context.Background(), []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "work"}}},
	})
	sys := rev.request(0).System
	if !strings.Contains(sys, AdvisorSystemPrompt) {
		t.Fatalf("reviewer contract missing from system prompt:\n%s", sys)
	}
	if !strings.Contains(sys, "<attention>") || !strings.Contains(sys, "watch the durable queue in src/jobs") {
		t.Fatalf("WATCHDOG.md guidance not injected:\n%s", sys)
	}
}

// TestDiscoverWatchdogGuidanceWalksUp: project WATCHDOG.md files load from
// the repo root down to cwd, ancestors first, and each file is bounded.
func TestDiscoverWatchdogGuidanceWalksUp(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, ".git"))
	leaf := filepath.Join(root, "a", "b")
	mustMkdir(t, leaf)
	mustWrite(t, filepath.Join(root, "WATCHDOG.md"), "root guidance")
	mustWrite(t, filepath.Join(leaf, "WATCHDOG.md"), "leaf guidance")

	// The walk stops at the repo root: nothing above it is even consulted.
	if dirs := watchdogWalkDirs(leaf); len(dirs) != 3 || dirs[0] != root || dirs[2] != leaf {
		t.Fatalf("walk = %v, want [%s <a> %s]", dirs, root, leaf)
	}
	got := DiscoverWatchdogGuidance(leaf, "")
	if !strings.Contains(got, "root guidance") || !strings.Contains(got, "leaf guidance") {
		t.Fatalf("guidance = %q, want both files", got)
	}
	if strings.Index(got, "root guidance") > strings.Index(got, "leaf guidance") {
		t.Fatalf("narrower guidance must come last:\n%s", got)
	}

	// Bounded: an oversized file is truncated instead of eating the prompt.
	huge := strings.Repeat("x", watchdogGuidanceMax+512)
	mustWrite(t, filepath.Join(leaf, "WATCHDOG.md"), huge)
	if got := DiscoverWatchdogGuidance(leaf, ""); len(got) > watchdogGuidanceMax {
		t.Fatalf("guidance = %d bytes, want <= %d", len(got), watchdogGuidanceMax)
	}
}

// TestDiscoverWatchdogRoster: WATCHDOG.yml entries parse (root before leaf),
// shared instructions concatenate, and a broken file is skipped.
func TestDiscoverWatchdogRoster(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, ".git"))
	leaf := filepath.Join(root, "a", "b")
	mustMkdir(t, leaf)
	mustWrite(t, filepath.Join(root, "WATCHDOG.yml"), `
instructions: shared review priorities
advisors:
  - name: Architecture
    model: onegw/dev
    patterns: ["src/api", "^module "]
  - name: Fixer
    patterns: ["flaky test"]
`)
	mustWrite(t, filepath.Join(leaf, "WATCHDOG.yml"), `
advisors:
  - name: Leaf
    model: onegw/free
  - model: onegw/free
`)

	entries, instructions := DiscoverWatchdogRoster(leaf, "")
	if !strings.Contains(instructions, "shared review priorities") {
		t.Errorf("shared instructions lost: %q", instructions)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %+v, want 3 (nameless dropped)", entries)
	}
	if entries[0].Name != "Architecture" || entries[1].Name != "Fixer" || entries[2].Name != "Leaf" {
		t.Errorf("roster order = %s, %s, %s (root before leaf)", entries[0].Name, entries[1].Name, entries[2].Name)
	}
	if got := entries[0].Patterns; len(got) != 2 || got[0] != "src/api" || got[1] != "^module " {
		t.Errorf("patterns = %q", got)
	}
	if entries[2].Model != "onegw/free" {
		t.Errorf("leaf model = %q", entries[2].Model)
	}

	// A file that does not parse is skipped, never fatal.
	broken := t.TempDir()
	mustMkdir(t, filepath.Join(broken, ".git"))
	mustWrite(t, filepath.Join(broken, "WATCHDOG.yml"), "advisors: [oops\n")
	if got, _ := DiscoverWatchdogRoster(broken, ""); len(got) != 0 {
		t.Fatalf("broken roster must yield nothing, got %+v", got)
	}
}

// TestMatchesPatterns covers the roster's routing rule: a regex hit or a
// case-insensitive literal hit, everything when unset.
func TestMatchesPatterns(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		text     string
		want     bool
	}{
		{"no patterns reviews everything", nil, "anything at all", true},
		{"regex match", []string{`^module\s+gpu`}, "module gpu: init failed", true},
		{"regex miss", []string{`^gpu`}, "cpu kernel", false},
		{"invalid regex falls back to substring", []string{"[unclosed"}, "line [unclosed bracket", true},
		{"substring is case-insensitive", []string{"watchdog"}, "WATCHDOG.md changed", true},
		{"empty pattern list entry ignored", []string{"", "sql"}, "run the SQL migration", true},
	}
	for _, tc := range cases {
		if got := matchesPatterns(tc.patterns, tc.text); got != tc.want {
			t.Errorf("%s: matchesPatterns(%q, %q) = %v, want %v", tc.name, tc.patterns, tc.text, got, tc.want)
		}
	}
}

// TestAdvisorRosterFansOutByPattern: only the advisor whose patterns match
// reviews; the others still consume the delta (no replay later).
func TestAdvisorRosterFansOutByPattern(t *testing.T) {
	sqlAdv, sqlRev, _ := newReviewer(t, reviewScripts(adviseEvents(AdviseConcern, "watch the query")))
	sqlAdv.Patterns = []string{"sql"}
	cssAdv, cssRev, _ := newReviewer(t, reviewScripts(adviseEvents(AdviseConcern, "watch the layout")))
	cssAdv.Patterns = []string{"css"}

	primary := &Agent{}
	facade := NewAdvisorRoster([]*Advisor{sqlAdv, cssAdv})
	facade.Primary = primary
	hist := []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "run the sql migration"}}},
		{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "applying sql now"}}},
	}
	facade.Feed(context.Background(), hist)

	waitFor(t, "matching reviewer to advise", func() bool {
		return len(steeringTexts(primary)) == 1
	})
	if got := steeringTexts(primary)[0]; got != "advisor (concern): watch the query" {
		t.Fatalf("steering = %q", got)
	}
	if got := sqlRev.reviews(); got != 1 {
		t.Fatalf("matching reviewer ran %d time(s), want 1", got)
	}
	if got := cssRev.reviews(); got != 0 {
		t.Fatalf("non-matching reviewer ran %d time(s)", got)
	}
	// The non-matching peer consumed the delta, so a later css turn is not a
	// full-transcript replay.
	waitFor(t, "non-matching reviewer cursor", func() bool {
		cssAdv.mu.Lock()
		defer cssAdv.mu.Unlock()
		return cssAdv.cursor == len(hist)
	})
	if notes := facade.Dump(); len(notes) != 1 || notes[0].Text != "watch the query" {
		t.Fatalf("facade dump = %+v", notes)
	}

	// Reset fans out to every peer, clearing the matched peer's dedupe state.
	facade.Reset(hist)
	if notes := facade.Dump(); len(notes) != 0 {
		t.Fatalf("dump after reset = %+v", notes)
	}
}

// TestTaskToolAttachesChildAdvisor: task.agentAdvisor gives the child its
// own reviewer, which is fed the child's transcript (task + turns).
func TestTaskToolAttachesChildAdvisor(t *testing.T) {
	childAdv, rev, _ := newReviewer(t, reviewScripts(adviseEvents(AdviseConcern, "recheck the migration")))
	var built *Advisor
	tt := &TaskTool{
		Provider:     &fakeProvider{calls: []fakeScript{{events: doneEvents("child step 1")}}},
		Model:        "m",
		ChildTools:   []tool.Tool{echoTool{}},
		MaxTurns:     3,
		ChildAdvisor: func() *Advisor { built = childAdv; return childAdv },
	}
	res, err := tt.Execute(context.Background(), json.RawMessage(`{"prompt":"review the migration script"}`))
	if err != nil || res.IsError {
		t.Fatalf("task execute: err=%v result=%+v", err, res)
	}
	if built == nil {
		t.Fatal("task.agentAdvisor never built a child advisor")
	}
	waitFor(t, "child advisor review", func() bool { return rev.reviews() >= 1 })
	prompt := rev.deltaPrompt()
	if !strings.Contains(prompt, "review the migration script") {
		t.Errorf("child advisor never saw the task:\n%s", prompt)
	}
	if !strings.Contains(prompt, "child step 1") {
		t.Errorf("child advisor never saw the child's turn:\n%s", prompt)
	}
	if built.Primary == nil {
		t.Error("child advisor has no steering target")
	}
}

// TestTaskToolWithoutChildAdvisorLeavesChildUnadvised: the default path is
// untouched (no reviewer provider is ever consulted).
func TestTaskToolWithoutChildAdvisorLeavesChildUnadvised(t *testing.T) {
	_, rev, _ := newReviewer(t, nil)
	tt := &TaskTool{
		Provider:   &fakeProvider{calls: []fakeScript{{events: doneEvents("child step 1")}}},
		Model:      "m",
		ChildTools: []tool.Tool{echoTool{}},
		MaxTurns:   3,
	}
	if _, err := tt.Execute(context.Background(), json.RawMessage(`{"prompt":"just work"}`)); err != nil {
		t.Fatalf("task execute: %v", err)
	}
	if got := rev.reviews(); got != 0 {
		t.Fatalf("unadvised child consulted a reviewer %d time(s)", got)
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
