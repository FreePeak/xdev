# Claude Code subagents, agent teams & Cowork vs xdev — deep dive (2026-09-16)

Method: primary sources only.

- **Claude Code**: official docs fetched live on 2026-09-16 —
  `code.claude.com/docs/en/{agents,sub-agents,agent-teams,workflows,agent-view,cross-session-messaging,tools-reference}.md`
  (current for **CC 2.1.263**, the build the repo already studied forensically in
  `cc-bundle-forensics.md`), plus the 2026-09-14 resweep's verbatim bundle extracts
  (`cc-resweep-2026-09-14.md`).
- **Cowork**: Anthropic help-center "Claude Cowork – Architecture, security, and usage
  guidance" (fetched 2026-09-16) + the bundle's `CLAUDE_CODE_*COWORK*` env keys.
- **xdev**: source read at HEAD — `internal/agent/{subagent,task,hub,hubtool,mailbox,discovery,prompt,loop,planmode}.go`,
  `internal/collab/*`, `cmd/xdev/{print,tui,rpc,acp,main,gccmd}.go`.

Related: `docs/research/parity-agent-system.md`, `docs/parity/agent-system.md`,
`docs/research/claude-code/cc-official-docs.md`, `cc-bundle-forensics.md`,
`cc-resweep-2026-09-14.md`, `docs/research/claude-code-internals.md`.

---

## 1. What CC actually ships: four distinct multi-agent products

The most useful research finding is that "subagents in Claude Code" is not one feature.
CC 2.1.263 ships **four separate mechanisms**, and conflating them produces the wrong
parity list:

| Mechanism | Where work runs | Context | Coordination | Result |
|---|---|---|---|---|
| **Subagent** (`.claude/agents/*.md`, `--agents` JSON) | own context window, same process | starts **fresh**; does **not** see the parent conversation | returns one report to the caller; "Subagents can't talk to the user" | summary only |
| **Forked subagent** (`/subtask`) | background subagent | **inherits the whole conversation** (same prompt/tools/model ⇒ shared prompt cache) | panel below the prompt: observe, steer (`f`), `x` to open | arrives as a message in the parent |
| **Agent teams** (experimental, `CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1`) | **separate Claude Code instances**; in-process teammates or tmux split panes | teammates own their own conversation; **context is not shared, only messages** | **lead + shared task list + file-locked claims + mailbox** | free-text both ways; teammates message each other directly |
| **Dynamic workflow** (`workflow` tool, JS script) | script-orchestrated subagents, up to 1000 agents | intermediate results live in **script variables**, not the model's context | script owns phases; approval gate before the run; savable as a command | one run summary |

