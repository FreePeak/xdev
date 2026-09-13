package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/logx"
)

// TTSR — Time-Traveling Stream Rules (M11 #35, PRD §2 row M11): every
// text/thinking/toolcall delta is matched against the configured rules. A
// matching rule either aborts the turn mid-stream — interruptMode
// permitting that delta kind — so the agent retries with a hidden
// <system-interrupt> notice, or folds a <system-reminder> into the tool
// result text when cutting the call short would be wrong. Fired state and
// the repeat gap live in-session only; settings are re-read per session
// (cmd wires a fresh engine from the settings group).
//
// The Agent embeds this state directly (a.TTSR); a nil engine is fully
// inert.

// Interrupt modes: which delta kinds a rule may abort the turn on.
const (
	// TTSRModeAlways interrupts on prose and tool deltas.
	TTSRModeAlways = "always"
	// TTSRModeProseOnly interrupts on text/thinking deltas; a match on a
	// tool delta folds a reminder into the tool result instead.
	TTSRModeProseOnly = "prose-only"
	// TTSRModeToolOnly interrupts on toolcall deltas only.
	TTSRModeToolOnly = "tool-only"
	// TTSRModeNever never aborts; a tool-delta match folds a reminder.
	TTSRModeNever = "never"
)

// Context modes: what happens to the content streamed before the abort.
const (
	// TTSRContextDiscard drops it from the context.
	TTSRContextDiscard = "discard"
	// TTSRContextKeep appends it as an assistant message so the model sees
	// what it had written when the interrupt landed.
	TTSRContextKeep = "keep"
)

const (
	// ttsrWindowBytes bounds the accumulated delta window a condition is
	// matched against (a rule may span several small deltas).
	ttsrWindowBytes = 4096
	// ttsrSettleDelay is the pause between the abort and the injected
	// system-interrupt (omp parity: the aborted stream must be torn down).
	ttsrSettleDelay = 50 * time.Millisecond
	// ttsrMaxInterruptsPerTurn bounds abort/retry cycles inside one turn;
	// past it the engine goes quiet for the rest of the turn so a model
	// that keeps re-triggering the same rule can finish its answer instead
	// of burning provider round-trips.
	ttsrMaxInterruptsPerTurn = 3
	// defaultRepeatGap mirrors the settings default.
	defaultRepeatGap = 3

	ttsrInterruptTag = "system-interrupt"
	ttsrReminderTag  = "system-reminder"
)

// ttsrKind is the delta stream one monitor call belongs to.
type ttsrKind int

const (
	ttsrProse    ttsrKind = iota // assistant text deltas
	ttsrThinking                 // reasoning deltas: prose policy, off by default
	ttsrTool                     // toolcall deltas

	// ttsrLaneCount sizes the per-lane carry-over windows; keep it last.
	ttsrLaneCount
)

func (k ttsrKind) String() string {
	switch k {
	case ttsrTool:
		return "tool"
	case ttsrThinking:
		return "thinking"
	}
	return "prose"
}

// isProse reports the kind's policy class: reasoning follows the prose rules
// (prose-only interrupts prose and reasoning alike) — the only difference is
// that reasoning is off unless the group opts in.
func (k ttsrKind) isProse() bool { return k == ttsrProse || k == ttsrThinking }

// TTSR is the compiled rule set plus its live in-session state. Live state
// is touched by the Run goroutine (streaming) and read by concurrent tool
// workers (only pending; see takeReminder).
type TTSR struct {
	cfg     *config.TTSRSettings
	regexes []*regexp.Regexp // aligned with cfg.Rules; nil = unusable rule

	turn    int
	win     [ttsrLaneCount]string // one carry-over window per policy lane
	seen    map[string]bool       // fired in the current turn (per rule)
	last    map[string]int        // rule name → turn of the last fire
	pending map[int][]string
	quiet   bool
}

