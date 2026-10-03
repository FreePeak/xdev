# Empryo: how token usage and the model bill are calculated

**Date:** 2026-10-03 · **Subject:** token accounting and USD pricing in [Empryo](https://empryo.com) (`npm @proxysoul/soulforge` v2.20.25, BUSL-1.1), read as a *reference implementation* for xdev's `internal/stats` and `internal/tui`.
**Question this answers:** how does Empryo turn provider usage blocks into a running token count and a dollar bill, and where does xdev already do the same thing better or worse?
**Companion:** [2026-10-03-empryo-tui-parity.md](2026-10-03-empryo-tui-parity.md) (the TUI pass over the same checkout), [PRD.md §3.5](../PRD.md) (xdev's TUI contract).

**Evidence standard.** Three source-level passes: pricing (the table and the lookup), usage extraction (per provider), and accumulation/billing (the store, the hook, the displays). Every claim below carries `file:line` on the side it is about. `node_modules/` is absent from that checkout, so anything inside the AI SDK is marked *not verified* rather than asserted.

## 0. Verdict

**Empryo's bill is a pure function of a per-model token breakdown; xdev's is whatever the provider reported.** That single difference produces every other difference in this document.

- Empryo keeps **no USD counter at all**. `computeTotalCostFromBreakdown` (`src/stores/statusbar.ts:321-333`) recomputes dollars from `modelBreakdown` on every store subscribe, so a restored session reads `$0` and the number dies with the process. xdev banks `Usage.Cost.Total` per message (`internal/ai/types.go:174`) and replays it out of the transcript (`cmd/xdev/tui.go:3134-3151`), so a resumed session keeps its spend.
- **Consequence for xdev:** the missing half of xdev's number is not the arithmetic, it is the *price table*. xdev already carries the tokens; `internal/stats/stats.go:78-83` says so in its own `ponytail:` comment. Empryo's `matchPricing` is a drop-in for the upgrade path that comment names.
- **One Empryo bug worth not copying:** `StatusDashboard.tsx:777-779` sorts the per-model rows by calling `computeModelCost("", …)`, so every row resolves to `DEFAULT_PRICING` and the sort orders by token count, not cost.
- **The compaction carve-out is the one thing to steal verbatim** (§5) — it is the difference between a ~10× overbill and an honest number, and xdev's compaction path has the same seam.

## 1. Stage 1 — usage extraction (Empryo delegates almost all of it)

### 1.1 The only hand-rolled parser

`src/core/llm/providers/codex/runner.ts` shells out to the `codex` CLI and parses its JSONL (`readline` + `JSON.parse` per line). Usage comes from the **`turn.completed`** event only (`:384-398`):

```ts
} else if (event.type === "turn.completed") {
  const rawUsage = event.usage as
    | { input_tokens?: number; cached_input_tokens?: number; output_tokens?: number }
    | undefined;
  if (rawUsage) {
    usage = {
      inputTokens: rawUsage.input_tokens,
      outputTokens: rawUsage.output_tokens,
      totalTokens:
        rawUsage.input_tokens != null && rawUsage.output_tokens != null
          ? rawUsage.input_tokens + rawUsage.output_tokens
          : undefined,
      cachedInputTokens: rawUsage.cached_input_tokens,
    };
  }
}
```

Three consequences worth naming:

1. **No cache-write field exists on this path.** `cached_input_tokens` is the only cache counter, and it is written to `cachedInputTokens`, not into `inputTokenDetails.cacheReadTokens` — the field every downstream consumer actually reads. So a Codex turn's cache reads are priced at the full input rate.
2. **Absent usage is not an error.** The initializer is all-`undefined` (`:360-364`); `doStream` still returns one `finish` with the whole-turn record (`:477-482`). A step with no usage costs $0 and says nothing.
3. **No streaming accumulation.** The CLI turn is buffered (`finalText`) and replayed as synthetic parts; there is no per-chunk summing.

### 1.2 Every other provider: no usage code at all

`src/core/llm/providers/*.ts` contain **zero** usage handling. Each is a thin factory returning the SDK's raw `LanguageModel`:

- `anthropic.ts:28` — `createAnthropic({ apiKey, fetch: withSessionHeaders() as typeof fetch })(modelId)`
- `src/core/llm/providers/openai.ts:28` — `createOpenAI({ apiKey, fetch: withSessionHeaders() as typeof fetch })(modelId)`
- `custom.ts:89-95` — `createOpenAICompatible({…}).chatModel(modelId)`
- `github-models.ts:43-48` — `createOpenAI({ baseURL, apiKey, headers: GH_HEADERS, … }).chat(modelId)`
- `bedrock.ts:21` — `createAmazonBedrock({…})(modelId)`

The only JSON those files parse is model listings and auth (`anthropic.ts:41` reads `context_window`; `custom.ts:125-142` reads `context_length` / `max_input_tokens` / `n_ctx_train`; `copilot.ts:40-62` reads `{ token, expires_at }`).

What every downstream consumer therefore receives, as declared by Empryo's own typed callbacks (`src/core/agents/subagent-tools.ts:187-194`):

```ts
usage?: {
  inputTokens?: number;
  outputTokens?: number;
  inputTokenDetails?: { cacheReadTokens?: number; cacheWriteTokens?: number };
};
```

*Not verified:* the SDK-internal mapping of raw `cache_creation_input_tokens` / `cache_read_input_tokens` / `prompt_tokens_details.cached_tokens` into `inputTokenDetails`. `node_modules/` is not installed in that checkout, so those greps returned no files rather than no matches.

### 1.3 The rule that makes the arithmetic work

`inputTokens` from the SDK is the **total** prompt — uncached + cache-read + cache-write. Empirical, not documented, but encoded everywhere in Empryo's derivation code (`useChat.ts:3074-3075`, `:1975-1976`, `:1269-1270`). The same is true of xdev: `internal/ai/anthropic.go:467-470` fills `Input` from `message_start`'s `input_tokens` alone and puts cache read/write in their own buckets, which on Anthropic's wire is the only uncached count the API sends.

## 2. Stage 2 — accumulation

### 2.1 The step, in the main loop

`src/hooks/useChat.ts:3058-3105`, on every `finish-step`:

```ts
const stepTotal = part.usage.inputTokens ?? 0;
const stepOut = part.usage.outputTokens ?? 0;
const details = (part.usage as { inputTokenDetails?: {
  cacheReadTokens?: number; cacheWriteTokens?: number; noCacheTokens?: number } }).inputTokenDetails;
const stepCache = details?.cacheReadTokens ?? 0;
const stepCacheWrite = details?.cacheWriteTokens ?? 0;
const stepNoCache = details?.noCacheTokens ?? 0;
// prompt = uncached input ONLY. cacheWrite tracked separately.
// inputTokens from SDK = total (noCache + cacheRead + cacheWrite)
const stepIn =
  stepNoCache > 0
    ? stepNoCache
    : Math.max(0, stepTotal - stepCache - stepCacheWrite);
```

then the session totals and the per-model row are advanced in one shot (`:3089-3105`), with `accumulateModelUsage` (`src/stores/statusbar.ts:347-362`) as the single additive primitive.

Two things to keep:

- **`Math.max(0, …)` on the subtraction.** A provider whose details over-report make the subtraction negative; unclamped it would mint tokens out of nothing. xdev needs the same clamp on any derived "uncached" figure.
- **`noCacheTokens` is preferred over the subtraction** when the SDK supplies it, and only falls back otherwise.

### 2.2 State shape

`src/stores/statusbar.ts:11-30`:

```ts
export interface TokenUsage {
  prompt: number;      // uncached input only
  completion: number;
  total: number;       // total incl. output; NOT a billing figure
  cacheRead: number;
  cacheWrite: number;
  subagentInput: number;
  subagentOutput: number;
  lastStepInput: number;
  lastStepOutput: number;
  lastStepCacheRead: number;
  modelBreakdown: Record<string, PerModelUsage>;
}
```

`prompt` is documented as uncached-only at the definition and in every comment at the derivation sites. That comment is load-bearing: it is what stops a reader treating `prompt` as the wire's `input_tokens`.

### 2.3 Subagents bill into the same counter, at their own model's rate

`src/hooks/useChat.ts:1945-2003`. Cumulative agent stats are diffed into deltas, uncached input is re-derived the same way, and the row key is the **subagent's** model:

```ts
if (!isOurDispatch(event.parentToolCallId)) return;
…
const subModelId = event.modelId ?? "unknown";
const uncachedIn = Math.max(0, deltaIn - (deltaCache > 0 ? deltaCache : 0) - (deltaCacheWrite > 0 ? deltaCacheWrite : 0));
…
modelBreakdown: accumulateModelUsage(base.modelBreakdown, subModelId, { input: uncachedIn, … }),
```

`event.modelId ?? "unknown"` is the load-bearing bit: a subagent on a cheaper model is priced at *its* rate, not the parent's. `if (!isOurDispatch(...))` drops another tab's dispatches so two tabs cannot double-charge one session.

### 2.4 Compaction re-bases but carries the billing parts

`src/hooks/useChat.ts:1267-1293`:

```ts
setTokenUsage((prev) => {
  let bd = prev.modelBreakdown;
  if (compactUsage) {
    // … Split so cache-read tokens are billed at the cache-read rate (10× cheaper)
    // instead of the full input rate. Without this, compaction over-bills ~10×.
    const cacheRead = compactUsage.cacheReadTokens ?? 0;
    const cacheWrite = compactUsage.cacheWriteTokens ?? 0;
    const noCache = Math.max(0, compactUsage.inputTokens - cacheRead - cacheWrite);
    bd = accumulateModelUsage(bd, compactModelId, { input: noCache, output: compactUsage.outputTokens, cacheRead, cacheWrite });
  }
  return {
    ...ZERO_USAGE,
    prompt: estimatedTokens,
    total: estimatedTokens,
    cacheRead: prev.cacheRead,
    cacheWrite: prev.cacheWrite,
    subagentInput: prev.subagentInput,
    subagentOutput: prev.subagentOutput,
    modelBreakdown: bd,
  };
});
```

`...ZERO_USAGE` wipes prompt/completion/total and re-seeds prompt from a char estimate; cache read/write, subagent totals and the per-model breakdown are carried forward. The 10× figure is not decoration — Anthropic's cache-read rate is 0.1× input (`statusbar.ts:49`), so billing the compaction call's cached prefix at the input rate overcharges it tenfold.

### 2.5 The session boundary is a shrink detector

`src/hooks/useChat.ts:674-684` — `if (next.total === 0 || next.total < prev.total)` re-bases the monotonic accumulator, so `/clear` and a session switch cannot leave a stale total running. `/clear` writes a literal zero usage (`src/core/commands/session.ts:177-189`) and a new session resets the store wholesale (`src/stores/statusbar.ts:548-556`).

## 3. Stage 3 — the bill

### 3.1 Prices are USD per million, in one hardcoded table

`src/stores/statusbar.ts:32-44`:

```ts
interface ModelPricing { input: number; cacheWrite: number; cacheRead: number; output: number }

// Prices in USD per million tokens. Sources (verified 2026-04-30):
// Anthropic: https://docs.claude.com/en/docs/about-claude/pricing
const MODEL_PRICING: Record<string, ModelPricing> = {
  // cacheWrite = 5-min TTL (1.25× base). 1h TTL would be 2× base; SDK doesn't
  // expose which TTL was used, and we always request the 5m default.
  // cacheRead = 0.1× base input.
  "claude-fable-5":   { input: 10,   cacheWrite: 12.5,  cacheRead: 1,     output: 50 },
  "claude-opus-4-8":  { input: 5,    cacheWrite: 6.25,  cacheRead: 0.5,   output: 25 },
  "claude-sonnet-4-6":{ input: 3,    cacheWrite: 3.75,  cacheRead: 0.3,   output: 15 },
```

~66 entries across Anthropic (18), OpenAI (12), Gemini (8), DeepSeek (6), Groq (6), xAI (8), Mistral (8). Hand-verified dates sit in the comments (`:39`, DeepSeek `:101`, Groq `:112`, Grok `:122`). **Cache is priced as its own rate, never a multiplier of input** — DeepSeek's cache-read is 0.0028 against an input of 0.14, a 2% ratio that no fixed multiplier would produce (`:104`).

A second table covers Copilot's premium-request billing, derived rather than measured (`:149-164`):

```ts
// Formula: multiplier × $0.04 per request, ~5k tokens/request avg → $/1M = multiplier × $8
// input:output ratio ~4:1 in typical coding, so input = mult×$2, output = mult×$10
const M1: ModelPricing = { input: 2, cacheWrite: 2, cacheRead: 0.2, output: 10 };
```

and inline Fireworks tiers (`:275-284`).

### 3.2 The lookup

`matchPricing(modelId)` (`src/stores/statusbar.ts:244-302`) is the single entry point:

1. local (`ollama`, `lmstudio` — `:222-229`) or free (`:free` / `-free` suffix, or a zero-priced OpenRouter entry — `:232-242`) → `FREE_PRICING` (all zero, `:146`)
2. `copilot/*` → longest-key `includes` over `COPILOT_PRICING`, else `M1` (`:251-254`, `:214-220`)
3. `github-models/*` → longest-key `includes` over `MODEL_PRICING`, else `DEFAULT_PRICING` (`:258-265`)
4. `openrouter/*` → the fetched catalog, falling through when it has no entry (`:267-271`)
5. `fireworks/*` → inline tiers, then a `>16B params` catch-all (`:275-284`)
6. otherwise longest-key-first `id.includes(key)` over `MODEL_PRICING` (`:287-290`)
7. family heuristics, then `DEFAULT_PRICING = { input: 3, cacheWrite: 3.75, cacheRead: 0.3, output: 15 }` (`:292-302`)

Longest-key-first is required, not cosmetic: `"claude-opus-4-6"` must win over `"claude-opus-4"`, or every Opus version bills at the wrong rate.

### 3.3 The arithmetic

`src/stores/statusbar.ts:321-333` — the production path:

```ts
export function computeTotalCostFromBreakdown(breakdown: Record<string, PerModelUsage>): number {
  let total = 0;
  for (const [modelId, usage] of Object.entries(breakdown)) {
    const p = matchPricing(modelId);
    const cost =
      ((usage.input ?? 0) / 1e6) * p.input +
      ((usage.cacheWrite ?? 0) / 1e6) * p.cacheWrite +
      ((usage.cacheRead ?? 0) / 1e6) * p.cacheRead +
      ((usage.output ?? 0) / 1e6) * p.output;
    if (Number.isFinite(cost)) total += cost;
  }
  return total;
}
```

`computeModelCost(modelId, usage)` (`:336-344`) is the same four terms for one row; `computeCost(usage, modelId)` (`:308-318`) is the legacy scalar path, marked *"exported for testing only; production code uses computeTotalCostFromBreakdown"* (`:307`).

PUT 245:
- **No USD is ever stored.** `totalCost` exists only as a local `const` in the dashboard (`StatusDashboard.tsx:782`); the store holds tokens only.
- **The header display is recomputed per subscribe, then eased.** `TokenDisplay.tsx:50-64` sets a target in cents on every store change and a `STEP_MS` interval eases the rendered value toward it with `EASE = 0.35` (`:14-18`, `:69-85`). "Local" and "FREE" replace the number entirely when every row is local/free (`:31-33`).
- **The header shows the active tab only.** The store is overwritten on tab activation (`useChat.ts:685-686`, `:733-741`); the cross-tab rollup exists only in the dashboard (`StatusDashboard.tsx:304-332`).

### 3.4 The one live price source

`getOpenRouterModelPricing` (`src/core/llm/models.ts:267-287`) converts OpenRouter's per-token strings to per-million and defaults the write rate to the input rate when absent:

```ts
const input = Number(p.prompt ?? "0") * 1e6;
const output = Number(p.completion ?? "0") * 1e6;
const cacheRead = Number(p.input_cache_read ?? "0") * 1e6;
// If no cache write price, default to input price (most providers charge same as input)
const cacheWrite = p.input_cache_write ? Number(p.input_cache_write) * 1e6 : input;
```

`findOpenRouterModel` (`:299-320`) matches exact id, then `/{model}` suffix, then hyphen→dot version normalization. The catalog is a process-lifetime cache (`openRouterCache`, `:264`) filled by `fetchOpenRouterMetadata` (`:294-297`), which early-returns once set.

**A visible consequence:** because the dollar figure is recomputed rather than accumulated, the number silently changes shape mid-session when the catalog fetch lands — `DEFAULT_PRICING` first, real prices after. There is no indicator and no stamp saying which table produced the current figure.

## 4. Context occupancy

Two independent implementations, both preferring the provider's number over a char estimate.

`src/components/layout/ContextBar.tsx:174-184`:

```ts
const ctxWindow = state.contextWindow || 200_000;
const isApi = state.contextTokens > 0;
const charEstimate = (systemChars + state.chatChars + state.subagentChars) / CHARS_PER_TOKEN; // CHARS_PER_TOKEN = 4
const chatCharsDelta = Math.max(0, state.chatChars - state.chatCharsAtSnapshot);
const totalTokens = isApi
  ? state.contextTokens + (chatCharsDelta + state.subagentChars) / CHARS_PER_TOKEN
  : charEstimate;
const pct = totalTokens > 0 ? Math.min(100, Math.max(1, Math.round((totalTokens / ctxWindow) * 100))) : 0;
```

The `chatCharsDelta` term is the interesting one: the provider's count is a snapshot from the last request, so everything typed or streamed since is added back as a char delta — the bar is live rather than a step behind. Clamped to 1–100 so a non-zero session never renders an empty bar.

`StatusDashboard.tsx:533-543` mirrors it with a hardcoded `/ 4` and adds an estimate marker (`~42%` when there is no API number). The compaction triggers are derived here (`:546-551`): client compact at 70%, clear at `max(80_000, 30% of window)`, server compact at `max(160_000, 80% of window)`.

Backend-side, `ContextManager.getContextPercent()` (`src/core/context/manager.ts:482-486`) is `round(conversationTokens / window * 100)` and picks a mode instruction only (`:492`, `:1296`) — it is not the display number.

## 5. What xdev should take

Ordered by value per line of code. Sizes S / M / L, where S is a day.

| # | Item | What | Cost |
|---|---|---|---|
| **R-USE-1** | Local price table | Add `pricing: {input, cacheWrite, cacheRead, output}` (USD per 1M) to `ModelConfig` in `internal/config/models.go:23-37`, plus a `matchPricing`-style longest-key-first lookup, and use it **only when `PricedTurns < Turns`** (`internal/stats/stats.go:78-83`). Provider-reported cost always wins; the table fills the gap a silent gateway leaves. | **M** |
| **R-USE-2** | Compaction carve-out | xdev's compaction summary call goes through `summarizeWith` (`internal/agent/compact.go:296-341`) and its usage is **discarded** — the stream is read for text only (`:330-341`). Two fixes: charge the summarizer call to the session, and when the transcript is rebuilt, carry the billed buckets forward rather than re-basing them to zero. This is Empryo's ~10× overbill guard. | **S** |
| **R-USE-3** | Per-model rows | `modelBreakdown` keyed by model id. xdev's `ModelStat` (`internal/stats/stats.go:93-102`) already has the shape minus the cache-write column; the cache-write bucket is already persisted per message (`internal/ai/types.go:171`), so the only new thing is the split by model. | **S** |
| **R-USE-4** | Live context delta | xdev's context segment shows `CtxUsed` from the last provider count (`internal/tui/app.go:1357-1374`, rendered at `:5561-5568`). Adding the post-snapshot char delta keeps the bar live the way `ContextBar.tsx:180-181` does. | **S** |
| **R-USE-5** | Subagent dispatch guard | xdev's roster sums `me.Message.Usage` off a JSONL scan (`internal/agent/hub.go:316-319`) and keys nothing by dispatch. A per-dispatch token floor lets a resumed session keep a running total. | **M** |

**Reject / do not copy:**

- **The no-USD-stored design.** A pure function of in-memory tokens means a restored session reads `$0` until the next step, and nothing survives a restart. xdev's replay-out-of-transcript (`cmd/xdev/tui.go:3134-3151`, `:3162-3173`) is the better shape; keep it.
- **`computeModelCost("", usage)` as a sort key** (`StatusDashboard.tsx:777-779`) — resolves to `DEFAULT_PRICING` for every row, so the dashboard's cost column is ordered by token count. If xdev ever sorts by cost, the key must be the row's own model id.
- **Copilot premium-request arithmetic as a general model.** It assumes a 5k-token average request (`statusbar.ts:156`) to turn a per-request charge into a per-token rate. Fine as a labelled estimate for one provider; wrong as an architecture.

## 6. Not verified / open

- **SDK-internal usage mapping** for every non-Codex provider (§1.2) — `node_modules/` is absent from the Empryo checkout, so `cache_creation_input_tokens` → `inputTokenDetails.cacheWriteTokens` and `prompt_tokens_details.cached_tokens` → `cacheReadTokens` are inferred from Empryo's own typed callbacks, not read from the SDK source.
- **Whether the headless path under-reports.** `src/headless/run.ts:390-400` reads `cacheReadTokens` but never `cacheWriteTokens`, so headless cache writes price as uncached input. Same shape as the compaction overbill in §2.4; not exercised by any test in `tests/`.
- **The `isOurDispatch` filter** (`useChat.ts:1946`) is a tab-scoping guard whose full contract is only visible in `TabInstance.tsx`; read before implementing R-USE-5.
- **No test in Empryo's `tests/` covers cache-token extraction**, pricing lookup, or the compaction carve-out. Every arithmetic claim in §3 is therefore read off the source, not confirmed by a passing test.

*Last updated: 2026-10-03 (Empryo usage/billing pass).* Three source-level passes over the same checkout as the TUI parity doc — pricing table + lookup (`src/stores/statusbar.ts`), usage extraction (`src/core/llm/providers/**`, the SDK-normalized shape as declared in `src/core/agents/subagent-tools.ts:187-194`), and accumulation/billing (`src/hooks/useChat.ts`, `src/stores/statusbar.ts`, the three display surfaces). Every claim carries `file:line` on the side it is about; what could not be read is listed in §6 rather than asserted. **Verdict: xdev's bill is provider-reported and replayed out of the transcript, which is the better shape; Empryo's per-model breakdown + local price table is the missing half, and xdev's own `ponytail:` comment at `internal/stats/stats.go:82` already names that upgrade path.** The one thing to adopt on sight is the compaction carve-out (R-USE-2), because zeroing the billed buckets while re-basing the transcript is the ~10× overbill Empryo guards against.
