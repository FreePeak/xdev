# Design: subagent chains, per-child model/effort, and a live agent panel

*Reference: `internal/agent/{task,subagent,discovery}.go`, `internal/tui/{app,keymap,queue}.go`,
`cmd/xdev/{print,tui}.go` · Milestone: M11 (subagents) · Date: 2026-10-14 · Status: **implemented**
(see `docs/PRD.md`'s dated entry for `feat/subagent-chains`)*

## 0. What was asked

Three things, from Claude Code's shape:

1. a **running-subagent panel at the bottom** of the TUI, with the ability to open a child's own
   transcript from it;
2. **per-subagent model and effort**, settable by the model at spawn time (not only by the agent
   definition file);
3. **subagent chains** — a child may spawn its own children, bounded by a depth limit.

Plus one blocker found while reconning: a batch `task` call whose items carry no `schema` poisoned
every child's `yield` tool with `{"result": null}` and the provider rejected the whole child request
(`HTTP 400 … /properties/result: null is not of types "boolean","object"`). That is fixed here,
because it is what makes (3) unrunnable in practice: a chain is spawned by a child, and a child that
cannot start cannot spawn anything.

## 1. Primary source: what Claude Code 2.1.299 actually does

Read out of the installed bundle (`~/.local/share/claude/versions/2.1.299`, a Bun Mach-O with the JS
embedded; string offsets below are from that file).

| Mechanism | Evidence (verbatim) |
| --- | --- |
| Per-spawn **model** | Agent tool param: *"Optional model override for this agent. Takes precedence over the agent definition's model frontmatter and the configured default subagent model. If omitted, uses the agent definition's model, else the default (inherits from the parent unless a default subagent model is configured). Ignored for `subagent_type: "fork"`"* |
| Per-spawn **effort** | Agent tool param: *"Reasoning effort for this agent. Set this ONLY when the user, or instructions such as CLAUDE.md or a skill, explicitly ask that this agent or delegated work run at a specific effort level, never on your own judgment…"*, and the enum form accepts *"lower"* / *"higher"* — one step from the effort the agent would otherwise run at |
| **Depth limit** | `CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH` (env), default **3** in the shipped build (`var n=3` beside the accessor); a GrowthBook value overrides it. Refusal: *"Subagent nesting limit reached (depth N). Complete this task directly using your tools instead of spawning another agent. If the user explicitly requested deeper nesting…"* |
| Tool removed at the cap | `agentToolRemovedByDepth` / `declaredAgentToolWithheld` ride the `subagent_tool_resolution` telemetry, i.e. the Agent tool is **taken away** from a child at the cap rather than refused at call time |
| Concurrency cap | `CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS`; refusal *"Concurrent subagent limit reached. You can run N subagents at once. Do not retry. If the user wants more concurrent subagents, ask them to increase …"* |
| **Agent panel** | A settings key `subagentStatusLine`: *"Custom per-subagent status line shown in the agent panel; receives row context as JSON on stdin"*. The panel is the background-tasks dialog, whose sections are `active agents` / `Local agents` / `Cloud agents` / `Dynamic workflows`, with per-row actions and a kill-all. Key hints found in the same region: *"Press ← again to open agents"*, `ctrl+o` bound to `app:toggleTranscript` |

Deliberately **not** adopted: the GrowthBook/telemetry surface, cloud agents, and the teams
mechanism (already rejected in `docs/parity/harness-cross-check-2026-09-14.md`).

## 2. What xdev already had

- **Chains existed, capped at 2 and hardcoded**: `const maxDepth = 2` (`task.go:398`), with the child
  getting a nested `TaskTool` only when its definition declares `tools: … task` and `Depth+1 < maxDepth`
  (`task.go:494-518`). The bundled `task.md` declares `spawns:`/`tools: … task`, so a chain of two was
  reachable — but the cap was a constant and the refusal text named no way to raise it.
- **Model/effort per child existed only via the definition file**: `model:` and `thinkingLevel:`
  frontmatter (`discovery.go`), resolved in `task.go:461-488`, with `ExpandEffort` supplied by the host
  (`cmd/xdev/print.go:1815`). The **task tool's own schema** (`task.go:201-231`) accepted neither, so the
  model could not ask for either at spawn time.
- **Foreground children already render**, as rows hanging under the `task` call row (`subLines`,
  `app.go:1317`), clickable to open the child's own transcript (`openSubChildTranscript`,
  `hubroster.go:76`); `Alt+B` opens the newest. There was **no bottom panel** — the transcript rows and
  the `/hub` overlay were the only two surfaces.
- **The mid-turn queue is the precedent for bottom chrome**: `drawQueue` (`queue.go:343`) paints
  `composerTop-1-queueRows()` upward, publishes a hit table, and is *not* subtracted from the transcript
  viewport — chrome over the transcript's tail, not a layout change.