Plus two supporting surfaces: **agent view** (`claude agents` — a supervisor for many
background sessions) and **cross-session messaging** (`ListAgents` + `SendMessage` between
independent sessions — "an independent session you open yourself, with no shared task list
or automatic coordination"). CC's own selection rule:

> Use subagents for a side task inside one session… **Use agent teams when agents need to
> work in parallel and each result needs to be seen, responded to, and acted on again.** …
> If you only need to send a message from one Claude session to another, cross-session
> messaging alone covers that.

### 1.1 What a CC subagent is given at start

A non-fork subagent's context is assembled from: its own system prompt (not the main one),
the task message, the **CLAUDE.md hierarchy**, a **git-status snapshot**, preloaded skills,
and — when the session can message agents — a **sibling roster** (a system reminder listing
`main` plus named agents, which supplies the valid `to` values for `SendMessage`). Explore
and Plan deliberately skip CLAUDE.md and git status; `omitClaudeMd: true` skips the
hierarchy for any agent.

Two CC statements are load-bearing for §3:

- Docs, sub-agent scope: *"The subagent's own files — its **system prompt, tool list, and
  hooks** — come from its definition…"*
- Docs, permissions: *"Their **permission rules, hooks, and MCP server configurations** are
  not inherited."* — meaning a subagent's rules come from **its own definition**, and that
  definition's `hooks:`/`permissionMode:`/`mcpServers:` fields are applied **on top of** the
  settings layers the session already loaded (project `.claude/settings.json` and managed
  settings are not dropped; the docs' worked example denies `rm` in project settings and
  reports the subagent still cannot run `rm`).

So CC's child is a **configured session**, not a bare loop: it gets project instructions and
whatever its definition declares, and the definition has first-class fields to declare them.

---

## 2. What xdev ships today (source-verified)

### 2.1 `SpawnChild` (`subagent.go:160`)

Genuinely strong, and stronger than a from-scratch design would be:

- An **isolated in-process goroutine session**: own `session.Store`, own restricted
  registry, `yield` auto-appended, transcript never enters the parent (only the validated
  payload does).
- Child session is **user-invisible** (`SetTitleSourceSubagent`, `:181`), persisted under
  `<dataDir>/subagents` with the parent stamped in the header (`:182-188`), and
  reachability-justified in the GC: *"Subagent sessions are children no user continuation
  can reach, so age alone decides"* (`gccmd.go:301-303`) — retention is the 720h default
  (`gccmd.go:89`), i.e. ~30 days.
- **Structured output** (`SubagentOutput{Schema, Strict}`) with a top-level validator, **one
  correction turn** on mismatch, `schema-mismatch` as a distinct status.
- **No-yield handling**: `yieldNudges = 2` (`:323`) corrective turns, then an explicit
  `res.Note`: *"child never called yield after 2 reminders — the text is its loose final
  reply, not a structured handoff"* (`:256`). Honest terminal state, not a silent pass.
- **Approval posture is inherited** (`:203` `childPolicy := spec.Policy`, and `yield` is
  force-allowed at `:208` so a child can always hand back its result).

### 2.2 The seam nobody fills: the child is 9 fields

`SubagentSpec.OnRun func(*Agent)` (`:52-54`) is the extension point — and `hub.go:126`
already uses it to capture the child for steering. **No shipped host passes any capability
wiring.** The child `Agent` literal (`subagent.go:211-221`) is:

```
Provider, Tools, Hooks(persist-only), Store, Model, MaxTokens, MaxTurns, Policy, Approve   (+ Thinking)
```

The print-mode parent (`print.go:524`, then `:540-598`) gets, in addition:

```
Compaction, Failovers, wireAgentMode → Vision + MemoryContext + Rulebook (+ Fallback),
TTSR, Handoff, Intercept (hook/extension chain), FollowUp/Steer, Goals
```

Concrete consequences, each verified at the enforcement site:

| Missing on the child | Where it bites |
|---|---|
| `Compaction` | `maybeCompact` returns `history` unchanged when `ContextWindow <= 0` (`compact.go:367`) — a long child **never compacts**; it overflows and dies |
| `Intercept` | `emit()` is nil-guarded (`loop.go:302-306`), so a child fires **zero hook/extension events** — not even the `before_agent_start` system-prompt rewrite or `tool_call` argument rewriting. The child is invisible to the user's configured extensions |
| `Redactor` | no redaction pass and no `Redactor.Expand` at `loop.go:1006` — a child's provider request is unredacted **and** a placeholder-rewritten arg would never be expanded back |
| `Rulebook` | rulebook notes exist to teach the model *live tool surfaces*; a child gets neither the notes nor the tools |
| `Vision` | no image degradation for non-vision models — a child handed a screenshot path cannot know its model can't see it |
| `Failovers` | zero provider redundancy |
| `TTSR`, `Handoff`, `PlanMode` | no same-model rule application; `applyPlanMode` (`planmode.go:266`) denies mutations **per agent**, so a plan-mode parent's read-only posture does **not** propagate to children |
| `AGENTS.md` | `LoadContextFiles` is called at exactly one site (`print.go:928`, inside `promptFnWithMemory`, used by `print.go:498` + `tui.go:183`) for the **parent only**; the child's system prompt is the 6-line `SubagentSystemPromptBase` (`prompt.go:32-39`) or a `.md` body |

**The strongest counter-argument, recorded so the issues do not oversell it.** CC's own docs
hedge the instructions half: *"The main conversation still has your full CLAUDE.md when it
reads these subagents' results, so most rules don't need to reach the subagent itself. If a
rule must, such as 'ignore the `vendor/` directory', restate it in the prompt you give Claude
when delegating."* So the AGENTS.md gap is a **convenience**, and `omitClaudeMd` exists
precisely because skipping it is sometimes the point. The **runtime-chain half is not**
likewise hedged — no CC doc says "hooks need not reach the child", and the redactor gap is a
secrets-exposure difference, not an ergonomics one. If only one half of the issue gets built,
build the chain.

**Framing matters here.** This is not "CC inherits, xdev doesn't". CC's child is a
*configured session* — it receives project instructions and its own declared hooks/permission
rules. xdev's child receives neither the project's instructions nor the parent's runtime,
**and** its definition format has no fields to ask for them (§2.7). That second half is what
makes this xdev-specific rather than a parity copy.

### 2.3 The child cannot be escalated either — a closed loop

`cmd/xdev/print.go:1711-1748` builds **one** `[]tool.Tool` literal — `ChildTools`:

```go
ChildTools: []tool.Tool{
    tool.NewReadTool(), tool.NewWriteTool(), tool.NewEditTool(),
    tool.NewBashTool(cwd), &tool.GrepTool{CWD: cwd}, &tool.GlobTool{CWD: cwd},
    &tool.ASTGrepTool{CWD: cwd}, &tool.ASTEditTool{CWD: cwd}, eval.NewTool(cwd),
}
```

The registry carries ~40 tools (`reg.Register` appears between `print.go:1664` and `:1850`);
the child pool carries 9, all constructed in that one literal. Then
`resolveAgentTools(def.Tools, t.ChildTools, …)` (`task.go:539-551`) filters **by name against
that pool**, and a miss is reported as *"tool(s) not available to children, dropped"*
(`task.go:369-374`). So a user who writes `tools: [lsp, github, web_search]` in an agent file
gets a warning and nothing else — **the file format offers no way to construct a tool**, and
the spawn path offers no way to add one. `ChildTools` is not a restriction of a wider set; it
*is* the set, and it is closed to authoring.

### 2.4 `task` tool (`task.go`)

- Args: `agent / prompt / name / schema / strict / max_turns / background`, plus omp's batch
  shape `{context, tasks[]}`.
- `description` embeds the named-agent menu via `advertiseAgents()` at **registration**
  (`task.go:99-104`) — a static snapshot. CC applies a mid-session definition edit on the
  *next call*; xdev's mtime-aware loader would pick the change up for spawning, but the menu
  the model reads would not.
- Bundled: `task, scout, reviewer, security-reviewer, sonic`.
- Caps: `maxBatchParallel = 8` (`:198`) — **batch-local only**, a `sem` inside `executeBatch`;
  nothing bounds concurrent children across turns, across batches, or against hub jobs.
  `const maxDepth = 2` (`:328`), hardcoded, no setting.
- A batch is **all-or-nothing on time**: `wg.Wait()` then render-all, so one 20-minute item
  holds the report for the other seven. Per-item results *are* rendered under their label, and
  the header counts failures (`"batch: N task(s), M failed"`), so this is a latency/atomicity
  gap — the parent cannot act on item 1 while item 2 runs.
- **`TaskTool.Approve` is never assigned** in any host (grep: the only `Approve:` wiring in
  `cmd/` is `acp.go:218`, for the *parent* agent). So a child running under a "prompt" tier
  rule hits `a.Approve == nil` at `loop.go:1066` and is **refused**. Today the tier model is
  bypassed in children by being dead, not by being permissive — which fails safe now, and
  breaks the moment M12's approval dialog lands.

### 2.5 Hub (`hub.go`, `hubtool.go`)

- Full job registry: `start / send / wait / jobs / status / result / cancel / transcript /
  park / revive`, plus a **process table** for long-running child processes.
- `context.WithoutCancel(parent)` (`hub.go:111`) — job lifetime is the session, not the turn.
  Correct, and the comment documents the bug it fixes.
- `hub wait` **blocks the parent's turn** (default 60s, `hubtool.go:96-110`) and returns
  **`id  status` pairs only**. `renderSubagentResult` — which does format the payload, the
  artifacts and the note — is reached only by the separate `result` op. So the designed shape
  is "wait, then make another call for each id", and nothing on the model-facing surface hints
  that the payload is waiting there: the tool description
  (`hubtool.go:25-27`) enumerates ops without saying what `wait` returns.
  (`transcript` is not even model-facing: the op enum at `:33` has no `transcript` — it is a
  `Hub` method consumed by the TUI inspector, `tui.go:624`.)
- Parked-job revival re-spawns with `revivePrompt(prior, text)` — text concatenation, not a
  transcript resume (the durable-resume gap is #271).
- The hub registers **zero** mailbox identities: no `RegisterIdentity` call in `hub.go`.

### 2.6 Mailbox (`mailbox.go`)

- One JSONL per recipient under `<dataDir>/mail` (cap 1000, oldest dropped, append-only,
  `O_EXCL` lock with a 10s stale steal, idempotent `msgID`).
- `send_message` (`:517`, params `:525`) accepts a short-id **or a registered name**;
  `RegisterIdentity` (`:332`) is real. But in production it is called from **exactly one
  place**, `print.go:2176`, guarded by `config.Alias() != ""` — i.e. the session must have
  been started with `--alias` (`main.go:169`). Default sessions are anonymous; `Resolve`
  (`:352`) falls through to "treat it as a session id".
- **No `ListAgents` equivalent.** `Mailbox` has no listing method, so `send_message` is
  uncallable in practice unless a human already knows the target's short-8. #42 shipped the
  transport; its closing note names the missing "shared registry + directory discovery".
- `inbox` is parameterless (`Parameters()` at `:572` = `{"properties":{}}`) and
  `Execute(_ context.Context, _ json.RawMessage)` (`:576`) discards structure: `renderInbox`
  flattens to text lines (which *do* carry timestamp/status/from/subject). No per-message
  fetch, no filter. The poller is live in both interactive hosts (`print.go:540`,
  `tui.go:154`) and a new message does wake the agent via `FollowUp`.
- Children never get `send_message`/`inbox` at all — they are absent from `ChildTools`, so a
  subagent cannot report a mid-run finding to anyone.
- **Host coverage is better than expected, worse than uniform**: all four hosts call
  `wireTaskParent` (binds mailbox owner + alias, `print.go:2156`) and the inbox sink is
  installed by print (`:541`), tui (`:154`) **and** rpc (`:92`). Only ACP declines — and
  deliberately, documented at `acp.go:222-225` (#90): one mailbox owner per process vs
  several sessions, so it refuses to route to "whichever agent was built last" and leaves
  messages unread instead of misdelivering. The real asymmetry left: the *hub* registers no
  identities at all (`RegisterIdentity` has zero callers in `hub.go`), and no host names
  children — only `--alias` names the session.

### 2.7 Agent definition surface (`discovery.go`)

Parsed frontmatter: `name, description, tools, spawns, model, thinkingLevel` (+ body as the
system prompt) — 4 of CC's 18 fields shared, 2 xdev-only, **11 functional CC fields with no
xdev meaning**: `disallowedTools, permissionMode, maxTurns, skills, mcpServers, hooks, memory,
background, omitClaudeMd, isolation, initialPrompt` (+ `color`, `experimental` cosmetic). Discovery roots: `<cwd>/.xdev/agents` → `~/.xdev/agent/agents` → plugin roots →
embedded. Exact-case, first-wins, mtime-aware reload, YAML-1.1 footgun guard, unknown keys
surfaced as `def.Unsupported` and reported to both the model and the user (`task.go:380-385`).
The reporting is the good part; the surface is the gap.

### 2.8 `internal/collab` — xdev's real Cowork analogue

Host/guest over WS with a genuine framing protocol (`frame.go`, `chunk.go`, `link.go`,
`render.go`, `guest.go`, `host.go`). This is the codebase's answer to "the session runs
somewhere else, I watch it and type into it" — and it is further along than the subagent
layer in some respects.

### 2.9 What is deliberately NOT here

- **No workflow runner.** `grep` finds no `workflow` tool, no `WorkflowTool`, no runner in
  `internal/` or `cmd/`. The only match is the `workflowz` **keyword**
  (`keywords.go:30`, notice at `:21`) that tells the model to "define the steps explicitly,
  run each as a subagent" — a prompt nudge with **no runtime behind it**. Issue **#258**
  ("Workflow tool + runner") was closed with the note *"bulk-filed during a research pass by
  the harness while a subagent sweep was still running; superseded by the curated §5.4 adopt
  list"* — it was **never implemented**, and the superseding list does not contain a runner.
  **#169** (transcript dir + `agentCap`/`tokenCap` + `/workflows` view) is open and currently
  specs the controls for a runner that does not exist.
- **No subagent worktree isolation.** Only `github pr_checkout` creates worktrees.
- **Agent teams** were surveyed and deliberately not ported
  (`docs/parity/harness-cross-check-2026-09-14.md`: dsh shipped it disabled; CC gates it behind
  an experimental env var; PRD rejects the telemetry/gating surface).

---

## 3. Delta table

Class **CORE** = needed to clear the bar CC sets · **PARITY** = closes specific CC behavior ·
**DEFER** = PRD M15 or gated · **REJECT** = contradicts a PRD non-goal.

| # | Feature (CC 2.1.263) | xdev today | Class | Action |
|---|---|---|---|---|
| 1 | The child is a configured session: project instructions + its own declared hooks/permissions/tools, and a definition format that can express them | child gets neither `AGENTS.md` nor the runtime chain, **and** cannot ask for either — see §2.2, §2.3, §2.7 | **CORE** | **new issue** |
| 2 | **18** definition fields (verified count): `name`, `description`, `tools`, `disallowedTools`, `model`, `permissionMode`, `maxTurns`, `skills`, `mcpServers`, `hooks`, `memory`, `background`, `omitClaudeMd`, `effort`, `isolation`, `color`, `initialPrompt`, `experimental` | xdev parses `name/description/tools/model` + its own `spawns`/`thinkingLevel` (≈`effort`). The other **11 functional** fields land in `Unsupported` and do nothing (`color`/`experimental` are cosmetic) | **CORE** | **new issue** (highest leverage: mostly parsing + wiring, unblocks rows 1, 6, 12, 14) |
| 3 | Live-concurrency cap (default 20, `CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS`) + depth cap (default 3, `CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH`) + per-session totals, machine-readable no-retry refusals | batch-local 8; hardcoded depth 2; no live cap; no reason codes | PARITY | **#191** (update: it already specs the taxonomy; correct the CC numbers) |
| 4 | Background completion **arrives** in the parent unprompted: *"A background subagent's results reach Claude as a completion notification in a later turn. Claude waits for that notification before reporting the subagent's results"* (2.1.211+; before it, Claude "sometimes reported results for a background subagent that hadn't finished") | nothing injects on settle; `wait` blocks the turn and returns bare `id status`; the payload needs a second, undiscoverable call | **CORE** | **new issue** |
| 5 | `ListAgents` + `SendMessage`: discovered roster, address **by name**, name in the prompt; `@session-name` typeahead; names auto-assigned in agent-teams mode | short-8 addressing; names exist but require `--alias`; no listing; children excluded entirely | **CORE** | **new issue** |
| 6 | Child turn budget: on the limit CC "returns its output marked as partial" and Claude "can resume it to continue" (2.1.246+) | budget exhaustion produces a wrap-up turn (`loop.go:462-470`) rendered as a plain **`completed`** result (`task.go:594`) | **CORE** | **new issue** (contract-sized) |
| 7 | Forked subagent `/subtask`: inherits full transcript, cache-shared, observable + steerable, result lands in the parent | not shipped; hub park/revive reconstructs from prior *text* | PARITY | **#204** (update with cache rationale + steer/close semantics) |
| 8 | `claude --agent <name>`: run the **whole session** as that agent (its prompt replaces the main one; CLAUDE.md/memory still load) | not supported; the loader + `wireTaskParent` already exist | PARITY | folded into the addressing issue |
| 9 | Workflow runtime: `agent()/pipeline()/parallel()/phase()/log()`, 16 concurrent, 4096 items/batch, 1000 agents/run, transcript dir, budget caps | nothing; `workflowz` promises the model a feature that does not exist | PARITY | **new issue** (the runner); re-scope **#169** as its controls tail; comment on **#258** |
| 10 | Agent teams: lead + shared task list + file-locked claims + `teams/{name}/inboxes/{agent}.json` + plan-approval routing + idle notification | parent-owned jobs, not peers | DEFER | no issue — §4.3, with a reversal trigger |
| 11 | Agent view / supervisor: fleet surviving terminal close, `--bg`, `attach/logs/stop/rm`, per-row status line | hub is session-in-process; `collab` covers remote viewing | PARITY | **#131** (already filed) |
| 12 | `isolation: worktree` for an agent; `.worktreeinclude` | not supported | PARITY | **new issue** (small against the existing layout) |
| 13 | Model-per-agent resolution (`model:` > built-in default > inherit; `inherit`; teammate chain) | `model:` + `@role` + `ExpandModel` + `childModel()` | PARITY | **#193** (already filed) |
| 14 | Child inherits the *mode*, not just the tier: a plan-mode parent's read-only research child | `applyPlanMode` is per-agent (`planmode.go:266`); `TaskTool` has no `PlanMode` field; research children can mutate the repo mid-plan | CORE | folded into #1's issue |
| 15 | OpenCode-derived: `deriveSubagentSessionPermission` (child from parent **deny** rules, not the parent's allow-list) | `Policy` copied verbatim (`subagent.go:203`) — no deny derivation; `Approve` never wired (§2.4) | CORE | **new issue** (M12 blocker; #249/#255 were closed as bulk-file artifacts, unimplemented) |
| 16 | `CLAUDE_CODE_FORK_SUBAGENT=1` for `-p`/SDK | n/a until #204 | DEFER | fold into #204 |
| 17 | Skill-precedence rule for a name shared between a subagent and a skill | xdev has no `Task` agent (its `task` is a tool, not a definition) | REJECT | nothing |

---

## 4. Cowork, and what is portable

### 4.1 What Cowork actually is (help center, 2026-09-16)

Cowork is a **product surface**, not an agent mechanism: chat with Claude, it runs multi-step
work against real files, "built on the same agentic loop as Claude Chat but optimized for
**parallel work**… **Claude Code's agentic architecture underneath** — planning, task
decomposition, subagent coordination."

- **Sessions run by default in a temporary Anthropic-managed sandbox**: no network egress by
  default, mandatory proxy, session-scoped short-lived credentials, connector tokens never
  enter the sandbox, destroyed at session end. Local files are reachable only through the
  desktop app over an Anthropic-brokered connection, restricted to connected folders.
- **Local desktop**: loop on-device, code execution inside a VM (Apple Virtualization.framework
  / Hyper-V) with egress filtering, syscall restrictions, per-session user isolation.
- **Dispatch**: assign work from phone/web to the desktop app; a persistent thread; needs the
  machine awake and the app open.
- Bundle keys: `CLAUDE_CODE_IS_COWORK`, `CLAUDE_CODE_COWORK_FRAME_ARTIFACTS`,
  `CLAUDE_CODE_USE_COWORK_PLUGINS`, `CLAUDE_COWORK_MEMORY_*`.

### 4.2 Portability verdict

The architecture is **cloud sandbox + brokered local tools** — a hosting model the PRD
explicitly disclaims (§1: "YOLO by default; containment belongs to external sandboxing";
§M15 rejects "built-in OS sandbox engine, claude.ai/desktop/web/mobile plumbing, account/org
management"), and #114/#117 own the repo-safety half. Building a sandbox is out of scope.

What **is** portable is the coordination contract Cowork shares with Claude Code, and it is
exactly rows 1, 4 and 5:

1. a delegated agent behaves like a **full session**, not a stripped loop;
2. **asynchronous completion returns to the parent unprompted**;
3. agents **address each other by name**, discovered rather than remembered.

### 4.3 Agent teams: keep deferred, but record the reversal condition

The 2026-09-14 cross-check declined them because dsh shipped the feature disabled and CC gates
it behind an experimental env var. That reasoning still holds. Two things changed since it was
written:

- The **addressing half is no longer experimental** — `ListAgents`/`SendMessage` are GA in
  2.1.224+ (native Windows 2.1.234+), and `SendMessage` "doesn't require agent teams to be
  enabled". That is why row 5 is CORE and row 10 is DEFER: port the addressing layer now and
  a future team board sits **on top of** it instead of replacing it.
- Cowork's "assign work from your phone, watch several sessions" is the same product shape,
  and #131's supervisor is xdev's analogue.

**Reversal trigger**: if a real workflow needs a *peer* — not parent-owned — agent that can
claim work from a shared queue, revisit. The current mailbox layout
(`<dataDir>/mail/<short8>/inbox.jsonl`) generalizes to CC's
`teams/{name}/inboxes/{agent}.json` with a directory rename, so deferring costs no rework.

---

## 5. Issues filed by this document

Filed 2026-09-16 from this document, in dependency order:

| Issue | Scope | Rows |
|---|---|---|
| **#295** | A subagent is not a session: no `AGENTS.md`, no hooks/redactor/rulebook/vision/failover, and no way to ask for them (includes opening the `ChildTools` pool) | 1, 14 |
| **#296** | Async completion + agent addressing: completion notification into the parent, names by default, `list_agents`, children may `send_message` — with CC's stale-name and no-approval-by-message guards | 4, 5, 8 |
| **#297** | Definition parity: the 11 functional frontmatter fields parsed-then-dropped (`maxTurns, permissionMode, hooks, mcpServers, disallowedTools, isolation, omitClaudeMd, memory, skills, background, initialPrompt`) | 2 |
| **#298** | Derived child permissions + approval relay — `TaskTool.Approve` is nil in every host, so M12's dialog must not land without it (filed as an M12 **prerequisite**) | 15 |
| **#299** | A budget-exhausted child reports its wrap-up turn as a finished answer; CC marks it partial (2.1.246+) | 6 |
| **#300** | Workflow runner: `workflowz` promises a runtime that does not exist; plan-file + hub runner, no JS embed | 9 |

Already-filed issues updated by comment the same day: **#191** (correct CC's defaults:
20 concurrent / depth 3 / real env var names), **#204** (fork: cache-sharing rationale,
steer/close, `isolation: worktree`), **#220** (state-carry census: name the two missing
dimensions), **#169** (re-scope: it specs caps/UI for a runner that was never built),
**#258** (closed unimplemented — superseded by #300).

