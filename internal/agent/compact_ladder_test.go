package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// ---- fixture helpers -------------------------------------------------------

// appendMsg persists one message the way a run's hooks do, so the fixture is
// exactly what BuildContext — and therefore the ladder — sees.
func appendMsg(t *testing.T, s *session.Store, m ai.Message) {
	t.Helper()
	if err := s.Append(&session.MessageEntry{Message: m}); err != nil {
		t.Fatal(err)
	}
}

func userText(text string) ai.Message {
	return ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: text}}}
}

func callMsg(id, name, args string) ai.Message {
	return ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{
		ai.ThinkingBlock{Thinking: "THINKING-MUST-NOT-SURVIVE"},
		ai.ToolCallBlock{ID: id, Name: name, Arguments: []byte(args)},
	}}
}

func resultMsg(id, name, text string) ai.Message {
	return ai.Message{Role: ai.RoleToolResult, ToolCallID: id, ToolName: name, Content: []ai.Block{ai.TextBlock{Text: text}}}
}

// ladderFixture persists the #24 fixture: /tmp/cfg.go is read twice (the first
// read is superseded by the second, which reaches further), a command returns
// nothing, a normal tool result comes back, and the tail question must survive
// the compaction.
func ladderFixture(t *testing.T, s *session.Store) {
	t.Helper()
	appendMsg(t, s, userText("investigate the config file"))
	appendMsg(t, s, callMsg("c1", "read", `{"offset":1,"path":"/tmp/cfg.go"}`))
	appendMsg(t, s, resultMsg("c1", "read", "1:package cfg\n2:var A = 1"))
	appendMsg(t, s, callMsg("c2", "read", `{"offset":2,"path":"/tmp/cfg.go"}`))
	appendMsg(t, s, resultMsg("c2", "read", "2:var A = 1\n3:// tail marker"))
	appendMsg(t, s, callMsg("c3", "bash", `{"command":"true"}`))
	appendMsg(t, s, resultMsg("c3", "bash", "   "))
	appendMsg(t, s, callMsg("c4", "echo", `{"text":"kept-result"}`))
	appendMsg(t, s, resultMsg("c4", "echo", "kept-result"))
	appendMsg(t, s, userText("keep this tail question"))
}

// ladderHistory is the model-visible history, the way Run's caller hands it in.
func ladderHistory(t *testing.T, s *session.Store) []ai.Message {
	t.Helper()
	res, err := session.BuildContext(s.Entries(), s.LeafID(), session.SystemPrompt{})
	if err != nil {
		t.Fatal(err)
	}
	return res.Messages
}

// ladderConfig compacts on every boundary: ContextWindow 1000 sits far below
// the reserve, so the token trigger always fires; KeepRecentTokens 5 keeps one
// message — the tail question — out of the discarded span.
func ladderConfig(methods ...string) CompactionConfig {
	return CompactionConfig{ContextWindow: 1000, KeepRecentTokens: 5, Methods: methods}
}

func compactionEntries(s *session.Store) []*session.CompactionEntry {
	var out []*session.CompactionEntry
	for _, e := range s.Entries() {
		if c, ok := e.(*session.CompactionEntry); ok {
			out = append(out, c)
		}
	}
	return out
}

// captureLogs turns logging on at info level for one test and returns the
// buffer the package writes to.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	logx.Enable(logx.LevelInfo)
	logx.SetOutput(&buf)
	t.Cleanup(func() {
		logx.Disable()
		logx.SetOutput(os.Stderr)
	})
	return &buf
}

// stubClock pins the idle trigger's clock to a caller-moved instant.
func stubClock(t *testing.T, now *time.Time) {
	t.Helper()
	old := compactionNow
	compactionNow = func() time.Time { return *now }
	t.Cleanup(func() { compactionNow = old })
}

