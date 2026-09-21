# ZCode — internals cross-check for xdev

**Researched:** 2026-09-22.
**Subject:** ZCode (AI coding workbench: Electron desktop + web + a terminal agent), read at
`/Users/linh.doan/work/harvey/freepeak/ZCode`, single commit `872ad96 "feat: open source"` (the repo
was published as one squashed commit; there is no useful history to date behaviour against).
Licence: MIT-style with a `NOTICE.md`/`THIRD-PARTY-NOTICES.md` chain.
**Method:** primary source read only — the peer's own tree, not its docs. The agent half is
`apps/zcode-cli/` (a nested pnpm workspace: 15 packages, ~38 MB of source), and the interesting
parts are `packages/core/src/runtime/` (the turn loop, 25.6 klines across `methods/`),
`packages/core/src/{context,tool,compact,permission,subagent}/`, `packages/adapters/src/{model,storage,context}/`
and `packages/bootstrap/src/zcode-protocol/session-resident-pool.ts`. File:line citations are given
per claim; every "the code does X" below was read, not inferred from a comment alone (where only the
comment asserts it, the claim says so).
**Maturity caveat (read before porting anything):** the tree has **zero test files** —
`find . -name '*.test.ts' | wc -l` is `0` in `apps/zcode-cli`, and `scripts/count-ts-lines.mjs`
explicitly *excludes* `__tests__/tests/`/`*.spec.ts` from its line count, so test dirs are a
supported shape that simply is not populated here. Verification entry points are `pnpm typecheck` +
`pnpm lint` (turbo) only, plus a `scripts/shadow-replay.mjs`. The peer also enforces a **400-line
per-file ceiling** (`architecture-policy.yaml:global.maxFileLines: 400`) with `forbidCycles` and
`forbidDeepImports`, and the tree is visibly shaped by that: the same mechanism is usually spread
across 5–20 single-purpose modules (`runtime/methods/` has 75+ files). Read the citations below as
"the design is stated and wired", not "the design is proven by a suite".

---

## 0. What ZCode is, in xdev's terms

Same category as xdev — an agent runtime with a session store, a tool surface, a provider layer, a
loop, and a TUI — on the opposite stack, and with three structural choices that are genuinely
different rather than merely bigger:

| Axis | ZCode | xdev |
|---|---|---|
| Session identity | a **content-addressed event ledger** in SQLite (`session` / `message` / `part` / `session_entry`), with a typed `SessionEvent` stream over it | an append-only **JSONL tree** with `parentId` + a mutable leaf pointer (omp's model, PRD §3.2) |
| Runtime residency | an explicit **idle-TTL/LRU pool** that keeps N sessions warm in one process, with eligibility facts and deactivation leases | one session per process; nothing to pool (`cmd/xdev` opens a store and runs) |
| Prompt assembly | a **sectioned `ContextBuilder`** producing three system messages plus meta-user attachments, each stamped with an explicit `cacheHint` (`stable`/`dynamic`) | one `SystemPromptBase` string + project-context + rules blocks, assembled ad hoc in `BuildSystemPrompt` |
| Turn state | an explicit `TurnMachineImpl` state machine the loop drives (`startModelRequest` → `receiveModelResponse` → `aggregateResults` → `complete`) | an implicit loop with a `turnError`/`turnContentEmitted` return-type protocol |
| File size | hard 400-line ceiling per file, enforced by `pnpm architecture:check` | soft, by convention |

The four below are the ones worth knowing even if nothing is ported: **the sectioned prompt with
declared cache hints**, **the residency pool with eligibility facts**, **streaming tool execution**
and **the rapid-refill compaction breaker**.

---

## 1. Session core

### 1.1 One event ledger, three audiences

ZCode writes one stream and reads it back through three separate projectors. The store is SQLite at
`~/.zcode/cli/...` with 22 numbered migrations (`adapters/src/storage/session-store/migrations.ts`,
`0001_base_session_store` … `0022_backfilled_session_reasoning`); `0001` creates `session`,
`message`, `part`, `todo`, `session_entry`, `permission`, `input_history`, and later migrations add
`session_input` (an input ledger), a dynamic-workflow journal, and provider/model selection columns.
Messages and parts carry a JSON `data` column rather than normalized columns — the schema is a
*container*; the shape lives in `@zcode/contracts`.

The three audiences of one append (`core/src/runtime/methods/events.ts:79-137`) are, in order:
`this.eventStore.append(event)` → `persistDurableSessionEvent(storedEvent)` → `recordToolUsageFromEvent`
→ `notifyEventSinks`. That ordering is the point: the event store assigns `sequenceNumber`, and the
live sink is handed **the stored event** rather than the freshly built one, so live, replay and
snapshot share one ordering fact. The comment above it states the bug it fixes: "live sink 以前拿到的是
createSessionEvent 默认的 sequenceNumber=0，而 replay/read 路径拿到的是 eventStore 补号后的事件，导致同一
session 有两套顺序事实".

**xdev: PARTIAL, and this is the single most load-bearing difference in the document.** xdev has a
JSONL tree of entries (`internal/session/entries.go`) — a *document* — plus a hooks event bus
(`internal/hooks`) that is not persisted and not replayable. ZCode has one append that is
simultaneously the durable record, the UI transcript, and the telemetry fact. What xdev gets from
the JSONL tree is fork/branch/rewind for free (PRD §3.2); what it does not get is the thing ZCode's
`SequenceNumber` buys: **one ordering authority**, so a UI that reattaches after a dropped
connection can resume from a sequence number it has already seen instead of re-reading the file and
reconciling. xdev's protocol does have an event stream (`internal/protocol`) with a materialized
`done` message and an ordered replay (`docs/reference/session-format.md`), so the gap is narrower
than it looks — but "the sequence number is assigned by the durable store, and the live sink only
ever sees stored events" is a rule worth copying into the seam verbatim. Filed below.

### 1.2 The turn loop: a state machine driven by a request history, not by the store

`runRegularTurnLoop` (`runtime/methods/turn-loop.ts:43-218`) is a `while(true)` over **one** logical
turn, and each iteration does the same seven things in the same order:

1. `throwIfTurnAborted` (the abort signal is checked at every stage boundary, not only at I/O);
2. drain runtime commands that arrived mid-loop (`drainPendingRuntimeCommandsForActiveLoop`) and
   append their entries to *this* request;
3. `microcompactIfNeeded` then `autoCompactIfNeeded` — with a circuit breaker
   (`evaluateRapidRefill`, §7);
4. `initializeMcp`, then build the tool list for this step;
5. inject the per-request system reminders (plan-mode exit, runtime mode, todo, output style);
6. project the request (`buildRuntimeProviderRequestMessages`) and record the turn machine's
   `startModelRequest`;
7. `runModelBackedTurnStep` — one provider round-trip plus its tool batch — and `break` when it
   says so.

The important structural fact is **the loop owns a `turnRequestState` that is separate from the
persisted history** (`runtime/methods/turn-loop-state.ts:75-83`):

```ts
export interface TurnRequestState {
  entries: readonly RuntimeMessageEntry[];
  outputTokenContinuationCount: number;
}
```

`state.turnRequestState.entries` is the *provider-local* history for the current turn, and it is
"Turn 结束后直接释放". The canonical history (`messageHistory`) is a different object. The two
converge through explicit commit points — `commitTurnRequestEntries`, `commitAssistantToTurnRequest`,
`commitTurnRequestEntries`, and the deliberate divergence of `filterOutputTokenContinuationEntries`
(build the request without the continuation marker, but record it *with* it). This is what buys the
model clean turn semantics without polluting the durable transcript.

**xdev: HAS the essence, by a different mechanism.** `internal/agent/loop.go` treats `history` as a
local slice and persists individual messages via `a.persist(m)` at explicit points (steering drain,
retry partials, continuation prompts, the wrap-up) — which is the same "local request view, explicit
commit points" split, achieved by convention rather than by a named state object. The delta is
auditability: ZCode can point at `TurnRequestState` and say what is in the request; xdev's answer is
"whatever `Run`'s local `history` happens to be at this line". Low priority on its own; it matters
as the substrate for §1.4.

### 1.3 Persistence discipline: no junk rows, and a removal path

Two rules in ZCode's turn that xdev does not have:

- **An assistant message is persisted before the request, then removed if nothing was committed.**
  `runModelBackedTurnStepImpl` calls `persistAssistantMessage(...)` *before* the provider call
  (`runtime/methods/turn-model-step.ts:180-186`), so the assistant row exists while the turn is in
  flight (the UI can render it) — and `persistCompletedAssistantStep`
  (`runtime/methods/turn-stop.ts:48-70`) calls `runtime.sessionStore.removeMessage(...)` when nothing
  was committed, then rewinds `latestAssistantMessageId`/`latestConversationMessageId` from a
  captured `assistantPersistenceAnchor`. A turn that produced no assistant content leaves no row.
- **A partial stream and its terminal error are two messages, not one.** `persistOutputTokenLimitErrorCarrier`
  (`turn-stop.ts:120-160`) explicitly refuses to put both on one durable assistant: "partial 与终态 error
  共用 durable assistant 时，Compact 无法同时重放 provider partial 并排除 transcript-only error."

**xdev: PARTIAL.** #122's write path (`internal/session/store.go:572-640`) is *stronger* than
ZCode's on the failure side — committed-byte boundary, torn-tail cut, `ErrConcurrentWriter` latch —
and xdev already avoids junk files by staying memory-only until the first assistant message
(`appendLocked` → `ensureOnDiskLocked`). What xdev lacks is the *removal* path: there is no
`removeMessage` equivalent, so an abandoned assistant turn can only be left in place (which xdev
does — the dangling toolCall is neutralized at `buildContext`, PRD §3.2, rather than removed).
ZCode's "persist early, remove on no-commit, rewind the anchors" is a cleaner answer than
"persist late, repair later", and it is what makes its live transcript honest during a long turn.
Worth a decision, not obviously worth a port.

### 1.4 Streaming tool execution — read-only tools run *during* the stream

The mechanism most likely to be worth stealing. While the model is still streaming,
`createStreamingToolCoordinator` (`runtime/methods/streaming-tool-coordinator.ts`) accepts each
tool call as its arguments finish and — for a strict subset — starts executing it immediately,
before the turn ends. The gate (`streaming-tool-coordinator.ts:339-358`):

```ts
return (
  metadata.readOnly &&
  metadata.concurrentSafe &&
  !metadata.destructive &&
  !metadata.needsApproval &&
  !requiresUserInteraction &&
  sideEffectScope === "none"
);
```

with `runtime.config.streamingToolExecution` (`"off" | "readOnly"`, `runtime/types.ts:128`) as the
switch and `"readOnly"` as the default. `drain()` then joins the already-running promises at the
end of the stream, so the tool batch's wall time overlaps the model's remaining output.

Two details make it safe rather than merely fast, and they are the design, not the optimization:

- **A failure falls back, never fails the turn.** The `.catch` on the streaming handle logs
  `tool.streaming.execution_failed` and returns `undefined`, which sends that call back through the
  normal end-of-stream path.
- **A model failure *keeps* the results it already has.** `recoverFromModelFailure` collects settled
  handles and synthesizes results for the unsettled ones
  (`createSyntheticStreamedToolResult`), then hands the batch to the recovery path — so a stream that
  dies after three of five reads does not throw three reads away.

**xdev: ABSENT.** `internal/agent/loop.go` runs `a.runTools(ctx, msg.ToolCalls())` *after* the
stream closes, on a bounded worker pool. A read-heavy turn (xdev's `read`/`grep`/`glob` are exactly
the qualifying shape) currently pays model-latency + tool-latency serially where ZCode pays the max.
Filed below with the gate spelled out — the honest risk is that xdev's classification lives in a
different place (`internal/tool/tool.go`'s `Tool` interface has no `concurrentSafe`/`sideEffectScope`
vocabulary; `internal/tool/policy.go` classifies by name), so the gate has to be built before the
execution can be overlapped.

### 1.5 Resume is a reconstruction, not a replay

`resumeFromStore` (`runtime/methods/resume.ts:59-240`) is worth reading as a checklist, because it
enumerates what a cold start must restore to be *equivalent* to the live process rather than merely
readable: session → messages → `extractPersistedEnvInfo` (the env the session ran under, so the
prompt is rebuilt with the same facts) → shell-environment selection → working directory and
`taskType` → **workspace identity for the memory root** ("Memory root 必须使用会话落盘时的
workspace identity，不能沿用进程启动 workspace") → fresh `MessageHistoryImpl` + context
reinitialization → read-file-state hydration → recover interrupted compact timelines → history
hydration → active-message selection applying rewind/fork cut markers → `turnNumber` recomputed from
the surviving user messages → discard persisted *pending* steer inputs.

**xdev: PARTIAL, and the delta is a real bug class.** xdev's resume is `--continue`/`--resume`
rebuilding context from the store (`internal/session/context.go`), and the dock-content report of
2026-09-17 ("when exit xdev session and resume again, the sidebar content disappear") is exactly the
symptom of a resume that restores *history* but not *process facts*. ZCode's list is a specification
of which process facts a resume owes you; the two most portable members are env-info (so a resumed
prompt is byte-identical to the one the session ran with, which is also a prompt-cache requirement)
and the workspace-identity-for-memory-root rule. Enriches the resume-depth work already filed as
#158/#220.

---

## 2. Plan mode

ZCode's plan mode is **two tools plus a mode flag plus a permission rule**, and the split is the
design.

- **`EnterPlanMode`** (`tool/handlers/plan-mode.ts:36-58`): calls
  `context.sessionModePort.enterPlanMode(...)`, returns the transition. Permission is
  `behavior: "allow"` with **no prompt** (`permission/plan-mode-policy.ts:23-29`) — entering
  read-only is free.
- **`ExitPlanMode`** (`plan-mode.ts:60-104`): refuses when not in plan mode
  (`CoreErrorType.InvalidStateTransition`, message: "You are not in plan mode … If your plan was
  already approved, continue with implementation."), writes the plan to a file
  (`writeApprovedPlanFile` via `runtime/helpers/plan-file-continuity.js`), then transitions. It is
  declared `needsApproval: true` + `requiresUserInteraction: true`, and its permission rule is
  `behavior: "deny"` when plan mode is off (`plan-mode-policy.ts:31-40`).
- **The plan travels in the tool argument.** `ExitPlanModeInputSchema` requires `plan`, and the
  output echoes it back (`plan: parsed.plan`). The user reviews the argument's contents — the model
  does not "finish planning in prose and hope".
- **A plan file survives the transition.** `persistApprovedPlanFileBeforeExitPlanMode` runs *before*
  the mode flip, so the approved plan exists on disk even if the session dies immediately after.
- **The provider descriptions are long and opinionated** (`tool/handlers/plan-mode-prompts.ts`): the
  enter description carries an explicit *when to use* / *when not to use* / *examples* structure with
  a GOOD/BAD list, and 7 numbered triggers (new feature, multiple valid approaches, code
  modifications, architectural decisions, multi-file changes, unclear requirements, user preferences
  matter). It tells the model "If unsure whether to use it, err on the side of planning".

The read-only enforcement is a **permission-mode policy over declared capability**, not a tool
allowlist (`permission/service.ts` `checkPlanMode`): read-only && !destructive → allow
(`mode.plan.readOnly`); non-destructive MCP → allow (`mode.plan.mcp`); an explicit
`allowedInPlanMode` capability whose `sideEffectScope === "session"` and which is neither
destructive nor `needsApproval` → allow (`mode.plan.explicitSessionCapability`); everything else →
deny (`mode.plan.nonReadOnly`). That is a strictly better shape than a name list because it *fails
closed on new tools*: a tool registered tomorrow is denied under plan mode until it declares
otherwise.

**xdev: PARTIAL, and this is where the peer is clearly ahead.** xdev's plan mode is
`internal/agent/planmode.go` (520 lines): a `PlanMode` struct, `propose`/`xd://propose`/`xd://resolve`
devices, yolo/plan-only postures, the dock's pending-plan card, and a **hardcoded allowlist**:

```go
var planReadOnlyTools = map[string]bool{
	"read": true, "grep": true, "glob": true, "ast_grep": true,
	"web_search": true, "lsp": true,
}
```

with the comment (correctly) explaining why `Classify` was rejected for it — the approval tier table
only covers the core four and errors on the rest. That is a fail-*open-on-omission* list: a new
read-only tool (say a future `web_fetch`/`security_scan`/`github`) is denied in plan mode until a
human edits the map, which costs turns, while a new *mutating* tool added to that map by mistake
would silently be permitted. The parity finding from 2026-09-12 (T3 #28: "full toolset (incl.
write/edit/bash/task) still advertised while planning; denial happens per call") is the other half —
ZCode's `EnterPlanMode` description tells the model *before* it acts that it is read-only.

The three portable pieces, in confidence order: **(a)** plan-mode policy by *declared capability*
(right shape, closes T3 #28/T3 #27); **(b)** the two-tool enter/exit split with `needsApproval` on
exit and a free enter (xdev has one `propose` and infers the rest); **(c)** the approved-plan file
written *before* the mode flip (xdev's dock card is memory-only — the 2026-09-17 resume report is
the same failure mode one surface over).

---

## 3. System prompts

### 3.1 A sectioned builder with declared cache hints

`ContextBuilder.build()` (`core/src/context/builder.ts:80-260`) assembles an ordered
`ContextSection[]`, each section carrying `{name, source, injectionTarget, cacheHint, chars, tokens,
content, preview}`. `injectionTarget` is `system | meta_user`; `cacheHint` is `stable | dynamic`.
Assembly then happens by *hint*, not by list order
(`assembleSystemMessages`, `builder.ts:181-230`):

- one `system` message from all `injectionTarget === "system" && source === "cli_prefix"` sections;
- one `system` message from all `system && cacheHint === "stable"` sections (excluding the prefix);
- one `system` message from all `system && cacheHint === "dynamic"` sections, prefixed with `\n\n`;
- meta-user sections become attachments (`skills_listing`, `context_prefix`), not messages.

**Every one of those three messages gets `cacheControl: {type: "ephemeral"}`.** That is the whole
trick: the prompt-cache boundary is a *declared property of the section*, computed once at build
time, instead of a provider-specific guess made at request time.

The section list, in build order, with the role of each:

| # | Section | Target / hint | Content |
|---|---|---|---|
| 1 | `cli_prefix` | system / stable | short leading identity ("You are ZCode, an interactive coding agent") |
| 2 | identity **or** custom **or** workflow-actor | system / stable | mutually exclusive; `workflowActor` + `customSystemPrompt` together **throw** ("loudly fail rather than silently pick one") |
| 3 | `desktop` | system / dynamic | only when `presentationSurface === "zcode_desktop"` |
| 4 | `dynamic_behavior` | system / dynamic | the communication contract (see below) |
| 5 | `session_guidance` | system / dynamic | tool-conditional lines; `null` when there are none |
| 6 | `memory` | system / dynamic | `MEMORY.md` root |
| 7 | `env_info` | system / dynamic | cwd/platform/shell/git |
| 8 | `output_style` | system / dynamic | only when a style has a non-empty prompt |
| 9 | `context_management` | system / dynamic | the compaction contract (see below) |
| 10 | `git_system_context` | system / dynamic | |
| 11 | `skills` | meta_user / dynamic | catalog listing, budgeted |
| 12 | `request_user_context` | meta_user / dynamic | workspace instructions / project memory |
| 13 | `current_date` | meta_user / dynamic | |
| 14 | custom sections | as declared | extension point |

The `meta_user` target renders as one message wrapped in a fixed frame
(`buildContextMetaUserBody`, `builder.ts`):

> As you answer the user's questions, you can use the following context:
> …
> IMPORTANT: this context may or may not be relevant to your tasks. You should not respond to this
> context unless it is highly relevant to your task.

### 3.2 The prompt text itself, and what it says

`context/dynamic-sections.ts` holds the prose. Three blocks are worth quoting because they are
better than xdev's equivalents in specific, testable ways:

**The communication contract** (`COMMUNICATION_PROMPTS.additional`, shipped as `Dynamic Behavior`):

> Your text output is what the user reads; they usually can't see your thinking or the raw tool
> results. Write it for a teammate who stepped away and is catching up, not for a log file …

> **Text you write between tool calls may not be shown to the user.** Everything the user needs from
> this turn — answers, summaries, findings, conclusions, deliverables — must be in the final text
> message of your turn, with no tool calls after it.

> Lead with the outcome. Your first sentence after finishing should answer "what happened" …

> Being readable and being concise are different things, and readable matters more. If the user has
> to reread your summary or ask you to explain, any time saved by brevity is gone. The way to keep
> output short is to be selective about what you include … not to compress the writing into
> fragments, abbreviations, arrow chains …

> Match the response to the question: a simple question gets a direct answer in prose, not headers
> and sections. Use tables only for short enumerable facts …

And the paired default (kept alongside the "additional" block, not replaced by it):
*"Write code that reads like the surrounding code: match its comment density, naming, and idiom."*

**The context-management contract** (`CONTEXT_MANAGEMENT_PROMPTS`, shipped as `Context Management`):

> When the conversation grows long, some or all of the current context is summarized; the summary,
> along with any remaining unsummarized context, is provided in the next context window so work can
> continue — **you don't need to wrap up early or hand off mid-task.**

plus, in the same section: *"When you have enough information to act, act. Do not re-derive facts
already established … Before ending your turn, check your last paragraph. If it is a plan, an
analysis, a question, a list of next steps, or a promise about work you have not done, do that work
now with tool calls. … Do not stop because the context or session is long."*

**The identity block** (`context/sections/identity.ts`) is short and carries one thing xdev should
notice: a **Harness** section that teaches the model how the *system* behaves — "Text you output
outside of tool use is displayed to the user as Github-flavored markdown in a terminal", "Tools run
behind a user-selected permission mode; a denied call means the user declined it — adjust, don't
retry verbatim", "The system may send updates, reminders, or modifications to rules via
mid-conversation system turns. These are system-controlled, unlike function results. Hooks may
intercept tool calls; treat hook output as user feedback", "Reference code as `file_path:line_number`
— it's clickable".

**xdev: PARTIAL.** `SystemPromptBase` (`internal/agent/prompt.go:20-32`) is 8 rules and no contract
for any of the three above: nothing tells the model that mid-turn text may be invisible, nothing
tells it that compaction is coming and it must not wrap up early, and nothing tells it how
permissions/hooks/system-reminders will behave. The *first* is the highest-value line in the peer's
prompt and it is one paragraph — xdev's own transcript discipline problem (the "only the final text
matters" issue) is exactly what it fixes. The `Context Management` paragraph is what makes a long
session *work* rather than *survive*: xdev has the compaction machinery (and the per-turn token
budget, and the wrap-up turn) but tells the model nothing, so the model discovers the boundary
itself. Filed below, cheap.

The sectioned builder's structure is a second, independent proposal: xdev's `BuildSystemPrompt`
concatenates `base + "# Project context" + "# Tools"` with character budgets
(`MaxToolRecapChars`, `MaxToolDescriptionChars`, `MaxContextBytes`) and no cache-boundary concept.
Moving to declared `{source, injectionTarget, cacheHint}` sections is what makes the *order*
cache-optimal by construction rather than by a human keeping it stable — the exact failure
mode the 2026-09-15 prompt-cache research (`docs/research/omp-prompt-cache-2026-09-15.md`) is about.

---

## 4. System tools

### 4.1 The declaration surface

A tool is a `ToolEntry` (`core/src/tool/types.ts:65-94`) whose `metadata` is a capability manifest:

```ts
export interface ToolMetadata {
  name: string; description?: string; modelInstructions?: readonly string[];
  allowedInPlanMode?: boolean;
  readOnly: boolean; destructive: boolean; concurrentSafe: boolean;
  requiresUserInteraction?: boolean;
  timeoutMs?: number; maxOutputBytes?: number;
  sideEffectScope: ModelToolSideEffectScope;   // none|workspace|git|network|system|session|userInteraction
  riskLevel: RiskLevel; needsApproval: boolean;
  providerVisible?: boolean;
  stopTurnOnSuccess?: boolean;                  // a tool that ends the turn by contract
  mcpPresentation?: {...};
}
```

plus a `ToolContractDeclaration` (`contracts/src/tools/contract.ts:168-187`) carrying
`inputSchema` + `outputSchema` (JSON Schema, generated from **zod** via `toToolJsonSchema`),
`strict?: boolean` (eligibility for provider-side constrained decoding), `permission`, `resultBudget`,
`timeout`, `cancellation`, `trace`. The registry (`tool/registry.ts:44-127`) is a map with a
carefully reasoned alias layer: canonical names always win over aliases, a colliding alias is
**refused with a warning rather than silently overriding** ("兼容 alias 若静默覆盖 canonical/另一个
alias，会把一次工具调用路由到错误的权限和 handler"). `toContracts()` filters
`providerVisible !== false` and folds `modelInstructions` into the description as a `Usage:` bullet
list (`toolDescriptionForProvider`) — one channel for prose, not two.

The result-budget declaration is a separate, explicit contract
(`contracts/src/tools/contract.ts:117-137`): `maxInlineBytes`, `maxModelBytes`, `strategy:
"inline" | "truncate" | "artifact"`, optional `preview` (`maxBytes`/`maxLines`/`head|tail`), optional
`artifact` (`enabled`, `retention: session|project|temporary`). Big results go to an artifact store
by *declaration*, not by the executor guessing from size.

Two derived predicates live in `contracts` (not in the solver) precisely because two layers need the
same answer: `isWorkspaceMutatingToolCall` (readOnly !== true AND scope ∈ {workspace, git, system};
**absent scope counts as mutating, conservative**) and `isWorldTouchingToolCall` (everything except
the protocol-only scopes `session`/`userInteraction`; used to decide whether a cached answer is
*pure*). The comments say why they are here rather than in the consumer: the producer (core) and the
consumer (bootstrap's workflow driver) must not drift.

A tool can also declare `stopTurnOnSuccess` — "the tool's intrinsic capability declaration (like
concurrentSafe/destructive), read by the executor, **not guessed at the call site by tool name**".
That last clause is the whole argument for the manifest.

**xdev: PARTIAL.** `internal/tool/tool.go`'s `Tool` interface is `Name/Description/Parameters/
Execute` — no capability vocabulary at all. Classification lives in a name-keyed policy table
(`internal/tool/policy.go`) and name sets in the agent (`planReadOnlyTools`, `READ_ONLY_TOOLS` in
the scheduler, the plan gate's `isReadOnlyTool`). The consequence shows up in three places at once:
plan mode's fail-open list (§2), the streaming-execution gate that cannot be written (§1.4), and
the scheduler's `READ_ONLY_TOOLS` set existing *parallel to* the plan list rather than being the
same fact. ZCode's manifest is one declaration that four consumers read. This is the largest single
structural idea in the peer and it should probably be its own milestone rather than an issue —
filed as such below.

### 4.2 Per-request visibility is enforced at the provider boundary

`buildTurnDisallowedTools` (`turn-loop.ts:221-238`) computes the step's hidden set, and the loop
then filters the *provider-visible contract list*:

```ts
const tools = state.automationCreateLimitReached
  ? []
  : turnDisallowedTools
    ? this.getTools(state.model).filter((tool) => !turnDisallowedTools.has(tool.name))
    : this.getTools(state.model);
```

with the comment that the automation query-id check must re-filter "at the provider request
boundary … otherwise the model would see and create/modify/delete task definitions before the
executor could deny them". So: *hiding is a request-shaping act*, and the executor's deny is
defence in depth for the same rule, not the primary enforcement.

**xdev: PARTIAL.** `internal/agent/loop.go`'s `toolDefs()` builds the request's tool list, and the
plan mode gate is applied at *call* time (`applyPlanMode`, `loop.go:1424`). xdev does inject a
per-turn disallow list in some paths, but the rule "the deny must also be expressed by removing the
tool from the request" is the one that saves a wasted round-trip per denied call.

### 4.3 Subagents are the same runtime with a narrowed profile

`runtime/methods/subagent.ts` constructs a **full second `AgentRuntime`** for a child (own
`sessionId`, own event sink sharing the parent's `eventStore`, own model factory, own permission
service), then resolves the child's identity from an `AgentProfile`
(`subagent/profile.ts:19-38`): `description`, `tools`, `disallowedTools`, `modelSelection`,
`permissionMode: "auto" | "plan"`, `maxTurns`, `mcpServers`, `skills`, `memory:
"user"|"project"|"local"`, `injectAgentsMd`, `background`, `color`. Profiles come from built-ins
(`general-purpose`, `Explore`), project dirs and user dirs; a project-level profile **cannot raise
its own permission mode** ("项目级 subagent markdown 属于仓库输入，不能通过 frontmatter 把 child runtime
提升到 bypass/yolo；权限模式只接受用户级或受信插件配置").

Three details worth carrying:

- **Plan mode is inherited as a *mode*, and the plan tools are force-removed from every child** —
  `SUBAGENT_CHILD_FORCED_DISALLOWED_TOOLS = [EnterPlanMode, ExitPlanMode]`
  (`subagent/tool-policy.ts:4-7`), with the reason stated: "子 agent 没有独立的 plan approval
  恢复面，暴露 plan tools 会让 ExitPlanMode 等待用户确认并卡住父 turn". A child cannot hang the
  parent on a confirmation the parent cannot answer.
- **Every blocking interaction routes back to the parent session** via `deriveChildClientPorts`
  (permission broker + AskUserQuestion + provider runtime headers): "桌面 UI 只认识父 task 的
  sessionId". The child's *events* persist under the child session id; its *questions* are the
  parent's.
- **The child's own note text is a stable, shared block** (`subagent/system-prompt.ts`): cwd resets
  between bash calls (so use absolute paths), share absolute paths in the final response, include
  code snippets only when the exact text is load-bearing, no emojis, no colon before tool calls,
  and — the interesting one — **"Do NOT Write report/summary/findings/analysis .md files. Return
  findings directly as your final assistant message — the parent agent reads your text output, not
  files you create."**

**xdev: PARTIAL to ABSENT.** xdev has task agents with markdown+frontmatter discovery and a spawn
policy (M11), and the subagent context-file gap was just fixed (#416: `SpawnChild` now appends the
context-file hierarchy). What xdev does not have: a per-profile `permissionMode`, per-profile
`mcpServers`/`skills`/`memory` scoping, or the "context files are injected per-profile
(`injectAgentsMd`)" *choice*. The `injectAgentsMd` flag is a real design point: ZCode's `Explore`
profile sets it **false** — a read-only search child does not need 32 KB of standing conventions in
its window, and xdev's #416 fix (correct for task children) would be wrong for an Explore-shaped one.
The "don't write report files, return findings" line is also worth stealing verbatim; it is a
failure mode xdev's own `task` children hit.

---

## 5. Talking to the LLM API

### 5.1 The stack, and the one interface that matters

The provider layer sits in `packages/adapters/src/model/` (12.3 klines) over the **Vercel AI SDK**
(`@ai-sdk/anthropic`, `@ai-sdk/openai`, `@ai-sdk/openai-compatible`) — the opposite choice from
xdev's hand-rolled `net/http` + SSE (`internal/ai`), and from the PRD's stated non-goal of a JS
runtime. What is worth reading is not the SDK choice but the *seam*:

- `model-execution.ts` (`AiSdkModelExecution.bindModel`) **freezes the provider's static facts at
  model-creation time**: baseURL, protocol/kind, SDK factory, provider options. The comment is the
  contract: "请求期鉴权只覆盖 API Key 与 Header；Registry 后续热更新不会让已经创建的 Model 静默切换
  Endpoint、协议、Provider Options 或 SDK Factory." So a session's model cannot be silently
  re-pointed mid-run by a config hot-reload — only credentials change per request.
- `runner.ts` (`AiSdkModelAdapter`) is the wrapper that adds what the SDK lacks and what xdev
  implements natively: retry policy, stream idle timeout, status sink, debug/IO recording,
  **reasoning-history normalization** (`normalizeReasoningHistory`), and the empty-completion retry.
- `transform.ts` / `tool-transform.ts` / `strict-tool-schema.ts` translate xdev-equivalent concepts
  (blocks, tool schemas, strictness eligibility) at the boundary rather than leaking SDK types
  upward.

**xdev: HAS the equivalent seam, differently shaped.** `internal/ai/provider.go`'s `Provider`
interface (`Stream(ctx, req) (<-chan Event, error)`) is *cleaner* than ZCode's OpenAI-SDK-shaped
surface and is the reason xdev's four adapters share one SSE reader. The portable half is the
`bindModel` freeze rule: xdev resolves a provider per request from config *and* from a live
`/model` switch, and the 2026-09-20 `fix/model-live-switch` entry (`_ = nprov`, the built provider
discarded) is the same class of bug the ZCode comment is defending against. Worth an explicit
statement in xdev's provider docs even if nothing changes.

### 5.2 Retry: a *budget object* passed on the request, plus a failure classifier

`retry-policy.ts` resolves `{maxAttempts, baseDelayMs, backoffFactor, maxDelayMs, jitter}` from
options → env (`ZCODE_MODEL_RETRY_*`) → defaults (`maxAttempts: 11`, `baseDelayMs: 2000`,
`backoffFactor: 2`, `maxDelayMs: 60000`). The retry loop itself is inside `runner-stream.ts`, and its
shape is the interesting part: `retryBudgetAllows(retryBudget, attempt)` consults a **per-request
retry budget object** (`input.request.modelRetryBudget`) rather than a per-process constant, and
`retryBudgetMaxAttempts(budget, fallback)` returns `0` to mean **unbounded** — the status events
publish `maxAttempts: 0` for "no ceiling". So the ladder's shape is a property of the request.

Failures are classified (`failure-classifier.ts`, 633 lines; `failure-inspection.ts`,
`failure-provider-business-codes.ts`, `failure-tls.ts`, `provider-finish-business-error.ts`) with
inspection helpers that reach *through* wrapped errors: `getErrorCode`, `getHttpResponseStatus`,
`getResponseHeaders`, `unwrapRetryError`, `findProviderBusinessError`. Two classes are handled by
name because they are not transient in the ordinary sense: **`PROVIDER_BUSINESS_ERROR`** (a 200 that
carries a business error in the body — captured with a 64 KB body cap and a 1 s cleanup timeout) and
**off-peak/queue holds** (`offpeak-retry.ts`: `resolveOffPeakFailureDecision`,
`offPeakTicketExpiredMessage`) where the correct behaviour is to *wait*, not to retry.

### 5.3 Stream liveness: idle timeout with a retry-aware escalation, and an abort race

`stream-idle-timeout.ts` is the piece xdev should read line by line, because xdev's watchdog
(`internal/ai/watchdog.go`) has the same job and a different, weaker answer to two questions.

The timeout itself escalates with the attempt number:

```ts
const MODEL_STREAM_IDLE_TIMEOUT_RETRY_INCREMENT_MS = 30_000;
// baseTimeoutMs + retryNumber * 30_000
```

so attempt 1 gets the base window and attempt 5 gets base + 2 minutes. xdev's
`FirstProgressTimeout`/`IdleTimeout` are fixed package vars (90 s); the peer's rule is "a stream that
has already failed twice deserves a longer leash", which is the difference between retrying a slow
gateway and hammering it.

The read is a **three-way race**, and the third arm is the one xdev's watchdog deliberately does not
have:

```ts
return await Promise.race([iterator.next(), timeoutPromise, abortPromise]);
```

where `abortPromise` rejects on `signal.aborted` with the signal's reason. The comment states the
bug it fixes: "Stop 按钮会 abort 当前模型请求；如果底层 provider 没有让 iterator.next() 立刻返回，这里必须
主动结束等待，否则 UI 会一直卡到 idle timeout." xdev's watchdog is careful in the opposite
direction — its comment (correctly, per #283) says liveness must be judged from the *source* side so a
stalled consumer can never kill a healthy stream, and it re-arms on `len(ch) > 0`. Both are right
about different failure modes: ZCode's race protects the *user's cancel* from a wedged provider;
xdev's source-side judgement protects a *healthy stream* from a wedged UI. **xdev is missing the
first.** Its watchdog forwards events and owns the timers, but the *consumer* of the channel (the
agent goroutine) has no arm that resolves a pending read when the turn is cancelled — `ai.Drain(ch)`
exists (called from `ttsrAbort` and the error path) but a cancel that lands while `for ev := range
ch` is blocked in the runtime has nothing to race against.

The `createLinkedAbortController` helper is also a small, exact idea: the attempt gets a *child*
signal linked to the parent, with the listener explicitly removed on cleanup (`{once: true}` +
`removeEventListener`), and the returned `cleanup()` is called in the caller's `finally`.

**xdev: PARTIAL.** Filed below.

### 5.4 Prompt caching as an explicit, per-request, budgeted act

`internal/ai/cache.go` is the model for this: cache markers are placed by a function that knows the
provider's budget and the message shape.

- `cacheRetention()` resolves `XDEV`-style env (`ZCODE_CACHE_RETENTION`, falling back to omp's
  `PI_CACHE_RETENTION`) to `long | short | none`, read *per call* rather than memoized, with the
  reason given ("a process-lifetime Once would make the override untestable").
- `cacheEnabled()` is false for `none` **and false once a gateway has rejected the field** —
  `cacheMarkersDisabled` is an `atomic.Bool` latch, with the comment "One bad 400 must not keep
  killing every turn of every session in the process".
- `applyAnthropicBreakpoints(msgs, budget)` spends a **budget of 4** (`cacheMaxBreakpoints`) in a
  deliberate order: the newest markable message first, then every **15th** older one
  (`cacheTailPeriod`, "deep enough to be worth a write, sparse enough to leave budget") walking
  backwards so the most expensive-to-rewrite marker is placed before the budget runs out, then the
  second-newest if budget remains.
- `markAnthropicMessage` walks a message's blocks backwards and **skips `thinking`/`redacted_thinking`
  on purpose**: "an unsigned replayed thinking block is rejected by the endpoint, so a marker there
  would fail the request instead of merely missing the cache (omp FHr skips thinking/redacted_thinking
  for the same reason)".
- `cacheRejected(err)` is the one error class a marker can cause: a 4xx whose body names the field.

The context builder's contribution (§3.1) is the *other* half: the system messages are marked
`ephemeral` at build time, and `finalizeLatestNonSystemMessageCacheControl`
(`runtime/helpers/provider-request-messages.ts:289-311`) clears every non-system marker and places
exactly one at the newest non-system message — **after** the ordering projection, because "provider-visible
user ordering projection 会改变最终 latest user 位置，cache-control 必须在 projection 后统一设置，
避免 raw synthetic entry 抢占缓存锚点". That is the same lesson as xdev's
`docs/research/omp-prompt-cache-2026-09-15.md`, independently derived and with a mechanism attached.

`skipCacheWrite` is a nice touch: a side request (compaction summary) can mark the *second-newest*
message instead of the newest, so it reads a warm prefix and does not write a marker whose span the
main chain will never re-read.

**xdev: HAS a good half.** `internal/ai/cache.go` already implements the env override, the
`none` opt-out, the periodic deep markers, the thinking-block skip, and the rejection latch — this
is one of the areas where xdev is ahead. The deltas worth filing: (a) xdev has no
`skipCacheWrite`/side-request concept for its own summarizer (`summarizeWith` builds its own
request); (b) the marker is not *reconciled after projection* in xdev's pipeline because xdev has no
projection step to reconcile with — markers are set on adapter wire messages directly.

### 5.5 Closing the stream, and the "no guessing" rule for tool args

`runner-stream.ts` maintains a `StreamingToolCallAssembler` and emits block lifecycle events; the
terminal parse is strict. The xdev-side equivalent already has the same hard-won rule
(`internal/ai/sse/partialjson.go` + PRD §3.3: "the final parse at `toolcall_end` is authoritative and
strict; when it fails the call is answered with `{}` … never a guessed prefix"). ZCode's
`tool-call-validation.ts` / `tool-input-normalization.ts` / `toolargs_test.ts` occupy the same
territory.

The genuinely different member here is **in-band tool calling** (`internal/ai/inband.go`,
M14 #62): `InBandProvider` wraps any provider whose model cannot emit structured calls, inlines the
tool catalog + dialect guide into the system prompt, drops the native `tools` field, rewrites
replayed calls/results into dialect text, and decodes the assistant's text back into `toolcall_*`
events (synthesizing ids). It is registered per-provider/per-model by a `ToolFormat` matrix
(`toolconv.go`). Its own comment states the ceiling honestly:

> ponytail: text is buffered per block, so an in-band model streams text a block at a time instead
> of token by token. Ceiling: long answers lose incremental rendering. Upgrade path: an incremental
> scanner keyed on the dialect's openers once a live in-band model makes it worth it.

**xdev: PARTIAL.** xdev's `internal/ai/toolconv.go` has the format matrix but the in-band *decoder*
(the response half) is where the value is. This is what PRD §2 lists as "Schema-flavor dispatcher +
auto toolconv matrix unreachable; ollama/CCA absent" (#98) — ZCode's implementation is a working
reference for the unreachable half.

---

## 6. Compaction, in three tiers

ZCode runs **three different mechanisms with three different triggers and three different costs**,
and the ordering is the design.

### 6.1 Microcompact — no model call, before the request

`core/src/compact/microcompact.ts`. Trigger: a threshold derived from the auto-compact threshold
(`buildDefaultMicrocompactThreshold` = `min(threshold * 0.9, threshold - 2000)`), or an **idle**
trigger (`DEFAULT_MICROCOMPACT_IDLE_THRESHOLD_MINUTES = 60`). Action: replace old **tool results**
with `"[Old tool result content cleared]"`, keeping the most recent
`DEFAULT_MICROCOMPACT_KEEP_RECENT_TOOL_RESULTS = 5`, over a declared set of tools
(`Read, Bash, Grep, Glob, WebFetch, WebSearch, Edit, Write, ApplyPatch`), with a
`minTokenSavings = 256` floor (do not churn for nothing) and an option to clear error results.
It runs *before* the model call in the step loop. A `MicrocompactBoundaryPayload` is emitted as an
event so the transcript can show what was dropped.

This is the Claude-Code microcompact, and the interesting part is that it is **a first-class
provider-visible boundary event**, not a silent mutation: the UI and the session record both know
that content was cleared at this point.

### 6.2 Auto-compact — a model call, with a circuit breaker

`compact/policy.ts` computes the decision from a `contextWindow`, an output reserve, and a buffer:
`effectiveContextWindow = contextWindow - min(maxOutputTokens, 21_000)`;
`threshold = effectiveContextWindow - 13_000`. The reserve subtraction is deliberate:
"provider 的 context window 是 input + output 共享窗口；自动压缩只能让出输入侧，因此阈值分母必须先扣掉当前
模型允许的 output token". The decision carries a `reason` union
(`disabled | not_enough_messages | circuit_breaker | below_threshold | above_threshold`) and a
`tokenSource: "estimate" | "provider_usage"` — the estimate is overridden by the provider's reported
usage when available, and the decision *records which one it used*.

`MAX_CONSECUTIVE_AUTOCOMPACT_FAILURES = 3` is a **circuit breaker**: three failures and the policy
stops trying.

### 6.3 The rapid-refill breaker — the loop-level guard

This is the piece xdev does not have at all (`runtime/methods/turn-loop-state.ts:15-17`):

```ts
export const RAPID_REFILL_TOOL_TURN_THRESHOLD = 3;
export const MAX_CONSECUTIVE_RAPID_REFILLS = 3;
```

`evaluateRapidRefill(compactTracking)` fires when a compaction is followed by fewer than 3 tool
turns before the context refills to the threshold again — i.e. the compaction is not buying space —
and after 3 consecutive such events `autoCompactOutcome === "rapid_refill_blocked"` throws
`createCompactRapidRefillError` with the counts in the message. The loop also tracks
`toolTurnsSinceCompact` and resets `repeatedToolCallSignature`/`repeatedToolCallStreakCount` when
mid-turn commands are drained.

zcode's `micromanage` counterpart: xdev's `maybeCompact` (`internal/agent/compact.go:353-390`)
compacts and, on failure, logs and returns — there is no counter, no breaker, and no signal to the
user that compaction is not working. A pathological session (a huge tool result that immediately
refills the window) will compact on every boundary forever, at full provider cost, silently.

### 6.4 The summarizer prompt and preservation

`compact/prompt.ts` composes a `NO_TOOLS_PREAMBLE` + `BASE_COMPACT_PROMPT` + `NO_TOOLS_TRAILER`:
the preface is an explicit "CRITICAL: Respond with TEXT ONLY. Do NOT call any tools. … Tool calls
will be REJECTED and will waste your only turn — you will fail the task", and the body demands an
`<analysis>` block before a `<summary>` block with 9 numbered sections (primary request/intent, key
technical concepts, files and code sections, errors and fixes, problem solving, **all user messages**,
pending tasks, current work, next step). The preserved-segment logic then keeps a tail
(`selectPersistedCompactTail`), and the compaction boundary record carries
`{summaryMessageId, lastSummarizedMessageId, firstKeptEntryId, preCompactTokenCount,
postCompactTokenCount, truePostCompactTokenCount, willRetriggerNextTurn, phase, trigger, reason}` —
`willRetriggerNextTurn` is a *prediction recorded as a fact for later audit*, and
`phase` distinguishes `PreRequest` from `MidTurn`.

**xdev: HAS the tier it matters most, plus the ladder.** `internal/agent/compact.go`'s threshold
(0.80), reserve/keep-recent numbers, `findCutPoint`'s "never split a tool turn" rule, the ladder
(`remote, snapcompact, handoff, shake, soft`) with `RegisterCompactionMethod`, the idle trigger, the
async trigger, the memory-pressure trigger, and the handoff document with its cache-aligned side
request are all present and, in the handoff case, more sophisticated than ZCode's. The three
portable members are, in value order: **(a)** the rapid-refill breaker (§6.3) — xdev has a
`maxEscalationRounds` for *retries* and nothing equivalent for *compaction*; **(b)** the
microcompact tier (§6.1) — xdev's `shake`/`soft` members elide at a *boundary* (after the threshold
is crossed and a summary is being produced), whereas microcompact clears old tool results *before*
the request without a model call and is therefore cheap enough to run on the ordinary path; **(c)**
the compaction record's provenance fields — xdev's `CompactionEntry` carries
`{summary, firstKeptEntryId, tokensBefore, method}` and nothing about *why* or *whether it worked*
(filed as an enrichment of #116 in the dsh study: "no provider/model/usage on the compaction record").

---

## 7. Lifecycle: how ZCode keeps a session alive for a long time

This is the section the peer answers most directly, because it is the section xdev has no
equivalent for. ZCode separates three questions that xdev conflates:

1. **Is this session allowed to be dropped?** (`SessionResidencyFacts`)
2. **How many sessions stay warm?** (`SessionResidentPool`, 8 target / 16 high-water / 10 min idle)
3. **Which work must outlive the current call stack?** (`trackResidencyBlockingWork`)

### 7.1 The residency pool

`packages/bootstrap/src/zcode-protocol/session-resident-pool.ts` (318 lines) is a pure
capacity/lifecycle controller — its header says so explicitly: "这是容量与生命周期控制器，不拥有
session 内容。宿主提供当前 resident registry、同步安全事实和去激活执行面；pool 只维护 idle TTL、LRU
touch、operation lease 与 deactivation gate."

The host supplies three things:

```ts
export interface SessionResidentPoolHost {
  listSessionIds(): string[];
  readResidencyFacts(sessionId: string): SessionResidencyFacts | null;
  deactivate(sessionId: string): Promise<void>;   // first sync slice must remove from registry
  onDeactivated?(sessionId, decision): void;
  onError?(sessionId, error, decision): void;
}
```

and the facts are the whole design:

```ts
export interface SessionResidencyFacts {
  persisted: boolean;
  hasResidencyBlockingWork: boolean;
  hasPendingInteractions: boolean;
  hasQueuedCommands: boolean;
  hasSubscribers: boolean;
  hasLegacySubscriber: boolean;
  lastActivityAt: number;
}
```

`isEligible` requires **all** of them clear plus a zero operation-lease count plus no in-flight
deactivation. Two mechanisms make it safe rather than merely correct-looking:

- **The facts are re-read immediately before eviction.** Both the idle sweep and the high-water
  sweep capture candidates, then re-read `readResidencyFacts` and re-check `eligibleSinceAt`
  before running the decision, with the comment: "候选收集后可能重新订阅或启动后台任务，TTL 到期也不能
  绕过 fresh facts 与 eligibleSince 二次校验" and "不能让过期 LRU 快照取消仍在运行的 session". This is
  the classic TOCTOU hole in an eviction loop, closed explicitly.
- **`eligibleSinceAt` is cleared on `touch`.** The TTL measures *continuous* idleness: "idle TTL 表达
  '最后一次使用后的连续空闲'，协议请求重新使用 session 后必须重新计时". So a session that is polled
  once a minute never becomes eligible no matter how small its work is.

Operations take a lease (`acquireOperation(sessionIds?)`) that increments both a per-session count and
a process-wide count; while `activeOperationCount > 0` **no rebalance happens at all** (`rebalance`
returns early), and a new operation waits for any in-flight deactivation of *its* session
(`waitForDeactivation`). The deactivation itself is guarded by an in-flight map, so two sweeps
cannot race the same victim.

### 7.2 Blocking work that outlives its caller

`runtime/methods/residency.ts` is 30 lines and contains the whole idea:

```go
export function trackResidencyBlockingWork<T>(this, work: Promise<T>): Promise<T> {
  this.residencyBlockingWorkCount += 1;
  return work.finally(() => { this.residencyBlockingWorkCount = Math.max(0, ...) });
}
export function hasResidencyBlockingWork(this): boolean {
  return this.hasActiveOrQueuedTurnWork() || this.hasRunningBackgroundTasks()
      || this.residencyBlockingWorkCount > 0
      || (this.memoryExtractionScheduler?.hasPendingWork() ?? false);
}
```

with the comment naming the bug: "session 常驻池过去只观察 active turn 与 task registry；title、MCP
startup、ledger write 等 detached Promise 不在两者中，可能在仍读写 runtime 时被当作 idle 关闭" and
the discipline "计数必须在 Promise 启动的同一同步片增加，并只在 finally 释放".

So the rule is: **every detached promise that reads or writes the runtime is registered in the same
synchronous tick that starts it.** The pool then has exactly one question to ask.

### 7.3 The rest of the long-lived machinery

- **A runtime command queue with priorities and batches** (`runtime/command-queue.ts`): commands are
  `prompt | target-continuation | target-continuation-loop | task-notification | subagent-message |
  control-only-turn`, ordered `now | next | later`. Background task notifications are **coalesced
  into one model round** ("将同批后台通知合并到一个模型轮，避免每条通知都单独发起请求"), and the drain
  loop asserts the batch is homogeneous rather than guessing.
- **A foreground promotion lease** (`acquireForegroundPromotionLease` / `foregroundPromotionLease`):
  a queued item can be promoted ahead of an in-flight turn, and the dequeue that consumes the lease
  and the lease's consumption happen in **one synchronous step** — with the comment naming the race
  it fixes: "sendQueuedNow 过去在 Stop A 与 promoted command 入队之间没有 Core 调度所有权，notification B
  会抢先出队".
- **Layered cancellation**: `createTurnAbortScope(options.abortSignal)` per turn, checked with
  `throwIfTurnAborted` at every stage boundary (not only at I/O), and a `TurnMachineImpl` that
  records terminal state so a cancelled turn is *typed* rather than inferred.
- **Directory-scoped shutdown with a watchdog** (`packages/cli/src/shutdown.ts`): SIGHUP/SIGINT/
  SIGTERM → abort → `runCliCleanupWithTimeout(cleanup, 2000ms)` → flush coverage → exit with the
  conventional code (130/143/129); plus a separate exit watchdog, and an explicit note that
  detached shell processes form their own process group so the parent must close the app before
  Node exits.
- **Many small correctness guards that read as a bug retrospective**: a 250 ms
  `STREAMING_TOOL_CANCEL_DRAIN_TIMEOUT_MS` on abandoning in-flight streaming tools; a
  `raceWithTimeout` helper; a permission-responder race (§4 of the permission section below) where
  the hook chain and the interactive broker start **concurrently** and the loser is aborted, because
  a synchronously-blocking hook used to leave the confirmation dialog visible-but-dead.

### 7.4 Permission: the two things xdev's model lacks

`core/src/permission/service.ts` + `tool/executor/permission-flow.ts` + `tool/executor/permission-responder-race.ts`
implement an approval path with two properties xdev's `internal/tool/approval.go` does not have:

- **Two responders race, and either can win.** `racePermissionResponders` starts the interactive
  broker *first* (so the dialog is answerable the instant it is visible) and the hook chain in
  parallel; whichever produces a decision first wins and the loser is aborted. A hook chain that
  *fails* forfeits rather than denying: "辅助应答方的基础设施故障不应替用户做拒绝决定". A hook that
  *modifies* the input re-validates and **re-checks the permission decision on the modified input**
  (`recheckPermissionHookModifiedInput`) — because a hook can redirect a path.
- **`askOptions.allowAlways: false | "session"`** (`contracts/src/tools/contract.ts:106-114`): a tool
  whose input is different code every call sets `allowAlways: false` so the persistent project rule
  can never be remembered, and `"session"` swaps the persistent rule for an in-memory, session-scoped
  one — "the gate stays closed for the rest of this runtime only, and every new session (restart,
  cold resume, `/new`) asks again first". Plus `alwaysAsk?: true`, which survives yolo and plan-mode
  pass-through: "the tool's unit of work is large enough that no permissiveness setting should be
  able to run it unattended", and which *overrides allow-granting branches only, never
  deny-granting ones*.

---

## 8. What to take, ranked

### Tier 1 — the peer is materially ahead, and the port is bounded

1. **Capability manifests on tools** (§4.1). One declaration (`readOnly`, `destructive`,
   `concurrentSafe`, `sideEffectScope`, `needsApproval`, `alwaysAsk`, `stopTurnOnSuccess`,
   result budget) read by plan mode, the scheduler, the permission gate, the streaming coordinator
   and the UI. It is the prerequisite for items 2–4 below and for closing T3 #27/#28.
2. **Streaming execution of read-only tools** (§1.4) — the gate is the manifest, the fallback is
   already how xdev behaves, and the payoff is a per-turn latency win on exactly the turn shape
   xdev is used for.
3. **The compaction circuit breaker** (§6.3) — small, self-contained, and it closes a real
   unbounded-cost hole.
4. **The prompt contract paragraphs** (§3.2) — the "text between tool calls may not be shown" and
   "context is summarized, don't wrap up early" paragraphs are a few hundred tokens for a
   behaviour change in exactly xdev's two weakest areas.
5. **Abort-aware stream reads** (§5.3) — race the pending read against the turn's abort signal, and
   escalate the idle timeout with the attempt number.

### Tier 2 — right shape, needs a design pass

6. **Sectioned prompt with declared cache hints** (§3.1) — the mechanism that makes §8's item 4 and
   the prompt-cache work structural instead of manual.
7. **Permission: two racing responders + `allowAlways: false|"session"` + `alwaysAsk`** (§7.4).
8. **Residency facts + a registered detached-work counter** (§7.1–7.2) — xdev is one-session-per-process
   today, so this is not an eviction port; it is the *vocabulary* for "what is this session still
   doing", which is what a future `--bg`/attach mode (#131) needs and what a correct `--resume` on a
   busy session needs today.
9. **Per-profile subagent scoping** (§4.3) — `injectAgentsMd: false` for a read-only explore child,
   `permissionMode`, forced removal of the plan tools, and the "don't write report files" child note.
10. **Microcompact as a pre-request tier** (§6.1) with a provider-visible boundary event.

### Tier 3 — worth knowing, low urgency

11. **One ordering authority for the session stream** (§1.1) — store-assigned sequence numbers, and
    a live sink that only ever sees stored events.
12. **The in-band tool-call decoder** (§5.5) — the response half of #98.
13. **Compaction provenance on the record** (§6.4) — `method`, `provider`, `model`, `usage`,
    `willRetriggerNextTurn`; enriches #116.
14. **Resume as an enumerated reconstruction** (§1.5) — env-info and workspace-identity-for-memory
    first; enriches #158/#220.
15. **The summoner's no-tools preamble** (§6.4) — xdev's `compactionPrompt` is one paragraph and
    does not forbid tool calls.

### Deliberately not taken

- **SQLite as the session store.** xdev's JSONL tree is a compatibility surface (omp interop) and a
  memory-budget decision (PRD §1 goals 1–2); ZCode's SQLite session store is ~2.7 klines of
  migration/codec machinery for a benefit xdev gets from `BuildContext`.
- **The Vercel AI SDK.** PRD's non-goal (no JS runtime) stands; the *seams* (`bindModel` freeze,
  request-scoped retry budget, idle-timeout escalation) are portable without it.
- **The 400-line file ceiling as a hard gate.** It is the reason the peer's turn loop is spread over
  75 files and why this document's citations cluster in `runtime/methods/`. xdev's soft version is
  the right trade for a codebase whose files are read by humans on a terminal.
- **`SessionModePort` as a port.** ZCode injects the mode through a port because the desktop, web and
  CLI all drive one core. xdev has one front end per process mode; `PlanMode` as a struct is fine.

---

## 9. Verification a reader can run

Nothing in this document is an inference from a comment alone, but three claims are worth checking
against the tree before acting on them, because they are the expensive ones:

```sh
cd /path/to/ZCode/apps/zcode-cli

# (a) zero tests — the maturity caveat
find . -name '*.test.ts' -o -name '*.spec.ts' | grep -v node_modules | wc -l   # → 0

# (b) the streaming-tool gate is exactly the six clauses quoted in §1.4
sed -n '339,358p' packages/core/src/runtime/methods/streaming-tool-coordinator.ts

# (c) the residency eligibility predicate is the conjunction quoted in §7.1
grep -n 'private isEligible' -A 14 packages/bootstrap/src/zcode-protocol/session-resident-pool.ts

# (d) the compaction breaker constants
grep -n 'RAPID_REFILL_TOOL_TURN_THRESHOLD\|MAX_CONSECUTIVE_RAPID_REFILLS' \
  packages/core/src/runtime/methods/turn-loop-state.ts

# (e) the plan-mode capability policy (the fail-closed shape)
grep -n 'private checkPlanMode' -A 40 packages/core/src/permission/service.ts
```

---

## 10. Issues filed from this study

| Issue | Proposal | Tier | Evidence |
|---|---|---|---|
| [#420](https://github.com/FreePeak/xdev/issues/420) | Tool capability manifests: one declaration, four consumers | 1 | §4.1 |
| [#421](https://github.com/FreePeak/xdev/issues/421) | Streaming execution of read-only tools during the model stream | 1 | §1.4 |
| [#422](https://github.com/FreePeak/xdev/issues/422) | Compaction rapid-refill + failure circuit breakers | 1 | §6.3, §6.4 |
| [#423](https://github.com/FreePeak/xdev/issues/423) | The prompt contract paragraphs (mid-turn invisibility, compaction, harness facts) | 1 | §3.2 |
| [#424](https://github.com/FreePeak/xdev/issues/424) | Abort-aware stream reads + attempt-escalated idle timeout | 1 | §5.3 |
| [#425](https://github.com/FreePeak/xdev/issues/425) | Sectioned system prompt with declared cache hints | 2 | §3.1 |
| [#426](https://github.com/FreePeak/xdev/issues/426) | Permission: racing hook/broker responders, `allowAlways: false\|"session"`, `alwaysAsk` | 2 | §7.4 |
| [#427](https://github.com/FreePeak/xdev/issues/427) | Plan mode: capability policy, filtered toolset, no reviewer ⇒ the plan is the deliverable | 1–2 | §2 |
| [#428](https://github.com/FreePeak/xdev/issues/428) | Residency facts + a registered detached-work counter | 2 | §7.1–7.2 |
| [#429](https://github.com/FreePeak/xdev/issues/429) | Subagent profile scoping: `injectAgentsMd`, permissionMode, plan-tool removal, child notes | 2 | §4.3 |
| [#430](https://github.com/FreePeak/xdev/issues/430) | Microcompact as a pre-request tier with a boundary event | 2 | §6.1 |

Filing was done on 2026-09-22; the PRD's §3 (architecture) and §5 (key decisions) were updated in the
same change with the four ideas that change a stated design assumption rather than adding a feature
(§0's table plus tiers 1 items 1, 2 and 3).