// TTSRMatch is one fired rule.
type TTSRMatch struct {
	Rule        config.TTSRRule
	Kind        ttsrKind
	StreamIndex int
	// Interrupt reports whether the caller must abort the turn (false =
	// a reminder was queued for the tool result).
	Interrupt bool
}

// NewTTSR compiles the `ttsr` settings group. nil settings, a disabled
// group, or a group without one usable rule returns nil: the agent runs
// exactly as before.
func NewTTSR(cfg *config.TTSRSettings) *TTSR {
	if cfg == nil || (cfg.Enabled != nil && !*cfg.Enabled) {
		return nil
	}
	t := &TTSR{
		cfg:     cfg,
		regexes: make([]*regexp.Regexp, len(cfg.Rules)),
		seen:    map[string]bool{},
		last:    map[string]int{},
		pending: map[int][]string{},
	}
	usable := false
	for i, r := range cfg.Rules {
		if r.Name == "" {
			logx.Errorf("ttsr: rule #%d has no name; skipped", i+1)
			continue
		}
		if r.Condition == "" {
			// omp's astCondition rides on top of a regex condition; a
			// rule with only astCondition has nothing to match on here.
			logx.Errorf("ttsr: rule %q has no condition; skipped", r.Name)
			continue
		}
		re, err := regexp.Compile(r.Condition)
		if err != nil {
			logx.Errorf("ttsr: rule %q: bad condition %q: %v", r.Name, r.Condition, err)
			continue
		}
		t.regexes[i] = re
		usable = true
	}
	if !usable {
		return nil
	}
	return t
}

// mode resolves the effective interrupt mode for one rule.
func (t *TTSR) mode(r config.TTSRRule) string {
	if r.InterruptMode != "" {
		return r.InterruptMode
	}
	if t.cfg.InterruptMode != "" {
		return t.cfg.InterruptMode
	}
	return TTSRModeAlways
}

// contextMode resolves the effective context mode for one rule.
func (t *TTSR) contextMode(r config.TTSRRule) string {
	if r.ContextMode != "" {
		return r.ContextMode
	}
	if t.cfg.ContextMode != "" {
		return t.cfg.ContextMode
	}
	return TTSRContextDiscard
}

// repeatGap resolves the effective minimum gap (in turns) between two
// fires of one rule.
func (t *TTSR) repeatGap(r config.TTSRRule) int {
	if r.RepeatGap > 0 {
		return r.RepeatGap
	}
	if t.cfg.RepeatGap > 0 {
		return t.cfg.RepeatGap
	}
	return defaultRepeatGap
}

// beginTurn resets the per-turn window/dedupe state and advances the turn
// counter the repeat gap is measured in. Caller: oneTurnWithRecovery, once
// per turn (a retry after an interrupt stays in the same turn, so the rule
// that just fired cannot fire again on the retried content).
func (t *TTSR) beginTurn() {
	t.turn++
	for i := range t.win {
		t.win[i] = ""
	}
	t.seen = map[string]bool{}
	t.pending = map[int][]string{}
}

