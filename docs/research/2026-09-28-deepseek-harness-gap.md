# DeepSeek Harness — gap analysis against xdev (2026-09-28 pass)

**Read:** [`github.com/deepseek-ai/deepseek-harness`](https://github.com/deepseek-ai/deepseek-harness) @ `21638c56` (`master`, 2026-09-27), 13,946 tracked files, ~4.6k `.ts`/`.tsx`.
**Relationship to the earlier pass:** [dsh-internals.md](dsh-internals.md) read the *doc tree* of a 2026-09-11 tree and described the runtime contracts. This pass read the **source** — eight deep passes over session core, agent loop, tool pipeline, subagents, context management on one side and xdev's own Go code on the other — and produced ~3.9k lines of `file:line` evidence. It supersedes the earlier pass's recommendations where the two disagree; the mechanism descriptions there still stand.
**Method:** code is the truth on both sides. Where `docs/PRD.md` claims a capability and the code does not implement it, that is called out rather than repeated.

---

## 0. Verdict

**xdev is not behind on the hard parts.** The session store is byte-exact omp JSONL with a committed-byte boundary, torn-tail truncate-on-append, `ErrConcurrentWriter`, hard-link refusal, latched errors, windowed materialization, a 50k-entry RSS test and an 11-entry interop test against a real omp fixture. dsh's *mechanisms* are better **articulated**, not fundamentally different — and several of its mechanisms are TypeScript ceremony (branded scalars, Zod state schemas, `internal/dispatch` middleware) that a Go binary gets for free or does not need.

The real gap is **three bounds and one vocabulary**, all now landed ([PRD §footer](../PRD.md)):
an unanswered tool call has no answer on rebuild, a tool call has no deadline, and a batch has a size ceiling of infinity.

---

## 1. Session core and lifecycle

| Mechanism | dsh | xdev | Port? |
|---|---|---|---|
| **`ignorable` flag per event** — an unknown type must refuse reconstruction unless the writer declared it informational | `packages/core/session/src/types.ts:501-511` | unknown types are swallowed as opaque `UnknownEntry` (`internal/session/store.go:206`) | **Yes, cheap, deferred** — needs a versioned format first (see below) |
| **Model-visible ⇒ logged, asserted at dispatch** — the outgoing request must deep-equal the log projection | `packages/core/agent-loop/src/invariant.ts:19-57` | persistence is a call-site convention; `a.persist` drops the error (`internal/agent/loop.go:1151-1158`) | Yes, ~80 ln under a build tag |
| **`assistant/attempt`** — every settled non-surface model attempt keeps its stream in the log but never projects | `types.ts:350-355`, `surface.ts:120-131` | no equivalent; only the successful message is persisted | Yes, small |
| **Crash-repair closers** — synthetic `TOOL_NOT_STARTED` / `TOOL_OUTCOME_UNKNOWN` results | `packages/core/session/src/repair.ts:15-208` | `ai.EnsureToolOutput` per message (`internal/ai/types.go:285-298`), blind to orphans | **Landed** in a cheaper form: `session.UnansweredToolCallNotice` at rebuild time |
| **Format version actually dispatched** — refuse `version > current` before shape checks; adjacent-only migration chain | `format.ts:333-346`, `chain.ts:30-32,79-87` | `MarshalHeader` hardcodes `Version: 3`; `ParseHeader` reads it and **nothing branches on it** (`entries.go:702-712` → `store.go:316-323`) | Deferred until a real v2 exists |
| **`flock` write lease**, no expiry, inode re-verified | `lease.ts:70-116` | `ErrConcurrentWriter`, no kernel lease | Optional |
| **`turn/end` typed reason** (`max-tokens` wins over a later `completed`) | `types.ts:201-229` | nine `emit("turn_end")` sites, no reason (`loop.go:562…721`) | Yes, with the state machine |
| **Projection registry** (incremental folds, `stateOf()`, checkpoint+tail replay) | `session-projection.md:60-102` | none | Only if a UI needs it |
| **session-query + fts5** | `session-query/session-query-sqlite/src/schema.ts` | `grep`/`search` scan the file | Not now |

Two corrections to the earlier pass, both from reading the source rather than the docs:
`session-query/src/cold-read.ts` is 57 lines and **does not** avoid loading anything — it is the deliberately naive path; the real savings are live-preferred corpus resolution (`corpus.ts:91-120`) and revision-token caching. And the migration chain's cost is one file per version, not a migration table.

---

## 2. Loop, turn/step lifecycle, thinking flow

dsh's whole turn/step machine is ~2,490 lines of TS, split cleanly into **13 durable session events** and **8 live extension points** (`docs/architecture.md:109` states the rule: a durable event is the only thing a resumed process can trust; a live event is the only thing that can change a decision mid-flight). So the state machine is a *small* port, not a large one.

| Mechanism | dsh | xdev |
|---|---|---|
| Named turn/step machine, `step/end` in a `finally`, one turn exit | `agent-loop/src/agent.ts:296-396` | `Agent.Run` is one flat 310-line function (`loop.go:416-726`); nine `turn_end` sites |
| **Cancellation causes**, copy-the-tag-never-log-an-error | `core/session/src/types.ts:188-194` | five cancel paths, one `ctx.Err()`, **no cause recorded anywhere** |
| Durable-before-wait retry with a provider-delay cap | `llm-retry/src/index.ts:188-190` | `auto_retry_start` fires around the backoff (`loop.go:1053-1071`) but the wait itself is not durable |
| Frozen request + freeze evidence reuse across retries | `agent.ts:669-687` | the request is rebuilt per attempt and `history` is **mutated between attempts** by the recovery ladder |
| Tool scheduling: exclusive barriers, bounded pool, model-ordered commits | `tool-calls.ts:85-100,122-247` | semaphore-bounded, in-order results (`loop.go:1374-1389`) — close enough |
| Thinking captured and model-visible | `llm/src/assembler.ts:135-179` | same, persisted with the message |

**xdev ahead:** stall detection. dsh has **none** — xdev's `internal/agent/watchdog.go` already covers the case the gap analysis flagged as missing.

---

## 3. Tool call pipeline

The five stages (`tool/call` → `tools/pre-execute` → `tools/execute` → `tools/post-execute` → `tool/result`) and their fail-closed rules are worth reading (`docs/tool-execution-pipeline.md`, `packages/core/tools/src/index.ts`). Two findings invert the brief:

- **The "revision" capability does not exist.** Claude Code's `updatedInput` is parsed, warned about, and deliberately *unrepresentable*: tool args are deep-frozen and three readers (history, audit, UI presentation) commit before execution. xdev's `Intercept.ToolCall` returning revised args is a *different, simpler* thing and is fine.
- **The biggest token lever is not in the tools package at all** but in the LLM seam (`deferLoading` deltas). xdev already has this (MCP deferral past 12 tools, `internal/mcpclient/mcp.go:409`).

Worth porting, cheap first:

| # | Mechanism | dsh | xdev | Cost |
|---|---|---|---|---|
| 1 | `concludeTurn()` — a tool that already answered marks its result terminal; the loop stops instead of spending another model turn | `core/tools/src/index.ts:1422-1424` | absent | **trivial** |
| 2 | Repeat-call detector (identical tool+args N times) | `guard/repeat-tool-reminder` | absent (only `isEmptyAssistant`) | low, buys whole turns |
| 3 | Model-free tool-result pruner (head/marker/tail) that can skip the summarizer | `compaction-tool-result-pruner/src/index.ts:83-182` | one compaction rung | low, best ratio in the report |
| 4 | **`ToolTimeout`** | every tool carries a bound | **absent** — a wedged call holds a worker, the turn, the session | **landed** |
| 5 | Read windowing with a continuation footer | `fs/tool-fs/src/read-render.ts` | OutputSink head+tail | trivial |

**Ceremony — do not port:** `run_code`/PTC transport (a product bet, not hygiene), the whole `agent-tool-presentation` package (72 lines wrapping one method), branded scalar types, the `wireSchemas`/`collapses` triple, the Windows ACL sandbox backend (1,280 lines of platform depth), the `spill` token-meter pricing (a byte budget gets 90% of it), and the i18n `.zh.md` doc pairs.

---

## 4. Subagents, team, fan-out cost

xdev has a **real** single-parent fan-out engine: in-process child with a restricted tool set, a `yield` contract with schema enforcement + repair turns + no-yield detection, depth-2 recursion with `spawns:` allowlists, a background hub with steer/cancel/wait/park/revive, a durable cross-session mailbox, and hub-notices (a model cannot poll what it does not know finished — most harnesses lack this). Where it is genuinely thin:

| Gap | dsh | xdev | Cost |
|---|---|---|---|
| **Continuable subagents** — durable child session, at most one process-local activation, `send_message` steering at the nearest step boundary, cold resume with no provider dispatch, a single pushed settlement notice as the parent's whole payment | `continuation*.ts`, ~1.7k LoC of lifecycle | hub keeps the job alive for the session but the child cannot be *resumed* mid-thought | high; do in 3 increments |
| **Team task board** — revision CAS, exclusive claim, acyclic `blockedBy` DAG, write-scope overlap warnings | `agent-team/src/task-board.ts:100-215` | nothing: no claims, leases, dependencies or durable roster | medium (~400 ln) — the one gap worth a follow-up issue |
| Live-child **slot budget** with typed refusal | `subagent/src/index.ts:191-203` | `maxBatchParallel` bounded concurrency, never size | **landed** (`maxBatchItems`) |
| `list_agents(children\|descendants)` + model-visible live transcript | `tool-subagent-control/src/list-agents.ts:85-176` | `Hub.Transcript` is wired TUI-only (`hub.go:342` → `tui/hubroster.go:222`); a parent debugging a stuck child is blind | low |
| `interrupt_agent` (stop the turn, keep the queue) | `tool-subagent-control/src/index.ts:74-111` | `hub cancel` is a whole-run kill | low-medium |

dsh's roster/tombstone/5-limit-config layer is mostly redundant with xdev's hub + persisted mailbox, and dsh has **no stall watchdog** — do not regress xdev's.

---

## 5. Context management

The transferable idea is that **compaction is a durable two-phase log transaction, not a rewrite**: `compaction/start` locks before any await, a model-free pruner runs first and can skip the summarizer entirely, and the compacted view stays a provable projection of the append-only log. Spill is a small, well-designed seam (opaque `SpillLocator` + `retrievalHint`, model recovers via its own `read`/`grep`). The workflow engine's real primitive is a fixed ralph loop (fresh child per round, ≤16 KB validated handoff) — worth taking as a *contract*, not as an engine.

**Where xdev can beat dsh:** dsh measures tokens but has **zero money accounting anywhere** (no per-turn USD, no budget parameter), and it has **no global system-prompt cap** (only per-contributor budgets). xdev already computes `ai.Usage.Cost` and reaches the TUI; the loop itself never reads it.

---

## 6. What landed in `perf/loop-cheap-wins`

1. `session.UnansweredToolCallNotice` — an unanswered call reaches the model as a retry-guiding sentence, not a zero-block assistant shell.
2. `Agent.ToolTimeout` / `DefaultToolTimeout` — a deadline on every call that has none, reported as *unknown*, not *cancelled*.
3. `maxBatchItems` — a batch size ceiling that names both the limit and the fix.

Next three cheap wins, in order: `concludeTurn`, the repeat-call detector, a named turn/step state machine with a `turn_end` reason. Then the team task board as a real issue.
