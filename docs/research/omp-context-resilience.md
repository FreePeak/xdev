# omp — model switching, context handling & API-error resilience (primary-source deep dive)

Sourced from the installed omp build at `~/.bun/bin/omp` → `~/node_modules/@oh-my-pi/pi-coding-agent` (which ships full TS source under `src/`, not just `dist/`), sibling packages `pi-agent-core` / `pi-ai` (also with `src/`), and live state under `~/.omp/agent/` (config.yml, models.yml, agent.db, 147 session dirs). Symbol-level claims cite `src/...` paths; JSONL samples are verbatim from real sessions on this machine.

Extends **docs/research/2026-09-09-omp-pi-architecture-go-rebuild.md** (the blueprint) — that doc states *that* these mechanisms exist; this doc supplies the *mechanics* needed to port them: thresholds, budgets, state machines, fallback ladders, and the failure-mode matrix. **Deltas vs prior docs** are called out inline and summarized in §0.2.

## 0. Sources and deltas

### 0.1 Primary sources used here (new evidence, not in the blueprint)

| Source | Path | What it proves |
|---|---|---|
| TurnRecovery class | `pi-coding-agent/src/session/turn-recovery.ts` (2,409 ln) | retry loop, fallback chains, empty-stop, usage preflight |
| SessionMaintenance | `pi-coding-agent/src/session/session-maintenance.ts` (4,028 ln) | compaction triggers, promotion, shake dead-loop guards, speculation |
| ModelControls | `pi-coding-agent/src/session/model-controls.ts` (767 ln) | setModel pipeline, role cycling, thinking/service tiers |
| PrewalkCoordinator | `pi-coding-agent/src/session/prewalk.ts` (348 ln) | plan→implement handoff incl. verbatim prompts |
| RetryFallbackChains | `pi-coding-agent/src/session/retry-fallback-chains.ts` (476 ln) | chain config grammar + resolution specificity |
| StreamGuards/LoopGuards | `pi-coding-agent/src/session/stream-guards.ts` (491 ln) | streaming edit abort, Gemini header runaway, tool-loop redirect |
| Exit forensics | `pi-coding-agent/src/session/exit-diagnostics.ts` (310 ln) | `session_exit`/`tool_execution_start`, synthetic abort message |
| AppendOnlyContextManager | `pi-agent-core/src/append-only-context.ts` | byte-stable prefix + append-only log, model-change invalidation |
| agent-loop partial-stream recovery | `pi-agent-core/src/agent-loop.ts` (~2,090+ ln) | `stream_interrupted_after_content`, `recoverTransientErrorToolTurn` |
| Watchdogs | `pi-ai/src/utils/idle-iterator.ts`, `sdk-stream-timeout.ts` | first-event/idle watchdog constants and racer design |
| Error taxonomy | `pi-ai/src/error/flags.ts`, `classes.ts`, `rate-limit.ts`, `auth-classify.ts`, `oauth.ts` | Flag bitmask, overflow patterns, rotation predicates |
| Auth retry | `pi-ai/src/auth-retry.ts`, `pi-ai/src/oneshot-retry.ts` | a/b/c credential policy, oneshot retry contract |
| Settings schema | `pi-coding-agent/src/config/settings-schema.ts` (6,270+ ln) | every default quoted below |
| Session loader/persistence | `session-loader.ts`, `session-persistence.ts`, `session-storage.ts` | streaming JSONL load, atomic tmp+rename writes |
| Live state | `~/.omp/agent/{config.yml,models.yml,agent.db,sessions/**}` | role config, `model_usage`/`auth_credential_blocks` tables, `model_change`/`compaction`/`branch_summary` JSONL samples |

### 0.2 Deltas vs the blueprint and parity docs

