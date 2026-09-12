package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/tool"
)

// Advisor is the background reviewer (M11, research §6 CORE): a second
// agent on a read-only toolset watches the primary transcript and emits
// advice through its advise tool. Advice routes by severity into the
// primary's steering queue:
//   - nit     → minor note, delivered at the next step boundary
//   - concern → interrupting steer (same channel as user steering)
//   - blocker → steer, always delivered (even near a terminal answer)
//
// The advisor runs its OWN single turn per Feed call — the reviewer's
// prompt is a stateless one-shot over the rendered delta (fresh session
// each feed; its own advice is never replayed back). An emission guard
// keeps a chatty advisor from flooding the run: normalized dedupe (FIFO),
// content-free phrase filter, and one delivered note per review.

const (
	AdviseNit     = "nit"
	AdviseConcern = "concern"
	AdviseBlocker = "blocker"
)

// AdvisorSystemPrompt is the reviewer's contract.
const AdvisorSystemPrompt = "You are the advisor: a silent reviewer watching another agent work. " +
	"Read the transcript delta you are fed. If something is wrong — a risky command, a wrong assumption, " +
	"a wasted step, drift from the user's goal — call advise with severity blocker (must stop now), " +
	"concern (should change course), or nit (minor, worth noting). " +
	"If nothing is wrong, call advise with severity nit and the text 'ok'. " +
	"Call advise exactly once, then stop."

// Advisor wires a reviewer provider to a primary run.
type Advisor struct {
	Provider ai.Provider
	Model    string
	Tools    *tool.Registry // the reviewer's read-only toolset + advise
	// Primary is the watched run; advice steers into it at the next step
	// boundary. Set by the host before the first Feed.
	Primary *Agent

	// Guidance (WATCHDOG.md discovery, M11 #39) is advisor-only review
	// guidance appended to the reviewer's system prompt. Empty = none.
	Guidance string
	// ImmuneTurns (settings advisorImmuneTurns, default 3): after an
	// interrupt reaches the primary, later concerns/blockers ride as
	// non-interrupting asides for this many primary turns.
	ImmuneTurns int
	// SyncBacklog (settings advisorSyncBacklog, 0 = off): bounded
	// catch-up. When the pending delta spans more than N primary turns —
	// a backlog formed while detached or lagging — one review covers only
	// the most recent N turns, capped at 30s.
	SyncBacklog int
	// Patterns (WATCHDOG.yml roster entry): when set, a delta matching no
	// pattern is consumed without review. A pattern is a regex when it
	// compiles, else a case-insensitive substring (see matchesPatterns).
	Patterns []string

	mu     sync.Mutex
	cursor int // primary history length already fed
	errs   int // consecutive feed failures (3 halts the advisor)
	// cursorBefore is the cursor at the start of the latest feed (log only).
	cursorBefore int
	halted       bool
	guard        *emissionGuard
	noteBuf      []note // notes delivered by the most recent review (for /advisor dump)
	turns        int    // primary updates seen (the immuneTurns clock)
	immuneUntil  int    // feed index through which interrupts ride as asides
	// peers makes this Advisor a roster facade: Feed fans out to each
	// peer whose Patterns match the delta (see NewAdvisorRoster). A
	// facade's own Provider/Tools/cursor are unused.
	peers []*Advisor
}

type note struct {
	Severity string
	Text     string
}

// NewAdvisor builds the runtime. The registry must contain the advise
// tool (see RegisterAdviseTool); other entries are the reviewer's eyes.
func NewAdvisor(prov ai.Provider, model string, reg *tool.Registry) *Advisor {
	return &Advisor{Provider: prov, Model: model, Tools: reg, guard: newEmissionGuard()}
}

// NewAdvisorRoster builds a facade over the WATCHDOG.yml advisor roster
// (M11 #39): Feed fans out to every peer whose Patterns match the delta,
// so several named reviewers can watch the same run on different beats. A
// peer without patterns reviews every delta. Primary set on the facade
// propagates to its peers at feed time.
func NewAdvisorRoster(peers []*Advisor) *Advisor {
	return &Advisor{peers: peers}
}

