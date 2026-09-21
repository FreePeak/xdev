package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// Subagent execution (M6 #7): in-process goroutine children with their
// own SessionStore, restricted tool sets (the caller decides what a child
// may hold — no ambient MCP/extensions/LSP by default), a `yield` tool
// that ends the run with a payload, and an output contract enforced
// permissively or strictly. Out-of-process children (same binary in
// print/rpc mode) are a later isolation concern; the shape is identical.

// DefaultSubagentMaxTurns bounds a child run (children do one job).
const DefaultSubagentMaxTurns = 30

// SubagentSpec configures one child run.
type SubagentSpec struct {
	Name     string // label for the child's session title
	Prompt   string // the task, delivered as the first user message
	System   string // the child's own system prompt
	Provider ai.Provider
	Model    string
	Tools    []tool.Tool // restricted set; yield is appended automatically
	CWD      string      // the child's working directory
	// DataDir persists the child's JSONL under <DataDir>/sessions
	// (empty = memory-only child session).
	DataDir   string
	MaxTurns  int // 0 → DefaultSubagentMaxTurns
	MaxTokens int
	Output    *SubagentOutput
	// ParentSessionID stamps parentSession into the child's header so
	// resume paths can tell children from user sessions.
	ParentSessionID string
	// Policy/Approve inherit the parent's approval posture: a session under
	// `write` must not gain an unapproved side channel through its children.
	// The child's `yield` is always allowed — a child that cannot hand back
	// its result is not a child, it is a hang.
	Policy  tool.ApprovalPolicy
	Approve ApprovalFunc
	// Thinking inherits the parent's resolved effort.
	Thinking *ai.ThinkingBudget
	// OnRun (optional) receives the live child Agent just before its run
	// starts — the hub uses it to expose steering to the parent session.
	OnRun func(*Agent)
}

// SubagentOutput is the yield payload contract.
type SubagentOutput struct {
	// Schema is a JSON Schema describing the result value. Empty =
	// free-form text result.
	Schema json.RawMessage
	// Strict validates against Schema and grants ONE correction turn on
	// mismatch before failing; permissive accepts the raw payload and
	// records the validation note.
	Strict bool
}

// SubagentResult is what the parent observes.
type SubagentResult struct {
	Status    string // "yielded" | "completed" | "schema-mismatch" | "failed"
	Text      string // final assistant text, or the yield's string value
	Yield     json.RawMessage
	Files     []string // artifact paths handed off by yield
	Note      string   // validation note (permissive mode / mismatch)
	Err       string   // terminal error when Status == "failed"
	SessionID string   // child session id for post-hoc inspection
}

// yieldTool ends a child run: it records the payload once and cancels the
// child context, which SpawnChild surfaces as a successful handoff rather
// than an abort (the yielded flag is checked first).
type yieldTool struct {
	mu     sync.Mutex
	done   bool
	result json.RawMessage
	files  []string
	cancel context.CancelFunc

	schema json.RawMessage // non-nil: the result value is schema-typed
}

func (y *yieldTool) Name() string { return "yield" }

func (y *yieldTool) Description() string {
	return "end the task and hand the result back to the caller; call it exactly once, as the last action"
}

func (y *yieldTool) Parameters() json.RawMessage {
	result := json.RawMessage(`{"type":"string","description":"the final result"}`)
	y.mu.Lock()
	sch := y.schema
	y.mu.Unlock()
	if len(sch) > 0 {
		result = sch
	}
	p, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"result": result,
			"files": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "artifact file paths the caller should pick up",
			},
		},
		"required": []string{"result"},
	})
	return p
}

