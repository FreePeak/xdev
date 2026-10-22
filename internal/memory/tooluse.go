package memory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// Tool-use recipes: the agent's own recovery from a failed tool call,
// remembered for the sessions that come after it.
//
// The loop is: a call fails -> the failure is recorded -> the agent tries
// again and finds a form that works -> the difference between the two
// calls is the recipe. The recipe is injected into later prompts, so the
// mistake is paid for once instead of once per session.
//
// A recipe is written only when the two calls are the SAME call: they
// share an argument key, the working one supplies what the failing one
// left out, or the difference is a renamed key. A failure followed by a
// different call that happens to work says nothing about how to use the
// tool, and a recipe that claims otherwise is worse than no recipe.

// ToolUseRecord is one remembered failure and — once the agent found a
// working form — the recipe that made it work.
type ToolUseRecord struct {
	// Tool is the tool the call was made against ("read", "bash"). For a
	// garbled tool name it is the name the agent invented, which no
	// session has.
	Tool string `json:"tool"`
	// Symptom is the failure the model saw: the tool's own error text,
	// collapsed to one line. It carries the tool's usage guidance, which
	// is why it is kept whole and not summarized.
	Symptom string `json:"symptom"`
	// Args is the failing call's arguments (capped). It is what the
	// working call is compared against.
	Args string `json:"args,omitempty"`
	// Fix is what the agent changed to make the call work. Empty while
	// the failure is still unresolved.
	Fix string `json:"fix,omitempty"`
	// FixTool names the tool the recipe recommends, when it is not the
	// tool that failed: a garbled name answered by a call to the real
	// one. Empty means the recipe is about Tool itself.
	FixTool string `json:"fixTool,omitempty"`
	// Hits counts how many times the same symptom was seen.
	Hits int `json:"hits"`
	// FailedAt is when the failure was first recorded.
	FailedAt time.Time `json:"failedAt"`
	// FixedAt is when the recipe was learned.
	FixedAt time.Time `json:"fixedAt,omitempty"`
}

// ToolUseStore keeps tool-use records in one JSONL file
// (<Dir>/tool-use.jsonl), one record per line.
//
// The format is JSONL rather than markdown because a record holds three
// fields the prompt has to render exactly (the tool, the symptom, the
// recipe), and a line-oriented format that has to be parsed back is where
// the quoting bugs live.
type ToolUseStore struct {
	// Dir is the memory directory. Empty = off.
	Dir string
	// Max bounds how many records the file keeps (default
	// DefaultToolUseMax); the oldest are dropped first.
	Max int

	mu sync.Mutex
}

// Defaults and caps for the tool-use store.
const (
	// DefaultToolUseMax is how many records the store keeps. A record
	// per distinct symptom is a small number in practice; the cap is
	// here so a long-lived store cannot grow without bound.
	DefaultToolUseMax = 40
	// toolUseFile is the record file inside the memory directory.
	toolUseFile = "tool-use.jsonl"
	// toolUseBlockMax is how many recipes one injected block carries.
	toolUseBlockMax = 8
	// toolUseBlockChars bounds the block itself. The system prompt has a
	// budget (PRD §1 goal 4), and a recipe is worth a few hundred
	// characters, not a few thousand: the block stops at the character
	// bound even when recipes remain.
	toolUseBlockChars = 1000
	// toolUsePerToolMax is how many recipes one tool may contribute to
	// the block, so one chatty tool cannot crowd out the rest.
	toolUsePerToolMax = 3
	// toolUseSymptomMax caps one symptom (the tool's error text).
	toolUseSymptomMax = 160
	// toolUseArgsMax caps the stored failing arguments.
	toolUseArgsMax = 600
	// toolUseFixMax caps one recipe.
	toolUseFixMax = 200
	// toolUseValueMax caps one rendered argument value.
	toolUseValueMax = 60
	// toolUseDiffMax is how many argument changes one recipe names.
	toolUseDiffMax = 4
	// toolUsePendingMax bounds the pending failures one session holds.
	toolUsePendingMax = 64
)

// Off reports whether the store is disabled (nil-safe).
func (s *ToolUseStore) Off() bool { return s == nil || s.Dir == "" }

// path is the record file.
func (s *ToolUseStore) path() string { return filepath.Join(s.Dir, toolUseFile) }

