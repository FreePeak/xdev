# Long-running sessions: how xdev keeps a big context alive, and how the six peers do it

> **Why this exists.** "How does xdev handle a long-running, big-token session?"
> is a question that has been answered three times in this repo's history —
> as PRD prose, as a compaction-ladder port spec, and as a cross-harness
> comparison — and each answer disagreed with the last on one point. This
> document is the one place that answer lives, with every claim carrying a
> `file:line`. Read it before touching `internal/agent/compact*.go`, before
> designing a compaction change, and before re-deriving any of the numbers.
>
> **Sources and their age.** xdev claims are read from this tree at the commit
> you are reading (verified `@ 4d590df`, 2026-10-03). Peer claims come from the
> research corpus in `docs/research/`; each is dated in its own §3 subsection, and
> §4 says plainly where a peer source has since drifted from its file here.
> Where the two disagree, this document wins and the older doc is the bug.

---

## 1. The one-paragraph answer

`maybeCompact` runs at **every step boundary** (`internal/agent/loop.go:642`).
It fires on whichever of five triggers comes first — token threshold, live heap
pressure, session idle, a provider overflow error, or a finished background
summarize — then finds a cut point that keeps the last 20 000 tokens intact,
runs a **ladder** of summarization methods and keeps the first that succeeds,
and appends **one** `CompactionEntry` to the session file. The raw history is
never rewritten: compaction changes what `BuildContext` *emits*, not what is on
disk, and entries older than the last boundary are never even loaded into
memory. That is the whole mechanism. Everything below is detail.

---

## 2. xdev, step by step

### 2.1 Triggers

`compactionDue` (`internal/agent/compact.go:391-412`) returns true on:

| Trigger | Condition | Source |
|---|---|---|
| **Token threshold** | `contextTokens(history) > ContextWindow − reserve` | `compact.go:411-412` |
| **Memory pressure** | live heap ≥ 85 % of the process `debug.SetMemoryLimit` ceiling | `compact.go:394-397`, `memlimit.HighPressure` |
| **Idle** | gap between the last two step boundaries ≥ `compaction.idleAfter` | `compact.go:420-433` |
| **Async apply** | a background summarize finished since the last boundary | `compact.go:360-362` |
| **Overflow** | a provider returned `ai.ClassContextOverflow` | handled in `loop.go:1179-1197`, not by `maybeCompact` |

**Threshold arithmetic** (`compact.go:105-124`):

```
reserve   = max(16384, 20 % of ContextWindow)     // DefaultReserveTokens, 20 % floor
threshold = ContextWindow − reserve
```

So a 200 000-token model compacts at ~160 000. The floor exists so the 80 %
threshold always fires *before* the window overflows and forces an
error-path recovery.

**The trigger input is honest** (`contextTokens`, `compact.go:164-174`):
`max(provider-reported usage, chars/4 estimate)`. A compression extension can
deflate reported usage — this is omp's `compactionContextTokens` rule — so the
estimate floor keeps the trigger from being silenced by a lying provider.
The estimate (`estimateTokens`, `compact.go:143-154`) is chars/4 over all
block text plus 8 per message, and it counts thinking blocks.

**Memory pressure overrides everything.** `memPressure() >= 0.85` returns true
before the threshold is even consulted, because the point of the backstop is
to act while there is still room to summarize.

### 2.2 Cut point

`findCutPoint` (`compact.go:187-209`) walks the history **backwards**
accumulating the estimate until `keepRecent` (20 000 tokens, `DefaultKeepRecentTokens`)
is covered. Three guards:

- never cuts below index 1 — something must be summarized;
- advances past `RoleToolResult` so a tool call is never split from its result;
- returns `-1` (nothing droppable) when the whole history fits the kept tail.

### 2.3 The ladder

`runCompactLadder` (`internal/agent/compact_ladder.go:126-155`) tries each
product member in `compaction.methodOrder` and returns the entry the **first
successful** member produced. A failing member is logged and the ladder falls
through; only when every member fails does the caller see an error, and then
compaction degrades to the pre-compaction behaviour rather than persisting a
half-result.

Triggers and products share one vocabulary (`config.CompactionMethodNames`):

- **Triggers** — decide *when*, produce nothing: `threshold`, `overflow`,
  `promotion` (`compact_ladder.go:36`).
- **Products** — decide *what the retained context is*:

