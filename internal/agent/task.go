package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/logx"
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
	// Agents is an explicit definition set: the host normally leaves it nil
	// and uses AgentRoots below, so tests and in-process callers can pin the
	// set a spawn resolves against.
	Agents []AgentDefinition
	// AgentRoots is the cwd task agents are discovered from (M11 #12). When
	// set, the definitions are resolved PER SPAWN (TaskTool.discoverAgents),
	// so a newly written .xdev/agents/foo.md is spawnable without a restart
	// (#272). Both empty = no named agents: the default child shape still
	// works and every `agent` argument fails as unknown.
	AgentRoots string
	// ExpandEffort resolves an agent's frontmatter thinkingLevel to a
	// reasoning budget for the child (config.EffortBudget in production).
	// nil keeps the parent's Thinking.
	ExpandEffort func(level string) *ai.ThinkingBudget
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
	// mu guards the per-spawn discovery cache below (Description() is
	// rendered on every model request, and batch spawns run concurrently).
	mu       sync.Mutex
	cache    []AgentDefinition
	cachedAt string

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
	desc := "spawn a subagent for one focused job (search, batch edits, a self-contained question); " +
		"it runs with a restricted tool set in its own session and returns only its final result — " +
		"its transcript never enters this conversation"
	if agents := t.advertiseAgents(); agents != "" {
		// #272: while nothing named the legal values, guessing an agent was
		// the expected outcome — so the whole feature read as broken.
		desc += "\n\nnamed agent types (agent field; omit for the default shape):\n" + agents
	}
	return desc
}