func (s *ToolUseStore) max() int {
	if s.Max > 0 {
		return s.Max
	}
	return DefaultToolUseMax
}

// Ensure creates the memory directory (first use).
func (s *ToolUseStore) Ensure() error {
	if s.Off() {
		return nil
	}
	return os.MkdirAll(s.Dir, 0o755)
}

// Records reads every stored record, oldest first. A malformed line is
// skipped: one bad line must not cost the whole store.
func (s *ToolUseStore) Records() []ToolUseRecord {
	if s.Off() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readLocked()
}

func (s *ToolUseStore) readLocked() []ToolUseRecord {
	raw, err := os.ReadFile(s.path())
	if err != nil {
		return nil
	}
	var out []ToolUseRecord
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r ToolUseRecord
		if json.Unmarshal([]byte(line), &r) != nil || r.Tool == "" {
			continue
		}
		out = append(out, r)
	}
	return out
}

// NoteFailure records a failed call. The same tool and symptom count as
// one record with more hits; the latest arguments win, because the latest
// attempt is the one the agent is about to correct.
func (s *ToolUseStore) NoteFailure(tool, symptom, args string) error {
	if s.Off() {
		return fmt.Errorf("memory: tool-use store off")
	}
	if tool == "" || symptom == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	recs := s.readLocked()
	now := time.Now().UTC()
	for i := range recs {
		if recs[i].Tool == tool && recs[i].Symptom == symptom {
			recs[i].Hits++
			recs[i].Args = args
			recs[i].FailedAt = now
			return s.writeLocked(recs)
		}
	}
	recs = append(recs, ToolUseRecord{
		Tool: tool, Symptom: symptom, Args: args, Hits: 1, FailedAt: now,
	})
	return s.writeLocked(recs)
}

// NoteFix records the recipe that made the call work. The failure record
// it belongs to is updated in place, so one symptom keeps one recipe; a
// fix for a symptom that was never recorded (the failure happened in an
// earlier session, or the store was cleared) becomes its own record.
//
// fixTool names the tool the recipe recommends when it is not the tool
// that failed (a garbled name answered by the real one); it may be empty.
func (s *ToolUseStore) NoteFix(tool, symptom, fix, fixTool string) error {
	if s.Off() {
		return fmt.Errorf("memory: tool-use store off")
	}
	if tool == "" || symptom == "" || fix == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	recs := s.readLocked()
	now := time.Now().UTC()
	for i := range recs {
		if recs[i].Tool == tool && recs[i].Symptom == symptom {
			recs[i].Fix = fix
			recs[i].FixTool = fixTool
			recs[i].FixedAt = now
			return s.writeLocked(recs)
		}
	}
	recs = append(recs, ToolUseRecord{
		Tool: tool, Symptom: symptom, Fix: fix, FixTool: fixTool,
		Hits: 1, FailedAt: now, FixedAt: now,
	})
	return s.writeLocked(recs)
}

// writeLocked persists the records atomically, dropping the oldest ones
// past the cap.
func (s *ToolUseStore) writeLocked(recs []ToolUseRecord) error {
	if err := s.Ensure(); err != nil {
		return err
	}
	if m := s.max(); len(recs) > m {
		recs = recs[len(recs)-m:]
	}
	var buf bytes.Buffer
	for _, r := range recs {
		raw, err := json.Marshal(r)
		if err != nil {
			continue
		}
		buf.Write(raw)
		buf.WriteByte('\n')
	}
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path())
}

// Clear removes the record file (/memory clear). A missing file is not an
// error.
func (s *ToolUseStore) Clear() error {
	if s.Off() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Recipes returns the records that carry a recipe, newest first.
func (s *ToolUseStore) Recipes() []ToolUseRecord {
	recs := s.Records()
	out := make([]ToolUseRecord, 0, len(recs))
	for _, r := range recs {
		if r.Fix != "" {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].FixedAt.After(out[j].FixedAt)
	})
	return out
}