// observe feeds one delta and returns the rule that fires, or nil. A rule
// that may not interrupt this delta kind queues a reminder for the tool
// result instead (returned non-nil with Interrupt=false so the caller can
// emit the event), and a rule that can do neither is skipped.
func (t *TTSR) observe(ctx context.Context, kind ttsrKind, delta string, streamIndex int) *TTSRMatch {
	if t.quiet || delta == "" {
		return nil
	}
	// Reasoning is out of the matched lanes unless the group opts in (#95,
	// omp's default): a rule firing on model-internal text interrupts a turn
	// the model never chose to send. It shares the prose window so a rule
	// split across a text/thinking boundary still matches once enabled.
	if kind == ttsrThinking && !t.cfg.ScanThinkingOn() {
		return nil
	}
	kindWindow := kind
	if kind == ttsrThinking {
		kindWindow = ttsrProse
	}
	w := t.win[kindWindow] + delta
	if len(w) > ttsrWindowBytes {
		w = w[len(w)-ttsrWindowBytes:]
	}
	t.win[kindWindow] = w
	for i := range t.cfg.Rules {
		r := t.cfg.Rules[i]
		interrupt, reminder := ttsrAction(t.mode(r), kind)
		if !interrupt && !reminder {
			continue
		}
		if t.regexes[i] == nil || t.seen[r.Name] || t.cfg.IsDisabled(r.Name) {
			continue
		}
		if last, ok := t.last[r.Name]; ok && t.turn-last < t.repeatGap(r) {
			continue // repeat policy: within the gap the rule stays silent
		}
		if !t.regexes[i].MatchString(w) {
			continue
		}
		if reminder && r.ASTCondition != "" && !ttsrASTSatisfied(ctx, r.ASTCondition, w) {
			continue
		}
		t.seen[r.Name] = true
		t.last[r.Name] = t.turn
		if reminder {
			t.pending[streamIndex] = append(t.pending[streamIndex], ttsrNotice(ttsrReminderTag, r, ttsrRulePath(kind, w)))
		}
		return &TTSRMatch{Rule: r, Kind: kind, StreamIndex: streamIndex, Interrupt: interrupt}
	}
	return nil
}

// ttsrAction resolves an interrupt mode against a delta kind: interrupt
// aborts the turn, reminder folds a notice into the tool result.
func ttsrAction(mode string, kind ttsrKind) (interrupt, reminder bool) {
	// Reasoning is judged as prose.
	switch mode {
	case TTSRModeAlways:
		return true, false
	case TTSRModeProseOnly:
		return kind.isProse(), kind == ttsrTool
	case TTSRModeToolOnly:
		return kind == ttsrTool, false
	default: // never (or an unknown mode from a hand-built struct)
		return false, kind == ttsrTool
	}
}

// ttsrNotice renders one hidden notice (interrupt or reminder). The tag
// carries omp's attributes — reason, rule and the file path when the rule
// fired on a tool call — so a reader (model or transcript) can tell WHICH
// rule matched and on what, without parsing prose (parity T3 #40).
func ttsrNotice(tag string, r config.TTSRRule, path string) string {
	msg := strings.TrimSpace(r.Message)
	if msg == "" {
		msg = "stop and adjust before continuing"
	}
	var b strings.Builder
	b.WriteString("<")
	b.WriteString(tag)
	b.WriteString(` reason="rule_violation" rule=`)
	b.WriteString(strconv.Quote(r.Name))
	if path != "" {
		b.WriteString(" path=")
		b.WriteString(strconv.Quote(path))
	}
	b.WriteString(">")
	b.WriteString(msg)
	b.WriteString("</")
	b.WriteString(tag)
	b.WriteString(">")
	return b.String()
}

// ttsrRulePath resolves the file a rule fired on: the path named by a tool
// call's digest ("" for prose matches, which have no file).
func ttsrRulePath(kind ttsrKind, digest string) string {
	if kind != ttsrTool {
		return ""
	}
	return ttsrDigestPath(digest)
}

// ttsrPathRe pulls a file path out of a tool-call digest. Regex rather than
// json.Unmarshal: an in-flight call's digest is frequently truncated JSON.
var ttsrPathRe = regexp.MustCompile(`"(?:filePath|file_path|path)"\s*:\s*"([^"]+)"`)

// ttsrDigestPath returns the file named by a tool-call digest ("" when the
// digest names none).
func ttsrDigestPath(digest string) string {
	if m := ttsrPathRe.FindStringSubmatch(digest); len(m) > 1 {
		return m[1]
	}
	return ""
}

