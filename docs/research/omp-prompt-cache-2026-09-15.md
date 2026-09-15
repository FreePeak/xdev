# omp prompt caching — how it works, and what xdev now does (#133)

Research date: 2026-09-15. Source: the installed binary at `~/.local/bin/omp`
(single-file Bun build), read by byte-searching the embedded JavaScript for the
provider modules' original identifiers — `packages/ai/src/providers/anthropic.ts`,
`openai-completions.ts`, `openai-responses.ts`, `google.ts`, `openai-shared.ts`.
Minified names are given as recovered (`rg`, `ME`, `hYs`, `DHr`/`GHr`/`LHr`/`FHr`)
plus the un-minified literals they contain, which are the load-bearing evidence.
Cross-checked against this repo's earlier sweeps
([2026-09-09-omp-pi-architecture-go-rebuild.md](2026-09-09-omp-pi-architecture-go-rebuild.md),
[parity-omp-internals.md](parity-omp-internals.md),
[../PRD.md](../PRD.md) §5.3).

## 1. The gap in xdev before this change

xdev **read** cache usage and never **caused** any:

| half | where | status before |
|------|-------|---------------|
| parse `cache_read_input_tokens` / `cache_creation_input_tokens` | `internal/ai/anthropic.go` | present |
| parse `usage.prompt_tokens_details.cached_tokens` | `internal/ai/openai_completions.go`, `openai_responses.go` | present |
| aggregate + report | `internal/stats/stats.go`, `cmd/xdev/usagecmd.go` | present |
| emit `cache_control` markers | — | **absent** |
| emit `prompt_cache_key` | — | **absent** |
| carry any cache identity on the request | `ai.StreamRequest` | **absent** |

So the counters could not move: Anthropic writes to the prompt cache **only up to
a `cache_control` marker**, and OpenAI routes cache affinity **only via
`prompt_cache_key`**. Every turn paid full input price and full read latency for
a 5–17k token prefix that never changed. PRD §5.3 already called this "the
cheapest real win found in the whole sweep".

## 2. How omp does it

### Anthropic (`anthropic.ts`)

Cached-prefix order is **tools → system → messages**, and a marker terminates a
span: everything at or before it becomes the cacheable prefix. omp therefore
spends its four-marker budget one tier at a time:

- **system rides as blocks.** A plain string cannot carry a marker, so the
  request body's `system` becomes `[{type:"text", text, cache_control:{…}}]`.
- **one marker on the LAST tool definition.** The tool array is the head of the
  prefix, so one marker caches every tool — and it survives a change to the
  system prompt, which omp's per-turn reminders are free to make. Skipping this
  tier is the common mistake: put a marker only at the end of `system` and the
  tool schema is not in any cached span.
- **rolling conversation tail**, newest first, plus a periodic **deep** marker
  behind it. Two recovered constants pin the policy: the marker budget (`4`) and
  the deep-marker stride (`15`). The tail alone still fails on a long session —
  a byte change 30 messages back rewrites every tail breakpoint, so the deep
  marker is what keeps a long run reading something.
- **skips non-cacheable blocks** (`FHr`): `thinking`, `redacted_thinking`, and
  omp's own `tool_addition`/`tool_removal`/`fallback` types. An unsigned replayed
  `thinking` block is a 400, not a cache miss.
- **counts before spending** (`WHr`/`GHr`): the budget left for the tail is
  `4 - (markers already on system + markers already on tools)`, so the tiers
  above cannot starve the tail, and a caller-supplied marker is never doubled.
- tail selection walks **backwards from the newest**, skipping the synthetic
  `"Continue."` turn that follows an assistant message, and prefers at most two
  recent messages plus every 15th *markable* one behind them.
- **never overwrites** a `cache_control` already on a block.
- **TTL buckets**: `{"type":"ephemeral"}` (5m) default;
  `{"type":"ephemeral","ttl":"1h"}` only when retention is long and the model
  advertises `extended-cache-ttl-2025-04-11`.

### Retention config

One resolver, recovered verbatim (`yd`), read from the `PI_CACHE_RETENTION` env or
the `providers.cacheRetention` setting:

```js
function yd(e, t = "short") { if (e) return e;
  const s = ke.PI_CACHE_RETENTION;
  if (s === "long" || s === "short" || s === "none") return s;
  return t; }
```