| Member | Cost | What it does |
|---|---|---|
| `handoff` | one provider round-trip | LLM summarize; the shipped default, capped at `MaxSummaryTokens` = 16 384 (`compact.go:29`) |
| `remote` | provider-side | provider-native compaction; **unimplemented** in xdev → `errMethodUnavailable` (`compact_ladder.go:180-187`) |
| `snapcompact` | zero tokens | the discarded span rendered to a deterministic 5×7 bitmap PNG a vision model can read back (`compact_snap.go:17-30`) |
| `shake` | zero tokens | mechanical elision: argument bodies and thinking dropped, results keep a head, repeated consecutive lines collapsed; 600 chars per result, 60 000 total (`compact_elide.go:17-42`) |
| `soft` | zero tokens | structural prune: `supersedeReads` (a read a later read of the same path went further into) + `dropUseless` (`compact_elide.go:44-71`) |

Three properties worth knowing before you change this:

- **The default `methodOrder` is triggers only** (`threshold,overflow,promotion`,
  `config/settings.go:41`), so `products()` falls back to `[handoff]`
  (`compact_ladder.go:99-113`). An unset config behaves exactly as it did before
  the ladder tails existed.
- **Naming a product in `methodOrder` also makes the threshold trigger fire**
  (`compact.go:395-402`): naming a method is a request to use it at the
  boundary. Only an order of pure triggers keeps the historical rule.
- **`RegisterCompactionMethod`** (`compact_ladder.go:71-87`) lets a companion
  file join the ladder without editing it. The registry is read on every
  compaction; an unknown name or nil func is refused loudly at registration,
  not silently at compaction time.

### 2.4 Async and idle

`compaction.async` hands the provider round-trip to a goroutine
(`compact_async.go`): one job at a time, output clamped by `MaxSummaryTokens`,
input clamped by the same transcript caps as the synchronous path, cancelable
via the ctx of the boundary that started it. It is only offered when the
ladder's first product is `handoff` (`firstProductIsHandoff`,
`compact_ladder.go:116-124`) — the deterministic members are already cheap, so
backgrounding them would be pointless. Memory pressure never defers:
`compact.go:369` refuses the async path above `HighPressure`, because the OOM
backstop must act now.

### 2.5 Persistence and what is *not* rewritten

One `CompactionEntry` (`internal/session/entries.go:105-118`):
`{summary, firstKeptEntryId, tokensBefore, method}`. `BuildContext`
(`internal/session/context.go:140-155`) emits the summary message followed by
entries from `firstKeptEntryId` forward. **The dropped entries stay in the
file** — `xdev compress` says so explicitly (`cmd/xdev/compresscmd.go:156`), and
that is what makes `/rewind`, `/branch` and the tree view work across a
compaction boundary.

### 2.6 Memory: the part that is *not* compaction

A 50 000-entry session must not become 50 000 entries in RAM. At load,
`internal/session/store.go:256-270` drops everything before the latest reset boundary or
compaction, rebuilding indexes in batches without touching the file. Measured:
109 MB RSS → 130 MB heap for a 50k-entry session, fixed by that windowing
(PRD M8).

---

## 3. The six peers

Every number below carries its source. "Both-same" means the two harnesses use
the same rule; the constants still differ.

### 3.1 Claude Code

Source: official docs, `docs/research/claude-code/cc-official-docs.md:66-68`;
the 9-section compaction prompt is quoted verbatim in
`docs/research/claude-code/cc-compact.txt` (78 lines).

- **Two-phase, cheapest first:** *"Auto-compact: clears older tool outputs
  FIRST, then summarizes."* This is the **microcompact** tier — new tool results
  beyond the newest ~5 are replaced with a cleared-content marker, over a
  declared tool set, with a minimum-savings threshold so it does not fire for
  nothing.
- **Window:** default = the model's context limit (200k models compact near
  200k; Sonnet 5 on native-1M compacts ~967k). Configurable via
  `/autocompact N`, `--autocompact`, `autoCompactWindow`, or
  `CLAUDE_CODE_AUTO_COMPACT_WINDOW` (100k–1M, three surfaces with a precedence
  order).
- **Thrash guard:** stops after a few failed compactions and reports an error,
  rather than compacting forever.
- **Manual:** `/compact [focus]` with custom instructions; a
  `CLAUDE.md` "Compact instructions" section feeds it.
- **What survives** (`cc-official-docs.md:66`, verbatim): *"project CLAUDE.md
  re-injected, MEMORY.md, conversation summary + kept key snippets; skill
  descriptions list NOT re-injected (only invoked skills re-attach w/ budgets)."*
  Invoked skills re-attach at 5k tokens each, 25k combined.
- **Hooks:** `PreCompact` / `PostCompact` (matcher `manual|auto`);
  `SessionStart` enum `startup|resume|clear|compact|fork`, where
  compact-matching hooks re-inject after compaction.
- **Visibility:** `/context` shows per-category budget usage; `/usage` reports
  *"1 expected rebuild (compaction or tool-result clearing)"* in the
  prompt-cache stats.

### 3.2 OpenCode