// ttsrASTSatisfied evaluates a rule's astCondition against the edit/write
// target named by the tool-call digest, by shelling out to ast-grep (omp
// parity). Contract: best-effort — when ast-grep is not installed the
// condition is skipped and the regex condition alone decides (the issue's
// "keep optional/skip if binary absent"); when it is installed but the
// digest names no file, the condition cannot be satisfied.
func ttsrASTSatisfied(ctx context.Context, pattern, digest string) bool {
	if !ttsrASTAvailable() {
		return true
	}
	path := ttsrDigestPath(digest)
	if path == "" {
		return false
	}
	out, err := exec.CommandContext(ctx, "ast-grep", "run", "--pattern", pattern, path).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return false // ran cleanly, no match (grep-like exit 1)
		}
		logx.Errorf("ttsr: ast-grep %q: %v", pattern, err)
		return true // unusable invocation: degrade, never swallow the rule
	}
	return strings.TrimSpace(string(out)) != ""
}

// ttsrASTAvailable reports whether the AST matcher is on PATH. Deliberately
// uncached: a test installs a stub, and a long session may gain the binary.
func ttsrASTAvailable() bool {
	_, err := exec.LookPath("ast-grep")
	return err == nil
}

// ttsrBeginTurn advances the engine's turn counter.
func (a *Agent) ttsrBeginTurn() {
	if a.TTSR != nil {
		a.TTSR.beginTurn()
	}
}

// ttsrSetQuiet mutes aborts for the rest of the current turn.
func (a *Agent) ttsrSetQuiet(v bool) {
	if a.TTSR != nil {
		a.TTSR.quiet = v
	}
}

// ttsrObserve feeds one delta to the engine and emits ttsr_triggered for a
// fired rule. It returns a match the caller must abort the turn on; a
// non-interrupting tool match only queues its reminder and returns nil.
func (a *Agent) ttsrObserve(ctx context.Context, kind ttsrKind, delta string, streamIndex int) *TTSRMatch {
	if a.TTSR == nil {
		return nil
	}
	m := a.TTSR.observe(ctx, kind, delta, streamIndex)
	if m == nil {
		return nil
	}
	if a.Intercept != nil {
		a.Intercept.Emit(ctx, "ttsr_triggered", map[string]any{
			"rule":        m.Rule.Name,
			"kind":        kind.String(),
			"interrupt":   m.Interrupt,
			"mode":        a.TTSR.mode(m.Rule),
			"contextMode": a.TTSR.contextMode(m.Rule),
		})
	}
	if m.Interrupt {
		return m
	}
	return nil
}

// ttsrReminder returns the queued <system-reminder> notices for one tool
// call ("" when none). Read-only: tool workers run concurrently, and the
// entries are cleared at the next beginTurn.
func (a *Agent) ttsrReminder(streamIndex int) string {
	if a.TTSR == nil {
		return ""
	}
	return strings.Join(a.TTSR.pending[streamIndex], "\n")
}

// ttsrResume applies one interrupted turn: the abort settle delay, the
// contextMode decision, and the hidden user-attributed system-interrupt
// notice that re-steers the model (the magic-keyword hidden-entry shape).
func (a *Agent) ttsrResume(ctx context.Context, ti *ttsrInterrupt, history []ai.Message) []ai.Message {
	if a.TTSR == nil {
		return history
	}
	select {
	case <-time.After(ttsrSettleDelay):
	case <-ctx.Done():
	}
	if a.TTSR.contextMode(ti.match.Rule) == TTSRContextKeep && ti.partial != nil {
		history = append(history, *ti.partial)
		a.persist(*ti.partial)
	}
	notice := ai.Message{
		Role:        ai.RoleUser,
		Content:     []ai.Block{ai.TextBlock{Text: ttsrNotice(ttsrInterruptTag, ti.match.Rule, ti.path)}},
		Attribution: "user",
	}
	history = append(history, notice)
	a.persist(notice)
	return history
}

// ttsrInterrupt is the abort signal oneTurn returns when a rule fires.
type ttsrInterrupt struct {
	match   *TTSRMatch
	partial *ai.Message // streamed assistant content at abort time
	// path is the file the rule fired on ("" for a prose match); it becomes
	// the notice's path attribute.
	path string
}

func (e *ttsrInterrupt) Error() string {
	return fmt.Sprintf("agent: ttsr rule %q interrupted the turn", e.match.Rule.Name)
}