// Block renders the injected tool-usage block: the recipes, one per line,
// newest first. has filters the recipes to the tools this session actually
// has — a block that names a tool the model cannot call teaches it to
// want a tool it does not have. A nil has keeps every recipe.
//
// The filter asks about the tool the RECIPE recommends, not the tool that
// failed: the recipe for a garbled tool name is about the real tool, and
// the garbled name is the mistake the record keeps.
//
// The block is empty when there is nothing to say: an empty heading costs
// prompt budget on every turn of every session.
func (s *ToolUseStore) Block(has func(tool string) bool) string {
	if s.Off() {
		return ""
	}
	var lines []string
	perTool := map[string]int{}
	used := 0
	for _, r := range s.Recipes() {
		tool := r.FixTool
		if tool == "" {
			tool = r.Tool
		}
		if has != nil && !has(tool) {
			continue
		}
		if perTool[tool] >= toolUsePerToolMax {
			continue
		}
		line := fmt.Sprintf("- %s: %q — fixed by: %s", r.Tool, r.Symptom, r.Fix)
		if len(lines) > 0 && used+len(line)+1 > toolUseBlockChars {
			break
		}
		perTool[tool]++
		lines = append(lines, line)
		used += len(line) + 1
		if len(lines) >= toolUseBlockMax {
			break
		}
	}
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Tool Usage\n\n")
	b.WriteString("These calls failed in earlier sessions; the recipe is what worked. " +
		"Use the recipe instead of repeating the mistake. Heuristic context, not authoritative.\n\n")
	b.WriteString(strings.Join(lines, "\n"))
	return b.String()
}

// SharedToolUse returns the process-wide tool-usage store for a
// directory. One store per directory, so two sessions in one process (the
// TUI) share one file and one lock instead of racing each other.
func SharedToolUse(dir string) *ToolUseStore {
	if dir == "" {
		return nil
	}
	toolUseMu.Lock()
	defer toolUseMu.Unlock()
	if s, ok := toolUseStores[dir]; ok {
		return s
	}
	s := &ToolUseStore{Dir: dir}
	toolUseStores[dir] = s
	return s
}

var (
	toolUseMu     sync.Mutex
	toolUseStores = map[string]*ToolUseStore{}
)

// ToolUseTracker watches tool outcomes and learns the working recipe for
// a tool the agent got wrong.
//
// One tracker per session; the store it writes to is shared, so a recipe
// learned in one session is read by the next.
type ToolUseTracker struct {
	// Store is where the records land. Required.
	Store *ToolUseStore
	// Redactor scrubs configured secrets before a record is written: a
	// recipe quotes the call that failed, and a call can carry a token.
	Redactor func(text string) string
	// OnLearn is called with every recipe that was recorded. nil is
	// silent.
	OnLearn func(rec ToolUseRecord)

	mu      sync.Mutex
	pending map[string]*pendingFailure
}

// pendingFailure is a failure the agent has not yet worked around.
type pendingFailure struct {
	tool    string
	symptom string
	args    string
	at      time.Time
}

// NewToolUseTracker returns a tracker that learns into store. redact
// scrubs secrets from a record before it is written and may be nil.
func NewToolUseTracker(store *ToolUseStore, redact func(string) string) *ToolUseTracker {
	return &ToolUseTracker{Store: store, Redactor: redact}
}

// ObserveTool records one tool outcome: a failure opens a pending
// failure, a success closes the pending failure it can explain. It is the
// agent.ToolLessonObserver seam.
func (t *ToolUseTracker) ObserveTool(call ai.ToolCallBlock, res tool.Result) {
	if t == nil || t.Store == nil || t.Store.Off() {
		return
	}
	if res.IsError {
		t.failed(call, res.Text)
		return
	}
	t.succeeded(call)
}

// failed opens (or refreshes) the pending failure for a call.
func (t *ToolUseTracker) failed(call ai.ToolCallBlock, text string) {
	symptom := symptomOf(text)
	if symptom == "" || harnessOutcome(symptom) {
		// A denied, blocked or abandoned call is not a usage mistake:
		// there is no form of the call that would have worked.
		return
	}
	// The symptom is the tool's own error text, and an error can quote
	// the secret that failed ("auth failed for token ghp_..."). It is
	// redacted before it becomes a key or reaches the file.
	symptom = t.redact(symptom)
	args := t.redact(capTextHint(string(call.Arguments), toolUseArgsMax, ""))
	t.mu.Lock()
	if t.pending == nil {
		t.pending = map[string]*pendingFailure{}
	}
	p := &pendingFailure{tool: call.Name, symptom: symptom, args: args, at: time.Now()}
	t.pending[pendingKey(p)] = p
	t.trimPendingLocked()
	t.mu.Unlock()
	_ = t.Store.NoteFailure(call.Name, symptom, args)
}