// advertiseAgents lists the spawnable definitions for the model, one line
// each. It rides the same cache as resolution, so a per-request call never
// re-scans the roots.
func (t *TaskTool) advertiseAgents() string {
	var b strings.Builder
	for _, d := range t.discoverAgents() {
		b.WriteString("- " + d.Name + ": " + oneLine(d.Description) + "\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// oneLine flattens a definition's description for a listing.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// discoverAgents resolves the definitions this spawn can name. A pinned
// Agents set wins (tests, in-process callers); otherwise the roots are
// re-read, so a file written mid-session is immediately spawnable — the
// frozen-at-startup snapshot was half of #272's invisibility. The result is
// cached against the file mtimes of the roots, because Description() is
// rendered on every model request and a plain re-scan would be a stat storm
// (same mtime discipline as config.Settings.Mtime).
func (t *TaskTool) discoverAgents() []AgentDefinition {
	if t.Agents != nil || t.AgentRoots == "" {
		return t.Agents
	}
	fingerprint := agentsFingerprint(t.AgentRoots)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cachedAt == fingerprint && t.cache != nil {
		return t.cache
	}
	defs, warnings := DiscoverAgents(t.AgentRoots)
	for _, w := range warnings {
		logx.Warnf("task agent: %s", w)
	}
	t.cache, t.cachedAt = defs, fingerprint
	return defs
}

// agentsFingerprint summarizes the state of every discovery root, bundled
// definitions included (they never change within a process, so they need no
// stat). A changed file — added, removed, or edited — changes the string.
func agentsFingerprint(cwd string) string {
	var b strings.Builder
	for _, root := range AgentDiscoveryRoots(cwd) {
		if root == AgentRootBundled {
			continue // embedded: cannot change inside a running process
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			fmt.Fprintf(&b, "%s/%d/%d;", root, info.Size(), info.ModTime().UnixNano())
		}
	}
	return b.String()
}

func (t *TaskTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "agent": {"type": "string", "description": "named agent type to dispatch; the available definitions and their descriptions are listed at the end of this tool description (omit for the default shape)"},
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

	// Resolved per spawn so a mid-session .xdev/agents addition is usable
	// without a restart (#272).
	defs := t.discoverAgents()

	// Back-compat for model habits: `name` doubles as the agent type when
	// it matches a discovered definition and `agent` was left empty.
	agentArg := a.Agent
	if agentArg == "" {
		if _, ok := FindAgent(defs, a.Name); ok {
			agentArg = a.Name
		}
	}

	// Resolve the agent definition (if named) and apply its restrictions.
	agentSystem := t.System
	agentTools := t.ChildTools
	agentModel := t.Model
	agentThinking := t.Thinking
	// What the definition asked for that this binary cannot do, rendered
	// into the handoff so BOTH readers see it: the model (whose spawn ran
	// with a narrower child than it authored) and, in the TUI, the user
	// (#272: every one of these was silent).
	var agentNotes []string
	if agentArg != "" {
		def, ok := FindAgent(defs, agentArg)
		if !ok {
			return tool.Result{Text: fmt.Sprintf("task: unknown agent %q (available: %s)", agentArg, agentNames(defs)), IsError: true}, nil
		}
		// Spawn policy: can this parent agent spawn the requested child?
		// A restrictive parent (None or a non-matching allowlist) denies.
		if t.AgentName != "" {
			if parent, ok := FindAgent(defs, t.AgentName); ok {
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
			if missing := missingAgentTools(def.Tools, agentTools, t.Depth, maxDepth); len(missing) > 0 {
				// A tool the parent does not carry is not the same as a typo,
				// and both silently narrowed the child (#272): report them.
				note := fmt.Sprintf("agent %q: tool(s) not available to children, dropped: %s", def.Name, strings.Join(missing, ", "))
				logx.Warnf("%s", note)
				agentNotes = append(agentNotes, note)
			}
		}
		for _, key := range def.Unsupported {
			note := fmt.Sprintf("agent %q: unsupported frontmatter key %q (ignored)", def.Name, key)
			logx.Warnf("%s", note)
			agentNotes = append(agentNotes, note)
		}
		if def.ThinkingLevel != "" {
			// An effort the resolver does not know keeps the parent's budget;
			// the field was parsed and dropped before #272.
			if t.ExpandEffort == nil {
				agentNotes = append(agentNotes, fmt.Sprintf("agent %q: thinkingLevel %q ignored (host supplies no effort resolver)", def.Name, def.ThinkingLevel))
			} else if b := t.ExpandEffort(def.ThinkingLevel); b != nil {
				agentThinking = b
			} else {
				note := fmt.Sprintf("agent %q: unsupported thinkingLevel %q (want %s) — parent effort kept",
					def.Name, def.ThinkingLevel, strings.Join(config.EffortLevels, "|"))
				logx.Warnf("%s", note)
				agentNotes = append(agentNotes, note)
			}
		}
		if def.Model != "" {
			// A literal provider/model is the child's model. A bare model
			// alias ("@…" — the removed model-role form) is not a ref:
			// report it instead of forwarding it to the wire, which 404s
			// one turn later (#272). The child then runs on the same model
			// as its parent.
			if strings.HasPrefix(def.Model, "@") {
				note := fmt.Sprintf("agent %q: frontmatter model %q is not a model ref — parent model kept", def.Name, def.Model)
				logx.Warnf("%s", note)
				agentNotes = append(agentNotes, note)
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
				Thinking:        agentThinking,
				// Inherit the resolution SOURCE, not a snapshot: a parent
				// that discovers keeps discovering, a pinned set stays pinned.
				Agents:       t.Agents,
				AgentRoots:   t.AgentRoots,
				ExpandEffort: t.ExpandEffort,
				Depth:        t.Depth + 1,
				AgentName:    agentArg,
				Hub:          t.Hub,
				ChildAdvisor: t.ChildAdvisor,
			})
		}
		// The granted set, once, for --verbose: which tools the child of a
		// named agent actually holds is otherwise invisible to the author.
		logx.Debugf("task: agent %q granted %s", def.Name, strings.Join(toolNames(agentTools), ", "))
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
		Thinking:        agentThinking,
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
			Text:    "started background job " + id + " (hub wait/jobs/send/cancel to drive it)" + renderAgentNotes(agentNotes),
			Details: map[string]string{"job": id},
		}, nil
	}
	res, err := SpawnChild(ctx, spec)
	if err != nil {
		return tool.Result{Text: "task: " + err.Error(), IsError: true}, nil
	}
	return tool.Result{
		Text:    renderSubagentResult(res) + renderAgentNotes(agentNotes),
		Details: res,
		IsError: res.Status == "failed" || res.Status == "schema-mismatch",
	}, nil
}

// renderAgentNotes appends what the definition asked for that could not be
// honoured. The child ran narrower than the parent intended, so the
// assumption has to be in the handoff the parent reads, not only in a log
// file nobody opens (#272).
func renderAgentNotes(notes []string) string {
	if len(notes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nwarnings:\n")
	for _, n := range notes {
		b.WriteString("- " + n + "\n")
	}
	return b.String()
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
// blocked structurally). Names the parent does not carry are skipped
// (forward-compat: the agent may name tools this binary does not have) and
// reported by missingAgentTools, so a typo no longer reads as a config that
// took effect (#272).
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

// toolNames lists a tool set by name for a log line.
func toolNames(tools []tool.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name())
	}
	slices.Sort(out)
	return out
}

// missingAgentTools lists declared tool names the child did not get: either
// the parent never carried them (a typo, or a tool this binary has no child
// version of) — in both cases the child is narrower than the author meant.
func missingAgentTools(declared stringList, granted []tool.Tool, depth, maxDepth int) []string {
	have := map[string]bool{"yield": true}
	for _, t := range granted {
		have[t.Name()] = true
	}
	// A declared `task` is granted structurally by the recursive spawn above,
	// and only below the depth cap.
	if depth+1 < maxDepth {
		have[TaskToolName] = true
	}
	var out []string
	for _, name := range declared {
		if !have[name] {
			out = append(out, name)
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