func (y *yieldTool) Execute(_ context.Context, args json.RawMessage) (tool.Result, error) {
	var a struct {
		Result json.RawMessage `json:"result"`
		Files  []string        `json:"files"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return tool.Result{Text: "yield: malformed arguments: " + err.Error(), IsError: true}, nil
	}
	if len(a.Result) == 0 {
		return tool.Result{Text: "yield: result is required", IsError: true}, nil
	}
	y.mu.Lock()
	cancel := y.cancel
	if !y.done {
		y.done = true
		y.result = a.Result
		y.files = a.Files
	}
	y.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return tool.Result{Text: "yield accepted"}, nil
}

// snapshot copies the current handoff state.
func (y *yieldTool) snapshot() (done bool, result json.RawMessage, files []string) {
	y.mu.Lock()
	defer y.mu.Unlock()
	if !y.done {
		return false, nil, nil
	}
	return true, append(json.RawMessage(nil), y.result...), y.files
}

// SpawnChild runs one subagent to completion and returns its handoff. The
// parent's ctx bounds wall time; ctx cancellation (not yield) fails the
// child. Errors from the provider surface in Result.Err with
// Status=="failed"; the Go error is reserved for spec/persistence faults.
func SpawnChild(ctx context.Context, spec SubagentSpec) (*SubagentResult, error) {
	if spec.Provider == nil || spec.Prompt == "" {
		return nil, fmt.Errorf("agent: subagent spec requires Provider and Prompt")
	}
	schema := spec.Output.schema()
	yt := &yieldTool{schema: schema}
	reg := tool.NewRegistry()
	reg.Register(yt)
	for _, t := range spec.Tools {
		reg.Register(t)
	}

	cwd := spec.CWD
	if cwd == "" {
		cwd = "."
	}
	// The child gets the SAME always-loaded conventions its parent does. Until
	// this line existed the context-file hierarchy reached only the parent
	// prompt (cmd/xdev/print.go promptFnWithMemory), so a spawn ran with the
	// bare base prose and none of the standing rules — and a subagent is
	// exactly the actor handed "just make this one-line fix".
	spec.System = childSystem(spec, cwd)
	title := "subagent: " + spec.Name
	store := session.OpenMem(cwd, title)
	// A child is user-invisible: titleSource "subagent" marks it so resume
	// paths skip it (forks keep "auto" + parentSession — those ARE user
	// sessions), and the collector stamps the parent link for lineage.
	store.SetTitleSourceSubagent()
	if spec.DataDir != "" {
		now := time.Now().UTC()
		store.EnableAutoPersist(
			session.SessionFilePath(spec.DataDir, cwd, now, store.ID()),
			session.Options{ParentSession: spec.ParentSessionID},
		)
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			logx.Errorf("subagent store close: %v", cerr)
		}
	}()

	hooks := TurnHooksFunc{
		OnMessageEndF:    func(m *ai.Message) { _ = store.Append(&session.MessageEntry{Message: *m}) },
		OnToolResultMsgF: func(m *ai.Message) { _ = store.Append(&session.MessageEntry{Message: *m}) },
	}
	mt := spec.MaxTurns
	if mt <= 0 {
		mt = DefaultSubagentMaxTurns
	}
	childPolicy := spec.Policy
	if childPolicy.PerTool == nil {
		childPolicy.PerTool = map[string]tool.Action{}
	}
	// yield is exempt from prompting: it is the handoff mechanism itself.
	if _, set := childPolicy.PerTool["yield"]; !set {
		childPolicy.PerTool["yield"] = tool.ActionAllow
	}
	ag := &Agent{
		Provider:  spec.Provider,
		Tools:     reg,
		Hooks:     hooks,
		Store:     store,
		Model:     spec.Model,
		MaxTokens: spec.MaxTokens,
		MaxTurns:  mt,
		Policy:    childPolicy,
		Approve:   spec.Approve,
		Thinking:  spec.Thinking,
	}
	if spec.OnRun != nil {
		spec.OnRun(ag)
	}

	user := ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: spec.Prompt}}}
	if err := store.Append(&session.MessageEntry{Message: user}); err != nil {
		return nil, err
	}

	res := &SubagentResult{SessionID: store.ID()}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	yt.mu.Lock()
	yt.cancel = cancel
	yt.mu.Unlock()

	final, err := ag.Run(cctx, spec.System, []ai.Message{user})
	res.apply(yt, final, err, cctx)

	// A child that ends its run WITHOUT calling yield returns loose prose,
	// and "completed" made that indistinguishable from a real handoff for
	// the parent (parity finding T3 #6). Nudge it — the same corrective-turn
	// machinery the strict-schema repair uses — and when the nudges run out,
	// say so in the Note the parent renders.
	//
	// ponytail: two nudges, not omp's three reminders: each one is a fresh
	// provider round-trip per child, and the warning after the second is
	// already the signal the parent acts on.
	for n := 1; res.Status == "completed" && n <= yieldNudges; n++ {
		f2, e2 := yieldNudge(ctx, ag, yt, spec, store, n)
		res.apply(yt, f2, e2, ctx)
	}
	if res.Status == "completed" {
		res.Note = fmt.Sprintf("child never called yield after %d reminders — the text is its loose final reply, not a structured handoff", yieldNudges)
	}

	// Output contract for schema-typed yields.
	if done, yres, _ := yt.snapshot(); done && len(schema) > 0 {
		note := validateTopLevel(schema, yres)
		switch {
		case note == "":
			res.Yield = yres
			res.setText(yres)
		case spec.Output.Strict:
			// One correction turn, fresh child context.
			strictRetry(ctx, ag, yt, spec, store)
			done2, yres2, files2 := yt.snapshot()
			if done2 && validateTopLevel(schema, yres2) == "" {
				res.Status = "yielded"
				res.Yield = yres2
				res.Files = files2
				res.setText(yres2)
				res.Note = "repaired after: " + note
			} else {
				res.Status = "schema-mismatch"
				res.Note = note
			}
		default:
			res.Note = note // permissive: accept raw, record why it's off-contract
			res.Yield = yres
		}
	}
	return res, nil
}

// apply classifies a finished child run into the result.
func (r *SubagentResult) apply(yt *yieldTool, final *ai.Message, err error, cctx context.Context) {
	done, yres, files := yt.snapshot()
	switch {
	case done:
		r.Status = "yielded"
		r.Files = files
		r.setText(yres)
		if err != nil && cctx.Err() == nil {
			r.Err = err.Error() // a real failure beside the yield still surfaces
		}
	case err != nil:
		r.Status = "failed"
		r.Err = err.Error()
	case final != nil:
		r.Status = "completed"
		r.Text = final.Text()
	default:
		r.Status = "failed"
		r.Err = "agent: subagent produced no result"
	}
}

// setText lifts a string yield into Text.
func (r *SubagentResult) setText(raw json.RawMessage) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		r.Text = s
	} else {
		r.Yield = raw
	}
}

// yieldNudges is how many times a child that finished without yielding is
// asked to call the yield tool before the parent is warned.
const yieldNudges = 2

// yieldNudge runs one corrective turn: the child is told the handoff must go
// through yield, and its answer re-enters the classification path. Mirrors
// strictRetry's contract (re-arm the yield tool, rebuild history from the
// store so the nudge is persisted and a resume sees the same transcript).
func yieldNudge(ctx context.Context, ag *Agent, yt *yieldTool, spec SubagentSpec, store *session.Store, n int) (*ai.Message, error) {
	msg := ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{
		Text: fmt.Sprintf("you finished without calling yield — call the yield tool now with your result (attempt %d of %d); a plain reply is not delivered to the caller", n, yieldNudges),
	}}}
	if err := store.Append(&session.MessageEntry{Message: msg}); err != nil {
		return nil, err
	}
	yt.mu.Lock()
	yt.done, yt.result, yt.files = false, nil, nil
	cctx, cancel := context.WithCancel(ctx)
	yt.cancel = cancel
	yt.mu.Unlock()
	defer cancel()
	hist, err := session.BuildContext(store.Entries(), store.LeafID(), session.SystemPrompt{})
	if err != nil {
		return nil, err
	}
	final, rerr := ag.Run(cctx, spec.System, hist.Messages)
	return final, rerr
}

// strictRetry grants the child one correction turn on schema mismatch.
func strictRetry(ctx context.Context, ag *Agent, yt *yieldTool, spec SubagentSpec, store *session.Store) {
	_, cur, _ := yt.snapshot()
	note := validateTopLevel(spec.Output.schema(), cur)
	msg := ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{
		Text: "your yield result does not match the required schema: " + note + " — fix it and call yield again",
	}}}
	if err := store.Append(&session.MessageEntry{Message: msg}); err != nil {
		return
	}
	yt.mu.Lock()
	yt.done, yt.result, yt.files = false, nil, nil
	cctx, cancel := context.WithCancel(ctx)
	yt.cancel = cancel
	yt.mu.Unlock()
	defer cancel()
	hist, err := session.BuildContext(store.Entries(), store.LeafID(), session.SystemPrompt{})
	if err != nil {
		return
	}
	if _, err := ag.Run(cctx, spec.System, hist.Messages); err != nil {
		logx.Debugf("subagent retry run: %v", err)
	}
}

// childSystem is the child's system prompt: whatever the spec (or a named
// agent definition) supplied, plus the always-loaded context-file hierarchy
// for the child's working directory. Appending — never replacing — is the
// point: a definition prompt is authored guidance, the global conventions are
// policy, and a definition that omits the policy must not be able to drop it.
//
// No memory guidance block here: recall is keyed to a session's own history
// and a child in a fresh session has none, so the parent's accumulated
// lessons would arrive in a context they were never about.
func childSystem(spec SubagentSpec, cwd string) string {
	files := LoadContextFiles(cwd)
	if files == "" {
		return spec.System
	}
	return spec.System + "\n\n# Project context\n" + files
}

// schema returns the configured output schema (nil-safe).
func (o *SubagentOutput) schema() json.RawMessage {
	if o == nil {
		return nil
	}
	return o.Schema
}

// validateTopLevel is a deliberately minimal JSON Schema checker: it
// verifies the value kind and, for objects, the declared required keys
// and the primitive type of every declared property. No recursion into
// nested schemas — deep validation needs a real validator dependency,
// which the CGO-free minimalism budget rejects for now (upgrade path:
// github.com/santhosh-tekuri/jsonschema when demand is proven).
// Returns "" when the value satisfies the schema.
func validateTopLevel(schema, value json.RawMessage) string {
	var sc struct {
		Type       string                     `json:"type"`
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schema, &sc); err != nil {
		return "schema is not a valid JSON object"
	}
	var v any
	if err := json.Unmarshal(value, &v); err != nil {
		return "result is not valid JSON"
	}
	if sc.Type != "" && kindOf(v) != sc.Type {
		return fmt.Sprintf("result is %s, schema wants %s", kindOf(v), sc.Type)
	}
	if sc.Type == "object" || sc.Type == "" {
		obj, ok := v.(map[string]any)
		if !ok {
			if sc.Type == "object" {
				return "result is not an object"
			}
			return ""
		}
		for _, rq := range sc.Required {
			if _, has := obj[rq]; !has {
				return fmt.Sprintf("missing required property %q", rq)
			}
		}
		for name, raw := range obj {
			ps, declared := sc.Properties[name]
			if !declared {
				continue
			}
			var pt struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(ps, &pt) != nil || pt.Type == "" {
				continue
			}
			if k := kindOf(raw); k != pt.Type {
				return fmt.Sprintf("property %q is %s, schema wants %s", name, k, pt.Type)
			}
		}
	}
	return ""
}

// kindOf maps a decoded JSON value to its schema type name.
func kindOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}