// trimPendingLocked drops the oldest pending failures past the cap. A
// session that trips over a hundred different tools must not hold a
// hundred pending failures: the recent ones are the ones the agent is
// about to correct.
func (t *ToolUseTracker) trimPendingLocked() {
	if len(t.pending) <= toolUsePendingMax {
		return
	}
	ordered := make([]*pendingFailure, 0, len(t.pending))
	for _, p := range t.pending {
		ordered = append(ordered, p)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].at.Before(ordered[j].at) })
	for _, p := range ordered[:len(ordered)-toolUsePendingMax] {
		delete(t.pending, pendingKey(p))
	}
}

// succeeded closes the pending failure the successful call can explain.
//
// Two ways a call can be the recovery:
//
//   - it is the SAME tool with different arguments — the recipe is the
//     difference between the two calls;
//   - it is the tool the failure NAMED — a garbled tool name ("unknown
//     tool \"ash_edit\"; the closest registered tool is \"ast_edit\"")
//     answered by a call to ast_edit.
func (t *ToolUseTracker) succeeded(call ai.ToolCallBlock) {
	t.mu.Lock()
	var same, named *pendingFailure
	for _, p := range t.pending {
		if p.tool == call.Name {
			if same == nil || p.at.After(same.at) {
				same = p
			}
			continue
		}
		if namesTool(p.symptom, call.Name) && (named == nil || p.at.After(named.at)) {
			named = p
		}
	}
	if same != nil {
		delete(t.pending, pendingKey(same))
	}
	if named != nil {
		delete(t.pending, pendingKey(named))
	}
	t.mu.Unlock()

	if same != nil {
		fix := recipe(same.args, string(call.Arguments))
		if fix == "" {
			// Nothing changed between the two calls: the failure was
			// transient, and a recipe for it would be a lie.
			return
		}
		t.record(same.tool, same.symptom, fix, "")
		return
	}
	if named != nil {
		t.record(named.tool, named.symptom,
			fmt.Sprintf("use %q instead of %q", call.Name, named.tool), call.Name)
	}
}

// record writes one recipe and reports it.
func (t *ToolUseTracker) record(tool, symptom, fix, fixTool string) {
	fix = t.redact(capTextHint(fix, toolUseFixMax, ""))
	if err := t.Store.NoteFix(tool, symptom, fix, fixTool); err != nil {
		return
	}
	if t.OnLearn != nil {
		t.OnLearn(ToolUseRecord{Tool: tool, Symptom: symptom, Fix: fix, FixTool: fixTool})
	}
}

// pendingKey is the pending map's key for one failure.
func pendingKey(p *pendingFailure) string { return p.tool + "\x00" + p.symptom }

// namesTool reports whether a failure message names a tool in quotes. The
// unknown-tool message names the closest registered tool that way, which
// is what makes a call to that tool the agent's recovery.
func namesTool(symptom, name string) bool {
	if !strings.Contains(strings.ToLower(symptom), "unknown tool") {
		return false
	}
	return strings.Contains(symptom, `"`+name+`"`)
}

// redact scrubs secrets from text before it is written.
func (t *ToolUseTracker) redact(text string) string {
	if t.Redactor == nil {
		return text
	}
	return t.Redactor(text)
}

// symptomOf reduces an error text to the one line that names the failure.
// The first non-empty line is the tool's own message; the rest is
// context the recipe does not need.
func symptomOf(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	line := text
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		line = text[:i]
	}
	line = collapseWS(line)
	if line == "" {
		return ""
	}
	if len(line) > toolUseSymptomMax {
		line = strings.TrimSpace(line[:toolUseSymptomMax-1]) + "…"
	}
	return line
}