Source: re-read from the source tree at `cd9a14a` (v2.0.18) on 2026-10-03.
Engine: `packages/core/src/session/compaction.ts`, 810 lines. This corrects
the v1.18.28 pass — see §4.

- **Proactive, before the request.** `required()` (`:744-763`) is consulted at
  the top of every logical step, before the request is built
  (`packages/core/src/session/runner/llm.ts:220-225`), and compacts then
  `continue`s the step loop with a fresh assistant-message id.
- **Settings** (`packages/schema/src/config/compaction.ts`): `auto` (default
  `true`), `buffer` (`DEFAULT_BUFFER = 20_000`, `:40`), `keep.tokens`
  (`DEFAULT_KEEP_TOKENS = 15_000`, `:41`). `tail_turns` and `prune` are
  **v1-only** and are now `unsupportedIfPresent` diagnostics
  (`packages/core/src/config/normalize.ts:328-329`).
- **Trigger** (`:744-763`): refuses to compact twice in a row (a completed
  checkpoint as the last message returns false), refuses before a primary
  response has anchored the new window, then:

  ```
  promptCeiling = min(inputLimit − buffer, context − max(min(outputLimit, 32_000), buffer))
  compacts when estimateTokens(messages) >= promptCeiling
  ```

  **It subtracts the completion allowance.** See §5.1 — this is the one thing
  xdev omits.
- **Token estimate** (`estimateTokens`, `:174-192`): the last assistant's
  reported usage (`input + cache.read + cache.write + output + reasoning`) plus
  locally priced parts appended after it. Fallback is `Token.estimate` over the
  base transcript, `CHARS_PER_TOKEN = 4`
  (`packages/core/src/util/token.ts:4-5`). Media priced by mime, not measured:
  image `1_500`, PDF `2_000` (`:44-45`, `estimateMedia :210-213`).
- **Cut point** (`findTailStart`, `:331-360`): accumulate backwards to
  `keep.tokens`, always keep at least the newest entry, then **rewind to a user
  boundary** so an assistant's tool calls and their results stay together. If
  everything fits, only the latest user exchange is retained so there is still
  a prefix to summarize.
- **The free tier** (`truncateToolOutput`, `:258-270`): clips any serialized
  tool result to `TOOL_OUTPUT_MAX_CHARS = 2_000` on a surrogate-pair-safe
  boundary and appends `[truncated]`, applied to tool results (`:305`) and shell
  output (`:318`). It runs *before* the summary request, so neither the
  summarizer nor the estimate ever sees the discarded bytes. What is truncated
  is what the summary is *given*; the DB keeps the full result.
- **Summary prompt** (`SUMMARY_TEMPLATE :46-86` + `SUMMARY_RULES :79-86`):
  required headings — Objective, Requirements, Decisions, Work State
  (Completed/Active/Blocked), Next Move, Relevant Files (at most 15, most
  important first), Important Context. Terse single-line bullets, preserve exact
  paths/symbols/commands/error strings/URLs, carry forward only unanswered user
  requests verbatim, record consequential workflow state (uncommitted /
  committed / pushed / under review / merged), never mention that compaction
  happened. Older checkpoints written against a longer template are detected by
  the catch-all `## Additional Context` heading (`LEGACY_HEADING :88`) and
  re-written to the current detail level rather than having their detail carried
  forward (`:372-381`). A summary missing a required heading is rejected and
  retried (`:660-680`); hitting the output token limit is a typed failure
  (`:676`), not a short summary.
- **No separate compaction model.** `compactionRequest` (`:469-494`) uses the
  session's own model ref. The v1 claim that a small/fast model compacts does
  not survive v2.
- **Provider-native compaction** (`packages/core/src/plugin/compaction.ts:9-27`):
  two mechanisms — `trigger` replaces history with
  `[...retained, assistant(checkpoint)]` and reports the compaction's own
  usage; `endpoint` delegates to `llm.compact`. Requires routing pinned in the
  catalog, not a `model.request` rewrite (`:518-527`), so a checkpoint is never
  installed that the next request would skip.
- **Overflow** (`llm.ts:265-272` → `compaction.ts:737-740` → `:497-500`): a
  one-shot recovery that **re-reads the original durable messages** instead of
  resubmitting the window that just overflowed — stated as the contract at
  `:120`. Exactly one per step: a success sets `recoveredOverflow`, disabling
  the hook for the rest of that step (`llm.ts:224`).
- **Durability**: the `compaction` message carries `status`
  running/completed/failed plus `reason` auto/manual, the kept `recent` tail and
  the summarizer's own usage (`packages/schema/src/session-message.ts:238-270`),
  so an interrupted compaction is *visible* rather than assumed done.
