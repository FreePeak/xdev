package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// TaskTool is the parent-facing entry point of the subagent system (M6):
// it spawns one in-process child with a restricted tool set and returns
// ONLY the child's handoff — the child transcript never enters the
// parent's context (PRD §5, yield-only isolation).
//
// Depth is capped structurally: ChildTools is the caller's restricted
// list and never includes another TaskTool, so children cannot spawn
// grandchildren (omp's task.maxRecursionDepth=1 shape; M11 adds the
// configurable guard and the spawn-policy allowlist).
type TaskTool struct {
	Provider   ai.Provider
	Model      string
	CWD        string
	ChildTools []tool.Tool // restricted set (no task, no ambient MCP/ext/LSP)
	DataDir    string      // child session persistence dir ("" = memory-only)
	MaxTurns   int
	MaxTokens  int
	System     string
	// ParentSessionID stamps the child's header (lineage for inspection;
	// the resume-path skip keys on titleSource "subagent" instead, since
	// forks legitimately carry parentSession too).
	ParentSessionID string
	// Policy/Approve/Thinking carry the parent's posture into the child so
	// approval cannot be sideloaded through delegation.
	Policy   tool.ApprovalPolicy
	Approve  ApprovalFunc
	Thinking *ai.ThinkingBudget
	// Agents are the discovered task-agent definitions (M11 #12). The
	// task tool looks up the named agent and uses its system prompt and
	// tool restrictions. nil = no named agents (default shape only).
	Agents []AgentDefinition
	// ExpandModel resolves an agent's frontmatter model — "@role" aliases
	// per discovery's documented contract — to a bare model id usable with
	// this tool's Provider. ok=false keeps the parent's model. Wired from
	// cmd where the settings live; nil disables expansion (a literal
	// frontmatter model never needs it).
	ExpandModel func(ref string) (model string, ok bool)
	// Depth counts how deep in the spawn chain we are. 0 = top-level
	// agent. omp: task.maxRecursionDepth=2; a child at the cap loses the
	// task tool (structurally guaranteed since ChildTools excludes it).
	Depth int
	// AgentName (when non-empty) is the named agent this task tool
	// was spawned FOR (spawn-policy check: can this agent spawn its
	// declared children?).
	AgentName string
	// Hub enables background task dispatch (hub tool, research §2). nil
	// makes background:true fall back to a synchronous spawn.
	Hub *Hub
	// ChildAdvisor (M11 #39, settings taskAgentAdvisor) builds the reviewer
	// attached to every spawned child; nil keeps children unadvised. The
	// host wires it (cmd/xdev/print.go): "on" resolves the advisor role's
	// model, an explicit value is a model reference. Additive: nil leaves
	// the spawn path exactly as it was.
	ChildAdvisor func() *Advisor
}

// TaskToolName is the tool name the model calls.
const TaskToolName = "task"

func (t *TaskTool) Name() string { return TaskToolName }

func (t *TaskTool) Description() string {
	return "spawn a subagent for one focused job (search, batch edits, a self-contained question); " +
		"it runs with a restricted tool set in its own session and returns only its final result — " +
		"its transcript never enters this conversation"
}

