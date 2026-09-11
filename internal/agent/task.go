package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

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
	// Depth counts how deep in the spawn chain we are. 0 = top-level
	// agent. omp: task.maxRecursionDepth=2; a child at the cap loses the
	// task tool (structurally guaranteed since ChildTools excludes it).
	Depth int
	// AgentName (when non-empty) is the named agent this task tool
	// was spawned FOR (spawn-policy check: can this agent spawn its
	// declared children?).
	AgentName string
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
    "max_turns": {"type": "integer", "description": "turn cap for the child (default 30)"}
  },
  "required": ["prompt"]
}`)
}

// Execute spawns the child and renders its handoff for the parent model.
func (t *TaskTool) Execute(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var a struct {
		Prompt   string          `json:"prompt"`
		Agent    string          `json:"agent"` // named agent type (optional)
		Name     string          `json:"name"`  // session label (optional)
		Schema   json.RawMessage `json:"schema"`
		Strict   bool            `json:"strict"`
		MaxTurns int             `json:"max_turns"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return tool.Result{Text: "task: malformed arguments: " + err.Error(), IsError: true}, nil
	}
	if strings.TrimSpace(a.Prompt) == "" {
		return tool.Result{Text: "task: prompt is required", IsError: true}, nil
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
		if t.AgentName != "" {
			if parent, ok := FindAgent(t.Agents, t.AgentName); ok {
				pol := parent.ResolveSpawnPolicy()
				if !pol.AllowAll && len(pol.Allow) > 0 && !slices.Contains(pol.Allow, agentArg) {
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
			agentModel = def.Model
		}
	}

	mt := a.MaxTurns
	if mt <= 0 {
		mt = t.MaxTurns
	}
	res, err := SpawnChild(ctx, SubagentSpec{
		Name: a.Name, Prompt: a.Prompt, System: agentSystem,
		Provider: t.Provider, Model: agentModel, Tools: agentTools,
		CWD: t.CWD, DataDir: t.DataDir, MaxTurns: mt, MaxTokens: t.MaxTokens,
		Output:          &SubagentOutput{Schema: a.Schema, Strict: a.Strict},
		ParentSessionID: t.ParentSessionID,
		Policy:          t.Policy,
		Approve:         t.Approve,
		Thinking:        t.Thinking,
	})
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
	if res.Note != "" && res.Status == "yielded" {
		b.WriteString("\nnote: " + res.Note + "\n")
	}
	if res.SessionID != "" {
		fmt.Fprintf(&b, "\nsession: %s\n", res.SessionID[:min(8, len(res.SessionID))])
	}
	return b.String()
}
