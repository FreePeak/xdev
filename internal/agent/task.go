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
// Depth is bounded by MaxDepth (default DefaultSubagentMaxDepth): a named
// agent whose definition declares `task` in its tools gets its own TaskTool
// while it is below the cap, and the guard at the top of Execute terminates
// the chain. A child's tool set never carries the task tool by accident —
// ChildTools excludes it, so the grant is explicit and depth-checked.
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
	// nil keeps the parent's Thinking. The model can also ask for a rung
	// directly on the call (`effort`), which resolves through this same
	// seam — one vocabulary, one place that knows the ladder.
	ExpandEffort func(level string) *ai.ThinkingBudget
	// ModelFor resolves a model reference ("provider/model" or a bare id) to
	// a provider and a model id for a child — the `model` argument on a
	// spawn, or an agent definition's `model:` frontmatter. nil means "the
	// parent's provider only": a definition that names another provider is
	// then reported as unhonoured rather than silently run on the wrong one.
	// The host wires it (cmd/xdev/print.go) with the same resolveModel +
	// buildProvider pair -model uses, so a ref resolves identically.
	ModelFor func(ref string) (ai.Provider, string, bool)
	// MaxDepth bounds the spawn chain: 0 → DefaultSubagentMaxDepth. A child
	// below the cap may spawn its own children, so the chain's length is one
	// number the whole tree shares (each nested tool inherits it).
	MaxDepth int
	// Depth counts how deep in the spawn chain we are. 0 = top-level
	// agent. omp: task.maxRecursionDepth=2; a child at the cap loses the
	// task tool (structurally guaranteed since ChildTools excludes it).
	Depth int
	// AgentName (when non-empty) is the named agent this task tool
	// was spawned FOR (spawn-policy check: can this agent spawn its
	// declared children?).
	AgentName string
	// ParentLabel names the child that owns THIS task tool ("" = the
	// top-level session owns it). A nested task tool is handed to a child,
	// so it knows the child's label — which is the parent link the child's
	// OWN children need. Without it a grandchild arrives with no parent
	// and the display can only draw a flat list.
	ParentLabel string
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
	// OnEvent receives every foreground child's progress (start, one per
	// tool call, end), so a host can show a user what the children are
	// doing while the call blocks. Set by the TUI (cmd/xdev/tui.go);
	// nil in print/rpc/acp leaves the path exactly as it was. Display
	// only: the parent's context is unaffected.
	OnEvent func(SubagentEvent)
}

// TaskToolName is the tool name the model calls.
const TaskToolName = "task"

func (t *TaskTool) Name() string { return TaskToolName }