- **NEW — full failover ladder**: parity-tools-providers.md mentions context promotion and models.yml; the blueprint's retry paragraph is one sentence. This doc adds the complete `retry.fallbackChains` grammar (role/model/wildcard keys, `provider/*` provider-swap vs `provider/prefix/*` id-reprefix semantics), chain-key resolution specificity, candidate filters (context-fit, effort ceiling, Anthropic thinking-signature reuse veto), `served`/attribution bookkeeping, cooldown suppression, and `cooldown-expiry` primary restoration (§1.6).
- **NEW — numeric budgets**: `retry.maxRetries=10` (this machine: `999999`), `baseDelayMs=500` → capped 8 s exponential with 25 % downward jitter, fail-fast `maxDelayMs=5 min`, watchdogs 300 s first-event / 300 s idle (env + settings overrides), compaction `keepRecentTokens=20000`, `reserveTokens` default 16384 with 15 % proportional floor, `MAX_SUMMARY_TOKENS=16384`, idle compaction `idleTimeoutSeconds=300` / `idleThresholdTokens=200000` (§1.6, §2.4, §3.2–3.3).
- **NEW — error taxonomy to port**: `pi-ai` Flag bitmask (19 flags), `RETRIABLE_KINDS`, 24 provider-specific context-overflow regexes, payload-413 vs token-overflow split (#9235), `stream stall` / HTTP2 reset / premature-close classifiers, `stream_interrupted_after_content` partial-stream recovery, synthetic tool results (`__synthetic`, `executed:false`) (§3.1–3.4).
- **NEW — credential/quota machinery**: central a/b/c auth-retry policy (`AUTH_RETRY_MAX_ATTEMPTS=64`), `markUsageLimitReached`/`rotateSessionCredential`/`getModelUsageHealth`, usage-aware fallback with `fail-closed|confirm|auto` reserve policies, Codex auto-reset redemption, and the live `auth_credential_blocks` table (with `block_scope` + Codex shared-pool mirror triggers) from `agent.db` (§1.7, §3.7).
- **NEW — partial-stream + aborted-turn semantics**: `retainCompletedToolCalls`, `recoverTransientErrorToolTurn` (rewrites a failed stream into a `toolUse` stop and *continues*), tool-scoped abort reasons, `isSyntheticToolResultMessage` replay proof, preserved-turn continuation for stall/reset/premature-close (§3.4).
- **NEW — context-reconstruction exactness**: emission-boundary precedence (`reset_boundary` vs latest `compaction`), the `hasExplicitDefaultModel` guard (#849), retry-scrubbed rebuilds (`retryRecovery`/empty-error filtering), remote-compaction replacement-history payloads, snapcompact archive re-attach, `cacheMissExplainedAt` transcript mode (§2.2).
- **NEW — compaction method ladder**: `methodOrder` default `[remote, snapcompact, handoff, shake, soft]` with per-method auto-advance on failure, 80 % recovery band + dead-loop guards (#2119/#2275), mid-turn dead-end parking with re-arm, speculative armed summaries + grace band (§2.4–2.5).
- **NEW — resume forensics detail**: exact conditions under which a resumed session synthesizes an aborted assistant record; `collectPendingToolCalls` state machine over `tool_execution_start` markers; breadcrumb resume quirks (fresh-marker honoring, stale-crumb re-root) (§4).
- **CONFIRMED with source**: blueprint's claims about `model_change`/`branch_summary`/`reset_boundary` entries, watchdogs, `session_exit` forensics, promotion-before-compaction — verified at symbol level below, now with the exact fields a port must write.
- **CORRECTION**: `pi-ai` `types.d.ts` comments say the first-event watchdog "falls back … to a 100s default"; the shipped constant is **300 s** (`DEFAULT_STREAM_FIRST_EVENT_TIMEOUT_MS = 300_000`, `idle-iterator.ts:8-9`). Port from constants, not comments.

---

## 1. Model switching

### 1.1 Roles, aliases, `:effort` suffixes — resolution grammar

Config (live `~/.omp/agent/config.yml`):

```yaml
modelRoles:
  default: onegw/free
  smol: onegw/free
  slow: onegw/free
  plan: onegw/dev:auto      # role + explicit thinking effort
  advisor: onegw/dev:auto
  task: onegw/free
  vision: onegw/dev:auto
  designer: onegw/free
  commit: onegw/free
  tiny: onegw/free
```

- Role ids (`model-roles.ts`): `default, smol, slow, vision, plan, designer, commit, tiny, task, advisor`; canonical alias prefix `@` (`@smol`), legacy `pi/` prefix accepted, `*` = `@default`. Role aliases resolve recursively through `expandRoleAlias` with a `visited` set (self/cross aliases like `modelRoles.default = "@smol"` must not loop — `model-resolver.ts:1053-1070`).
- Selector grammar (`parseModelString`, `model-resolver.ts:183-206`): `provider/model-id:effort` — the `:effort` suffix (incl. `auto` and a context-aware `:max` alias that loses to a *literal* model id ending in `:max`) is stripped first; `provider/model@upstream` routes through a single upstream (`openrouter/z-ai/glm-4.7@cerebras`) via `splitUpstreamRouting`, guarded so ids that legitimately contain `@` (`claude-opus-4-8@default`) are never split.
- Resolution of a role value (`resolveModelRoleValue`, `model-resolver.ts:1287+`): expand alias → comma-separated fallback patterns → match against available models with preference context (provider/model priority rank from `pi-catalog`). Returns `{model, thinkingLevel, explicitThinkingLevel, warning}`; an explicit per-role thinking level rides along so role application re-applies it (e.g. `plan: onegw/dev:auto` → thinking `auto`).
- `DEFAULT_PREWALK_TARGET = "@smol"` (`model-resolver.ts:1208`).

### 1.2 The switch pipeline (`ModelControls.setModel` → session funnel)

`model-controls.ts:224-250` — every explicit switch (slash `/model`, alt+P temp, role cycle, plan slider) runs the same pipeline:

1. **Auth preflight** (synchronous, no OAuth refresh): `modelRegistry.hasConfiguredAuth(model)` else throw `No API key for {provider}/{id}`.
2. `refreshSelectedModelMetadata(model)` — re-read live metadata (context window etc.).
3. `clearSuppressedSelector(...)` (cooldown suppression from §1.6 cleared for the target).
4. `clearActiveRetryFallback()` — an explicit user switch always cancels fallback attribution/pending restore.
5. **`#setModelWithProviderSessionReset(model)`** (`agent-session.ts:7613-7663`) — the single funnel:
   - `#closeProviderSessionsForModelSwitch(current, next)`: drop cached provider sessions for `openai-codex-responses` (either side of the switch) and `openai-responses:{provider}` for both old and new provider — server-side conversation state is not carried across switches.
   - `clearInheritedProviderPromptCacheKey()` when the model actually changes (§1.8).
   - `agent.setModel(model)`; if changing, emit `model_changed` (synchronous fan-out; **not** an extension event, deliberately, so error-path fallback switches don't pay extension-await latency).
   - `#syncAppendOnlyContext(model)`: enable/disable/re-`invalidateForModelChange()` the AppendOnlyContextManager per `provider.appendOnlyContext` (default `"auto"`).
   - Reconcile code-mode, `inspect_image`, and `think` tool availability against the new model's capabilities (fallback switches bypass `syncAfterModelChange`, so this central reconcile keeps the tool slate fresh).
6. **`sessionManager.appendModelChange("provider/id", role)`** — the session-tree record (§1.3).
7. If `options.persist` → `settings.setModelRole(role, formatRoleModelValue(...))` (preserving an existing explicit thinking selector for the role) + `recordModelUsage("provider/id")` → **live table `agent.db model_usage(model_key PK, last_used_at)`** (verified: `onegw/free | 1788935535`).
8. Re-apply thinking: prefer the target model's `thinking.defaultLevel`, else preserve current level (or `auto`); `syncAfterModelChange` re-derives edit mode.

`setModelTemporary` is the same minus settings persistence, and records role `"temporary"` — or `"fallback"` (`EPHEMERAL_MODEL_CHANGE_ROLE`) for recovery-driven switches (§1.3).

### 1.3 Session-tree records: `model_change` variants (with live evidence)

Entry type (`pi-agent-core/src/compaction/entries.d.ts`):

```ts
export interface ModelChangeEntry extends SessionEntryBase {
    type: "model_change";
    model: string;   // "provider/modelId"
    role?: string;   // "default", "smol", ... ; undefined ⇒ "default"
}
```

plus a persisted `resolvedModelIsFallback?: boolean` (present in every sample). Role values actually written on this machine (grep over 147 session dirs, 1,256 `model_change` entries):

```jsonl
{"type":"model_change","id":"e6343d99","parentId":null,"timestamp":"2026-09-09T02:15:12.587Z","model":"onegw/free","resolvedModelIsFallback":false}
{"type":"model_change","id":"268ffe93","parentId":"ed693a5e","timestamp":"2026-09-07T09:55:55.893Z","model":"router/free","role":"fallback","resolvedModelIsFallback":false}
{"type":"model_change","id":"9ba6bc65",...,"model":"omniroute/cmd/minimax/minimax-m3-free","role":"fallback","resolvedModelIsFallback":true}
```

- No `role` → user cycled model (`Ctrl+P` scoped or available-model cycle; `model-controls.ts:439,470`).
- `role:"default"` (or a named role) → explicit `/model <@role>` with settings persistence.
- `role:"temporary"` → alt+P temp switch; `role:"fallback"` = `EPHEMERAL_MODEL_CHANGE_ROLE` (`session-entries.ts:26`) → retry-fallback / Fireworks-Fast degrade / primary restore; `resolvedModelIsFallback: true` marks the two *apply-fallback* writes (`turn-recovery.ts:1620,1788`), while primary restoration writes role `fallback` without the flag (`turn-recovery.ts:1852`).
- **Resume consumption**: `buildSessionContext` rebuilds `models[role]` from the path; `getLastModelChangeRole()` picks which entry is "the active role", and `getRestorableSessionModels` returns `[roleModel]` or `[roleModel, defaultModel]` fallback order (`session-context.ts:86-103`). `EPHEMERAL_MODEL_CHANGE_ROLE` is explicitly *not* restored as a role (`session-context.ts:94`) — a fallback swap resumes as the default model, not as a fake role. The `hasExplicitDefaultModel` guard (§2.2) stops legacy assistant-message inference from clobbering a user pick (#849).

### 1.4 Mid-session switch surfaces

- **`/model <selector|@role>`** → `setModel(..., role, {selector, thinkingLevel, persist})`.
- **`Ctrl+P` model cycle** — `#getScopedModelsWithApiKey()` filters the `--models` scope by per-provider credential availability, cycles, and writes a role-less `model_change` (`model-controls.ts:392-446`); without a scope it cycles all available models.
- **Role cycle** (`getRoleModelCycle`/`cycleRoleModels`, `model-controls.ts:313-389`): resolves the configured role order, drops roles with no model/unavailable model, computes `currentIndex` from `getLastModelChangeRole()` — **trusted only while its resolved model still equals the active model** (stale recorded role ⇒ match by model equality ⇒ index 0) — and applies via `applyRoleModel` (which re-applies an explicit role thinking level).
- **`/fast`** — per-family service tiers (`#serviceTierByFamily`); `isFastModeActive` requires `realizesPriorityServiceTier(effectiveServiceTier(model), model)`; the session-level `agent.serviceTierResolver = model => #models.effectiveServiceTier(model)` scopes priority to a family without mutating the shared session tier (`agent-session.ts:1226-1228`); Anthropic models that report `disabledFeatures: ["priority"]` auto-downgrade the family tier with a notice (`agent-session.ts:2829-2836`).
- **Thinking**: `cycleThinkingLevel` order `off → auto → minimal..max → off`; `auto` = per-turn classification (`applyAutoThinkingLevel`, 4 s timeout/abort, `ultrathink` keyword bypasses the classifier to `Effort.Max`, result dropped if the turn generation changed); every change appends `thinking_level_change` with both concrete + `configured:"auto"` fields (§2.2 explains why `configured` is load-bearing on resume).

### 1.5 Prewalk handoff (big → @smol) — verbatim mechanics

`PrewalkCoordinator` (`prewalk.ts`) implements the one-way "plan on the big model, implement on @smol" flow. Config: `prewalk.enabled: true` (live config.yml), `DEFAULT_PREWALK_TARGET="@smol"`.

- **Armed at startup** with `{target: @smol-resolved, thinkingLevel?}` (or explicitly via `armPrewalk()`). Arming immediately steers a hidden `prewalk-plan` custom message (customType `prewalk-plan-message`, `display:false`, `attribution:"agent"`):

  ```
  STOP: In NEXT reply, before further exploration, write complete plan. Enough known; do not defer.
  Plan first; explicit, comprehensive; reference for remainder: …
  Then, same reply and only after complete plan, use todo tool to capture 5–9 items …
  Checkpoint, not final answer: after todo list, continue task; do not stop on plan alone.
  ```

- **`advanceAtTurnEnd`** (wired to the agent's turn-end hook, `agent-session.ts:1273-1277`):
  1. Skip unless an assistant turn just ended. Noop check `prewalkWouldBeNoop` disarms when target == current model+effort.
  2. **Todo gate**: `todoSeen` set when a successful `todo` tool result lands; if the `todo` tool isn't active the gate opens by default.
  3. Until the gate opens, the first turn injects the plan nudge; a following turn with tool results sets `continuePending`, then steers the `prewalk-continue` prompt ("Continue task now; do not end turn here.").
  4. **Handoff trigger**: the first completed tool result satisfying `isPrewalkImplementationAction` — `edit`/`write` always; an `xd://` device dispatch counts only when its approval `tier` is `write`/`exec` (read-tier device calls like LSP/debug/ast_edit-on-internal-URLs must not flip models mid-investigation, #7312).
  5. **Persistence barrier**: `waitForSessionMessagePersistence` for the assistant message and *every* tool result — the switch must never outrun the journal.
  6. `#scrubPlanNudge` removes the hidden plan message from live context **and** agent state (`invalidateMessageCache` + splice/replaceMessages) so the prompt engineering doesn't pollute the implementation model's prefix.
  7. `setModelTemporary(target, thinkingLevel, {ephemeral:true})` → writes `model_change` with role `EPHEMERAL_MODEL_CHANGE_ROLE` (`model-controls.ts:281`), then steers the `prewalk-checklist` prompt (consistency/scope/verification checklist, `prompts/system/prewalk-checklist.md`).
- Plan-yolo is a sibling flow on the same coordinator (`planYolo`), reusing the same inject/scrub machinery.

### 1.6 Failover chains (`retry.fallbackChains`) — the when-provider-X-errors→model-Y ladder

Config (settings-schema 1867-1884; live config has `fallbackChains: {}`):

```yaml
retry:
  enabled: true
  maxRetries: 10            # this machine: 999999
  baseDelayMs: 500
  maxDelayMs: 300000        # fail-fast ceiling (0 = unlimited; this machine: 0)
  modelFallback: true
  fallbackRevertPolicy: cooldown-expiry   # or "never"
  usageAwareFallback: false # + usageReservePct:10, usageReservePolicy: confirm
  fallbackChains:
    default: ["@smol", "providerA/*"]         # role key
    openrouter/google/gemini-*: [...]         # exact-model / wildcard keys
    anthropic/*: ["bedrock-mantle/*"]         # provider wildcard
```

**Key grammar** (`retry-fallback-chains.ts`): keys are role names, exact model selectors, or wildcards. Wildcards: `provider/*` (swap provider, keep failing id: `google-antigravity/x → google/x`) and `provider/prefix/*` (re-prefix bare id: `openrouter/google/* : google-antigravity/x → openrouter/google/x`); bare id carried across aggregator→direct when the target lacks the prefixed id (`openrouter/google/x → google-vertex/x`). Chain *entries* accept the same wildcards.

**Chain-key resolution specificity** (`resolveRetryFallbackChainKey`, lines 415-462): (1) exact model-selector keys, (2) longest matching provider wildcard (id-prefix beats plain `provider/*`), (3) hinted role (from `getLastModelChangeRole()` if its configured model still equals the live model), then role keys whose assigned model matches with `default` preferred, (4) the `default` chain when `default` has no explicit primary. `expandDefaultRetryFallbackChains` copies the `default` chain onto every other configured role that lacks one.

**Candidate filters** (`#tryRetryModelFallback`, `turn-recovery.ts:1641-1693`):
- cooldown suppression (`noteRetryFallbackCooldown` pins a selector for the provider's retry-after window),
- **Anthropic thinking-signature veto**: a same-provider candidate different from the latest assistant model is skipped if that assistant carries signed `thinking`/`redactedThinking` blocks (signatures are model-bound; byte-identical replay impossible) — cross-provider candidates remain eligible,
- effort-floor vs per-spawn ceiling (`modelSupportsEffortCeiling`),
- **context fit**: `contextFitsModel(candidate, failedMessage)` — the failed assistant is excluded because retry drops it first (#8065),
- credential existence via `getApiKey`.

**Application** (`applyRetryFallbackCandidate`, lines 1567-1638): capture `configuredThinkingLevel` (auto-aware), clamp the fallback's level to the ceiling, `#markFallbackRouted()` **before** `setModelWithProviderSessionReset` (listeners in the `model_changed` window must see fallback routing immediately), abort-window rollback restores the previous model, then writes `model_change` `(candidateSelector, EPHEMERAL_MODEL_CHANGE_ROLE, resolvedModelIsFallback:true)`, re-applies thinking, records model usage, emits `retry_fallback_applied {from, to, role}`. `ActiveRetryFallbackState.served` is set only after a turn *settles* on the fallback (attribution: `#lastServed`/`ServingModel{selector, isFallback}`), and `reanchorServedAttribution` re-binds on session-id change.

**Primary restoration** (`#maybeRestoreRetryFallbackPrimary`): policy `cooldown-expiry` (default) restores the original selector+thinking (carrying the fallback's level only if the user changed it meanwhile) once the original is no longer suppressed; `pinned` fallbacks (classifier refusals) never auto-restore; `never` disables restoration. Restoration writes `model_change(primary, EPHEMERAL_MODEL_CHANGE_ROLE)` and callers re-run the pre-send context-fit check against the (possibly smaller) primary window.

**Retry budget interplay** (`#handleRetryableError`, §3.3): fallback model switch ⇒ `#retryAttempt = 1` (fresh budget); credential rotation keeps the cumulative count but bypasses the same-route budget (every distinct account must be tried); a Fireworks `-fast` variant degrades to its base id on pre-content hard/transient failures regardless of `retry.enabled` (`isFireworksFastFallbackEligible`, `#tryFireworksFastFallback`); hard non-retryable errors still consult the chain (`isHardErrorFallbackEligible`) unless abort-flavored, overflow, refusal, or replay-unsafe; a thinking-loop abort is explicitly *not* a fallback signal (#8760 — it needs the same model + redirect notice).

### 1.7 Credential rotation, usage-aware fallback, Codex resets

- **a/b/c resolver contract** (`pi-ai/src/auth-retry.ts`): (a) initial resolve; (b) on auth error, force-refresh the *same* account; (c) rotate to a sibling account. Bounded by `AUTH_RETRY_MAX_ATTEMPTS = 64`. `isAuthRetryableError` (`error/auth-classify.ts`): typed token-refresh, 401, 403 (plan/org denial a sibling may not share), account-policy denial (`cyber_policy`), body-classified usage limits, invalidated-oauth-token text; transient per-minute 429s stay in the upstream-backoff lane.
- **Usage-limit latching**: `recordUsageLimitOutcome` (`turn-recovery.ts:472-494`) → `authStorage.markUsageLimitReached(provider, sessionId, {retryAfterMs, baseUrl, modelId})` → the retry waits for `min(provider window, earliest sibling unblock + 1 s buffer)` instead of blindly sleeping the provider window. `parsedRetryAfterMs` hierarchy: `retry-after-ms: N` → `retry-after: seconds|HTTP-date` → `extractRetryHint` → `x-ratelimit-reset-ms` / `x-ratelimit-reset` (epoch-ms/date arithmetic for absolute values).
- **Live persistence** (`agent.db`): `auth_credential_blocks(credential_id, provider_key, block_scope, blocked_until_ms)` with an `idx_auth_credential_blocks_expires` index **and mirror triggers** — a Codex `shared`-scope block insert is mirrored into metering tables via `auth_credential_block_mirror_guard`, i.e. block state is durable and shared across processes. `usage_history`, `model_usage`, `model_perf` tables back `getModelUsageHealth`.
- **Usage-aware fallback** (`#maybeApplyUsageAwareFallback`, `turn-recovery.ts:1392-1520`, default off): pre-turn `getModelUsageHealth` probe → `healthy|reserve|depleted|unknown`; `usageReservePolicy`: `confirm` (interactive; background agents auto-fallback), `auto` (always fall back), `fail-closed` (throw `Usage preflight blocked: ...` — the `#isUsagePreflightBlocked` predicate then makes the error non-retryable); candidates additionally re-probed healthy and context-fitting; `releaseSessionCredentialForReselection` re-ranks an unhealthy selected account while the pool is healthy. Probe failures **fail open** (logged, no fallback).
- **Codex auto-reset**: `#maybeAutoRedeemCodexReset(activeBlockUnblockAtMs)` spends a saved reset to unblock the pool mid-retry (`codex-auto-reset.ts`, host method `agent-session.ts:1163`), consulted in the retry loop before falling back to waits.

### 1.8 `providerPromptCacheKey` implications when switching

- Requests forward `promptCacheKey: this.agent.promptCacheKey ?? this.agent.sessionId` (`agent-session.ts:7969`); Codex/Responses transports derive `prompt_cache_key`/routing session id from the pair (`pi-agent-core/src/compaction/compaction-v2-streaming.ts:315-317,398-400`).
- `/fork` inherits the parent's cache key (`providerPromptCacheKeySource === "fork"` → `#inheritedProviderPromptCacheKey`, `agent-session.ts:1436-1437`); `#adoptInheritedProviderPromptCacheKey` only adopts when inherited-or-unset (never stomps an explicit one); inheritance is **dropped** when the fork overrides model/thinking/system-prompt/tools (mechanism: `clearInheritedProviderPromptCacheKey` on each of those).
- **Model change ⇒ cache-key reset + append-only invalidation**: `#clearInheritedProviderPromptCacheKey` on any real model change inside `#setModelWithProviderSessionReset`; `AppendOnlyContextManager.invalidateForModelChange()` (prefix rebuild + log clear, `append-only-context.ts:259-263`) — the next turn is a cold prefix by construction; the transcript's `cacheMissExplainedAt` explains the miss in `/dump` output (§2.2).
- **Thinking-level change** likewise clears the inherited key (`setThinkingLevel` → `#host.clearInheritedProviderPromptCacheKey()` on every auto↔concrete transition) — a `reasoning.effort` change invalidates provider-side cache reuse.
- Compaction/handoff/branch-summary oneshots deliberately *reuse* the live `sessionId`/`promptCacheKey` pair (`SummaryOptions.sessionId/promptCacheKey`, `HandoffFromContextOptions.streamOptions`) so remote compaction and handoff READ the warm prefix cache.
- Provider-session state (`providerSessionState` map) is closed for old/new provider on switch, on Responses stale-replay errors (`#resetCurrentResponsesProviderSession`), on history rewrites (Codex `#closeCodexProviderSessionsForHistoryRewrite`), and on `/fresh`/`/clear`/`freshSession()` (`agent-session.ts:4336-4346, 7636-7663`).

### 1.9 What a switch preserves (context/memory survival)

Session-tree state (`model_change`, `thinking_level_change` incl. `configured:"auto"`, `service_tier_change`, `ttsr_injection`, `mode_change`) is untouched by switches; `buildSessionContext` re-derives it. Per-run custom messages (`prewalk-plan` nudge) are scrubbed instead of kept. Memory backend state is keyed by session id (`rekeyForCurrentSessionId`) and unaffected by model switches; only `/fresh`/`/clear`/fork re-key or reset it (§2.6).

---

## 2. Context handling

### 2.1 The JSONL tree — entry catalog as persisted

Header line 1 is a **fixed-width title slot** (non-JSON, peeled by the loader, `session-loader.ts:79-93`), then `{"type":"session","version":3,...}`. Entry types (from `entries.d.ts` + live samples):

| Entry | Fields written | Consumed by |
|---|---|---|
| `message` | full `AgentMessage` (assistant incl. `usage`, `cost`, `stopReason`, `stopDetails`, `retryRecovery?`) | context + transcript |
| `model_change` | `model`, `role?`, `resolvedModelIsFallback?` | role restore, chain role hint, attribution |
| `thinking_level_change` | `thinkingLevel?`, `configured?` (`"auto"`) | thinking restore (auto survives resume) |
| `service_tier_change` | `serviceTier: ServiceTierByFamily\|null` | `/fast` state restore |
| `compaction` | `summary`, `shortSummary?`, `firstKeptEntryId`, `tokensBefore`, `tokensAfter?`, `method?`, `details?`, `preserveData?` (incl. `openaiRemoteCompaction` payload, snapcompact archive), `fromExtension?`, `warning?` | context rebuild |
| `branch_summary` | `fromId`, `summary`, `details?` (file ops), `fromExtension?` | abandoned-branch context |
| `custom_message` / `custom` | `customType`, `content`/`data`, `display`, `details?`, `attribution?` | lifecycle markers (`tool_execution_start`, `session_exit`, `thinking-loop-redirect`, `prewalk-*`, `gemini-tool-call-reminder`, `accepted-terminal-empty-stop`, …) |
| `reset_boundary` | none | `/clear` context boundary |
| `label` / `title_change` / `ttsr_injection` / `session_init` / `mode_change` | — | tree UX, TTSR rules, plan mode |

Real compaction entry (session `2026-09-04T06-45-50…jsonl`): `{"type":"compaction","id":"875e8194",...,"firstKeptEntryId":"6b1da7c3","tokensBefore":81618,...,"method":"handoff","details":{...},"fromExtension":false}`. Real branch summary: `{"type":"branch_summary","fromId":"cbd44e08","summary":"","details":{"kind":"discarded-entry-branch","discardedEntryId":"df322ea0"}}`.

Counts across this machine's sessions: `model_change` in 1,256 files, `thinking_level_change` 314, `service_tier_change` 50, `compaction` 27, `branch_summary` 40, `tool_execution_start`/`session_exit` 0 (features landed in recent builds — the live corpus predates them; the source paths below are the authority).

### 2.2 `buildSessionContext` — context reconstruction from the tree

`session-context.ts:174-599` (pure function; `agent-session.buildDisplaySessionContext` wraps it):

1. **Leaf + cycle-bounded path walk** (`parentId` → root, `seenPathIds` stops corrupt cycles), reversed.
2. **State derivation over the path**: `thinkingLevel`/`configuredThinkingLevel` from the last `thinking_level_change` (default `"off"`); `models[role]` from `model_change` entries with the `hasExplicitDefaultModel` guard — assistant-message model inference only fills `models.default` **before** the first explicit `role:"default"` `model_change`, so temporary fallbacks, promotions, and server-side model downgrades stamped on assistant messages can't clobber the user pick (#849); `serviceTier`; `injectedTtsrRules` (set-union); `mode`/`modeData` from `mode_change`.
3. **Emission boundary** — precedence:
   - `transcript && !collapseCompactedHistory` (full export): every entry, compactions inline as dividers, nothing elided — durable pre-reset history stays on disk.
   - `reset_boundary` **after** the latest compaction ⇒ emit only entries after the boundary (for *both* the collapsed live transcript and the model-context rebuild that feeds `agent.replaceMessages` on resume/`/shake`/reload/image-drop).
   - latest `compaction` ⇒ emit `compaction` summary first (as `compaction-summary` message carrying `providerPayload`/snapcompact `blocks`/`warning`/`method`/`tokensAfter`), then kept messages from `firstKeptEntryId` forward, then post-compaction entries. With `preserveData.openaiRemoteCompaction`, kept turns are replaced by the provider-native `openaiResponsesHistory` payload (LLM context only; visible transcript still renders kept rows).
4. **Filters (model-context mode only)**: assistant entries carrying `retryRecovery` (latched auto-retry bookkeeping) or qualifying as `isEmptyErrorTurn` are skipped; hidden `prewalk-plan` custom messages are skipped (live-run steering only, `isPrewalkPlanNudge`); `branch_summary` entries become `branch-summary` messages.
5. **Transcript mode extras**: `cacheMissExplainedAt[]` parallel array marks assistant turns that immediately follow a compaction/model_change/plan-transition (`pendingReset`) or a model change — the status line explains provider cache misses rather than hiding them.
6. **Snapcompact re-attach**: the preserved snapcompact `Archive` in `preserveData` is re-rendered as `historyBlocks` on every rebuild (with a crash-risk guard dropping legacy oversized frame payloads, `session-context.ts:26-59`) so archived history stays readable after compaction.

### 2.3 Append-only context mode (prefix-cache stability)

`pi-agent-core/src/append-only-context.ts`: `StablePrefix` freezes system prompt + tool specs behind a fingerprint (rebuilt only on `invalidate()` — e.g. MCP reconnect, model switch); `AppendOnlyLog` is append-only with `replaceTail` reserved for compaction. `syncMessages` keeps the byte-stable prefix on in-place rewrites (per-turn pruning, transformContext re-render, image strip) by finding the longest stable prefix and truncating to it (#3406: avoided ~40k-token full re-prefill per turn on llama.cpp backends). `invalidateForModelChange()` resets both. Enabled per provider via `provider.appendOnlyContext` (`"auto"`) in `#syncAppendOnlyContext`; `buildSessionContext`'s rebuild feeds `agent.replaceMessages`, after which the manager re-syncs (first turn after a resume is a guaranteed cache miss — `cacheMissExplainedAt` says so).

### 2.4 Compaction — thresholds, methods, cut points, speculation

**Settings** (settings-schema 2478-2680; engine constants in `pi-agent-core/compaction/compaction.ts`):

| Setting | Default | Notes |
|---|---|---|
| `compaction.enabled` | `true` | |
| `compaction.thresholdPercent` | `-1` | `-1` ⇒ legacy reserve-based threshold |
| `compaction.thresholdTokens` | unset | fixed limit takes priority over % |
| `compaction.keepRecentTokens` | `20000` | kept tail target |
| `compaction.reserveTokens` | unset ⇒ `DEFAULT_RESERVE_TOKENS = 16384` | effective reserve ≥ 15 % of window (`effectiveReserveTokens`); small windows recover to the proportional floor (`resolveBudgetReserveTokens`) |
| `compaction.midTurnEnabled` | `true` | compact inside tool loops |
| `compaction.methodOrder` | `[remote, snapcompact, handoff, shake, soft]` | ordered ladder, per-method fallback |
| `compaction.asyncEnabled` | `true` | speculative pre-threshold runs |
| `compaction.idleEnabled` | `false` | + `idleThresholdTokens: 200000`, `idleTimeoutSeconds: 300` |
| `compaction.autoContinue` | `true` | schedule agent-authored continuation |
| `compaction.handoffSaveToDisk` | `false` | write handoff doc to disk |
| `compaction.v2RetainedMessageBudget` | `64000` | remote streaming-v2 retained budget |

Threshold math (`resolveThresholdTokens`): fixed token clamp `[1, window-1]` > percent `floor(window·pct/100)` > legacy `window − reserve`. Trigger: `contextTokens > thresholdTokens` where `contextTokens = max(provider-reported, stored-conversation estimate)` (`compactionContextTokens` — a compression extension can deflate reported usage; the estimate floor keeps the trigger honest). `calculateContextTokens` prefers provider occupancy and excludes provider-orchestration tokens from context sizing.

**Cut point** (`findCutPoint`): walk backwards accumulating tokenizer-estimated sizes until ≥ `keepRecentTokens`; cuts only at user/assistant messages (never tool results); mid-turn cuts record `turnStartIndex` for split-turn handling (`isSplitTurn` → the pre-cut part becomes a *turn-prefix summary* via `generateTurnPrefixSummary`, OTEL kind `compaction_turn_prefix`).

**Method ladder** (`runAutoCompaction` → `resolveCompactionMethodOrder`): `remote` (provider-native OpenAI/Codex compaction incl. streaming-v2 with `v2RetainedMessageBudget`; gated by `shouldUseProviderNativeCompaction`), `snapcompact` (archive to dense bitmap frames the vision model reads back; no LLM call; frame budget `#computeSnapcompactMaxFrames`), `handoff` (generate handoff document via a **side request that mirrors the live turn's cache routing** — `streamOptions` carry apiKey/signal/sessionId/promptCacheKey/serviceTier/payload hooks so the oneshot reads the warm prefix), `shake` (local, drop recoverable heavy content: old tool outputs/images), `soft` (LLM summary with the active or `compactionModel` model). On any method failure the ladder advances: `auto_compaction_end {errorMessage: "...trying the next preferred compaction method"}` then recursive `runAutoCompaction(..., methodIndex+1)`. `NativeCompactionError` keeps auth-failure classification attached but **must not** fall through to another provider.
- **Summary budgets**: `MAX_SUMMARY_TOKENS = 16384`; summary budget = `floor(0.8 × reserveTokens)` (a 1M window authorizes ~120k-token summary — clamped hard). Effort: `resolveCompactionEffort` honors the session's configured thinking level (historical default `Effort.High` only when unset/inherit; `Off` omits reasoning).
- **Inner retries**: summarizer oneshots run with `oneshotRetry` (default enabled — manual `/compact` shouldn't die on one 529; `pi-ai/src/oneshot-retry.ts`: default 3 attempts, 500 ms base, 30 s max, abort-preserving); auto-compaction passes `false` because the outer ladder already retries (else 10×3 = 30 stacked requests).

**Speculative compaction** (`maybeStartSpeculativeCompaction`/`#runSpeculation`/`deferThresholdCompactionToSpeculation`): in the pre-threshold band `[threshold − lead, threshold)` start a background summary (LLM methods only) over a branch snapshot; the next real maintenance pass claims it (`#claimArmedSpeculation` with apply-time branch validation — stale results discarded) or defers within a grace band so mid-run tool loops keep moving; extensions with `session_before_compact` handlers disable speculation (veto preservation).

**Mid-turn maintenance** (`maintainContextMidRun`, lines 1611-1725): at safe tool-loop boundaries (turn-end hook, `willContinue`), decide from live context *before* awaiting the journal (an await per tool turn would stall the TUI); on threshold: grace band → `persistTurnMessagesForMidRunCompaction` barrier → dead-end parking check (`#midTurnCompactionDeadEnds` — a turn that couldn't be reduced is parked, but re-armed as soon as `prepareCompaction` finds a cut point, #7153) → `#promoteContextModel()` **before** compacting (long loops shouldn't compact on a model that deserves promotion) → `runAutoCompaction("threshold", {suppressContinuation: true, phase:"mid_turn"})` → splice compacted messages into the live array.

**Idle compaction** (`runIdleCompaction` → reason `"idle"`): timer-gated (300 s), exempt from the dead-loop fallbacks (a later idle tick re-checks usage).

### 2.5 Overflow, incomplete output, payload-413 — the recovery ladder

`checkCompaction` (`session-maintenance.ts:1741-1930`) runs post-turn and pre-prompt, four cases in order:

1. **Input overflow** (`sameModel && !errorIsFromBeforeCompaction && AIError.isContextOverflow(msg, window)`) → remove failed assistant from active context → **context promotion first** (`#tryContextPromotion` — switch to `model.contextPromotionTarget`, settings `contextPromotion.enabled` default `false`; drop the persisted dead turn, `scheduleAgentContinue({delayMs:100})`); no target ⇒ recovery compaction (`runRecoveryCompactionWithRollback("overflow", ...)` — restores the failed turn when no rewrite happens).
   - **Payload-shaped 413s are not token overflow**: `isPayloadRejection` (Flag.PayloadRejected) + trust heuristics (`reportedInputTokens ≤ window` AND `storedTokens < 80 %·window` ⇒ `trustedPayloadRejection`) ⇒ withhold from compaction, warn (bytes/media budgets can't be fixed by summarizing, #9235); ambiguous/untrusted ⇒ treat as overflow.
   - **Stale-error handling**: overflow errors from a *different* model than the live one are ignored (switch smaller→bigger mid-flight); overflow stamped by the failed model while the current model *is* its promotion target is still recovered (promotion raced the failing call).
2. **Threshold** (turn succeeded, context above threshold): supersede-reads + drop-useless pruning first (`#pruneToolOutputs`, `compaction.supersedeReads/dropUseless`), stale-usage guard (an assistant predating the last compaction carries pre-rewrite usage that would re-trip the threshold on the auto-continue, #3412), then maintenance with `phase: pre_turn/post_turn`.
3. **Output incomplete** (`stopReason:"length"`, e.g. OpenAI `response.incomplete`): input is fine — drop the dead turn, promotion first, else compaction/handoff (`runRecoveryCompactionWithRollback("incomplete", ...)`, forced inline).
4. **Shake fallback guards** (#2119/#2275): post-shake, if context is still above the 80 % recovery band (`COMPACTION_RECOVERY_BAND = 0.8`) with provider-anchored accounting (`triggerContextTokens − tokensFreed`), advance to the next method instead of spinning; `"idle"` exempt.

**Recovery compaction with rollback** (`#runRecoveryCompactionWithRollback`): if the pass can't rewrite history, the failed assistant turn is *restored* (`#restoreFailedAssistantTurn`) so the session keeps its error evidence; dead-end notice `usageOverflowDeadEndNotice` when provider usage proves overflow but no recovery exists (#9235).

### 2.6 Memory pipeline interaction

- **MEMORY.md injection**: the local backend (`memory-backend/local-backend.ts` → `memories/index.ts`) exposes `buildDeveloperInstructions(agentDir, settings, session)` — the system-prompt append section built from the memory root (`MEMORY.md`, `memory_summary.md`, skills). `SessionMemory` (`session-memory.ts`) owns backend transitions: `refreshBaseSystemPrompt()` on rebuilds, **per-turn memory promotion** via `beforeAgentStartPrompt` (the only hook that can affect the first answer of a fresh session) with a `capturePromotionSnapshot`/`restorePromotionSnapshot` rollback pair, `rekeyForCurrentSessionId()` on session switches/forks, `resetContextForNewTranscript()` on `/clear`/`/new`.
- **Compaction seam**: `MemoryBackend.preCompactionContext(messages, settings, session)` (`session-maintenance.ts:1104-1121`) appends backend context into the summarization prompt's `extraContext` — memories participate in what the summary preserves.
- **`memory://` seam**: the `read` tool resolves `memory://root`, `memory://root/MEMORY.md`, `memory://root/learned.md`, `memory://root/skills/<name>/SKILL.md`, `memory://<id>` (parity-knowledge-ui covers the URL grammar; confirmed at `tools/read.ts:2169`, `tools/memory-edit.ts:54`).
- Consolidation is background (startup two-phase: extraction on `default` role, consolidation on `smol`; lease+heartbeat in `agent.db jobs`), failures logged-and-swallowed — a broken memory backend must never break the loop (`MemoryBackend.start` doc: "MUST be non-throwing").

---

## 3. API error handling — full taxonomy

### 3.1 Classification (`pi-ai/src/error/flags.ts`)

```ts
export const Flag = {
  Class: 0x1000,
  ThinkingLoop: 0x0001_0000, Transient: 0x0002_0000, Timeout: 0x0004_0000,
  UsageLimit: 0x0008_0000, StaleResponsesItem: 0x0010_0000, MalformedFunctionCall: 0x0020_0000,
  ProviderFinishError: 0x0040_0000, EmptyResponse: 0x0000_2000, ContentBlocked: 0x0000_8000,
  AccountPolicy: 0x0000_4000, ContextOverflow: 0x0080_0000, AuthFailed: 0x0100_0000,
  SilentAbort: 0x0200_0000, UserInterrupt: 0x0400_0000, Abort: 0x0800_0000,
  Grammar: 0x1000_0000, FastModeUnsupported: 0x2000_0000, OAuthExpiry: 0x4000_0000,
  PayloadRejected: 0x8000_0000,
};
const RETRIABLE_KINDS = Transient | UsageLimit | ThinkingLoop | StaleResponsesItem
                      | ProviderFinishError | EmptyResponse;
// retriable(id): ContentBlocked → false; PayloadRejected → false;
//                MalformedFunctionCall → true; else RETRIABLE_KINDS.
```

- **Overflow detection**: 24 provider-specific regexes (`prompt is too long` Anthropic, `input is too long for requested model` Bedrock, `exceeds the context window` OpenAI, `n_ctx` llama.cpp variants, `exceeded model token limit` Kimi, `chat history exceeds the N-message limit`, …) + generic `exceeds the limit of \d+` — with `isUsageBackedContextOverflow` requiring usage evidence and cause-chain walking (`hasCauseTokenContextOverflowEvidence`).
- **Typed error classes** (`error/classes.ts`): `ProviderHttpError` (status+headers+code), `OpenAIHttpError` (captured body, envelope parser tolerating compat hosts), `AnthropicApiError` (bounded body drain), `AnthropicConnectionError/TimeoutError`, `AnthropicStreamEnvelopeError` (out-of-order SSE; message-prefixed for classification), `BedrockApiError`, `GeminiCliApiError`, `GoogleApiError`, `OllamaApiError`, `AuthGatewayError`, `CodexWebSocketTransportError`, `CodexWhitespaceToolCallLoopError`, `CodexProviderStreamError{retryable}`, `AuthBrokerError{status}`.
- **Rate-limit reason taxonomy** (`error/rate-limit.ts`): `QUOTA_EXHAUSTED | INSUFFICIENT_G1_CREDITS_BALANCE | RATE_LIMIT_EXCEEDED | CONCURRENT_LIMIT | MODEL_CAPACITY_EXHAUSTED | SERVER_ERROR | UNKNOWN`, each with a reason-specific backoff (`calculateRateLimitBackoffMs`; `MODEL_CAPACITY_EXHAUSTED` gets jitter); 402 is a *categorical* billing cap; `isUsageLimitOutcome(status, message)` is the rotate-vs-backoff decision (opaque bodies rotate conservatively; explicit `retry in 5s` stays in the provider's own backoff lane).
- **Stream-turn classifiers in the coding agent** (`turn-recovery.ts:76-84`): `STREAM_STALL_ERROR_RE = /stream stall/i`; `HTTP2_STREAM_RESET_ERROR_RE` (nghttp2 internal_error/refused_stream variants); `PREMATURE_STREAM_CLOSE_ERROR_RE = /stream closed before a (finish_reason|terminal response event)/i`; `IMMUTABLE_ANTHROPIC_THINKING_ERROR_PATTERN` (400 + "latest assistant message cannot be modified" — thinking signatures are immutable, retrying the same request can never work).

### 3.2 Stream watchdogs (first-progress + idle) — layered

Settings: `providers.streamFirstEventTimeoutSeconds` / `providers.streamIdleTimeoutSeconds` (default `-1` = auto; `0` = off; UI options 300/600/1800) → wired into every request by `createSettingsAwareStreamFn` (`session/settings-stream-fn.ts:58-93`), which also threads `retry.maxDelayMs` as `maxRetryDelayMs`, `providers.maxInFlightRequests`, OpenRouter variant / antigravity endpoint mode, cache retention, and the Anthropic server-side `fallbacks` chain.

Defaults & layering (`pi-ai/src/utils/idle-iterator.ts:8-9`, `sdk-stream-timeout.ts`):
- `DEFAULT_STREAM_FIRST_EVENT_TIMEOUT_MS = 300_000`, `DEFAULT_STREAM_IDLE_TIMEOUT_MS = 300_000`; env `PI_STREAM_FIRST_EVENT_TIMEOUT_MS`, `PI_STREAM_IDLE_TIMEOUT_MS`, `PI_OPENAI_STREAM_IDLE_TIMEOUT_MS`, `PI_OPENAI_STREAM_FIRST_EVENT_TIMEOUT_MS` (OpenAI-family first-event floor = `max(base, resolved idle)` so slow local prompt processing isn't undercut).
- Layer 1: **SDK request timeout** — `createSdkStreamRequestOptions` sets `timeout` + **`maxRetries: 0`** (the SDK must never extend the caller's deadline by retrying after a timeout).
- Layer 2: **pre-response guard** — `armPreResponseTimeout` (clearable timer, NOT `AbortSignal.timeout`, whose absolute deadline killed streaming bodies at the budget — #2422); cleared the instant headers arrive; Bedrock's SDK-bypass caller still arms it (POST accepted, no headers would otherwise hang forever).
- Layer 3: **iterator watchdog** — `iterateWithIdleTimeout`: separate `firstItemTimeoutMs` and idle deadline, `isProgressItem` filter (pings don't count as progress), `hasPendingLocalWork` extends deadlines (consumer-side work is not provider stall), persistent abort/timeout racers re-minted every 1024 iterations (listener churn bounded), `iterator.return()` on every exit path (the #912 "Working… forever" fix for HTTP/2 proxies that swallow aborts).
- Anthropic specifics: 15 s `STREAM_PING_INTERVAL_MS` before `message_start` so slow-first-token streams aren't classified stalled; a ping must not consume the first-event budget (would flip a retryable pre-content stall into a terminal mid-stream idle timeout); `AIError.StreamTimeoutError("Anthropic stream timed out while waiting for the first event")` / idle equivalents abort *locally* and classify as retryable Timeout.
- Subagent wall-clock backstop: `agents.timeout` ("Hard wall-clock limit per subagent (ms) … Defense-in-depth against provider-side stream hangs that escape the inference-layer watchdog").

### 3.3 The retry loop (`TurnRecovery.#handleRetryableError`)

Entry conditions: `stopReason === "error"` + `isRetryableError` — with `isUsagePreflightBlocked` veto, immutable-Anthropic-thinking-400 veto, context-overflow veto (compaction owns it), and replay-unsafe-output veto unless classifier-refusal/malformed-call with proven-unexecuted tools. Settings: `retry.enabled` (default true), `maxRetries` (10), `baseDelayMs` (500), `maxDelayMs` (300000 fail-fast).

Flow: `#retryAttempt++` → budget-exhausted consult (the chain still gets a last look so provider-wide caps can fail over) → delay = `calculateRetryBackoffDelayMs(base, attempt)` = `min(base·2^(attempt−1), 8000) × (1 − 0.25·rand)` (downward jitter) → reason-specific overrides:
- `StaleResponsesItem` ⇒ `delayMs = 0` + `resetCurrentResponsesProviderSession("stale replay error")` (the server-side history item is gone; a fresh Responses session is the only fix);
- usage-limit outcome (§1.7) ⇒ credential switch (`delayMs=0`) or wait `min(provider window, earliest sibling unblock + buffer)`;
- `CONCURRENT_LIMIT`/`RATE_LIMIT_EXCEEDED` reason backoff when the error carries no parsed hint;
- explicit `retry-after` authoritative in both directions.

Then, in order: account-policy denial ⇒ `rotateSessionCredential`; `modelFallback` ⇒ chain consult + cooldown pinning (`pinFallback` for classifier refusals); Fireworks Fast degrade; hard-error chain consult (entered even for non-retryable errors when `hardErrorFallback`); budget-exhausted terminal (`auto_retry_end {success:false, finalError:"Retry budget exhausted after N retries: …"}` + `persistTerminalEmptyErrorTurn`); classifier-refusal saga close; fail-fast cap (provider asks > `maxDelayMs` with nothing to switch to ⇒ surface instead of sleeping — defends against 3-hour Anthropic windows hanging subagents/interactive sessions).

On retry: `#recordPendingRetryError` (latched onto the persisted branch entry, §3.8), `auto_retry_start {attempt, maxAttempts, delayMs, errorMessage, errorId}`, thinking-loop redirect injection if flagged, `#stripFailedAssistantTail` positional backstop (an error/aborted assistant tail is never legal continuation input), `scheduleAgentContinue({delayMs:1, generation})`. Every dead-end closes the saga with `auto_retry_end` so subscribers never stay latched (#5382). Local `continue()` failure also closes the saga (`#failRetryAfterLocalContinueError`).

### 3.4 Partial-stream recovery — deltas already rendered but message not persisted

- **`retainCompletedToolCalls`** (`agent-loop.ts:1930-1960`): on `error|aborted` stop, tool calls that never completed are dropped from the assistant content; completed ones kept; `stopDetails = {type: "stream_interrupted_after_content", category, explanation}` records the truncation.
- **`recoverTransientErrorToolTurn`** (`agent-loop.ts:1962-2000`): if the failed turn's tool calls are *all complete and known*, and the error text is a transient stream read/envelope/parse error (`isStreamReadErrorText` / `isStreamEnvelopeErrorText` / `isTransientStreamParseError`, incl. Anthropic envelope errors and the upstream-1302 pattern), the loop **rewrites the stop to `toolUse` and clears error fields** — the turn continues into tool execution as if the provider finished. Refusal/sensitive categories are exempt.
- **Synthetic tool results** (`createSyntheticToolResultMessage`): for calls emitted but never executed, a persisted `toolResult` with `details = {__synthetic: true, source: "assistant_stop_aborted" | "assistant_stop_error" | "assistant_stop_skipped" | "assistant_stop_length" | "interrupt_skipped", executed: false, upstreamError?}` preserves the provider's tool_use/tool_result pairing without implying a real run (#4321: a provider-side stream death after tool-call emission — e.g. Codex websocket close — was mislabeled by the CLI as a local tool failure).
- **Replay safety** (`#hasReplayUnsafeOutput` + `#unexecutedToolCallsReplaySafe`, `turn-recovery.ts:1124-1243`): committed text (when `textOutputCommitted`), images, and server-tool blocks veto retry/credential-rotation/fallback; the exception is a refusal or `MALFORMED_FUNCTION_CALL` where **every** emitted call has a later synthetic result with `executed:false`, located by walking back over the synthetic tail (`syntheticToolResultTailStart`).
- **Preserved-turn continuation** (`classifyResolvedInterruptedToolTurn`): reasonless aborts (no `abortInProgress`), `stream stall`, HTTP/2 resets (`nghttp2_internal_error|refused_stream`), and gateway premature closes keep the failed assistant + synthetic pair in context and continue after the partial output — never re-rendering or replaying completed side effects.
- **User interrupts**: `abortReasonText` surfaces string/error abort reasons; tool-scoped aborts (`createToolScopedAbortReason`) blame only the matching calls (`message.toolCallAbortMessages`), siblings get neutral labels; terminal post-tool-hook aborts (`TERMINAL_TOOL_RESULT_ABORT_REASON`) stop after persisting the completed tool batch without synthesizing an aborted assistant boundary.
- **Harmony leak mitigation**: GPT-5 Harmony protocol leakage is detected mid-stream; the partial is discarded/recovered and an `onHarmonyLeak` audit event fires (`agent-loop.ts:1900-1920`).

### 3.5 Empty and unexpected stops

- **Empty stop** (`#handleEmptyAssistantStop`, cap 3 per prompt): includes `Flag.EmptyResponse` turns whose content is thinking/whitespace only. Each retry reparents the leaf **durably** (`#dropAssistantTurnDurably` — an in-memory-only reparent resurfaces after reload or a mid-retry process kill) and injects a developer reminder (`emptyStopRetryTemplate`); on cap: terminal error enriched with forensics — billed-output analysis distinguishes provider-side content filters ("provider billed N output tokens … content was generated and then dropped before delivery") from context problems, suggests model switch or `/shake images`; the capped empty turn is dropped durably so its zero-value usage can't anchor the next prompt at the failed-request size.
- **Unexpected stop** (`#handleUnexpectedAssistantStop`, `features.unexpectedStopDetection` default `"mechanical"`; `"smart"` adds small-model classification via `providers.unexpectedStopModel` — local tiny or `online`, `unexpected-stop-classifier.ts`): `isUnexpectedStopCandidate` = terminal `stop` with visible text, or thinking-only turn (reasoning models can trap the answer in a thinking block); tool-call turns excluded. Mechanical mode retries thinking-only stops directly; each retry appends a developer reminder (`unexpectedStopRetryTemplate`). Cap 3.
- Both paths reset counters per prompt (`resetForNewPrompt`).

### 3.6 Loop guards (`stream-guards.ts`)

- **Streaming edit guard**: as `edit` tool-call args stream in, `preCache`/`maybeAbort` verify the removed lines actually exist in the target (time-sliced scans with per-file serialization and per-file epochs so a check queued before an edit result can't validate stale content) and `previewPatch` parses; failure aborts the turn *before execution* (`edit.streamingAbort` setting; auto-generated-file guard runs unconditionally).
- **Gemini header runaway**: `GeminiHeaderRunDetector` counts planning headers in `thinking_delta`; on runaway: `agent.abort("Interrupted: emit a tool call instead of more planning")` → post-prompt task discards the aborted assistant turn (`discardAssistantTurn`) and injects a hidden `gemini-tool-call-reminder` custom message, then `agent.continue()`. Gated by `model.loopGuard.enabled` + `model.loopGuard.toolCallReminder` + `PI_NO_THINKING_LOOP_GUARD`.
- **Cross-turn tool-call loop** (`model.toolCallLoopGuard.enabled`, threshold + exemptTools): on repeated identical calls across turns, inject a `tool-call-loop-redirect` custom message (persisted via `appendCustomMessageEntry`) so the next turn reads the redirect.
- **Thinking-loop retries** (Flag.ThinkingLoop): same-model resample with a hidden `thinking-loop-redirect` notice injected on every retry (the failed assistant is dropped each attempt, so the notice doesn't accumulate); deliberately excluded from model fallback (#8760).

### 3.7 Auth & quota failures mid-stream

- **Refresh + replay**: the a/b/c resolver (§1.7) runs below the transport: a 401 mid-request triggers step (b) force-refresh of the same OAuth grant (`OAuthError.kind === "token-refresh"` explicitly means "refresh and replay once"); invalidation text ("invalidated oauth token") jumps to step (c). `isDefinitiveOAuthFailure` (Flag.OAuthExpiry) disables the credential rather than looping.
- **Usage caps mid-stream** surface as resolved `AssistantMessage{stopReason:"error"}` with Flag.UsageLimit; `recordUsageLimitOutcome` latches the wait; `maybeAutoRedeemCodexReset` can clear Codex blocks without a fallback.
- **Oneshots have their own retry** (`pi-ai/src/oneshot-retry.ts`): `streamSimple`/`completeSimple` deliberately surface transient errors to the agent loop (TurnRecovery owns them and must not replay unsafe output), but oneshots (summaries, titles, handoffs, classifiers) are side-effect-free and retry via `retryTransientCompletion` (default 3 attempts, 500 ms base, 30 s max, abort-preserving, usage limits included with provider hints honored).

### 3.8 Latched persistence errors — the journal is never left ambiguous

- **Persistence keys** (`turn-persistence.ts`): every persisted message carries `sessionMessagePersistenceKey` — assistant: `timestamp:provider:model:responseId:stopReason`; toolResult: `timestamp:toolCallId:toolName`; user/developer: `timestamp:attribution`. `planTurnPersistence` decides which of a turn's messages still need appending and detects out-of-order persistence (bails rather than splicing a stale message between newer entries).
- **Retry bookkeeping latches onto entries**: `#recordPendingRetryError` finds the branch entry by persistence key + structural compare and pushes `{entryId, recovery: "credential"|"model"|"wait"|"plain", attempt, note}`; on recovery/supersession, `#markPendingRetryErrors` **rewrites the entries** (`rewriteEntries`) to set `message.retryRecovery = {kind:"auto-retry", status:"recovered"|"superseded", attempt, note, supersededBy?}` — the transcript records every automatic intervention without polluting provider context (the rebuild filters `retryRecovery` entries out, §2.2).
- **Terminal empty errors are persisted too**: `persistTerminalEmptyErrorTurn` guarantees even a dead-end (`stopReason:"error"`, no content) leaves a journal record of why the run stopped — the JSONL never ends on an unexplained cliff.
- **Write mechanics**: `session-storage.ts` rewrites go through temp file + `fs.renameSync` (atomic); appends are queued with `flush()`/`flushSync()` drains (no fsync by design — power loss may drop the last page, but the loader's lenient parse keeps the file loadable); `session_exit` handlers call `flushSync()` before teardown.

### 3.9 Failure-mode matrix

| Error class | Detection | Recovery action | Context/memory impact | Resume path |
|---|---|---|---|---|
| Transient HTTP (408/429/5xx, `overloaded_error`, 529) | `Flag.Transient` via `classifyMessage`/status | backoff `min(500·2^n, 8s)·jitter`; reason-specific windows; retry same model | none (failed turn dropped if empty; `retryRecovery` latched) | `scheduleAgentContinue`; auto_retry events |
| First-token stall | first-event watchdog (300 s) → `StreamTimeoutError` abort | retryable Timeout → retry loop / fallback chain | failed turn dropped pre-content; nothing persisted | retry continues |
| Mid-stream idle stall | idle watchdog (300 s) | `stream stall` class: preserved-turn continuation if tool calls resolved, else retry | partial output kept; synthetic results pair unexecuted calls | continue after partial |
| HTTP/2 reset, premature gateway close | message regexes + `retriable` | preserved-turn continuation (stall class) | partial kept; no re-render | continue after partial |
| Transient stream error after complete tool calls | `recoverTransientErrorToolTurn` | rewrite stop to `toolUse`, continue tools | none — turn proceeds | n/a (never surfaces) |
| Provider error after content streamed (deltas rendered) | `STREAM_INTERRUPTED_AFTER_CONTENT_STOP_DETAIL` | failed assistant persisted with stopDetails; retry only if replay-safe; else `preserveFailedTurn` | partial content kept in context; user sees rendered text | manual retry strips synthetic tail + failed turn |
| Empty stop / EmptyResponse | `isEmptyAssistantStop` / Flag.EmptyResponse | ≤3 retries w/ developer reminder; durable drop each time; terminal forensics on cap | dropped turns leave no usage anchor | journal records terminal error |
| Unexpected stop (text-only/trapped thinking) | `isUnexpectedStopCandidate` + optional classifier | ≤3 retries w/ reminder (mechanical), smart classify | none | continue |
| Context overflow (token evidence) | Flag.ContextOverflow + 24 patterns + usage backing | remove failed turn → **promotion** to `contextPromotionTarget` → else recovery compaction (rollback-safe) | history rewritten; summary entry; append-only prefix reset | compaction entry in JSONL; retry after continue |
| Payload 413 (bytes/media) | Flag.PayloadRejected + trust heuristics | withhold from compaction; warn; block auto-continuation | none | manual intervention surfaced |
| Output incomplete (`length`, `response.incomplete`) | `stopReason:"length"` | drop dead turn → promotion → compaction/handoff (inline, forced) | history rewritten if compacted | retry after continue |
| Usage limit / quota (account cap) | Flag.UsageLimit + body classes | `markUsageLimitReached` (durable block) → rotate sibling credential or Codex reset redeem; wait `min(provider, sibling)` | none | credential state in agent.db; retry after unblock |
| Auth failure (401/403, refresh, invalidation) | `isAuthRetryableError` | a/b/c: refresh same → rotate sibling → OAuthExpiry disables credential | none | next turn re-resolves credential |
| Account policy denial (e.g. `cyber_policy`) | Flag.AccountPolicy | immediate credential rotation (skips refresh); fallback chain with `pinFallback` on refusals | none | rotate/retry |
| Classifier refusal / `MALFORMED_FUNCTION_CALL` | stopDetails type / Flag | retry allowed **only** with proven-unexecuted calls; fallback pinned | failed turn kept only when replay-safe | journal latches recovery |
| Stale Responses item | Flag.StaleResponsesItem | reset Responses provider session; delay 0 retry | provider session state cleared | fresh server-side session |
| Thinking loop / Gemini header runaway / tool loop | loop detectors | same-model resample + redirect notices; discard + remind + continue | redirect messages persisted (hidden) | continue |
| Compaction LLM call fails | method error | next method in `methodOrder`; oneshot retry for manual runs | none until a method commits | ladder state in `auto_compaction_end` events |
| Compaction cancelled (Esc/hook) | `CompactionCancelledError` | typed outcome "cancelled"; no rewrite | none | clean |
| Persistence hiccup mid-turn | `planTurnPersistence` out-of-order | bail; wait for barrier; never splice stale | journal order preserved | reload-safe |
| Process death mid-turn | `session_exit` (fatal/signal/process_exit) + pending markers | see §4 | synthetic aborted assistant on resume | §4 forensics |

**Invariants** (what "context is never lost" concretely means):
1. Every message enters the JSONL before any recovery mutation touches live state (persistence barriers in prewalk, mid-turn compaction, retry).
2. Drops are *branch reparents*, never deletions: `discardAssistantTurn`/`discardEntryDurably` re-parent the leaf and append a marker; full history stays exportable.
3. Recovery actions that rewrite history are transactional: compaction commits an entry (atomic append) and only then splices live state; failure restores the failed turn.
4. Retry/fallback never replays committed output; unexecuted calls are provably paired with `executed:false` synthetics.
5. All recovery state (`retryRecovery`, `model_change role:"fallback"`, `thinking_level_change configured:"auto"`, compaction entries) is durable in the tree, so a crash during recovery resumes correctly.

---

## 4. Resume — `--continue`/`--resume`, forensics, guarantees

### 4.1 CLI resume flows (`main.ts:679-975`)

- `--continue`/`-c` → `SessionManager.continueRecent(cwd, sessionDir)`: **terminal breadcrumb first** (`~/.omp/agent/terminal-sessions/`, keyed by TTY path or `ZELLIJ_PANE_ID TMUX_PANE CMUX_SURFACE_ID KITTY_WINDOW_ID WEZTERM_PANE TERM_SESSION_ID WT_SESSION`); a *fresh* crumb pointing at a never-materialized `/new` session starts a new session instead of resurrecting pre-`/new` history; stale crumbs re-root to the interactive parent (subagent artifacts walk up); cwd mismatch falls back to cwd-matching sessions. `normalizeContinueSessionArgs` promotes `--continue <uuid>` to `--resume <uuid>`.
- `--resume <id|prefix>` / `--session` (empty rejected) → prefix resolution (case-insensitive startsWith over id / filename / id-suffix, mtime order), picker UI when bare.
- `autoResume` setting (default false; false on this machine): behave like `--continue` when a prior session exists — and marks `parsed.continue` so model/thinking restore wins over CLI defaults.
- `--fork <id|path>`: new file + `parentSession` header; cache-key inheritance dropped when model/thinking/prompt/tools are overridden (§1.8).

### 4.2 `session_exit` + `tool_execution_start` forensics

- **`tool_execution_start` marker** (`agent-session.ts:2075-2090`): a `custom` entry (`customType:"tool_execution_start"`) written *before* the tool implementation runs, carrying `{toolCallId, toolName, args:{command?,path?} (≤200 chars/field), intent?, startedAt}` — a compact projection; the assistant message already holds full args.
- **`session_exit` marker** (`#recordSessionExit`, `agent-session.ts:2092-2130`): written during normal AND fatal teardown (postmortem hook): `{reason, kind: "normal"|"signal"|"fatal"|"process_exit", recordedAt, pendingToolCalls?}` where `collectPendingToolCalls` (`exit-diagnostics.ts:272-283`) walks the branch: assistant toolCalls set pending (clearing per new assistant), `tool_execution_start` markers enrich `startedAt/args/intent`, `toolResult` messages clear their id. Skipped when nothing pending and no assistant messages exist (routine exits stay clean); `flushSync()` before returning; abnormal teardown or pending calls log at warn.
- **Resume warning** (`describePendingToolCalls`): "Previous session ended while N tool calls remained pending: bash `cmd …`, edit `path` … The prior OMP process exited before recording tool result(s)."

### 4.3 Synthetic aborted assistant message on resume

`createInterruptedTurnAbortMessage` (`exit-diagnostics.ts:105-175`) — appended at session load (`agent-session.ts:8257-8266`) only when ALL hold:
1. A `session_exit` entry exists whose `kind != "normal"` OR it lists pending tool calls;
2. The last message entry precedes the exit entry;
3. The tail is an assistant carrying a toolCall block, or a toolResult;
4. If the tail is a toolResult, the previous assistant is NOT already terminal (`error`/`aborted`).

The synthesized record: `stopReason:"aborted"`, `errorMessage:"Previous OMP process exited before completing the turn."`, zero usage/cost, timestamp = exit `recordedAt` (parsed; falls back to now), model metadata from the previous assistant or the loading model. It terminates the dangling turn so the next request has a legal tail (providers reject unpaired tool_use), then `buildDisplaySessionContext()` re-derives context with the abort in place.

### 4.4 State restored on resume/switch (`agent-session.ts:8199-8330`)

1. `agent.replaceMessages(buildDisplaySessionContext().messages)` (§2.2 semantics).
2. Provider sessions closed when switching files or when the reload changed messages.
3. **Model restore**: `getRestorableSessionModels` (role-first order, §1.3) matched against available models; provider/api change ⇒ `#setModelWithProviderSessionReset`, else plain `agent.setModel` (keeps provider session state for same-model reloads).
4. Synthetic interrupted-turn abort (§4.3) + context rebuild.
5. **Thinking restore**: prefers `configuredThinkingLevel` from the path — `auto` resumes as auto (reclassifying next turn), legacy entries (pre-`configured`) fall back to the concrete level; with no entry, the global `defaultThinkingLevel` applies.
6. **Service tiers** restored from `service_tier_change` (or settings defaults), without persisting duplicates (`restoreServiceTiers`).
7. Memory re-key, advisor reset (`preserveCost`), checkpoint/rewind state rehydration, `session_switch {reason:"resume"|"fork"}` hook event.

### 4.5 Durability mechanics backing "resume whatever it takes"

- Append-only JSONL with queued writers; rewrites (retry latching, discard) are temp+rename atomic; loaders parse leniently — malformed records are skipped with a counter (`parseJsonlLenient`, `session-loader.ts`), ≥8 MiB files stream in chunks with bounded memory (never fully materialized; multibyte-safe byte buffer).
- Branch reconstruction tolerates parent cycles (§2.2) and orphans (`getTree` treats broken chains as roots).
- `discardEntryDurably` reparents known metadata children before removal and always appends a `discarded-entry-branch` marker + `rewriteEntries()` — a discarded turn cannot silently resurrect.
- The title slot is a physical first line, so title updates never require rewriting entries.
- Every boundary operation (`/clear`, `/fresh`, compaction, fallback, abort) ends with a durable tree mutation before live state changes — the invariant chain from §3.9.

---

## xdev impact

Mapped to PRD §4 milestones. The blueprint's M5 row ("Compaction …, retry/failover") and M9/M10 need the following concretization; M2/M3 carry the persistence/loop substrates.

- **M1/M3 (substrate)** — Adopt the `Flag` bitmask + `RETRIABLE_KINDS` classification and the three-layer watchdog design (SDK timeout w/ `maxRetries:0` → clearable pre-response timer → iterator first-event/idle racers) as *library* pieces, not CLI glue; port `iterateWithIdleTimeout` semantics including `isProgressItem` and local-work deadline extension. Map omp error regexes verbatim (they encode years of provider quirks); add a table-driven `classify(error) → flags` with tests per provider phrase.
- **M2 (session core)** — Port the entry catalog exactly as §2.1 (fields incl. `retryRecovery`, `configured`, `resolvedModelIsFallback`, `preserveData`, `method`), the title-slot first line, cycle-bounded path walks, lenient streaming loads for ≥8 MiB, temp+rename rewrites, and `reset_boundary` emission precedence. The `hasExplicitDefaultModel` guard and `EPHEMERAL_MODEL_CHANGE_ROLE` non-restore rule are acceptance criteria, not details.
- **M5 (compaction + retry/failover)** — Port: threshold math (§2.4 table), `methodOrder` ladder with per-method advance and `NativeCompactionError` no-fallthrough, 80 % recovery band + dead-loop guards, mid-turn maintenance with persistence barrier + dead-end parking, speculation with apply-time branch validation, promotion ladder (`contextPromotionTarget` before compaction; per-model `compactionModel`), the full `TurnRecovery` state machine (§3.3) incl. fail-fast `maxDelayMs`, usage-limit latching with sibling-unblock waits, `retry.fallbackChains` grammar + resolution specificity + candidate filters + `served`/attribution, empty/unexpected-stop bounded retries with durable drops, and the `stream_interrupted_after_content` + `recoverTransientErrorToolTurn` partial-stream semantics. Acceptance: kill -9 mid-stream on a 200k-token session → resume must synthesize the aborted boundary, restore model/thinking/tier, and continue with context intact.
- **M9 (model roles/providers/auth)** — Role resolution grammar (§1.1) incl. alias loop protection and `@upstream` routing; `modelRoles` persistence semantics (`formatRoleModelValue` preserving explicit effort); credential layer must expose the a/b/c resolver contract, durable block table (the `auth_credential_blocks` analog incl. shared-pool mirroring), `markUsageLimitReached`/`rotateSessionCredential`/usage-health probes, and `AUTH_RETRY_MAX_ATTEMPTS`-bounded rotation. `providerPromptCacheKey` inheritance/clearing rules (§1.8) belong here.
- **M10 (session UX/lifecycle)** — `--continue` breadcrumb incl. fresh-marker + stale-crumb re-root, `autoResume`, synthetic aborted-message conditions (§4.3) as acceptance tests, `session_exit`/`tool_execution_start` marker writes with the exact field projection, pending-tool-call resume warning, and `cacheMissExplainedAt`-style transcript annotations.
- **M11 (prewalk)** — Port the prewalk state machine verbatim (§1.5): todo gate, write-tier trigger incl. `xd://` tier discrimination, persistence barrier, plan-nudge scrubbing, ephemeral role write. The prompts are products, port them as templates.
- **M12 (memory seam)** — Keep `preCompactionContext` and `buildDeveloperInstructions` as the two memory↔context seams; promotion snapshot/rollback and re-key-on-switch are the durability contract.
- **Reject/simplify for xdev v1**: snapcompact bitmap archiving (vision-model dependent), remote streaming-v2 compaction, Fireworks Fast degrade, tiny/local classifier backends, and multi-backend memory — mark NICE/SKIP; the ladders must survive their absence (methodOrder shrinks; fallbackChains may be empty).

## Issues to update

- **#3 (M2 session core)**: add AC — "buildContext honors `reset_boundary` vs latest-compaction precedence (later boundary wins), filters `retryRecovery`/empty-error assistant entries from model context, and re-derives `models[role]`/thinking `configured`/service tiers from path entries with the `hasExplicitDefaultModel` inference guard; loading a ≥8 MiB JSONL with malformed lines streams with bounded memory and skips bad records."
- **#6 (M5 compaction + retry/failover)**: add ACs — (1) compaction triggers on threshold/overflow/incomplete/mid-turn/idle with the §2.4 defaults (reserve 16384, 15 % floor, keepRecent 20000, MAX_SUMMARY 16384); (2) method ladder `[remote, handoff, shake, soft]` (v1, no snapcompact/v2) advances on failure and never loops (80 % recovery band, dead-end parking w/ re-arm); (3) `TurnRecovery` port: backoff `min(500·2^n,8s)` with 25 % down-jitter, fail-fast `maxDelayMs`, usage-limit credential rotation with sibling-unblock waits, `retry.fallbackChains` (role/exact/wildcard keys, provider-swap + id-reprefix, context-fit + effort-ceiling + thinking-signature candidate filters, cooldown suppression, `cooldown-expiry` restore, fresh budget on model switch); (4) partial-stream recovery: `stream_interrupted_after_content` stopDetails, `recoverTransientErrorToolTurn` rewrite-to-toolUse, synthetic `executed:false` tool results with replay-safety proof; (5) empty/unexpected-stop bounded retries with durable branch drops.
- **#9 (M8 hardening)**: add AC — "fuzz the JSONL loader with truncated/corrupt records and parent cycles: loads must be lenient (skip + count), bounded in memory, and never lose valid entries; resume after kill -9 mid-tool-execution synthesizes the aborted assistant boundary exactly under the §4.3 conditions."
- **#10 (M9 model roles/auth)**: add ACs — role selector grammar (`provider/id:effort@upstream`, `@role` recursive aliases w/ loop guard, literal-id precedence over `:max`); `model_change` writes for all surfaces (default/temporary/fallback + `resolvedModelIsFallback`); `promptCacheKey` = `agent.promptCacheKey ?? sessionId` with fork inheritance cleared on model/thinking/tool overrides; a/b/c auth-retry with durable credential blocks (provider+scope keyed, expiry index) and usage-limit latching.
- **#11 (M10 session UX)**: add ACs — `--continue` breadcrumb (TTY/pane-keyed) honoring fresh markers and stale-crumb re-root; `autoResume`; `session_exit` + `tool_execution_start` custom entries written at the specified points (before tool run; during normal + fatal teardown w/ `flushSync`); resume warning for pending tool calls; synthetic aborted assistant on non-normal exits with pending tail; thinking `configured:"auto"` survives resume.