func (t *TaskTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "agent": {"type": "string", "description": "named agent type to dispatch (from discovered definitions; omit for the default shape)"},
    "prompt": {"type": "string", "description": "the task, self-contained: state the goal, the files/paths involved, and the expected result"},
    "name": {"type": "string", "description": "short label for the child session (optional)"},
    "schema": {"type": "object", "description": "JSON Schema the result must satisfy (optional)"},
    "strict": {"type": "boolean", "description": "with schema: grant the child one correction turn on mismatch (default false = accept with a note)"},
    "max_turns": {"type": "integer", "description": "turn cap for the child (default 30)"},
    "background": {"type": "boolean", "description": "start the child in the background and return a job id immediately (default false = wait for the result)"}
  },
  "required": ["prompt"]
}`)
}

// maxBatchParallel bounds a concurrent fan-out: every item is its own child
// agent loop with its own session, and the process memory budget (<100 MB
// RSS) is what an unbounded batch would actually break.
const maxBatchParallel = 8

// executeBatch runs omp's `{context, tasks[]}` shape. Each item is dispatched
// through the single-spawn path (so agent resolution, the spawn policy, the
// depth guard and the child advisor are identical between the two shapes) and
// the items run concurrently, which is the point of a batch.
func (t *TaskTool) executeBatch(ctx context.Context, a taskArgs) (tool.Result, error) {
	if t.Provider == nil {
		return tool.Result{Text: "task: no provider configured for subagents", IsError: true}, nil
	}
	type slot struct {
		text string
		err  bool
	}
	slots := make([]slot, len(a.Tasks))
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxBatchParallel)
	for i, item := range a.Tasks {
		prompt := strings.TrimSpace(item.Task)
		if prompt == "" {
			prompt = strings.TrimSpace(item.Prompt)
		}
		if prompt == "" {
			// Named like its siblings, so a failed section is attributable.
			slots[i] = slot{text: batchItemLabel(i, item) + ": no `task` text", err: true}
			continue
		}
		if a.Context != "" {
			// The shared context leads, so every child sees the same framing
			// the batch author intended (omp's semantics for `context`).
			prompt = a.Context + "\n\n" + prompt
		}
		sub, err := json.Marshal(taskArgs{
			Prompt: prompt, Agent: item.Agent, Name: item.Name,
			Schema: item.Schema, Strict: item.Strict, MaxTurns: item.MaxTurns,
			Background: a.Background,
		})
		if err != nil {
			slots[i] = slot{text: batchItemLabel(i, item) + ": " + err.Error(), err: true}
			continue
		}
		wg.Add(1)
		go func(i int, sub json.RawMessage, label string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res, err := t.Execute(ctx, sub)
			if err != nil {
				slots[i] = slot{text: label + ": " + err.Error(), err: true}
				return
			}
			slots[i] = slot{text: label + "\n" + res.Text, err: res.IsError}
		}(i, sub, batchItemLabel(i, item))
	}
	wg.Wait()

	var sb strings.Builder
	failed := 0
	for _, sl := range slots {
		if sl.err {
			failed++
		}
		sb.WriteString(strings.TrimRight(sl.text, "\n"))
		sb.WriteString("\n\n")
	}
	head := fmt.Sprintf("batch: %d task(s), %d failed", len(a.Tasks), failed)
	return tool.Result{Text: head + "\n\n" + strings.TrimRight(sb.String(), "\n") + "\n"}, nil
}

// batchItemLabel names one batch section for the parent's report.
func batchItemLabel(i int, item taskItem) string {
	switch {
	case item.Name != "":
		return "· " + item.Name
	case item.Agent != "":
		return "· " + item.Agent
	default:
		return fmt.Sprintf("· task #%d", i+1)
	}
}

// taskArgs is one spawn request. Tasks/Context carry omp's batch shape
// (`{context, tasks[]}`): a model following the baseline's tool docs sends a
// batch by default, and rejecting it with "prompt is required" made the whole
// feature look broken (parity finding T3 #1).
type taskArgs struct {
	Prompt     string          `json:"prompt"`
	Agent      string          `json:"agent"` // named agent type (optional)
	Name       string          `json:"name"`  // session label (optional)
	Schema     json.RawMessage `json:"schema"`
	Strict     bool            `json:"strict"`
	MaxTurns   int             `json:"max_turns"`
	Background bool            `json:"background"`
	// Context applies to every item of a batch (shared project state).
	Context string `json:"context"`
	// Tasks are the batch items; `task` is omp's field for the assignment
	// and `prompt` is accepted as its alias.
	Tasks []taskItem `json:"tasks"`
}

// taskItem is one batch assignment.
type taskItem struct {
	Name     string          `json:"name"`
	Agent    string          `json:"agent"`
	Task     string          `json:"task"`
	Prompt   string          `json:"prompt"`
	Schema   json.RawMessage `json:"schema"`
	Strict   bool            `json:"strict"`
	MaxTurns int             `json:"max_turns"`
}

// Execute spawns the child (or the batch) and renders the handoff for the
// parent model.
func (t *TaskTool) Execute(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var a taskArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return tool.Result{Text: "task: malformed arguments: " + err.Error(), IsError: true}, nil
	}
	if len(a.Tasks) > 0 {
		return t.executeBatch(ctx, a)
	}
	if strings.TrimSpace(a.Prompt) == "" {
		return tool.Result{Text: "task: prompt is required (or send a batch with {context, tasks[]})", IsError: true}, nil
	}
	if t.Provider == nil {
		return tool.Result{Text: "task: no provider configured for subagents", IsError: true}, nil
	}

	// Depth guard: omp task.maxRecursionDepth=2. At the cap, the child
	// has no ChildTools with a TaskTool and cannot spawn further.
	const maxDepth = 2
	if t.Depth >= maxDepth {
		return tool.Result{Text: "task: recursion depth limit reached (cannot spawn deeper)", IsError: true}, nil
	}

	// Back-compat for model habits: `name` doubles as the agent type when
	// it matches a discovered definition and `agent` was left empty.
	agentArg := a.Agent
	if agentArg == "" {
		if _, ok := FindAgent(t.Agents, a.Name); ok {
			agentArg = a.Name
		}
	}

	// Resolve the agent definition (if named) and apply its restrictions.
	agentSystem := t.System
	agentTools := t.ChildTools
	agentModel := t.Model
	if agentArg != "" {
		def, ok := FindAgent(t.Agents, agentArg)
		if !ok {
			return tool.Result{Text: fmt.Sprintf("task: unknown agent %q (available: %s)", agentArg, agentNames(t.Agents)), IsError: true}, nil
		}
		// Spawn policy: can this parent agent spawn the requested child?
		// A restrictive parent (None or a non-matching allowlist) denies.
		if t.AgentName != "" {
			if parent, ok := FindAgent(t.Agents, t.AgentName); ok {
				pol := parent.ResolveSpawnPolicy()
				blocked := pol.None ||
					(!pol.AllowAll && len(pol.Allow) > 0 && !slices.Contains(pol.Allow, agentArg))
				if blocked {
					return tool.Result{Text: fmt.Sprintf("task: %s is not allowed to spawn %s (spawns policy)", t.AgentName, agentArg), IsError: true}, nil
				}
			}
		}
		if def.SystemPrompt != "" {
			agentSystem = def.SystemPrompt
		}
		if len(def.Tools) > 0 {
			agentTools = resolveAgentTools(def.Tools, t.ChildTools, t.Depth, maxDepth)
		}
		if def.Model != "" {
			if strings.HasPrefix(def.Model, "@") && t.ExpandModel != nil {
				// The alias resolves per modelRoles; an unresolvable one
				// keeps the parent model (discovery promises expansion,
				// not a hard failure on a typo).
				if m, ok := t.ExpandModel(def.Model); ok {
					agentModel = m
				}
			} else {
				agentModel = def.Model
			}
		}
		// Recursive spawn (omp task.maxRecursionDepth semantics): a child
		// below the cap gets its own task tool so it can dispatch further
		// (still-capped) children. The child inherits the resolved agent
		// name, so ITS spawns policy governs the next level. The depth
		// guard at the top of this function terminates the chain.
		if slices.Contains(def.Tools, "task") && t.Depth+1 < maxDepth {
			agentTools = append(agentTools, &TaskTool{
				Provider:        t.Provider,
				Model:           agentModel,
				CWD:             t.CWD,
				ChildTools:      t.ChildTools,
				DataDir:         t.DataDir,
				MaxTurns:        t.MaxTurns,
				MaxTokens:       t.MaxTokens,
				System:          agentSystem,
				ParentSessionID: t.ParentSessionID,
				Policy:          t.Policy,
				Approve:         t.Approve,
				Thinking:        t.Thinking,
				Agents:          t.Agents,
				Depth:           t.Depth + 1,
				AgentName:       agentArg,
				Hub:             t.Hub,
				ChildAdvisor:    t.ChildAdvisor,
			})
		}
	}

	mt := a.MaxTurns
	if mt <= 0 {
		mt = t.MaxTurns
	}
	spec := SubagentSpec{
		Name: a.Name, Prompt: a.Prompt, System: agentSystem,
		Provider: t.Provider, Model: agentModel, Tools: agentTools,
		CWD: t.CWD, DataDir: t.DataDir, MaxTurns: mt, MaxTokens: t.MaxTokens,
		Output:          &SubagentOutput{Schema: a.Schema, Strict: a.Strict},
		ParentSessionID: t.ParentSessionID,
		Policy:          t.Policy,
		Approve:         t.Approve,
		Thinking:        t.Thinking,
	}

	// task.agentAdvisor (M11 #39): give the child its own reviewer, wired
	// into the child's transcript before the run starts.
	if t.ChildAdvisor != nil {
		attachChildAdvisor(ctx, &spec, t.ChildAdvisor)
	}
	if a.Background {
		if t.Hub == nil {
			return tool.Result{Text: "task: no hub configured — cannot dispatch in the background", IsError: true}, nil
		}
		id, err := t.Hub.Start(ctx, spec)
		if err != nil {
			return tool.Result{Text: "task: " + err.Error(), IsError: true}, nil
		}
		return tool.Result{
			Text:    "started background job " + id + " (hub wait/jobs/send/cancel to drive it)",
			Details: map[string]string{"job": id},
		}, nil
	}
	res, err := SpawnChild(ctx, spec)
	if err != nil {
		return tool.Result{Text: "task: " + err.Error(), IsError: true}, nil
	}
	return tool.Result{
		Text:    renderSubagentResult(res),
		Details: res,
		IsError: res.Status == "failed" || res.Status == "schema-mismatch",
	}, nil
}

// agentNames lists available agent names for error messages.
func agentNames(defs []AgentDefinition) string {
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name)
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// resolveAgentTools builds the child's tool set from the agent's declared
// tool names. At the depth cap, the task tool is removed (recursive spawn
// blocked structurally). Unknown tool names are skipped (forward-compat:
// the agent may name tools this binary does not have).
func resolveAgentTools(declared stringList, parentTools []tool.Tool, depth, maxDepth int) []tool.Tool {
	// We cannot rebuild the full registry here; we filter parent tools by
	// name. yield is added by SpawnChild.
	allowed := map[string]bool{}
	for _, name := range declared {
		allowed[name] = true
	}
	var out []tool.Tool
	for _, t := range parentTools {
		if allowed[t.Name()] {
			out = append(out, t)
		}
	}
	return out
}

// renderSubagentResult is the parent-visible surface: status, the
// handoff payload, artifact paths, and the child session id for
// post-hoc inspection. Never the transcript.
func renderSubagentResult(res *SubagentResult) string {
	var b strings.Builder
	switch res.Status {
	case "yielded", "completed":
		fmt.Fprintf(&b, "subagent %s\n", res.Status)
	case "schema-mismatch":
		fmt.Fprintf(&b, "subagent result does not match the required schema: %s\n", res.Note)
	default:
		fmt.Fprintf(&b, "subagent failed: %s\n", res.Err)
	}
	if res.Text != "" {
		b.WriteString("\n" + res.Text + "\n")
	} else if len(res.Yield) > 0 {
		b.WriteString("\n" + string(res.Yield) + "\n")
	}
	if len(res.Files) > 0 {
		b.WriteString("\nartifacts:\n")
		for _, f := range res.Files {
			b.WriteString("- " + f + "\n")
		}
	}
	// The note renders for a yielded repair AND for a no-yield completion —
	// the latter is precisely the warning the parent must see (T3 #6).
	if res.Note != "" && (res.Status == "yielded" || res.Status == "completed") {
		b.WriteString("\nnote: " + res.Note + "\n")
	}
	if res.SessionID != "" {
		fmt.Fprintf(&b, "\nsession: %s\n", res.SessionID[:min(8, len(res.SessionID))])
	}
	return b.String()
}

// attachChildAdvisor wires a per-child reviewer (M11 #39 task.agentAdvisor)
// into a spawn: the reviewer gets the child as its steering target and sees
// the child's transcript deltas through the turn hooks. Each delta triggers
// one background review — the same shape as the session-level advisor, so a
// slow reviewer never blocks the child.
func attachChildAdvisor(ctx context.Context, spec *SubagentSpec, build func() *Advisor) {
	base := spec.OnRun
	spec.OnRun = func(ag *Agent) {
		if base != nil {
			base(ag) // the hub registers the live child here
		}
		adv := build()
		if adv == nil {
			return
		}
		adv.Primary = ag
		// The reviewer's view of the child: the task itself plus every
		// assistant/tool-result message in hook order (which is the child's
		// own history order). Synthetic user messages — steering,
		// continuations — stay out, since the advisor should not review its
		// own injected advice.
		var mu sync.Mutex
		hist := []ai.Message{{
			Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: spec.Prompt}},
		}}
		feed := func() {
			mu.Lock()
			snap := append([]ai.Message(nil), hist...)
			mu.Unlock()
			go adv.Feed(ctx, snap)
		}
		// Preserve the hooks SpawnChild installed (the child's session
		// mirror) and add the delta mirror on top.
		var wrapped TurnHooksFunc
		if f, ok := ag.Hooks.(TurnHooksFunc); ok {
			wrapped = f
		}
		prevMsg, prevTool := wrapped.OnMessageEndF, wrapped.OnToolResultMsgF
		wrapped.OnMessageEndF = func(m *ai.Message) {
			if prevMsg != nil {
				prevMsg(m)
			}
			mu.Lock()
			hist = append(hist, *m)
			mu.Unlock()
			feed()
		}
		wrapped.OnToolResultMsgF = func(m *ai.Message) {
			if prevTool != nil {
				prevTool(m)
			}
			mu.Lock()
			hist = append(hist, *m)
			mu.Unlock()
		}
		ag.Hooks = wrapped
	}
}