- **Manual** (`compactManual`, `:764-800`): typed
  `compaction.unavailable` ("Nothing to compact yet") when there is no cut.

### 3.3 DeepSeek Harness

Source: `docs/research/dsh-internals.md:123-165` +
`docs/research/deepseek-harness-deepdive.md:209-229`.

- **Triggers** (`packages/compaction/compaction/src/index.ts:32`):
  `CompactionTrigger = 'pressure' | 'context-overflow'` — a two-value enum, not
  a percentage; `pressure` is sampled at a step boundary.
- **Backend numbers** (`@deepseek-ai/dsh-compaction-basic`):
  `thresholdRatio 0.8`, `headroomTokens 65_536`, `retainRatio 0.16`
  (mutually exclusive with `retainTokens`), `maxTokens 8_192`,
  `compactionRetries 1`, `maxOverflowRetries 1`, `auto: true`.
- **The free tier ships too** (`compaction-tool-result-pruner`): shrinks one
  oversized result by code-point-safe head+tail — `thresholdChars 8_192`,
  `headChars 4_096`, `tailChars 1_024` — *before* summarization, with **no
  model call**. Same ordering Claude Code uses, with the numbers written down.
- **Envelope validation at config time** (`compaction-basic/src/config.ts:172-188`):
  fails if `contextWindow − reservedCompletionTokens` leaves no message budget.
  See §5.1.
- **Durable transaction**: `compaction/start{turn}` → summarize →
  `compaction/summary{…}` → replacement message → `compaction/end{turn, error?}`,
  with the lock released **last**, so a crash mid-operation becomes a
  detectable **orphaned lock** rather than an `end` that falsely claims success.
- **Token meter as a first-class object**: one detached snapshot carrying both
  route-priced `tokens` and route-independent `heuristicTokens` per node — dsh's
  `docs/subsystems/token-meter.md`, quoted from
  [dsh-internals.md](dsh-internals.md):157-159. The dual pricing is the part
  worth stealing: a bill and an estimate are different questions and conflating
  them is how a meter becomes a lie.
- **Manual verb**: `compactNow()` with typed failure codes
  (`busy | cancelled | …`) — a failed manual compaction is a logged fact.
- **Also**: image offload before compaction; spill-to-disk with
  `maxInlineTokens` + head/tail retention.

### 3.4 omp

Source: `docs/research/omp-context-resilience.md:246-285`. xdev's ladder is a
semantic port of this; the entry names are shared so sessions interop.

- **Six trigger paths.** `thresholdPercent` (default `-1` ⇒ legacy
  reserve-based), `thresholdTokens` (fixed limit beats %), `keepRecentTokens
  20_000`, `reserveTokens` unset ⇒ `DEFAULT_RESERVE_TOKENS = 16_384` with a
  **15 % proportional floor**, `midTurnEnabled true`, `asyncEnabled true`
  (speculative pre-threshold runs), `idleEnabled false` +
  `idleThresholdTokens 200_000` / `idleTimeoutSeconds 300`.
- **Threshold math** (`resolveThresholdTokens`): fixed-token clamp
  `[1, window−1]` > percent `floor(window·pct/100)` > legacy `window − reserve`.
  Trigger is `contextTokens > thresholdTokens` with the same
  `max(reported, estimate)` honesty rule as xdev.
- **Method ladder** (`methodOrder` default `[remote, snapcompact, handoff,
  shake, soft]`) with per-method auto-advance on failure — the exact shape
  xdev's `runCompactLadder` ports, minus `remote`.
- **Summary budgets**: `MAX_SUMMARY_TOKENS = 16_384`; summary budget
  `floor(0.8 × reserveTokens)`, so a 1M window authorizes ~120k-token summary,
  clamped hard. Effort honors the session's thinking level.
- **Overflow recovery is four cases in order** (`checkCompaction`,
  `session-maintenance.ts:1741-1930`): input overflow → **context promotion
  first** (switch to a bigger-window model), then compaction; threshold →
  supersede-reads + dropUseless pruning *first*, then maintenance; incomplete
  output (`stopReason: "length"`) → drop the partial, input was fine;
  payload-shaped 413s → *withheld* from compaction, because bytes/media cannot
  be fixed by summarizing.
- **Dead-loop guards**: 80 % recovery band; a turn that repeatedly fails to
  compact is parked rather than retried forever.
- **Speculative compaction**: in the band `[threshold − lead, threshold)` a
  background summary starts; the next maintenance pass claims it with
  apply-time branch validation, and a stale result is discarded.
- **Cache-aware**: compaction and handoff oneshots deliberately reuse the live
  `sessionId` / `promptCacheKey` so the summarize **reads the warm prefix
  cache**.
