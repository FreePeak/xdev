package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

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

	mu     sync.Mutex
	cursor int // primary history length already fed
	errs   int // consecutive feed failures (3 halts the advisor)
	// cursorBefore is the cursor at the start of the latest feed (log only).
	cursorBefore int
	halted       bool
	guard        *emissionGuard
	noteBuf      []note // notes delivered by the most recent review (for /advisor dump)
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

// Feed reviews the primary history delta since the last call. One review
// = one provider round-trip on a fresh single-turn session: stateless per
// feed, so the advisor never sees its own past advice replayed. Safe to
// call concurrently with the primary's turns (it only reads a snapshot).
func (a *Advisor) Feed(ctx context.Context, primaryHistory []ai.Message) {
	if a == nil || a.Provider == nil || a.halted {
		return
	}
	a.mu.Lock()
	if a.cursor >= len(primaryHistory) {
		a.mu.Unlock()
		return // nothing new
	}
	delta := renderAdvisorDelta(primaryHistory[a.cursor:])
	a.cursorBefore = a.cursor
	a.cursor = len(primaryHistory)
	a.noteBuf = nil
	adviseTool := a.adviseToolLocked()
	a.mu.Unlock()
	if adviseTool == nil {
		return // registry without advise: nothing to route into
	}

	var delivered int
	adviseTool.setSink(func(sev, text string) {
		if !a.guard.allow(sev, text) {
			return
		}
		if delivered >= 1 {
			return // one note per review (omp emission guard)
		}
		delivered++
		a.mu.Lock()
		a.noteBuf = append(a.noteBuf, note{Severity: sev, Text: text})
		a.mu.Unlock()
		// All severities steer into the primary: the steering queue
		// delivers at the next step boundary (the batched-aside
		// channel). The severity rides the text so the primary can
		// weigh it.
		a.Primary.Steer("advisor (" + sev + "): " + text)
	})
	defer adviseTool.setSink(nil)

	ag := &Agent{
		Provider: a.Provider,
		Tools:    a.Tools,
		Model:    a.Model,
		MaxTurns: 4,
	}
	hist := []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "delta to review:\n\n" + delta}}},
	}
	if _, err := ag.Run(ctx, AdvisorSystemPrompt, hist); err != nil {
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
	total := len(primaryHistory)
	a.mu.Unlock()
	logx.Debugf("advisor: reviewed entries %d..%d, %d note(s)", before, total, delivered)
}

// Halted reports whether repeated failures stopped the advisor.
func (a *Advisor) Halted() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.halted
}

// Dump returns the notes delivered by the most recent review.
func (a *Advisor) Dump() []note {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]note(nil), a.noteBuf...)
}

// Reset rewinds the feed cursor (compaction / session switch / branch):
// the advisor re-reads from the current history boundary, not the whole
// transcript.
func (a *Advisor) Reset(primaryHistory []ai.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cursor = len(primaryHistory)
	a.errs = 0
	a.halted = false
	a.noteBuf = nil
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