// Description is the task tool's whole behavioural contract. The capability
// has been here since M11, but this text was a bare capability statement
// ("spawn a subagent for one focused job"), so nothing told the model when a
// fan-out was the cheaper path or when it was pure overhead — and the session
// stores show serial delegation as the default. The constraints are the ones
// that measurably change behaviour: a cheap job handed to a subagent costs a
// full model round trip to say nothing, and a batch left implicit is a batch
// that never gets sent.
func (t *TaskTool) Description() string {
	desc := "spawn a subagent for one focused job (search, batch edits, a self-contained question); " +
		"it runs with a restricted tool set in its own session and returns only its final result — " +
		"its transcript never enters this conversation.\n\n" +
		"Reach for it when the answer means reading a lot of code, or when independent slices can " +
		"run at once. Skip it for a single file read, a quick lookup, or a command you could just " +
		"run — handing those off costs more than doing them. Do not use one subagent to check on " +
		"another.\n\n" +
		"Send independent jobs as one batch (context + tasks[]); they run concurrently, up to 8. " +
		"State in each prompt exactly what the result must contain, because that result is all you " +
		"get. A child that reaches max_turns returns whatever it has, so scope a job to what fits."
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

// resolveChildModel turns a model reference into the provider and model id a
// child runs on. The host wires ModelFor (cmd/xdev/print.go) with the same
// resolveModel + buildProvider pair `-model` uses, so a reference resolves
// exactly the way the launch flag does — one resolver, one set of rules.
//
// A nil resolver (print/rpc/acp hosts that never wired one, and every test)
// has no way to build a second provider, so the reference is refused and the
// caller reports it in the handoff. Silently keeping the parent's model would
// make the child report a model it never ran on — the #272 failure this
// codebase keeps re-learning.
func (t *TaskTool) resolveChildModel(ref string) (ai.Provider, string, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" || t.ModelFor == nil {
		return nil, "", false
	}
	return t.ModelFor(ref)
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

// Parameters advertises both call shapes. The batch shape ({context, tasks[]})
// has been parsed since parity finding T3 #1 but was absent from this schema,
// so a model reading the tool contract could not see it and the concurrency the
// engine implements was unreachable from the model's side. `required` is
// deliberately absent: one spawn needs prompt, a batch needs tasks, and Execute
// picks between them. Pinning required:["prompt"] also made the batch shape
// unsendable on providers that validate strictly.
func (t *TaskTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "agent": {"type": "string", "description": "named agent type to dispatch; the available definitions and their descriptions are listed at the end of this tool description (omit for the default shape)"},
    "model": {"type": "string", "description": "model for this child, \"provider/model\" or a bare id; takes precedence over the agent definition's model frontmatter, else the parent's model is used. Set it only when the user or the task explicitly calls for a different model — a child that does not need one should inherit, so its prompt cache is shared"},
    "effort": {"type": "string", "enum": ["minimal", "low", "medium", "high", "xhigh", "max"], "description": "reasoning effort for this child; omitted = the agent definition's thinkingLevel, else the parent's effort. Set it only when the user or the task asks for a specific level — \"low\" for mechanical, checkable work, \"high\" when one well-specified piece needs more reasoning than the work around it"},
    "prompt": {"type": "string", "description": "the task, self-contained: state the goal, the files/paths involved, and the expected result. Use this for one job, or tasks[] for independent jobs that can run at once."},
    "context": {"type": "string", "description": "batch only: shared framing prepended to every item in tasks[]"},
    "tasks": {
      "type": "array",
      "description": "batch of independent jobs, run concurrently (max 8). Use instead of prompt when the jobs do not depend on each other.",
      "items": {
        "type": "object",
        "properties": {
          "task": {"type": "string", "description": "this item's assignment, self-contained (alias: prompt)"},
          "agent": {"type": "string", "description": "named agent type for this item"},
          "name": {"type": "string", "description": "short label for this item's child session"},
          "model": {"type": "string", "description": "model override for this item's child (see the top-level model)"},
          "effort": {"type": "string", "enum": ["minimal", "low", "medium", "high", "xhigh", "max"], "description": "reasoning effort for this item's child (see the top-level effort)"},
          "schema": {"type": "object", "description": "JSON Schema this item's result must satisfy"},
          "strict": {"type": "boolean", "description": "grant this item one correction turn on schema mismatch"},
          "max_turns": {"type": "integer", "description": "turn cap for this item's child"}
        },
        "required": ["task"]
      }
    },
    "name": {"type": "string", "description": "short label for the child session (optional)"},
    "schema": {"type": "object", "description": "JSON Schema the result must satisfy (optional)"},
    "strict": {"type": "boolean", "description": "with schema: grant the child one correction turn on mismatch (default false = accept with a note)"},
    "max_turns": {"type": "integer", "description": "turn cap for the child (default 30)"},
    "background": {"type": "boolean", "description": "start the child in the background and return a job id immediately (default false = wait for the result)"}
  }
}`)
}

// maxBatchParallel bounds a concurrent fan-out: every item is its own child
// agent loop with its own session, and the process memory budget (<100 MB
// RSS) is what an unbounded batch would actually break.
const maxBatchParallel = 8

// maxBatchItems bounds the SIZE of one batch, where maxBatchParallel bounds
// how many of its items run at once. Both are needed: the semaphore alone
// admits a batch of any length, and a length unbounded batch is the one
// fan-out that can break the process rather than merely slow it down.
//
// ponytail: a fixed ceiling rather than a per-model token/cost budget. It
// bounds the worst case, not the spend. Upgrade path if it shows up: refuse
// on the batch's own `max_turns` sum instead of a constant.
const maxBatchItems = 32

// executeBatch runs omp's `{context, tasks[]}` shape. Each item is dispatched
// through the single-spawn path (so agent resolution, the spawn policy, the
// depth guard and the child advisor are identical between the two shapes) and
// the items run concurrently, which is the point of a batch.
func (t *TaskTool) executeBatch(ctx context.Context, a taskArgs) (tool.Result, error) {
	if t.Provider == nil {
		return tool.Result{Text: "task: no provider configured for subagents", IsError: true}, nil
	}
	// The semaphore below bounds how many run at once, not how many the model
	// asked for: a 200-item batch spawns 200 child loops that each hold a
	// session, a provider stream and their share of the <100 MB RSS budget,
	// then hands the parent 200 results to read. maxBatchItems refuses the
	// oversize request instead, naming the fix — split it and run the halves.
	if n := len(a.Tasks); n > maxBatchItems {
		return tool.Result{Text: fmt.Sprintf(
			"task: batch of %d exceeds the %d-item limit (up to %d run at once) — send it as separate batches",
			n, maxBatchItems, maxBatchParallel), IsError: true}, nil
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
			Prompt: prompt, Agent: item.Agent,
			// A batch item with no `name` is still a child a user is
			// watching, so it is labelled the way its own report
			// section already is (`batchItemLabel`) instead of
			// arriving nameless.
			Name:  childLabel(item.Name, item.Agent, i),
			Model: item.Model, Effort: item.Effort,
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

// childLabel names one batch child — the label a user sees on its live row.
// It is batchItemLabel without the report's `· ` bullet: the bullet belongs
// to the report section, not to a child's name.
func childLabel(name, agent string, i int) string {
	switch {
	case name != "":
		return name
	case agent != "":
		return agent
	default:
		return fmt.Sprintf("task #%d", i+1)
	}
}

// batchItemLabel names one batch section for the parent's report.
func batchItemLabel(i int, item taskItem) string {
	return "· " + childLabel(item.Name, item.Agent, i)
}

// taskArgs is one spawn request. Tasks/Context carry omp's batch shape
// (`{context, tasks[]}`): a model following the baseline's tool docs sends a
// batch by default, and rejecting it with "prompt is required" made the whole
// feature look broken (parity finding T3 #1).
type taskArgs struct {
	Prompt string `json:"prompt"`
	Agent  string `json:"agent"` // named agent type (optional)
	Name   string `json:"name"`  // session label (optional)
	// Model and Effort override the agent definition (and the parent) for
	// THIS spawn. Both are the model's own request, so an unresolvable value
	// is reported in the handoff rather than silently ignored.
	Model      string          `json:"model"`
	Effort     string          `json:"effort"`
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
	Model    string          `json:"model"`
	Effort   string          `json:"effort"`
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

	// Depth guard. The cap is the WHOLE chain's, so every nested task tool
	// carries the same number (see the nested grant below); a child at the
	// cap is refused here and never receives a task tool of its own.
	maxDepth := t.MaxDepth
	if maxDepth <= 0 {
		maxDepth = DefaultSubagentMaxDepth
	}
	if t.Depth >= maxDepth {
		// The refusal names the fix, the way Claude Code's own depth refusal
		// does: a model told only "cannot spawn deeper" retries the same call,
		// and the user never learns the chain is configurable.
		return tool.Result{Text: fmt.Sprintf(
			"task: subagent nesting limit reached (depth %d of %d) — do this work directly with your own tools, or raise taskMaxDepth in settings",
			t.Depth, maxDepth), IsError: true}, nil
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
	// Precedence for model and effort, both Claude Code's and the one a user
	// expects: the call's own argument, then the definition's frontmatter,
	// then the parent's. The two overrides are applied AFTER the definition
	// block below, which is what makes the call win.
	agentSystem := t.System
	agentTools := t.ChildTools
	agentProvider := t.Provider
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
			} else if p, m, ok := t.resolveChildModel(def.Model); ok {
				agentProvider, agentModel = p, m
			} else {
				note := fmt.Sprintf("agent %q: frontmatter model %q could not be resolved — parent model kept", def.Name, def.Model)
				logx.Warnf("%s", note)
				agentNotes = append(agentNotes, note)
			}
		}
		// Recursive spawn: a child below the cap gets its own task tool so it
		// can dispatch further (still-capped) children. The child inherits
		// the resolved agent name, so ITS spawns policy governs the next
		// level, and it inherits MaxDepth, so the whole chain shares one cap
		// instead of each level re-deriving it. The guard at the top of this
		// function terminates the chain.
		if slices.Contains(def.Tools, "task") && t.Depth+1 < maxDepth {
			agentTools = append(agentTools, &TaskTool{
				Provider:        agentProvider,
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
				ModelFor:     t.ModelFor,
				MaxDepth:     maxDepth,
				Depth:        t.Depth + 1,
				AgentName:    agentArg,
				// The child's own label is the parent link for everything
				// IT spawns: the grandchild's events arrive here, and
				// without a name to point at they read as roots.
				ParentLabel:  a.Name,
				Hub:          t.Hub,
				ChildAdvisor: t.ChildAdvisor,
				// The grandchild's progress has to reach the same display
				// seam the child's does, or a chain is invisible in the panel
				// and in the transcript: the events arrive with no call row
				// of their own, so dropping them here loses the whole level.
				OnEvent: t.OnEvent,
			})
			// The child can delegate, so say so — and ONLY then. A child that
			// received no task tool must not be told about one: an advertised
			// capability that does nothing is the #272 failure, one level down.
			agentSystem = agentSystem + "\n\n" + SubagentChainPrompt
		}
	}

	// The CALL's own overrides win over the definition (Claude Code's
	// precedence: the tool argument, then the agent definition, then the
	// parent). Applied here, after the definition block, so there is exactly
	// one order and no branch can invert it.
	if ref := strings.TrimSpace(a.Model); ref != "" {
		if p, m, ok := t.resolveChildModel(ref); ok {
			agentProvider, agentModel = p, m
		} else {
			note := fmt.Sprintf("model %q could not be resolved — parent model kept", ref)
			logx.Warnf("task: %s", note)
			agentNotes = append(agentNotes, note)
		}
	}
	if lvl := strings.TrimSpace(a.Effort); lvl != "" {
		switch {
		case t.ExpandEffort == nil:
			agentNotes = append(agentNotes, fmt.Sprintf("effort %q ignored (host supplies no effort resolver)", lvl))
		default:
			if b := t.ExpandEffort(lvl); b != nil {
				agentThinking = b
			} else {
				note := fmt.Sprintf("unsupported effort %q (want %s) — parent effort kept",
					lvl, strings.Join(config.EffortLevels, "|"))
				logx.Warnf("task: %s", note)
				agentNotes = append(agentNotes, note)
			}
		}
	}

	mt := a.MaxTurns
	if mt <= 0 {
		mt = t.MaxTurns
	}
	spec := SubagentSpec{
		Name: a.Name, Prompt: a.Prompt, System: agentSystem,
		Provider: agentProvider, Model: agentModel, Tools: agentTools,
		CWD: t.CWD, DataDir: t.DataDir, MaxTurns: mt, MaxTokens: t.MaxTokens,
		Output:          &SubagentOutput{Schema: a.Schema, Strict: a.Strict},
		ParentSessionID: t.ParentSessionID,
		Policy:          t.Policy,
		Approve:         t.Approve,
		Thinking:        agentThinking,
		// Who spawned this child. The top-level session owns its children
		// (""), a child owns the ones it spawns — and that is the edge the
		// display draws the tree along.
		Parent: t.ParentLabel,
	}

	// A foreground spawn is the one a user is watching: the TUI learns what
	// the child is doing through this callback. The hub composes its own
	// OnEvent additively (launchLocked, the way it does for OnRun), and
	// neither is the parent's context — that still sees only the yield.
	if t.OnEvent != nil {
		spec.OnEvent = t.OnEvent
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
	var fgID string // the transcript id this foreground child is readable under

	// A foreground child's transcript is the one thing /hub cannot show — it
	// blocks this very call, so it is not a job — and the TUI has to be able
	// to open it while the child is still running. TrackForeground captures
	// the child's store through the same OnRun seam the hub uses and returns
	// the id its transcript is readable under; the child's start event
	// carries that id to the host, which is what binds a click on the child's
	// transcript row to the transcript it can fetch.
	if t.Hub != nil {
		var capture func(*Agent)
		fgID, capture = t.Hub.TrackForeground(spec)
		spec.TranscriptID = fgID
		spec.OnRun = composeOnRun(spec.OnRun, capture)
		spec.OnEvent = withForegroundID(t.OnEvent, fgID)
	}
	res, err := SpawnChild(ctx, spec)
	if fgID != "" {
		t.Hub.SettleForeground(fgID, res, err)
	}
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

// composeOnRun chains a second OnRun onto the first, keeping the existing
// hook's position (the advisor's, the hub's) ahead of the new one. Both are
// cheap and neither blocks, so the order is only a readability rule.
func composeOnRun(base, next func(*Agent)) func(*Agent) {
	if next == nil {
		return base
	}
	if base == nil {
		return next
	}
	return func(ag *Agent) { base(ag); next(ag) }
}

// withForegroundID wraps a host's OnEvent so the child's start event carries
// the id its transcript is readable under. Every later event repeats it: a
// host keys children by label (labels are unique per batch), and the end
// event is what settles the row, so a host that bound on start must still
// recognise the same child on the way out.
func withForegroundID(base func(SubagentEvent), id string) func(SubagentEvent) {
	if base == nil {
		return nil
	}
	return func(ev SubagentEvent) {
		if ev.TranscriptID == "" {
			ev.TranscriptID = id
		}
		base(ev)
	}
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