So the effective vocabulary is `short` | `long` | `none`, and an unset or
unrecognised value **defaults to `short`** — `auto` in the settings surface is
tier-selected upstream and reaches this resolver already resolved. `none`
short-circuits **both** vocabularies: no markers, no affinity key.

### OpenAI (`openai-completions.ts`, `openai-responses.ts`, `openai-shared.ts`)

Recovered verbatim, the whole affinity story in four lines:

```js
function rg(e) { if (yd(e?.cacheRetention) === "none") return;
                 return ME(e?.promptCacheKey ?? e?.sessionId); }
function ME(e) { return hYs(e, 64, "pc_"); }
function hYs(e, t, s) { const o = e.toWellFormed();
                        if (o.length <= t) return o;
                        return `${s}${Bun.hash(o).toString(36)}`; }
```

- `rg` — used by the **Responses** builder — is `promptCacheKey ?? sessionId`: a
  caller can override the bucket. The chat-completions path calls `doe`, which is
  `sessionId` alone, and `Xqt` puts the same id through a 256-char
  `session_`-prefixed variant for a header. Everywhere it is a **session**-level
  value, never a per-request id — so a side request (compaction, a title) reads
  the bucket the main chain wrote.
- 64-char ceiling, over-long ids collapse to `pc_<hash>` — one stable bucket,
  never several for one session. The hash is `Bun.hash`, so the exact digits are
  Bun-specific; the *policy* (verbatim, else one prefixed hash) is the portable
  part and the part xdev copies.
- Both sit behind a per-model compat flag: `compat.supportsPromptCacheKey`
  (3780 hits in the bundle — it is set per model, not assumed).
  `prompt_cache_options: {mode:"explicit", ttl}` + `prompt_cache_breakpoint`
  belong to a second flag, `supportsPromptCacheBreakpoints`; an endpoint asked to
  do explicit mode without it gets a thrown error naming the flag. That is the
  whole reason the explicit path is opt-in: the same field is a 400 elsewhere.

### Google (`google.ts`)

`cachedContent` — a **managed cache resource** created ahead of the request; the
request then names it instead of resending `systemInstruction`/`tools`/`toolConfig`
(mixing both throws). Stateful CRUD, TTL renewal, cache-id storage.

## 3. What xdev does now

Two provider-native mechanisms, no generic prefix tree. xdev's wire types are
per-provider, so markers live in the provider adapters.

- **`ai.CacheOpts{Key, SideRequest}` on `StreamRequest`** (`internal/ai/provider.go`).
  `Key` is the stable session id. Zero value ⇒ the pre-change request shape,
  byte for byte: every provider without a cache vocabulary ignores the field.
- **Anthropic** (`internal/ai/cache.go` + `buildRequest` in
  `internal/ai/anthropic.go`): system as marker-bearing blocks, one marker on the
  last tool, then the remaining budget over the conversation tail (newest first,
  plus every 15th markable message behind it), cap 4. Skips `thinking` /
  `redacted_thinking`; never overwrites an existing marker. Budget is spent
  bottom-up — the tail is what makes a long run cheap, so the tiers above take
  only what they need.
- **OpenAI** (`promptCacheKey` in `internal/ai/cache.go`, wired into
  `openai_completions.go` + `openai_responses.go`): `prompt_cache_key`, verbatim
  within 64 chars else `pc_<fnv64a base36>`, gated on the host being
  `api.openai.com`.
- **Side requests** (`CacheOpts.SideRequest`): markers stop at the last completed
  tool round instead of the newest message, so a served-and-dropped request
  writes only a prefix the next main turn reads. Wired on the handoff summarize
  call (`internal/agent/handoff.go`).
- **Identity** (`cacheOpts()` in `internal/agent/handoff.go`, used by
  `liveRequest`): `Store.ID()` — one value for the main chain and its side
  requests, mirroring omp's `promptCacheKey ?? sessionId`.
- **Latch** (`disableCacheMarkers`): a 4xx naming `cache_control` rebuilds the
  request without markers, retries once on the same context, and latches markers
  off for the process with one logged line. Not every anthropic-messages provider
  is Anthropic.