- **After compaction**: `handoffSaveToDisk`, `autoContinue`, and preserved
  `preserveData` (the remote-compaction payload, the snapcompact archive) that
  is re-rendered on every rebuild.

### 3.5 pi

Source: `docs/research/parity-pi-internals.md:336-340, 500-511`. pi is the
minimal base omp forked; its context rule is one sentence and it is the one
xdev borrowed verbatim.

- **Context overflow → compaction, never retry.** The rule is explicit at the
  retry layer: `isRetryableAssistantError(message) && !isContextOverflow(message)`.
- **Promote to a bigger model first**, then compact.
- Two-layer retry is the surrounding mechanism (provider transient retry with
  exponential backoff, then session-level turn replay) — not a context
  mechanism, and not the subject of this document.

### 3.6 ZCode

Source: [zcode-internals.md](zcode-internals.md) §6, read from primary source at
`872ad96` on 2026-09-22. ZCode is the only harness in this list whose
**whole** compaction story is three explicit tiers with three triggers and
three costs, written down with the reasoning. Two caveats before porting: the
tree has **zero test files**, so the design is *stated and wired*, not proven;
and §7.3 lists xdev's own advantages, which are real.

- **Tier 1, microcompact** (§6.1) — no model call, runs **before** the request:
  replace old **tool results** with `"[Old tool result content cleared]"`,
  keeping the most recent `DEFAULT_MICROCOMPACT_KEEP_RECENT_TOOL_RESULTS = 5`,
  over a declared tool set (`Read, Bash, Grep, Glob, WebFetch, WebSearch, Edit,
  Write, ApplyPatch`), with a `minTokenSavings = 256` floor and an option to
  spare error results. Trigger: `min(threshold * 0.9, threshold − 2000)`, or an
  **idle** trigger at `DEFAULT_MICROCOMPACT_IDLE_THRESHOLD_MINUTES = 60`. It
  emits a `MicrocompactBoundaryPayload` — the interesting part is that clearing
  is a **provider-visible boundary event**, not a silent mutation.
- **Tier 2, auto-compact** (§6.2) — one model call. Policy
  (`compact/policy.ts`): `effectiveContextWindow = contextWindow − min(maxOutputTokens,
  21_000)`; `threshold = effectiveContextWindow − 13_000`. The subtraction is
  deliberate and documented in the peer: the window is shared between input and
  output, so the input-side threshold must yield the output allowance first —
  **the same defect as §5.1**. The decision carries a `reason` union
  (`disabled | not_enough_messages | circuit_breaker | below_threshold |
  above_threshold`) and a `tokenSource` of `estimate | provider_usage`, i.e. it
  records which one it used. `MAX_CONSECUTIVE_AUTOCOMPACT_FAILURES = 3` is the
  failure circuit breaker.
- **Tier 3, the rapid-refill breaker** (§6.3) — a **loop-level** guard, the one
  piece xdev has nothing like: `RAPID_REFILL_TOOL_TURN_THRESHOLD = 3` /
  `MAX_CONSECUTIVE_RAPID_REFILLS = 3` (`turn-loop-state.ts:15-17`). A compaction
  followed by fewer than 3 tool turns before the window refills is a compaction
  that did not buy space; three consecutive such events throw
  `createCompactRapidRefillError` instead of compacting forever at full cost.