// harnessOutcome reports whether an error is the harness talking rather
// than the tool: a refused, denied, blocked, canceled or abandoned call is
// not a usage mistake, and there is no form of it that would have worked.
func harnessOutcome(symptom string) bool {
	s := strings.ToLower(symptom)
	for _, marker := range []string{
		"refused by user",
		"tool call denied",
		"tool call blocked",
		"canceled before execution",
		"did not finish within",
		"was still running",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// argChange is one difference between two calls' arguments.
type argChange struct {
	key  string
	from any
	to   any
	// dropped marks a key the working call no longer passes.
	dropped bool
	// added marks a key the working call passes and the failing one did
	// not.
	added bool
}

// recipe renders what the agent changed to make the call work, or "" when
// there is nothing to learn:
//
//   - the same call failed and then worked — a transient failure;
//   - the two calls are different calls — the agent gave up on the tool
//     and did something else that happened to work, which says nothing
//     about how to use the tool.
//
// The difference is read as a difference in the CALL, not in the world: a
// changed value, a supplied argument, a dropped argument, or a renamed
// argument.
func recipe(failedArgs, okArgs string) string {
	f, ferr := decodeArgs(failedArgs)
	o, oerr := decodeArgs(okArgs)
	if ferr != nil || oerr != nil {
		return ""
	}
	var changed, added, dropped []argChange
	for k, ov := range o {
		fv, has := f[k]
		if !has {
			added = append(added, argChange{key: k, to: ov, added: true})
			continue
		}
		if !sameArgValue(fv, ov) {
			changed = append(changed, argChange{key: k, from: fv, to: ov})
		}
	}
	for k, fv := range f {
		if _, has := o[k]; !has {
			dropped = append(dropped, argChange{key: k, from: fv, dropped: true})
		}
	}
	sortArgs(changed)
	sortArgs(added)
	sortArgs(dropped)

	switch {
	case len(changed) > 0:
		// The agent changed a value and the call worked.
		return renderChanges(changed, added, dropped)
	case len(added) > 0 && len(dropped) == 0:
		// The agent supplied what the failing call left out.
		return renderChanges(nil, added, nil)
	case len(added) == 1 && len(dropped) == 1 && sameArgValue(added[0].to, dropped[0].from):
		// The agent renamed the argument: the value is the same and the
		// key changed. This is the mistake worth remembering most.
		return fmt.Sprintf("use %s=%s instead of %s=%s",
			added[0].key, renderArgValue(added[0].to),
			dropped[0].key, renderArgValue(dropped[0].from))
	case len(added) == 0 && len(dropped) > 0:
		// The agent dropped the argument the call choked on.
		return renderChanges(nil, nil, dropped)
	}
	return ""
}

// renderChanges renders the differences as one recipe line.
func renderChanges(changed, added, dropped []argChange) string {
	var parts []string
	for _, c := range changed {
		parts = append(parts, fmt.Sprintf("%s: %s → %s", c.key, renderArgValue(c.from), renderArgValue(c.to)))
	}
	for _, c := range added {
		parts = append(parts, fmt.Sprintf("pass %s=%s", c.key, renderArgValue(c.to)))
	}
	for _, c := range dropped {
		parts = append(parts, fmt.Sprintf("drop %s", c.key))
	}
	if len(parts) > toolUseDiffMax {
		parts = append(parts[:toolUseDiffMax], fmt.Sprintf("(+%d more)", len(parts)-toolUseDiffMax))
	}
	return strings.Join(parts, ", ")
}

// sortArgs orders the changes by key, so the same difference always
// renders the same way.
func sortArgs(changes []argChange) {
	sort.Slice(changes, func(i, j int) bool { return changes[i].key < changes[j].key })
}

// decodeArgs decodes a call's arguments into an object. Numbers keep
// their written form: 30 and 30.0 are the same argument, and a recipe
// that calls them different would teach a difference that is not there.
func decodeArgs(raw string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("memory: arguments are not an object")
	}
	return obj, nil
}

// sameArgValue reports whether two argument values are the same.
func sameArgValue(a, b any) bool {
	an, aok := a.(json.Number)
	bn, bok := b.(json.Number)
	if aok && bok {
		return an.String() == bn.String()
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

// renderArgValue renders one argument value for a recipe: quoted for a
// string, plain for a number or a boolean, compact JSON for anything
// else.
func renderArgValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		s := collapseWS(t)
		if len(s) > toolUseValueMax {
			s = s[:toolUseValueMax-1] + "…"
		}
		return strconv.Quote(s)
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	s := string(raw)
	if len(s) > toolUseValueMax {
		s = s[:toolUseValueMax-1] + "…"
	}
	return s
}