// Feed reviews the primary history delta since the last call. One review
// = one provider round-trip on a fresh single-turn session: stateless per
// feed, so the advisor never sees its own past advice replayed. Safe to
// call concurrently with the primary's turns (it only reads a snapshot).
func (a *Advisor) Feed(ctx context.Context, primaryHistory []ai.Message) {
	if a == nil {
		return
	}
	if len(a.peers) > 0 {
		// Roster facade: every peer reviews the same snapshot, each gated
		// by its own patterns. One goroutine per peer — a review is its
		// own provider round-trip and the peers are independent.
		for _, p := range a.peers {
			if p == nil {
				continue
			}
			if p.Primary == nil {
				p.Primary = a.Primary // the host sets Primary on the facade
			}
			go p.Feed(ctx, primaryHistory)
		}
		return
	}
	if a.Provider == nil || a.halted {
		return
	}
	a.mu.Lock()
	if a.cursor >= len(primaryHistory) {
		a.mu.Unlock()
		return // nothing new
	}
	a.turns++ // the immuneTurns clock ticks once per primary update
	start := a.cursor
	skipped := 0
	if a.SyncBacklog > 0 {
		// Bounded catch-up: one review covers at most N turns.
		if n := countTurns(primaryHistory[start:]) - a.SyncBacklog; n > 0 {
			start = indexAfterTurns(primaryHistory, start, n)
			skipped = n
		}
	}
	delta := renderAdvisorDelta(primaryHistory[start:])
	if !matchesPatterns(a.Patterns, delta) {
		// Not this reviewer's beat: consume the delta so it cannot pile
		// up into every later review.
		a.cursor = len(primaryHistory)
		a.mu.Unlock()
		return
	}
	a.cursorBefore = a.cursor
	a.cursor = len(primaryHistory)
	a.noteBuf = nil
	adviseTool := a.adviseToolLocked()
	a.mu.Unlock()
	if adviseTool == nil {
		return // registry without advise: nothing to route into
	}

	// Tool calls execute concurrently, so a reviewer emitting two advises
	// in one turn races on this counter — atomic, not a plain int.
	var delivered atomic.Int32
	adviseTool.setSink(func(sev, text string) {
		if !a.guard.allow(sev, text) {
			return
		}
		if delivered.Add(1) > 1 {
			return // one note per review (omp emission guard)
		}
		interrupting := sev == AdviseConcern || sev == AdviseBlocker
		aside := false
		a.mu.Lock()
		a.noteBuf = append(a.noteBuf, note{Severity: sev, Text: text})
		target := a.Primary
		if target != nil && interrupting {
			// A recent interrupt buys the primary quiet: later
			// concerns/blockers ride as asides until the window closes
			// (advisor.immuneTurns). The interrupt itself arms the window.
			if a.turns <= a.immuneUntil {
				aside = true
			} else if n := a.effectiveImmuneTurns(); n > 0 {
				a.immuneUntil = a.turns + n
			}
		}
		a.mu.Unlock()
		if target == nil {
			return // unwired review: the note stays in the dump
		}
		// Every severity steers into the primary: the steering queue
		// delivers at the next step boundary (the batched-aside channel).
		// The severity rides the text so the primary can weigh it.
		if aside {
			target.Steer("advisor aside (" + sev + "): " + text)
			return
		}
		target.Steer("advisor (" + sev + "): " + text)
	})
	defer adviseTool.setSink(nil)

	// A bounded catch-up review is capped at 30s; an unbounded review
	// rides the caller's context.
	if a.SyncBacklog > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, advisorBacklogCap)
		defer cancel()
	}

	ag := &Agent{
		Provider: a.Provider,
		Tools:    a.Tools,
		Model:    a.Model,
		MaxTurns: 4,
	}
	hist := []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "delta to review:\n\n" + delta}}},
	}
	if _, err := ag.Run(ctx, a.sysPrompt(), hist); err != nil {
		a.mu.Lock()
		a.errs++
		if a.errs >= 3 {
			a.halted = true
			logx.Errorf("advisor: 3 consecutive failures — halted")
		}
		a.mu.Unlock()
		logx.Debugf("advisor feed: %v", err)
		return
	}
	a.mu.Lock()
	a.errs = 0
	before := a.cursorBefore
	a.mu.Unlock()
	logx.Debugf("advisor: reviewed entries %d..%d (%d turn(s) skipped), %d note(s)",
		before, len(primaryHistory), skipped, delivered.Load())
}