- **The record's provenance** (§6.4) — the boundary entry carries
  `{summaryMessageId, lastSummarizedMessageId, firstKeptEntryId, preCompactTokenCount,
  postCompactTokenCount, truePostCompactTokenCount, willRetriggerNextTurn, phase,
  trigger, reason}`. Two of those are worth stealing outright:
  `willRetriggerNextTurn` is a **prediction recorded as a fact for later
  audit**, and `truePostCompactTokenCount` is the real post-compaction size
  rather than the summary's self-report. xdev's `CompactionEntry` carries
  `{summary, firstKeptEntryId, tokensBefore, method}` — no *why*, no *whether it
  worked* (an enrichment of **#116**).
- The summarizer prompt forces **TEXT ONLY** (tool calls "will be REJECTED and
  will waste your only turn") and demands an `<analysis>` block before a
  `<summary>` block of 9 numbered sections, including **all user messages**
  and pending tasks. This is the `phase`/`trigger`/`reason` surface §5.3 needs.
- xdev is ahead here in two places ZCode is not: the **ladder** (§2.3) and the
  cache-aligned side request in `handoff`.

---

## 4. Where the peer docs in this repo have drifted

Correcting a doc is only half the job; naming the drift stops the next session
from re-deriving it.

| Doc | Claim | Status |
|---|---|---|
| `opencode-internals.md` §7 | *"OpenCode never summarizes proactively and relies on the provider's overflow error"* (`tail_turns: 15`, hidden `compaction` agent, `ContextOverflowError`, small/fast model) | **Wrong for v2.** `required()` runs at the top of every step; overflow is a one-shot recovery. `tail_turns` is v1-only. `ContextOverflowError` survives only in the v1 schema. The cited `prompt.ts:1164` / `processor.ts:493` do not exist. Rewritten in PR #533. |
| `PRD.md` §5.4 | the same claim, twice (the section is duplicated in the file) | same correction, both copies |
| `harness-comparison.md` | "OpenCode — SQLite as the source of truth … " | accurate; the compaction column was in §7 and inherits the correction |
| `deepseek-harness-deepdive.md:216` | "Trigger threshold … both-same in spirit, different constants" | still true; xdev's 20 % reserve floor vs dsh's `headroomTokens 65_536` are genuinely different numbers doing the same job |
| `deepseek-harness-deepdive.md:228-229` | memory-pressure trigger, idle trigger: **xdev-only** | still true and still unique among the six |

---

## 5. The gap list: what xdev does not have

Ordered by what each would buy, cheapest first. Five of the six gaps already have
open issues; §5.4 (the in-session `/compact` verb) does not, and says so.

**The peer to implement against is ZCode.** It is the only harness whose
compaction loop is written down end-to-end
([zcode-internals.md](zcode-internals.md) — written from primary source) *and*
which contributes two of the five tracked gaps below: the pre-request tier
(§5.2) and the breaker (§5.5). The other five peers are scouting; ZCode is the
spec.

### 5.1 The threshold does not subtract `MaxTokens` — a correctness bug

```
xdev   threshold = ContextWindow − reserve                          // compact.go:119-124
oc     promptCeiling = min(inputLimit − buffer,
                          context − max(min(outputLimit, 32_000), buffer))
dsh    fails at config time if contextWindow − reservedCompletionTokens
          leaves no message budget                                // config.ts:172-188
```

If a model's `MaxTokens` is large and the reserve is small, xdev can pass its
own "safe" threshold and still overflow on the completion allowance. **Four**
peers guard this (§3.2 OpenCode, §3.3 dsh, §3.4 omp's promotion-first, §3.6
ZCode); xdev does not. This is a one-line change in `threshold()` plus a test,
and it is the smallest item on this list.
guard this; xdev does not. This is a one-line change in `threshold()` plus a
test, and it is the smallest item on this list.

The blast radius is measurable, not hypothetical. Walking the shipped catalog
(`internal/config/connect_catalog.go`, 1 585 model rows) and comparing
`window − MaxTokens` against xdev's own `window − max(16384, 20 % window)`:

| | |
|---|---|
| distinct `(window, MaxTokens)` pairs | 187 |
| pairs whose safe ceiling sits **below** xdev's trigger | 109 |
| catalog rows affected | 778 of 1 585 |
| worst single gap | 1 600 000 tokens (`window = MaxTokens = 2 000 000`) |
| rows where `MaxTokens ≥ window` outright | 8 families, incl. 31 rows at `1048576/1048576` and 1 at `2000000/2000000` |

Those are catalog rows where the *declared* completion allowance alone
exceeds the prompt budget — real config, not a hypothetical model. Note the
shipped path also passes `MaxTokens: 0` (provider default; Anthropic then
sends `8192`, `internal/ai/anthropic.go:134-136`), so the practical failure is
"compact at 800 000, then overflow on a 1 048 576-token model" rather than a
guaranteed one on every request.

The fix has to decide whose number wins. `MaxTokens` is a *per-request* knob
(`Agent.MaxTokens`, `internal/agent/loop.go:293-294`) and the catalog's is a
*per-model* one, so `CompactionConfig` needs a completion-allowance field
sourced the same way `ContextWindow` is (`modelWindow`,
`cmd/xdev/print.go:924-938`) — and must clamp to a positive value, or a model
with `MaxTokens ≥ window` leaves no prompt budget at all.

Nearest open issue: **#168** (per-model window table + autocompact window
grammar) covers the *source* of the number; none of the five tracked gaps
covers the *arithmetic*.

### 5.2 No free pre-summarize tier

The gap this repo has been citing under other names. **Four** peers ship a
model-free pass that shrinks or clears old tool results before the summarizer
runs:

- Claude Code: microcompact — clear old tool outputs, keep the newest ~5, over
  a declared tool set, with a `minTokenSavings` floor and a
  `microcompact_boundary` event so the transcript knows content was dropped
  ([PRD §5.1 adopt bullet](https://github.com/FreePeak/xdev/blob/main/docs/PRD.md);
  [cc-resweep-2026-09-14.md:300](claude-code/cc-resweep-2026-09-14.md))
- ZCode: `microcompactIfNeeded` **before** `autoCompactIfNeeded` in the step
  loop (`runRegularTurnLoop`), same 5-keep rule
  ([zcode-internals.md:87](zcode-internals.md)) — and it is the peer that also
  ships the breaker in §5.5
- dsh: `compaction-tool-result-pruner`, 8 192/4 096/1 024 chars, no model call
- OpenCode: `truncateToolOutput` at 2 000 chars, ~15 lines, no model call

xdev has the *seam* — `internal/agent/offload.go:15-22` defines
`ArtifactOffloader` with a 16 KiB gate (`OffloadThresholdBytes`) — but
`Agent.Offload` is nil, so results sit verbatim. Two distinct things are
missing and they are not the same work:

1. the **pre-request pass** (cheap, in-loop, before the ladder) — **#430**,
   which spells out the ordering, the 5-keep rule, the `minTokenSavings = 256`
   floor, the auditable `reason` union, the boundary event, and the one real
   correctness risk: never clear a result the *current* step produced, and
   never one whose call is still open;
2. the **offload backend** (`ArtifactOffloader` implementation) — **#115**:
   blob-store retention + the model-facing re-read.

**#116** is the adjacent token-accounting bug (the trigger estimates instead
of measuring the serialized body) and is a *prerequisite* for trusting any of
the above: until the trigger measures bytes, "the window was full" is an
assumption. Do **#116** first, then #430.

OpenCode's is the smallest proof that the pass is ~15 lines; ZCode's is the
better spec (it has the ordering, the floor, and the audit trail). #430 already
cites both.

### 5.3 Nothing is re-injected after compaction

Claude Code re-attaches project `CLAUDE.md`, `MEMORY.md`, and invoked skills
(5k each, 25k combined), and `SessionStart(compact)` re-runs hooks.
OpenCode's checkpoint template does the same job in prose: its "Relevant Files"
section names at most 15 paths for the next agent to open, and its rules say to
preserve consequential workflow state and unanswered user requests verbatim.
xdev's summary is the only thing that survives — `internal/agent/compact.go:176-180`
is a 5-line prompt with no file list, no skill budget, no state carry. Tracked
as **#137**, which specifies the contract: re-read up to 5 most-recently-modified
touched files (>5k tokens emits a `Referenced file: <path>` stub), re-inject
invoked skill bodies under the 5k-each / 25k-total budget, re-inject the plan
from disk.

### 5.4 No in-session `/compact`

`xdev compress` (`cmd/xdev/compresscmd.go`) works on a **file**, off-line, and
defaults to a deterministic member so a CLI command never silently spends a
provider round-trip (`compresscmd.go:48-57`). There is no way, inside a live
session, to say "compact now and tell me what it saved". Claude Code has
`/compact`, OpenCode has `compactManual`, dsh has `compactNow()` with typed
failure codes, omp has `/compact` with `oneshotRetry`. Named in
`harness-comparison.md` and `dsh-internals.md:142-146` as the missing member.
**No issue exists** — this is the one gap on the list with no ticket behind it,
and the reason it is item 4 rather than item 1 is that it is the least
load-bearing (a session can still compact itself; it just cannot be told to).

### 5.5 No thrash guard

CC stops after a few failed compactions with an error. omp has an 80 %
recovery band and dead-loop parking. ZCode has the sharpest version of the idea:
a compaction followed by fewer than `RAPID_REFILL_TOOL_TURN_THRESHOLD = 3`
tool turns before the window refills is a compaction that **did not buy
space**; `MAX_CONSECUTIVE_RAPID_REFILLS = 3` of those in a row throws at the
boundary instead of compacting at full provider cost forever
([zcode-internals.md §6.3](zcode-internals.md), `evaluateRapidRefill`,
`turn-loop-state.ts:15-17`).
xdev's ladder falls through to the next member and, if all fail, logs and
continues — a session that cannot compact will keep trying at every boundary
forever. Tracked as **#422**, which also carries the separate failure circuit
breaker (`MAX_CONSECUTIVE_AUTOCOMPACT_FAILURES = 3`) and the audit counters that
make the trip auditable. Implement the audit the way ZCode does the tiers: a
boundary record with a `reason` union, so the decision is inspectable instead of
merely logged.

## 6. Numbers worth remembering

| | xdev | CC | OpenCode | dsh | omp | pi | ZCode |
|---|---|---|---|---|---|---|---|
| Threshold | `window − max(16384, 20 %)` — **no `MaxTokens` subtraction** | model's limit (near 200k; ~967k on 1M) | `min(input−buffer, ctx−max(out,buffer))`, buffer 20 000 | `ratio 0.8`, headroom 65 536 | `%` / fixed / `window − reserve` (15 % floor) | overflow-driven | `ctx − min(maxOut, 21 000) − 13 000` |
| Kept tail | 20 000 tok | newest ~5 tool results | 15 000 tok | `retainRatio 0.16` | 20 000 tok | — | `selectPersistedCompactTail` |
| Summary cap | 16 384 tok | — | output-limit typed failure | `maxTokens 8_192` | 16 384 tok (budget `0.8×reserve`) | — | 9 sections + `<analysis>` first |
| Free tier | ✗ (seam only) | microcompact | 2 000-char truncation | 8 192/4 096/1 024 pruner | supersede-reads + dropUseless | ✗ | microcompact, keep 5, floor 256 |
| Ladder members | 5 + registry | 1 | 1 + native | 1 + post-processors | 5 + per-method advance | 1 | 1 (auto-compact) |
| Manual verb | `xdev compress` (file) | `/compact` | `compactManual` | `compactNow()` | `/compact` | — | — |
| Thrash guard | ✗ | few failures → error | — | — | 80 % band + parking | — | rapid-refill breaker, 3/3 |
| Boundary provenance | `method` only | `PreCompact`/`PostCompact` hooks | `status`/`reason`/`recent`/usage | `compaction/start`…`end` + orphaned lock | `preserveData` + handoff doc | — | `phase`/`trigger`/`reason`/`willRetriggerNextTurn` |
| Mem-pressure trigger | ✓ 85 % | ✗ | ✗ | ✗ | ✗ | ✗ | ✗ |
| Idle trigger | ✓ | ✗ | ✗ | manual only | ✓ (opt-in) | ✗ | microcompact only, 60 min |
| Async compaction | ✓ | ✗ | ✗ | ✗ | ✓ speculative | ✗ | ✗ |

---

## 7. When you come back to this

- **Changing the trigger** → read `compact.go:105-174` first. §2.1 is the
  whole surface; there are five branches and no hidden state beyond
  `compactIdle` and the async job.
- **Adding a ladder member** → `RegisterCompactionMethod`, and add the name to
  `config.CompactionMethodNames` or `ParseMethodOrder` drops it with a warning
  (`compact.go:66-83`). Write the test against `runCompactLadder`'s
  fallthrough, not against your member in isolation.
- **Porting microcompact** → the shape is OpenCode's (a pure function over
  serialized tool output, no model call, no config surface) but the spec is
  ZCode's, and **#430 already carries it** — do not re-derive it. Decide
  deliberately whether truncation applies to what the *summary sees* or what the
  *transcript stores*; OpenCode does the former and the distinction is worth
  keeping. Two numbers you cannot copy blind: ZCode's `13_000` and `21_000` are
  tuned to its own window and model set, not to xdev's 20 % reserve.