- **Accounting** (`cmd/xdev/usagecmd.go`): `/usage` prints the cache-hit ratio
  (`cache_read / (input + cache_read + cache_write)`) — the one number that says
  whether breakpoints are working. It was already in `stats.ModelStat`; surfacing
  it is one format string.

### Deliberately not copied

| omp has | xdev skips | why |
|---------|-----------|-----|
| per-model `compat.supportsPromptCacheKey` / `…Breakpoints` | ✗ host gate instead | no per-model compat table exists here; a proxy 400s on an unknown top-level field. Host is the honest test. omp's own code throws on explicit mode without the flag — the field really does break routes. |
| mid-conversation `system` messages, `tool_addition`/`tool_removal`, per-message effort | ✗ | no xdev wire vocabulary for them |
| OAuth-subscription TTL gating, the `prompt-caching-scope-2026-01-05` beta header | ✗ | that header rides a nine-flag subscription beta list on `api.anthropic.com`; no subscription auth in xdev |
| managed `cachedContent` CRUD (Google) | ✗ | stateful resource mgmt + TTL renewal + cache-id storage; its own issue |
| `prompt_cache_options:{mode:"explicit"}` + breakpoints | ✗ | compat-flagged even in omp; key-only is the safe half |
| `auto` retention | collapses to short | tier-aware TTLs need the subscription tier |

`XDEV_CACHE_RETENTION` (wins) / `PI_CACHE_RETENTION` (compat) take
`short|long|none`; `none` drops both markers and the affinity key. Read per call
rather than `sync.OnceValue`: a process-lifetime cache made the override
untestable, and it is two map lookups once per turn. Env-only by necessity —
`internal/ai` cannot import `internal/config` (`config` imports `ai`), the same
seam `provider.go` already notes for `installIdentity`.

**Compaction stays unmarked on purpose.** `summarizeWith` sends
`System: compactionPrompt` over a flattened transcript — it shares no prefix with
the live chain, so caching it would be a fresh write with no reader. The handoff
summarize call, which does go through `liveRequest` and share the live prefix, is
the one that opts in as a side request.

## 4. Checks

`internal/ai/cache_test.go` — 10 cases, all offline (bodies decoded from the test
server, or `buildRequest` directly): marker placement on system/tools/tail; the
4-marker cap; the pre-cache shape when no key is set; `ttl:"1h"` under long
retention; nothing at all under `none`; the reject-then-retry-then-latch sequence
including that a **later** turn stops sending markers; hash stability/collision;
the host gate on both OpenAI wires; `thinking` never marked.

`go build ./... && go vet ./internal/ai/ ./internal/agent/ ./cmd/xdev/ && go test
./internal/ai/ ./internal/agent/ ./internal/stats/ ./cmd/xdev/` — green.

## 5. Known ceiling

- **A marker's span is keyed by prefix content**, so anything that changes the
  system prompt or the tool table invalidates it; `/switch` and `/fork` move the
  store id too, which changes the OpenAI affinity bucket along with it. Nothing
  detects *silent* invalidation (a prefix that should have hit and did not), which
  is why the hit ratio is now visible in `/usage`. omp forwards
  `options.promptCacheKey ?? sessionId` the same way — no fork-inheritance
  identifier exists in the bundle — so this matches rather than diverges.
- **No per-model TTL capability probe**: `long` sends `ttl:"1h"` and takes the
  400 → latch → keyless fallback path if the model lacks
  `extended-cache-ttl-2025-04-11`. Default `short` never does.
- **Side requests are coarse**: a boolean stop point. omp has no side-request
  flag either — it routes them through the *same* session key by design, and puts
  the fine grain in the compat table instead
  (`promptCacheMinimumTokens: 1024`, `promptCacheMaximumCheckpoints: 4`,
  `promptCacheMode: "explicit"`, per provider class from `classes/*.kdl`), which
  xdev has no equivalent of. Its cache bench harness is the one place that
  distinguishes cold from warm (`coldAlreadyWarm`), reporting cache-read/write and
  TTFT per phase.
- **Google/Bedrock/gRPC untouched**: they receive a field they ignore.
- **Anthropic thinking carry** (#137) remains the one cache breaker this change
  does not address, and the next thing to look at if hit rates disappoint.