// waitChan fails the test if ch is not signaled within the deadline.
func waitChan(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// ---- deterministic methods -------------------------------------------------

// TestPruneUselessDropsSupersededReadsAndEmptyResults pins soft's two prunes
// on the fixture: the earlier read of /tmp/cfg.go goes (the later read reached
// further), and the whitespace-only bash result goes.
func TestPruneUselessDropsSupersededReadsAndEmptyResults(t *testing.T) {
	p := &fakeProvider{}
	_, s, _ := storeAgent(t, p, ladderConfig("soft"))
	ladderFixture(t, s)
	msgs := ladderHistory(t, s)

	kept, dropped := pruneUseless(msgs)
	if dropped != 2 {
		t.Fatalf("dropped = %d, want 2 (superseded read + empty result)", dropped)
	}
	if len(kept) != len(msgs)-2 {
		t.Fatalf("kept = %d, want %d", len(kept), len(msgs)-2)
	}
	var keptText string
	for i := range kept {
		keptText += kept[i].Text() + "\n"
	}
	for _, gone := range []string{"1:package cfg", "c1", "c3"} {
		if strings.Contains(keptText, gone) {
			t.Errorf("pruned span still holds %q:\n%s", gone, keptText)
		}
	}
	for _, want := range []string{"3:// tail marker", "kept-result"} {
		if !strings.Contains(keptText, want) {
			t.Errorf("pruned span lost %q:\n%s", want, keptText)
		}
	}
}

// TestLadderSoftRetainsPrunedSpan runs the ladder end to end with soft: the
// entry records the method, the retained text is the pruned span, and the tail
// question stays in the live context.
func TestLadderSoftRetainsPrunedSpan(t *testing.T) {
	stubPressure(t, 0)
	p := &fakeProvider{}
	a, s, _ := storeAgent(t, p, ladderConfig("soft"))
	ladderFixture(t, s)
	_ = a.maybeCompact(context.Background(), ladderHistory(t, s))

	entries := compactionEntries(s)
	if len(entries) != 1 {
		t.Fatalf("compaction entries = %d, want 1", len(entries))
	}
	if entries[0].Method != methodSoft {
		t.Fatalf("method = %q, want %q", entries[0].Method, methodSoft)
	}
	text := entries[0].Summary.Text()
	if strings.Contains(text, "1:package cfg") {
		t.Errorf("soft kept the superseded read:\n%s", text)
	}
	if !strings.Contains(text, "3:// tail marker") || !strings.Contains(text, "kept-result") {
		t.Errorf("soft lost live content:\n%s", text)
	}
	// The tail is not part of the span: it must still be in the live context.
	live := ladderHistory(t, s)
	last := live[len(live)-1].Text()
	if !strings.Contains(last, "keep this tail question") {
		t.Fatalf("tail = %q, want the kept question", last)
	}
	if len(p.gotReqs) != 0 {
		t.Fatalf("soft must not call the provider, got %d calls", len(p.gotReqs))
	}
}

// TestLadderShakeElidesBodiesKeepsSkeleton pins shake: argument values and
// thinking disappear, the tool skeleton and result heads stay.
func TestLadderShakeElidesBodiesKeepsSkeleton(t *testing.T) {
	stubPressure(t, 0)
	p := &fakeProvider{}
	a, s, _ := storeAgent(t, p, ladderConfig("shake"))
	ladderFixture(t, s)
	_ = a.maybeCompact(context.Background(), ladderHistory(t, s))

	entries := compactionEntries(s)
	if len(entries) != 1 || entries[0].Method != methodShake {
		t.Fatalf("entries = %d, method = %q", len(entries), entries[0].Method)
	}
	text := entries[0].Summary.Text()
	if !strings.Contains(text, "[tool read(offset,path)]") {
		t.Errorf("skeleton lost the call shape:\n%s", text)
	}
	if !strings.Contains(text, "toolResult(read): 1:package cfg") {
		t.Errorf("skeleton lost the result head:\n%s", text)
	}
	for _, gone := range []string{"/tmp/cfg.go", "THINKING-MUST-NOT-SURVIVE"} {
		if strings.Contains(text, gone) {
			t.Errorf("shake kept %q:\n%s", gone, text)
		}
	}
	if len(p.gotReqs) != 0 {
		t.Fatalf("shake must not call the provider, got %d calls", len(p.gotReqs))
	}
}

// ---- ladder order, fallback, remote ---------------------------------------

// TestLadderHonorsMethodOrderAndRecordsTheWinner pins selection: soft wins
// over shake because it is listed first, and remote is a no-op that falls
// through — with a log line — to the next member.
func TestLadderHonorsMethodOrderAndRecordsTheWinner(t *testing.T) {
	stubPressure(t, 0)
	logs := captureLogs(t)
	p := &fakeProvider{}
	a, s, _ := storeAgent(t, p, ladderConfig("remote", "soft", "shake"))
	ladderFixture(t, s)
	_ = a.maybeCompact(context.Background(), ladderHistory(t, s))

	entries := compactionEntries(s)
	if len(entries) != 1 {
		t.Fatalf("compaction entries = %d, want 1", len(entries))
	}
	if entries[0].Method != methodSoft {
		t.Fatalf("method = %q, want the first working member %q", entries[0].Method, methodSoft)
	}
	if !strings.Contains(logs.String(), "remote (provider-native streaming) not supported") {
		t.Errorf("remote must say why it fell through, logs:\n%s", logs.String())
	}
	if len(p.gotReqs) != 0 {
		t.Fatalf("the deterministic members must not call the provider, got %d calls", len(p.gotReqs))
	}
}

// TestLadderFallsThroughOnFailure pins fallback-on-error: a member that dies
// hands the boundary to the next one in the order, and the entry records the
// member that actually produced the context.
func TestLadderFallsThroughOnFailure(t *testing.T) {
	stubPressure(t, 0)
	logs := captureLogs(t)
	RegisterCompactionMethod("snapcompact", func(context.Context, *Agent, []ai.Message) (ai.Message, error) {
		return ai.Message{}, errors.New("test: method broken")
	})
	t.Cleanup(func() { delete(compactMethodOverrides, "snapcompact") })

	p := &fakeProvider{calls: []fakeScript{{events: []ai.Event{doneEvent("SUMMARY")}}}}
	a, s, _ := storeAgent(t, p, ladderConfig("snapcompact", "handoff"))
	ladderFixture(t, s)
	_ = a.maybeCompact(context.Background(), ladderHistory(t, s))

	entries := compactionEntries(s)
	if len(entries) != 1 {
		t.Fatalf("compaction entries = %d, want 1", len(entries))
	}
	if entries[0].Method != MethodHandoff {
		t.Fatalf("method = %q, want the fallback %q", entries[0].Method, MethodHandoff)
	}
	if got := entries[0].Summary.Text(); got != "SUMMARY" {
		t.Fatalf("summary = %q, want the provider's answer", got)
	}
	if len(p.gotReqs) != 1 || p.gotReqs[0].System != compactionPrompt {
		t.Fatalf("provider calls = %d (system %q), want one summarize", len(p.gotReqs), p.gotReqs[0].System)
	}
	if !strings.Contains(logs.String(), "snapcompact: test: method broken") {
		t.Errorf("the failing member must be logged, logs:\n%s", logs.String())
	}
}

// TestLadderExhaustedOrderPersistsNothing pins the other end: when every
// member fails the boundary degrades to the pre-compaction behavior instead of
// persisting a half-result.
func TestLadderExhaustedOrderPersistsNothing(t *testing.T) {
	stubPressure(t, 0)
	captureLogs(t)
	RegisterCompactionMethod("shake", func(context.Context, *Agent, []ai.Message) (ai.Message, error) {
		return ai.Message{}, errors.New("test: method broken")
	})
	t.Cleanup(func() { delete(compactMethodOverrides, "shake") })

	p := &fakeProvider{}
	a, s, _ := storeAgent(t, p, ladderConfig("shake"))
	ladderFixture(t, s)
	before := len(s.Entries())
	_ = a.maybeCompact(context.Background(), ladderHistory(t, s))
	if got := len(s.Entries()); got != before {
		t.Fatalf("store grew by %d entries, want none appended", got-before)
	}
}

// ---- idle trigger ---------------------------------------------------------

// TestIdleTriggerFiresOnlyAfterTheConfiguredGap pins the idle trigger: it
// ignores a fresh boundary, ignores a gap shorter than compaction.idleAfter,
// fires once the gap reaches it, and re-arms afterwards. The order carries no
// threshold member, so only idleness can explain the compaction.
func TestIdleTriggerFiresOnlyAfterTheConfiguredGap(t *testing.T) {
	stubPressure(t, 0)
	now := time.Unix(1_700_000_000, 0)
	stubClock(t, &now)

	p := &fakeProvider{}
	// A window far larger than the fixture keeps the token trigger out of the
	// picture: idleness is then the only thing that can compact.
	cfg := CompactionConfig{ContextWindow: 1 << 20, KeepRecentTokens: 5, Methods: []string{"soft"}, IdleAfter: 10 * time.Minute}
	a, s, _ := storeAgent(t, p, cfg)
	ladderFixture(t, s)
	hist := ladderHistory(t, s)
	ctx := context.Background()

	_ = a.maybeCompact(ctx, hist) // first boundary: nothing to measure from
	if n := len(compactionEntries(s)); n != 0 {
		t.Fatalf("a fresh session must not be idle, entries = %d", n)
	}
	now = now.Add(5 * time.Minute)
	_ = a.maybeCompact(ctx, hist)
	if n := len(compactionEntries(s)); n != 0 {
		t.Fatalf("5m < 10m idle must not compact, entries = %d", n)
	}
	// The gap is measured between two boundaries, so this one sits a full
	// idleAfter after the previous boundary.
	now = now.Add(10 * time.Minute)
	_ = a.maybeCompact(ctx, hist)
	entries := compactionEntries(s)
	if len(entries) != 1 {
		t.Fatalf("10m idle must compact, entries = %d", len(entries))
	}
	if entries[0].Method != methodSoft {
		t.Fatalf("method = %q, want %q", entries[0].Method, methodSoft)
	}
	now = now.Add(time.Minute)
	_ = a.maybeCompact(ctx, hist)
	if n := len(compactionEntries(s)); n != 1 {
		t.Fatalf("the trigger must re-arm, entries = %d", n)
	}
}

// ---- async trigger --------------------------------------------------------

// asyncAgent wires the same persistence hooks storeAgent uses around an
// arbitrary provider — the async tests need a provider they can drive.
func asyncAgent(t *testing.T, prov ai.Provider, cfg CompactionConfig) (*Agent, *session.Store) {
	t.Helper()
	s := session.OpenMem("test", "ladder")
	hooks := TurnHooksFunc{
		OnMessageEndF:    func(m *ai.Message) { _ = s.Append(&session.MessageEntry{Message: *m}) },
		OnToolResultMsgF: func(m *ai.Message) { _ = s.Append(&session.MessageEntry{Message: *m}) },
	}
	return &Agent{Provider: prov, Tools: tool.NewRegistry(), Hooks: hooks, Store: s, Compaction: cfg}, s
}

// ladderProvider answers turn requests at once and holds the summarize call
// until the test releases it, which makes the boundary that applies an async
// result deterministic.
type ladderProvider struct {
	mu         sync.Mutex
	turns      int
	summaries  int
	canceled   bool
	started    chan struct{}
	released   chan struct{}
	cancelSeen chan struct{}
}

func newLadderProvider() *ladderProvider {
	return &ladderProvider{
		started:    make(chan struct{}),
		released:   make(chan struct{}),
		cancelSeen: make(chan struct{}),
	}
}

func (p *ladderProvider) Stream(ctx context.Context, req ai.StreamRequest) (<-chan ai.Event, error) {
	if req.System != compactionPrompt {
		p.mu.Lock()
		p.turns++
		p.mu.Unlock()
		return eventChan(doneEvent("turn")), nil
	}
	p.mu.Lock()
	p.summaries++
	first := p.summaries == 1
	p.mu.Unlock()
	if first {
		close(p.started)
	}
	select {
	case <-p.released:
	case <-ctx.Done():
		p.mu.Lock()
		already := p.canceled
		p.canceled = true
		p.mu.Unlock()
		if !already {
			close(p.cancelSeen)
		}
		return nil, ctx.Err()
	}
	return eventChan(doneEvent("SUMMARY-FROM-ASYNC")), nil
}

func (p *ladderProvider) Name() string { return "ladder" }
func (p *ladderProvider) API() string  { return "ladder-api" }

func (p *ladderProvider) counts() (turns, summaries int, canceled bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.turns, p.summaries, p.canceled
}

// eventChan delivers events on a closed channel (the provider contract).
func eventChan(events ...ai.Event) <-chan ai.Event {
	ch := make(chan ai.Event, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch
}

// TestAsyncCompactionAppliesAtTheNextBoundary pins the async trigger: the
// boundary that is due hands the summarize to a goroutine and returns without
// compacting, and the result lands — recorded as handoff — at a later boundary.
func TestAsyncCompactionAppliesAtTheNextBoundary(t *testing.T) {
	stubPressure(t, 0)
	p := newLadderProvider()
	cfg := ladderConfig("handoff")
	cfg.Async = true
	a, s := asyncAgent(t, p, cfg)
	ladderFixture(t, s)
	hist := ladderHistory(t, s)
	ctx := context.Background()

	_ = a.maybeCompact(ctx, hist)
	if n := len(compactionEntries(s)); n != 0 {
		t.Fatalf("the async boundary must not block on the summary, entries = %d", n)
	}
	waitChan(t, "the background summarize", p.started)
	close(p.released)

	deadline := time.Now().Add(2 * time.Second)
	for len(compactionEntries(s)) == 0 && time.Now().Before(deadline) {
		_ = a.maybeCompact(ctx, hist)
		time.Sleep(time.Millisecond)
	}
	entries := compactionEntries(s)
	if len(entries) != 1 {
		t.Fatalf("async result never applied, entries = %d", len(entries))
	}
	if entries[0].Method != MethodHandoff {
		t.Fatalf("method = %q, want %q", entries[0].Method, MethodHandoff)
	}
	if !strings.Contains(entries[0].Summary.Text(), "SUMMARY-FROM-ASYNC") {
		t.Fatalf("summary = %q, want the background answer", entries[0].Summary.Text())
	}
	if _, summaries, _ := p.counts(); summaries != 1 {
		t.Fatalf("summarize calls = %d, want exactly one job", summaries)
	}
}

// TestAsyncCompactionCanceledOnAbort pins cancellation: an aborted boundary
// cancels the provider call and nothing it produced is ever applied.
func TestAsyncCompactionCanceledOnAbort(t *testing.T) {
	stubPressure(t, 0)
	p := newLadderProvider()
	cfg := ladderConfig("handoff")
	cfg.Async = true
	a, s := asyncAgent(t, p, cfg)
	ladderFixture(t, s)
	hist := ladderHistory(t, s)
	ctx := context.Background()

	_ = a.maybeCompact(ctx, hist)
	waitChan(t, "the background summarize", p.started)

	a.CancelAsyncCompaction() // the abort path
	waitChan(t, "the provider to observe cancellation", p.cancelSeen)
	close(p.released)

	_ = a.maybeCompact(ctx, hist)
	if n := len(compactionEntries(s)); n != 0 {
		t.Fatalf("a canceled async job must not be applied, entries = %d", n)
	}
	if _, _, canceled := p.counts(); !canceled {
		t.Fatal("the provider call must see the cancellation")
	}
	// The canceled job's result channel was drained by the boundary above:
	// a later boundary starts a fresh job instead of replaying the old one.
	if a.compactAsync == nil {
		t.Fatal("the canceled job must be forgotten, not replayed")
	}
	a.CancelAsyncCompaction()
	if a.compactAsync != nil {
		t.Fatal("canceling with a job in flight must clear it")
	}
}

// TestParseMethodOrderAcceptsLadderMembers pins the vocabulary the ladder
// reads: the M5 #24 members parse (order preserved) even though the shipped
// default order names only triggers.
func TestParseMethodOrderAcceptsLadderMembers(t *testing.T) {
	got := ParseMethodOrder("remote,snapcompact,handoff,shake,soft")
	want := []string{"remote", "snapcompact", "handoff", "shake", "soft"}
	if len(got) != len(want) {
		t.Fatalf("ParseMethodOrder = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ParseMethodOrder[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if got := ParseMethodOrder("snapcompact,bogus"); len(got) != 1 || got[0] != "snapcompact" {
		t.Fatalf("unknown members must still be dropped: %v", got)
	}
}

// TestLadderProducts pins the trigger/product split: an order of triggers
// alone means the builtin handoff summarize (the pre-ladder behavior), while a
// named product is used in order.
func TestLadderProducts(t *testing.T) {
	for _, tc := range []struct {
		methods []string
		want    []string
	}{
		{nil, []string{"handoff"}},
		{[]string{"threshold", "overflow", "promotion"}, []string{"handoff"}},
		{[]string{"threshold", "soft"}, []string{"soft"}},
		{[]string{"remote", "snapcompact", "handoff"}, []string{"remote", "snapcompact", "handoff"}},
	} {
		cfg := CompactionConfig{Methods: tc.methods}
		got := cfg.products()
		if len(got) != len(tc.want) {
			t.Fatalf("products(%v) = %v, want %v", tc.methods, got, tc.want)
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Fatalf("products(%v)[%d] = %q, want %q", tc.methods, i, got[i], tc.want[i])
			}
		}
	}
}
