# DeepSeek Harness (`dsh`) — internals cross-check for xdev

**Researched:** 2026-09-14, second pass (the first pass, 2026-09-09, produced
[tool-inventories/deepseek-harness.md](tool-inventories/deepseek-harness.md) — the tool catalog and
tool-call mechanics. That inventory is still accurate; **this document is the runtime half it never
covered**: session model, persistence, compaction, goals, scheduling, subagents, and the
contract-level invariants worth porting).
**Subject:** [`github.com/deepseek-ai/deepseek-harness`](https://github.com/deepseek-ai/deepseek-harness)
— "DeepSeek Harness: Everything is a Plugin", MIT, TypeScript/pnpm, 55 packages, powered by the
Cordis plugin framework. Repo state at read time: created 2026-08-13, last pushed 2026-09-11,
223.6k stars, `@deepseek-ai/dsh-session` at `0.1.5-rc.2`.
**Method:** the doc tree (127 `docs/**.md` + 54 subsystem pages, 965 KB of prose) fetched verbatim
from `raw.githubusercontent.com` at `master`; the `<!-- BEGIN GENERATED cordis-surface -->` trailer
on every page was treated as boilerplate and stripped. Quotes below are from the hand-written prose
above those markers.
**Maturity caveat:** `README.md` self-labels "developer preview — THERE WILL BE
COMPATIBILITY-BREAKING CHANGES*; `SAFETY.md` states the software is*experimental" and has "not
undergone a security audit", and — the part that matters for a parity study — **specifies no
concrete default approval behavior** for shell/edit. dsh's *contracts* are worth reading; its
*defaults* are unspecified.

---

## 0. What dsh is, in xdev's terms

Same category as xdev (agent-side runtime: loop, tools, sessions, providers, UI). Opposite stack
choices on three axes, which is what makes it a useful counterfactual rather than a clone:

| Axis | dsh | xdev |
|---|---|---|
| Extension model | in-process Cordis fibers ("no privileged core" — even the loop and the LLM adapters are plugins) | subprocess JSONL extensions with per-event timeout + SIGKILL (PRD §1 goal 5) |
| Session identity | linear append-only `SessionEvent` log with a monotonic `seq`, `seq = log.length` | append-only tree with `parentId` + a mutable leaf pointer (omp's model, PRD §3.2) |
| Surface | one log; only four event types project to model messages ("surface" vs "log-only") | one JSONL file; entries are all model-visible except markers (`reset_boundary`, `compaction`) |

xdev's tree gives it fork/branch/rewind for free (§3.2), which dsh does not have; dsh's explicit
surface/log-only split gives it cheaper compaction bookkeeping, which xdev does **not** have. Both
peers (dsh and fx, §5.2) independently converge on the same core loop shape: log-first, derive the
request from the log, bounded everything.

## 1. The session log (docs/subsystems/session.md, core.md)

1. **Model-visible means logged.** "The session log is the context source of truth: every
   model-visible input must be a durable `SessionEvent`" (`docs/agent-lifecycle.md`). Streaming
   frames (`agent/assistant-stream`) are explicitly **transient** — they carry incrementality to the
   UI and never enter the log; the settled `assistant/message` embeds "the exact compact timed
   stream".
2. **A failed attempt still settles.** `assistant/attempt` records "one model attempt that committed
   no surface message. The embedded stream preserves a failed, retried, cancelled, or stream-error
   attempt that reached settlement **without fabricating model-visible history**." xdev: PARTIAL —
   a failed turn persists nothing, and the store synthesizes an aborted assistant message only on
   resume (PRD §3.9 item 3). The forensic difference: dsh can answer "what did the model actually
   say before it died" from the log; xdev can only say that a turn was interrupted here.
3. **`ignorable?: true` — forward compatibility with a spine.** Every event carries an explicit
   skip marker; absent means required: "a reader meeting an unrecognized type without this marker
   MUST refuse to reconstruct the session instead of silently dropping the event, because an
   unrecognized required event may change how the rest of the log is interpreted. … defaulting to
   required means a forgotten marker over-refuses (an inconvenience) rather than silently resuming a
   gutted session." xdev: PARTIAL — `internal/session/entries.go:326` returns
   `ErrUnknownEntryType` and the store keeps *chain connectivity* for unknown types
   (`entries.go:389` `ParseEnvelope`), so xdev tolerates every unknown entry with no way for a
   writer to say *this one actually matters*. One optional field in the entry envelope closes the
   gap in both directions (**#265**).
4. **Turn end is a typed enum with a tie-break rule.** `TurnEndReasonMap`: `completed`,
   `aborted{reason}`, `blocked`, `error{error}`, `max-tokens`, `interrupted` — and the rule worth
   stealing verbatim: "any max-tokens step in a turn makes the whole turn end max-tokens rather
   than completed (**the cut-short fact wins** over a later continuation)". `interrupted` is
   synthesized *only* by crash repair, never by the loop. xdev: PARTIAL — the four
   `ai.StopReason` values (`internal/ai/types.go:26-29`) are per-message; nothing rolls the turn's
   worst step up, and `internal/tool/bash.go:93-99` folds a signal death into `exitCode` carrying a
   signal number instead of an orthogonal field (§7 below).
5. **The request envelope is logged state.** `request/header` records `EpochHeader{config,
   adapterDefaults?, tools?}` so "every conversation request is a pure function of the log"; the
   system prompt is *not* in the header — it is derived history (`system/message` at surface node 0).
   Acceptance is strict: "any `system` field is forbidden, and `tools: []` and `adapterDefaults: {}`
   must be omitted. … Seed, append, and current persistence reads **reject noncanonical headers
   rather than silently normalizing them**". A second event, `request/context`, carries
   `{provider, model, contextWindow?, systemPromptUpdate?}` and is written only when route/capacity
   changes. xdev: ABSENT — the header lives in process state; the state-carry half of **#220**'s
   census is exactly this record.
6. **`session/end-seed` + `inheritedEventCount`.** A fork's boundary is a durable marker in the log
   itself ("the last tagged marker is the current Session's cut"), paired with a header field
   carrying the exact inherited prefix length — "storage metadata paired with the header for every
   body read, never part of the replayable event log". xdev: PARTIAL — `fork.go` copies the header
   and records `parentSession`, but the child's log carries no cut marker, so the boundary is
   inferred from file length, which a torn tail can move (**enriches #122**).

## 2. Persistence (docs/subsystems/persistence.md)

1. **A handle is the door.** Every read and write flows through a `SessionHandle` — the single door
   the cross-process write lease guards. `read(offset, length)` never backtracks below what the
   handle already observed; a `read` handle that is written to is a **runtime error rather than a
   typed split**; `close()` is idempotent, asynchronous, deliberately not cancellable. Freshness is
   specified across handles: once an append or flush resolves, every read *started afterwards* on the
   same backend instance observes at least that prefix; concurrent reads promise nothing beyond the
   valid contiguous prefix. xdev: PARTIAL — `Store` is process-local with a `sync.Mutex`; the second
   concurrent `xdev` on one session file has only the byte-count guard **#122** added, no lease, no
   `SessionAlreadyOwnedError`.
2. **`flush()` is the only durability barrier** and `append()` is explicitly best-effort; a
   created-but-empty session becomes listable *at flush*, and "a backend may defer physical
   materialization (a pure optimization) until the first append or flush". xdev already defers
   ordinary new sessions to disk (`ensureOnDisk`); the delta is *listability* — a session with no
   flush is invisible to `--resume`, which is exactly what a crashed-first-turn session wants.
3. **Crash recovery preserves an interrupted turn; it does not truncate or repair it** — repair
   appends `interruptedTurnClosers` (missing tool-result errors, an open `step/end`, a synthetic
   `turn/end{reason: interrupted}`) — and repair writes only under write ownership (a read handle
   answers `SessionReadOnlyError`, and a lost owner answers `SessionOwnershipLostError`).
   xdev: opposite at the tail (**#122** discards an unfinished trailing turn — the right call for a
   *partially written* batch; wrong for a *fully written* turn whose process died mid-tool) and has
   no ownership concept at all. The two rules compose: cut only torn bytes, close but never rewrite
   complete turns.
4. **Format refusal names the file.** `SessionFormatUnsupportedError` carries the raw log path;
   a too-new format is a named refusal, never a silent open. xdev: PARTIAL — `Version: 3` on the
   header; no named-refusal message pointing at the repair — filed with the other
   forward-compat contracts as **#265** (required-vs-ignorable + logged envelope).
5. **A revision token keys cold caches.** `stat()` returns a `SessionPersistenceRevision` that
   tells a listing cache whether the file changed — an opaque token owned by the backend rather than
   a guess from filesystem timestamps. xdev: **HAS the useful half** — the listing cache key is
   `path + size + modUnix + modNanos` (`internal/session/listing.go:164`), which is collision-free on
   local disks; the delta is only that a portable token survives a copy that rewrites mtimes (a
   session moved between machines, or restored from a backup), where the stat key silently reuses
   stale metadata. Small, and #159/#223's move/restore paths are where it would actually bite.

## 3. Compaction (docs/subsystems/compaction.md, config-catalog)

1. **Compaction is a bracketed transaction recorded in the log**:
   `compaction/start{turn}` → summarize → `compaction/summary{…}` → the replacement `user/message` →
   `compaction/end{turn, error?}`, and "releasing the lock **last** turns a crash mid-operation into
   a detectable **orphaned lock** (a `compaction/start` with no matching `compaction/end`) rather
   than an `end` that falsely claims compaction finished. A live unmatched start blocks every entry
   point." xdev: ABSENT — `CompactionEntry{summary, firstKeptEntryId, tokensBefore, method}` is one
   record written after the fact, so a crash *during* summarization is indistinguishable from one
   *before* it. Filed as a new issue 2026-09-14.
2. **The summary record carries its own provenance and shadow bookkeeping:**
   `{summary, rawOutput?, llmStreamCall?, shadowedRange{start,end}, shadowedSeqs[], shadowedTokenCount,
   provider, model, maxTokens?, usage?}` — and `shadowedRange` is a **surface-position** span, not a
   numeric interval, "after a prior replace lands a fresh high-seq summary node at an older range's
   position, `start` can be GREATER than `end`; `shadowedSeqs` is the authoritative set". xdev:
   PARTIAL — `firstKeptEntryId` is a position, not a set; there is no provider/model/usage on the
   compaction record, so `xdev stats` cannot attribute what summarization cost (**enriches #116**).
3. **Triggers are a two-value enum, not a timestamp:** `CompactionTrigger = 'pressure' |
   'context-overflow'` — "implementations may treat confirmed overflow more aggressively than
   ordinary pressure" — plus `compactNow()` — "one useful idle-session reduction even below pressure" —
   which returns `null` without writing when no useful range exists. xdev: PARTIAL —
   `compactTriggers = [threshold, overflow, promotion]` (`internal/agent/compact_ladder.go:36`) is
   the same split; the missing member is the manual/idle `compactNow` *verb* — `xdev compress` is an
   offline file tool; there is no in-session *compact now and tell me what it saved* (`/compact`).
4. **Typed failure codes for a manual attempt:** `ManualCompactionErrorCode = 'busy' | 'cancelled' |
   'changed' | 'summary' | 'commit' | 'persistence'`, with the rule that "`changed` and `summary`
   leave the conversation surface unchanged but still close and persist the failed attempt" — a
   failed compaction is a logged fact, never a silent no-op.
5. **The shipped backend's numbers** (`@deepseek-ai/dsh-compaction-basic`, `docs/config-catalog.md`):
   `thresholdRatio 0.8`, `retainRatio 0.16` (mutually exclusive with `retainTokens`), `maxTokens 8192`,
   `compactionRetries 1`, `maxOverflowRetries 1`, `auto: true`. The pruner
   (`@deepseek-ai/dsh-compaction-tool-result-pruner`) defaults `thresholdChars 8192 / headChars 4096 /
   tailChars 1024` and runs *before* summarization, with **no model call** — the microcompact-before-
   compaction ordering §5.1 adopted from Claude Code, with numbers on it (**enriches #116/#83**).
6. **The token meter is a first-class object** (`docs/subsystems/token-meter.md`): one detached
   snapshot `{logRevision, baseline, surfaceDeltaTokens, totalTokens, surfaceTokens, nodes[]}`, where
   each node carries *both* a route-priced `tokens` and a route-independent `heuristicTokens` —
   "shadow prices replace content with the heuristic value so the O(1) projection fold stays in
   agreement with its own appends". `logRevision` = "the number of durable events consumed for every
   field in the measurement", i.e. the estimate states its own staleness. xdev: ABSENT —
   `estimateTokens`/`contextTokens` (`internal/agent/compact.go:138-170`) is a chars/4 heuristic
   plus last-reported usage, with no revision and no per-node pricing. Full meter = L; the portable
   half is **#116**'s *measure the serialized body, report the revision the number was computed at*.

## 4. Goals (docs/subsystems/goal.md)

dsh's goal state is a **compare-and-set record with a durable phase and a process-local
activation** — a split xdev does not have:

- `GoalRef{id, revision}`; "every accepted durable mutation increments the revision". `update_goal`
  is CAS on that pair — an agent and a human editing one goal contend instead of clobbering.
- Durable `GoalPhase = active | paused | blocked | complete`. `blocked` is "the single durable
  stopped-by-a-problem state", and its reason is `{code: stable lower-kebab-case, message}` —
  machine-routable *and* human-readable. Blocking also requires an **admitted-round minimum
  (default 3)**: a goal cannot give up before three rounds were spent on it.
- `activation` (whether a continuation consumer may start another round) is **process-local and
  never persisted**, published through its own `GoalActivationChanged` edge. Persistence answers
  "what happened to the objective"; the live process answers "may I run"; conflating them is how a
  resumed session starts running rounds nobody asked for.
- Continuation is attributed: each admitted turn carries `{kind: 'goal', goalId, revision, round}`,
  and "replay rejects non-positive rounds, gaps, stale revisions, stopped phases, and cap overflow".
- Mutations need direct-human authority for some operations (a model cannot complete a goal the
  human owns).

xdev: PARTIAL — `internal/agent/goal.go` has a mutex-guarded record (active/completed/dropped/
budget_exhausted + token budget), no `paused`/`blocked` phase, no reason, no revision (the tool is
the only writer, so CAS is moot *today* — the loop's new continuation from **#244** is a second
writer the moment it lands, and that is the moment revision matters), and `roundsStarted` is
implicit. **#244 is the right home**: its stated residual (*no `/goal pause`, no budget flush*) is
exactly this vocabulary. Commented 2026-09-14.

## 5. Scheduling (docs/subsystems/schedule.md)

Three selectors and four rules, all cheap to copy: `after_seconds` | absolute `at` |
`every_seconds ≥ 300`, "exactly one selector"; every first target canonicalizes to four-digit-year
RFC 3339 UTC, and the *local* input form is a structured `{date, time, time_zone}` — offset-free
strings, non-future targets, and DST-gap times are **rejected**, an overlap takes the earlier
instant ("replay never depends on ambient time-zone state"); a recurring record that was due many
times contributes **only its latest due occurrence** and advances past the missed ones "without
enumerating, persisting, or replaying missed intervals"; and the five-minute floor exists by name:
"Batching bounds model turns; the five-minute minimum bounds each record's timer frequency."
Delivery is a follow-up batch, so many due reminders cost one turn. **Enriches #162** — that
issue's `cron + jitter + expiry` is now: three selectors, no cron grammar, UTC canonical,
latest-due collapse, ≥300 s floor.

## 6. Subagents (docs/subsystems/subagent.md, agent-team.md, jobs.md)

- Continuable background children are steered with `send_message`, whose contract is
  `admitted | refused{reason}` — every send gets an answer, never a silent drop — and `interrupt_agent`
  cancels **the turn, not the child**. xdev: PARTIAL — the hub has `send`/`kill`, but `send` has no
  typed admission verdict.
- **Delegation cannot widen authority.** Agent Teams states it as code-not-comment: a member's tools
  are *the parent's minus the team controls*, and the invariant "delegation cannot grant a child
  broader access than its parent" holds by construction. xdev: **HAS** the tool half
  (`internal/agent/task.go:372 resolveAgentTools` intersects declared ∩ parent ∩ allowlist and
  drops `task` at the depth cap; `task.go:229-233` enforces the depth limit with an explanatory
  refusal) — the invariant already holds on the tool axis; only the *approval* axis has no rule.
  Not filed; recorded here so the next sweep does not re-discover it.
- **Jobs** (`docs/subsystems/jobs.md`): id `<kind>-N` (access control by owner authorization, "not id
  secrecy"); `JobStatus = running|stopping|completed|killed|failed`; `readOutput()` is a **consuming
  cursor** — "consume output produced since the previous call", one cursor per job;
  `JobOutcome.detail` is "kind-specific detail rendered into status lines"; and the `reported` flag
  suppresses a duplicate completion notice once anyone has "delivered or committed to deliver" the
  terminal state — including the teardown cancel, "because the owner being destroyed leaves no
  reader: a reporter that opens a turn on notice would otherwise spend a model request per teardown
  layer". That last sentence is the argument for **#127's** job-completion feed and **#243**'s
  resume-side replay; both now have the dedupe key they need. Commented 2026-09-14.

## 7. Execution: bash, subprocess, sandbox (shell.md, subprocess.md, sandbox.md)

- **Orthogonal outcomes, reported independently.** "`timedOut`, `aborted`, `signal`, and `exitCode`
  are each their own field; a caller never reads a cut-short run as a clean success", with a fused
  deadline making `timedOut` and `aborted` mutually exclusive ("the FIRST cause to cut the command
  short"). xdev: PARTIAL — `internal/tool/bash.go:93-99` reports `exitCode` (signal number smuggled
  in when `killed`), `durationMs`, `truncated`, `backgrounded`; the *reason* a run ended early is
  prose in `Text`, not a field. Filed as a new issue 2026-09-14.
- **Request vs resolved spec.** The model-facing request has optional `workdir`/`timeoutMs`; the
  tool calls `ctx.shell.resolve(request)` to get a fully-resolved `ShellExecSpec`; trusted
  in-process-only knobs (`stdin`, `env`, `stdoutMaxBytes`) are "not exposed by `dsh-tool-bash`" —
  hooks write a JSON payload to a command's stdin through the seam, "a model that needs stdin uses
  shell syntax like a heredoc or a pipe". xdev: PARTIAL — bash has per-stream sinks
  (`StreamHeadLimit/TailLimit = 16 KiB`, `bash.go:25-26`), but every in-process consumer (hooks,
  ext, eval) re-implements subprocess plumbing because `runShell` is private and the model-facing
  32 KiB window is the only budget. Filed with the bash issue above.
- **`DSH_*` managed env namespace.** Ambient `XDEV_*`-equivalent names are deleted before the
  harness re-publishes its *current* facts, "so an unavailable current fact cannot inherit a stale
  value from the harness process", and caller `env` entries merge *before* the managed map, so a
  caller can never displace a managed fact. **Enriches #186** (identity vars + env-file preamble).
- **Sandbox enforcement is a reported fact:** `SandboxEnforcement = 'full' | 'partial'`, and
  "`partial` means an active backend or older kernel ABI cannot govern every promised file effect;
  callers requiring an absolute boundary must not treat it as `full`". Plus the denial-dialect
  lesson (postmortem 0004): each backend produces *different* stderr for a denial (EROFS/Landlock
  EACCES/Seatbelt EPERM), so a consumer matches **the selected backend's dialect**, never the union
  — "the union claims denials a given backend never produces" — and requires a fatal signature in
  one line *plus* any exit-code gate before declaring runner failure, because "exit status alone
  never proves runner failure". xdev: ships documented recipes, not an engine (PRD §1), but
  `docs/reference/container-and-sandbox.md`'s verification checklist should say exactly which
  effects are proven vs assumed on each OS. **Enriches #115's sibling docs; PRD row.**

## 8. Tool pipeline (docs/subsystems/tools.md — read in full by the principal)

Contract-shaped highlights, verified against xdev:

- `isConcurrencySafe(args)` is a **pure synchronous opt-in classifier**: "Only `true` opts in;
  omission, exceptions, non-`true` returns … are exclusive. This metadata is never model-visible."
  xdev: ABSENT — `runTools` (`internal/agent/loop.go:862+`) runs *every* batch concurrently on a
  4-worker pool. Two `edit` calls on one file from one assistant message race today; the loser's
  read-snapshot fails with a confusing error. Filed as a new issue 2026-09-14.
- `timeoutMs` on a definition is "NEVER sent to the model — `schemas()` whitelists only
  name/description/parameters", and declaring it asserts the implementation *can reach quiescence*.
  xdev: PARTIAL — per-call `timeout` args are model-visible (correct for bash); no per-tool
  host-side deadline table for MCP/ext tools (**#149**'s cascade is the same gap from the other end).
- `additionalContexts` are injected "as a user message **after** the recorded tool results …
  (call/result adjacency preserved)" — a context note never separates a call from its result. xdev
  has `aside` steering; the injection-point guarantee is not stated in §3.4 and should be
  (**PRD-only**, and it is the invariant **#126** leaned on — "a cancelled turn stops even when the
  tool does not" is the same adjacency contract).
- `finalizeContent` is *the last content-only invariant*: a tool's pure transform of its own result
  runs exactly once for *every* normalized outcome, **including pipeline failures that bypass the
  post-execute waterfall**, and "must be total and must not throw". xdev: ABSENT; the sink marker is
  produced inside `Result.Text` at the call site, so each tool re-derives its own truncation notice.
- Presentation is pure and replayable: `presentCall(args)` / `presentResult(args, result)` "may be
  called during live streaming AND a session-log replay, so it must depend only on args" (and the
  result). The TUI is a fold over the log, not a consumer of live events. xdev: PARTIAL — the TUI
  does replay from the store, but the render path also has live-only state (elapsed, running tint),
  and that mixture is the root of the `resume-renders-text-only` class fixed in 108c63e (§4 notes).
  **PRD row** — this is the invariant `docs/reference/session-format.md` should state.

## 9. Configuration (docs/subsystems/settings.md)

One rule worth stealing whole: a section registers with a `validate` function, and validation
failure *rejects the write that produced it* — "a stored section that fails validation keeps its
last good value; at registration a rejected validation rejects the registration itself." Combined
with `update({})` = *remove my overrides, revert to defaults, emit one reset event*, that gives
`xdev doctor` (the unshipped **#121**) a repair verb that is *defined* rather than invented per key.
Also: layers are named **schema-defaults / base / user**, `watch` delivers the resolved value (never
a raw layer), and the API is one `Settings` facade for both plugin-scoped and global state, "so the
plugin never enumerates files or resolves precedence". Commented onto **#121** 2026-09-14.

## 10. Credentials (docs/subsystems/credentials.md)

- "Consumers **re-resolve at each operation and never cache across operations** — that per-operation
  read is the hot-update mechanism." xdev: PARTIAL — `internal/config/credentials.go` has the
  source chain; caching lives at the provider-construction layer, so a rotated key needs a session
  restart. Matches what **#123** shipped (verify-at-read).
- The surface worth copying is the **read-only-source signal**: `CredentialInfo{configured,
  source?, writable}` where a live process-env value reports `writable: false` *because* "a write
  would appear to succeed while resolution kept returning the shadowing value." xdev's `/login`
  today can write a key that env keeps shadowing — a lie by silence. Small, honest fix:
  **enriches #121** (provenance) or the auth-surface tail of **#130**.
- `authorization.json` is an *independent* grant layer (*an empty allow-list denies by default*,
  separate files *so the two never overwrite each other*) — the right shape for #117's reviewer and
  #129's write-back; **PRD-only**.

## 11. Programmatic tool calling (`run_code`) and the code-runtime seam

`dsh-tools` ships `run_code`: a model-written async TypeScript body that calls other tools through
host bindings (`await tools.name(args)`); the *only* host imports are those bindings, no
fs/network/timers; every sub-call is serialized through the **same** guarded pipeline as a direct
call (`tool/ptc-dispatch` events), and only printed/returned values become output. The design
payoff, from the docs' own framing: a fan-out of N tool calls costs one assistant message of
arguments plus the printed result, instead of N model-call round trips.

xdev: the kernel seam already exists — `internal/eval` runs one persistent CPython subprocess per
session speaking NDJSON on stdio, with auto-backgrounding (`DefaultBackgroundAfter = 30 s`,
`internal/eval/tool.go:14`) and state that survives between cells. What is missing is the binding:
the kernel has no route back to the registry. A Go port needs no new runtime and no JS — `run()`
in `Kernel.startLocked` already wires two output sinks plus a stdin/stdout JSON protocol; adding a
`tool/call` request frame the loop answers through the *same* `runOneTool` path (plan-mode gate →
approval → interceptor → hooks) is the whole feature, plus a per-cell dispatch cap and a
depth-1 guard so a bridged call cannot re-enter the bridge (the exact invariant
`internal/tool/catalog.go:22-27` already enforces for `tool_call`). **Filed as a new issue
2026-09-14.** OpenCode ships the same capability from the other direction (`packages/codemode` +
the `execute` tool over its MCP catalog), so this is now a three-harness consensus item.

## 12. Cross-session query (docs/subsystems/session-query.md, tool-catalog)

Five read-only opt-in tools: `session_search` (one strongest match per session),
`session_event_search` (seq/time/type/surface filters), `session_event_read` (one event + neighbor
summaries), `session_event_trace`, `session_trace` (lineage). All authorize "from the immutable
calling agent session*. The filter algebra is two rules — filters AND, values OR: a
`type`-selection of `[tool/call, tool/result]` never also admits `user/message`. Surface
classification is typed as exactly `'current | shadowed | log-only'` — a query can ask for what a
live model would see *today* vs what compaction shadowed — and `current` is computed by the same
`foldSurface()` transitions that derive model history. xdev: ABSENT (the model has no session
search; `xdev search` is human CLI); filed as a new issue 2026-09-14 with `current | shadowed |
log-only` as the honest vocabulary **#159/#26**'s picker needs.

## 13. Spill storage (docs/subsystems/spill.md) — the smallest honest version of #115

`ctx.spillStore` is a one-method seam: `saveText{owner.sessionId, source, suggestedName, content} →
{locator, bytes, retrievalHint}`. Four decisions in it are portable as-is and they close #115's
open design questions:

1. **`retrievalHint` travels with the handle** — consumers "render it with `retrievalHint` instead of
   assuming `read` is always the right retrieval mechanism". That is exactly what stops a spill file
   from becoming a second, undocumented tool surface.
2. **`suggestedName` is a hint, never a path**; the local backend sanitizes it to one segment and
   writes `<root>/session-<sha256(sessionId)>/<random>-<safeName>` under 0700 with an **exclusive
   `wx` 0600 create** — "so a planted symlink cannot redirect it" (the same rule as
   `#122`'s write-side discipline and `defensive-patterns.md`'s "predictable world-readable paths
   invite symlink races").
3. **`source` is "descriptive provenance only, never access control"** — the caller who *reads* the
   bytes authorizes, not whoever wrote them. This is the answer to #115's open question about who
   may re-read a retained result.
4. **The seam owns storage only:** `saveText` "persists the FULL content and REJECTS on a real
   storage failure"; **retention and cleanup are explicitly not this seam's policy.** The invariant
   to copy into xdev's sink: a run whose retention failed must not be reported as if the bytes were
   recoverable — today `internal/tool/sink.go:30`'s marker claims `showing first %d and last %d bytes`,
   which stays *true*; it must never become `full output at <path>` and be wrong.

xdev: PARTIAL (= **#115**'s own thesis). Commented onto **#115** with the four contracts.

## 14. Prompt assembly (docs/subsystems/system-prompt.md, README)

Sections are `(order, name)`-keyed with **duplicate registration throwing** (a name collision is a
startup failure, not a silent shadow). Two named slots pin the cache boundary —
`deployment:persona-prefix` (first) and `deployment:persona-suffix` (last) — which is xdev's
static/dynamic split (**#217**) already solved and named.
`PromptContext` is the shape worth copying: dynamic, model-visible material **persisted as durable
user-role messages rather than as prompt edits** ("the assembly resolves and orders these
contributions, while the agent loop logs their complete current snapshot … only when it changed or
compaction removed it"). That is cache-safe by construction, and it is xdev's
`#137`/`#216`/`#138` problem solved in one shape: rule/instruction injection as durable messages,
re-logged only on change, means prompt-cache churn and post-compaction amnesia are the same solved
problem instead of two.

## 15. What dsh does NOT have (and xdev keeps ahead on)

- No RSS/memory *process* budget (same shape as fx §5.2); dsh bounds per structure.
- No fork/branch/rewind: a linear log has no `parentId`, no leaf pointer — xdev's tree is the
  stronger model for `/tree`, `/branch`, `/fork`, checkpoint/rewind (**#109**).
- No prompt-injection scanning, no TTSR stream rules, no memory backend seam, no skills budget, no
  hooks subprocess protocol — all in xdev's §2 scope already.
- No terminal tape/replay tier, no CI budget gates (fx has both; xdev tracks them as #120/#118).
- No local tiny-model seam (#70).

## 16. Reject list (with the reason, so the next sweep does not re-raise)

- **The Cordis plugin architecture itself** — in-process fibers with a shared mutable `ctx`. PRD §1
  goal 5: extensions are processes; a stray throw in an in-process plugin kills the session, which
  is the failure mode the subprocess protocol exists to make killable.
- **typert / RemoteProxy** (generated RPC proxies over a browser↔host transport) — xdev's RPC and
  ACP layers already cover the embedder case; a generated-client framework is the web-GUI problem,
  which is out of scope (§1).
- **Web/desktop apps, slots, sidebar, client resource model** — §2 out of scope (TUI + RPC).
- **`schedule` delivery *only while a live Agent owns the Session*** — fine as dsh's rule; xdev has
  no always-on host, so the same floor is "#162 is session-scoped until a supervisor exists (#223
  shape)".
- **OTel session telemetry** — §5.3 already rejected OTel export; dsh shipping `session-telemetry`
  and `session-telemetry-otel` is a second peer's opinion, not a change of verdict. xdev makes zero
  non-essential network calls (§1).
- **`cordis_*` self-extending tools** (the model registers new model-facing tools at runtime behind
  an approval gate) — the *capability* xdev wants; the *mechanism* is in-process plugin loading. The
  subprocess equivalent is #53/#152 (installed plugins contribute tools), not a runtime
  self-extension path.
- **`dsh-ralph`** (fresh-context agent rounds over a shared workspace) — the same shape as xdev's
  vibe/worker mode (#58); a duplicate engine, not a missing feature.
- **Webhook → auto-spawned sessions** — fire-and-forget inbound prompt injection into fresh agent
  sessions is exactly what §1's *injected content is hostile until scanned* rule is for; without an
  inbound-content scanner this is a remote-control surface, not a feature.

## 17. Net verdict

dsh **confirms** xdev's architecture from the third direction (after Claude Code §5.1/§5.3 and fx
§5.2): log-first session, derived context, bounded queues, four message-producing surface types,
hand-rolled provider adapters, approvals as fail-closed waterfalls, and "no built-in OS sandbox
promise" (dsh ships one and *still* documents `enforcement: partial` as a live state). What it adds
is **contracts with numbers and state machines**, in five places xdev today leaves implicit: the
compaction bracket (start/summary/end, release-last), the `ignorable` required-vs-skippable split,
turn-end `max-tokens`-wins, goal CAS revision + persisted `roundsStarted` + a `blocked` phase with a
machine-routable code and a round minimum, and the job `reported`-dedupe key that #127/#243 need.
The single biggest capability gap on xdev's side is **programmatic tool calling** (#11 above): both
remaining peers now ship it (dsh `run_code`, opencode `execute`/`codemode`), and xdev's eval kernel
is one NDJSON frame type away from having it too.

*Research artifact, not a specification: every claim above names a dsh doc; every "xdev:" is a
2026-09-14 read of this tree.*