// Halted reports whether repeated failures stopped the advisor. On a
// roster facade it reports halted only when every peer has halted.
func (a *Advisor) Halted() bool {
	a.mu.Lock()
	peers, halted := a.peers, a.halted
	a.mu.Unlock()
	if len(peers) == 0 {
		return halted
	}
	for _, p := range peers {
		if p != nil && !p.Halted() {
			return false
		}
	}
	return true
}

// Dump returns the notes delivered by the most recent review. A roster
// facade aggregates its peers.
func (a *Advisor) Dump() []note {
	a.mu.Lock()
	peers := a.peers
	out := append([]note(nil), a.noteBuf...)
	a.mu.Unlock()
	for _, p := range peers {
		if p != nil {
			out = append(out, p.Dump()...)
		}
	}
	return out
}

// Reset rewinds the feed cursor (compaction / session switch / branch):
// the advisor re-reads from the current history boundary, not the whole
// transcript. It also clears the immuneTurns window. A roster facade
// resets every peer.
func (a *Advisor) Reset(primaryHistory []ai.Message) {
	if a == nil {
		return
	}
	if len(a.peers) > 0 {
		for _, p := range a.peers {
			if p != nil {
				p.Reset(primaryHistory)
			}
		}
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cursor = len(primaryHistory)
	a.errs = 0
	a.halted = false
	a.noteBuf = nil
	a.turns = 0
	a.immuneUntil = 0
	if g := a.guard; g != nil {
		g.mu.Lock()
		g.seen = map[string]bool{}
		g.fifo = nil
		g.mu.Unlock()
	}
}

// AdviseToolName is the reviewer's only output channel.
const AdviseToolName = "advise"

// RegisterAdviseTool adds the advise tool to a reviewer registry.
func RegisterAdviseTool(reg *tool.Registry) {
	reg.Register(&adviseTool{mu: &sync.Mutex{}})
}

// adviseTool routes the reviewer's verdicts into the Advisor sink.
type adviseTool struct {
	mu   *sync.Mutex
	sink func(sev, text string)
}

func (t *adviseTool) Name() string { return AdviseToolName }
func (t *adviseTool) Description() string {
	return "route your review verdict: severity blocker|concern|nit and the advice text (one call per review)"
}
func (t *adviseTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "severity": {"type": "string", "enum": ["blocker", "concern", "nit"]},
    "text": {"type": "string", "description": "the advice; be specific and brief"}
  },
  "required": ["severity", "text"]
}`)
}
func (t *adviseTool) Execute(_ context.Context, args json.RawMessage) (tool.Result, error) {
	var a struct {
		Severity string `json:"severity"`
		Text     string `json:"text"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return tool.Result{Text: "advise: malformed arguments", IsError: true}, nil
	}
	t.mu.Lock()
	sink := t.sink
	t.mu.Unlock()
	if sink != nil {
		sink(a.Severity, a.Text)
	}
	return tool.Result{Text: "advise delivered"}, nil
}
func (t *adviseTool) setSink(fn func(sev, text string)) {
	t.mu.Lock()
	t.sink = fn
	t.mu.Unlock()
}