- **Fixing §5.1** → it is one expression in `threshold()` plus a test that
  pins "a large `MaxTokens` raises the trigger point". Do it before any of the
  larger items, and read the catalog table in §5.1 first — 778 rows are
  affected, so "reasonable default" is not a decision the code can make for you.
- **Re-verifying a peer** → §3 is dated per peer. §4 lists what is known
  stale. If a peer's source tree has moved, the peer row is wrong before the
  xdev row is.
- **Reading a peer's §6 as a spec** → ZCode is the only one written for porting
  (three tiers, three triggers, three costs), but read its caveats first: zero
  tests, and `zcode-internals.md` §7.3 names the three places xdev is already
  ahead. Do not port the whole of §6 into §2; port tier by tier and keep the
  ladder where it is.

---

*Last updated: 2026-10-03 (docs/long-session-context — a single standing answer to
"how does xdev handle a long-running, big-token session"): consolidates the
compaction answer that previously lived in three places and disagreed in one, with
`file:line` on every claim. Peer sections dated individually (§3); §4 names the
peer docs that have drifted from their source; §5 is the ordered gap list with the
existing issue ids (#430, #116, #115, #137, #422 — §5.4 is the one gap with no
ticket, and says so), re-ranked by cost after the OpenCode v2 re-read showed the
free tier is ~15 lines rather than a feature. Six peers, not five: ZCode (§3.6)
joins because it is the only harness whose compaction story is written down as
three tiers with three triggers and three costs, and it contributes two of the
tracked gaps. Three findings that changed the ranking: the prompt-ceiling
arithmetic xdev's `threshold()` omits (§5.1 — one expression, but 778 of the
1 585 shipped catalog rows have a `MaxTokens` that puts the safe ceiling below
xdev's own trigger, so the fix needs a per-model number, not a constant); the
pre-summarize tool-output truncation that turns the "no microcompact" gap from a
project into a patch; and the ordering, which is the part peers agree on and
xdev has backwards — the cheap pass belongs before the request, not at the
boundary where the expensive one already fires.*