## 3. The design

### 3.1 Per-child model and effort (ask 2)

Two new task-tool arguments, both optional, both with the same precedence Claude Code uses
(tool argument → agent definition → parent):

```jsonc
"model":  {"type": "string", "description": "model override for this child (provider/model or bare id); takes precedence over the agent definition's model, else the parent's"},
"effort": {"type": "string", "enum": [...config.EffortLevels], "description": "reasoning effort for this child, one rung of the ladder; omitted = the definition's thinkingLevel, else the parent's"}
```

`effort` needs no new seam: `ExpandEffort` already exists and is already wired. `model` does —
a cross-provider child needs its own provider client, and the task tool holds exactly one. So:

```go
// ModelFor resolves a model reference to a provider and a model id for a
// child. nil = same provider as the parent only (the pre-existing shape).
ModelFor func(ref string) (ai.Provider, string, bool)
```

The host wires it in `cmd/xdev/print.go` with the same `resolveModel` + `buildProvider` pair the
failover chain and the advisor already use, so a model ref resolves exactly the way `-model` does.
A ref the host cannot resolve is **reported in the handoff** (`renderAgentNotes`) rather than silently
falling back, the #272 discipline every other unhonoured field already follows.

### 3.2 Chains (ask 3)

The cap becomes a field, default 3 (Claude Code's shipped default):

```go
// MaxDepth bounds the spawn chain. 0 → DefaultSubagentMaxDepth (3): a
// top-level session, its child, and one more level below that.
MaxDepth int
```

- wired from a new settings key `taskMaxDepth` (0 = default), so a user raises it without a rebuild;
- inherited by every nested `TaskTool` (`task.go:495-518`), so the whole chain shares one number;
- the refusal names the fix, Claude Code's own wording: *"task: recursion depth limit reached
  (depth N) — raise taskMaxDepth in settings, or complete this task with your own tools"*.

The child's `OnEvent` is propagated into the nested task tool too, so a grandchild's progress reaches
the panel — today it produced no events at all, which is why a chain was invisible.

### 3.3 The agent panel (ask 1)

A new file `internal/tui/agentspanel.go`, shaped like `queue.go`:

- **Rows**: up to 3 running children, newest first, plus `+N more`; each row is
  `spinner name · agent · tool detail · elapsed` with the model dim on the right when it differs from
  the parent's. The phrase comes from the **existing** `toolDetail` (`blocks.go`), so a panel row and a
  transcript row read alike and the naming precedence lives in one place.
- **Placement**: above the composer, above the queue (`panelTop = queueTop - panelRows`). Chrome over
  the transcript's tail, exactly like the queue — the viewport arithmetic does not move.
- **Opening a child's TUI**: a click on a panel row calls the **existing**
  `openSubChildTranscript(id, label)`, and `Alt+B` keeps opening the newest. One Esc contract, one
  renderer, no second transcript surface.
- **Toggle**: `Alt+P` (`app.agents.panel`), default on. A panel that cannot be dismissed is chrome
  nobody asked for.
- **Data**: a flat registry fed by `SubagentEvent`, not the per-call `SubActivity` rows. Flat is what
  makes a chain renderable: a grandchild's events arrive with no call row of their own, and the
  transcript can only hang them under the ancestor call. The panel shows the true tree-flat list.
- **Not included**: background hub jobs. `/hub` (Alt+A) and the dock's agents section already cover
  them, and one panel mixing a blocking child with a background job would offer one set of controls
  for two lifetimes. Deliberate.

## 4. Files touched

| File | Change |
| --- | --- |
| `internal/agent/subagent.go` | `SubagentOutput.schema()` treats a literal JSON `null` as empty — the batch-poisoning fix, at the one place every spawn path shares |
| `internal/agent/task.go` | `model`/`effort` args + schema, `ModelFor`, `MaxDepth`, nested-task inheritance of both plus `OnEvent`, refusal text |
| `internal/agent/agents/task.md` | the worker brief names the depth budget |
| `internal/config/settings.go` | `taskMaxDepth` |
| `cmd/xdev/print.go` | wire `ModelFor`, `MaxDepth` |
| `cmd/xdev/tui.go` | feed the panel registry from `taskChildSink` |
| `internal/tui/agentspanel.go` | new: state, painter, hit table, toggle |
| `internal/tui/{app,keymap}.go` | draw call, `Alt+P`, mouse dispatch |

## 5. Verification

- `go test ./internal/agent/ ./internal/tui/ ./internal/config/ ./cmd/xdev/`
- `go build ./...`
- the panel driven for real (pty) rather than only asserted on a simulation screen — the
  `scripts/tui-subagent-transcript-drive.py` precedent exists because a simulation-screen test passed
  while the click opened nothing.