// adviseToolLocked returns the registry's advise tool (caller holds a.mu
// conceptually; the registry itself is mutex-guarded).
func (a *Advisor) adviseToolLocked() *adviseTool {
	if a.Tools == nil {
		return nil
	}
	t, ok := a.Tools.Get(AdviseToolName)
	if !ok {
		return nil
	}
	at, _ := t.(*adviseTool)
	return at
}

// renderAdvisorDelta builds the reviewer's view of new primary entries:
// roles and text, tool intents, no noise.
func renderAdvisorDelta(msgs []ai.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case ai.RoleUser:
			fmt.Fprintf(&b, "[user] %s\n", messageText(m))
		case ai.RoleAssistant:
			txt := messageText(m)
			if txt != "" {
				fmt.Fprintf(&b, "[assistant] %s\n", txt)
			}
			for _, blk := range m.Content {
				if tc, ok := blk.(ai.ToolCallBlock); ok {
					fmt.Fprintf(&b, "[tool call] %s %s\n", tc.Name, string(tc.Arguments))
				}
			}
		case ai.RoleToolResult:
			fmt.Fprintf(&b, "[tool result] %s: %s\n", m.ToolName, messageText(m))
		}
	}
	return b.String()
}

func messageText(m ai.Message) string {
	var out string
	for _, b := range m.Content {
		if tb, ok := b.(ai.TextBlock); ok {
			out += tb.Text
		}
	}
	return out
}

// emissionGuard keeps a chatty advisor from flooding the run (research
// §6): normalized dedupe (FIFO), content-free phrase filter.
type emissionGuard struct {
	mu   sync.Mutex
	seen map[string]bool
	fifo []string
}

const (
	guardFIFO    = 256
	guardMaxNote = 400
)

var contentFreePhrases = []string{"ok", "lgtm", "looks good", "all good", "fine", "nothing to add"}

func newEmissionGuard() *emissionGuard {
	return &emissionGuard{seen: map[string]bool{}}
}

func (g *emissionGuard) allow(sev, text string) bool {
	if g == nil {
		return false
	}
	norm := strings.ToLower(strings.TrimSpace(text))
	norm = strings.Join(strings.Fields(norm), " ")
	if norm == "" {
		return false
	}
	if sev != AdviseBlocker {
		for _, p := range contentFreePhrases {
			if norm == p {
				return false
			}
		}
	}
	if len(norm) > guardMaxNote {
		norm = norm[:guardMaxNote]
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.seen[norm] {
		return false
	}
	g.seen[norm] = true
	g.fifo = append(g.fifo, norm)
	if len(g.fifo) > guardFIFO {
		delete(g.seen, g.fifo[0])
		g.fifo = g.fifo[1:]
	}
	return true
}

// advisorBacklogCap bounds one catch-up review when SyncBacklog is on.
const advisorBacklogCap = 30 * time.Second

// effectiveImmuneTurns is the immune window actually applied (default 3).
func (a *Advisor) effectiveImmuneTurns() int {
	if a.ImmuneTurns > 0 {
		return a.ImmuneTurns
	}
	return 3
}

// sysPrompt is the reviewer's contract plus WATCHDOG.md guidance.
func (a *Advisor) sysPrompt() string {
	if a.Guidance == "" {
		return AdvisorSystemPrompt
	}
	return AdvisorSystemPrompt + "\n\nEspecially pay attention to:\n<attention>\n" + a.Guidance + "\n</attention>"
}

// countTurns counts primary turns in msgs: one assistant message each.
func countTurns(msgs []ai.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == ai.RoleAssistant {
			n++
		}
	}
	return n
}

// indexAfterTurns returns the index just past the nth assistant message
// at or after start, so a caller can skip that many whole turns.
func indexAfterTurns(msgs []ai.Message, start, skip int) int {
	seen := 0
	for i := start; i < len(msgs); i++ {
		if msgs[i].Role != ai.RoleAssistant {
			continue
		}
		seen++
		if seen == skip {
			return i + 1
		}
	}
	return start
}
