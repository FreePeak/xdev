# Decision: code graph via LeanKG, not a Soul Map

*Reference: `internal/tool/verify.go` · `internal/agent/prompt.go` · `internal/lsp/` ·
`docs/decisions/leankg-memory-backend.md` · local Empryo checkout
(`~/work/harvey/freepeak/Empryo`, public **SoulForge v2** core `@proxysoul/soulforge` 2.20.25) ·
Date: 2026-10-02 · Status: **adopted — implementation tracked by
[issue #543](https://github.com/FreePeak/xdev/issues/543)**

## 0.1 The decision in one line

xdev does **not** build an in-process code graph (a "Soul Map"). The graph stays
**out of process** in LeanKG, reached through thin deferred tools
(`impact`, `code_query`); the system prompt keeps its small size by default and
gains an **opt-in, hard-capped** outline block.

## 0.2 Why this is even on the table

Empryo's public core demonstrates a genuinely different way to make an agent
understand a repository. Its four layers are:

1. **Index** — a SQLite graph (`files`, `symbols`, `edges`, `calls`,
   `cochanges`, `refs`, trigram postings) built by a tree-sitter pass
   (`src/core/intelligence/repo-map.ts`).
2. **Rank** — personalized PageRank over import edges, with type-barrel edge
   down-weighting, a structural-type ratio penalty, co-change and neighbor
   boosts, and a hard token budget that truncates the render.
3. **Inject** — a **frozen** `<soul_map>` snapshot in the system prompt plus a
   **delta** channel (`<soul_map_update>`) injected between turns, so the map
   stays fresh without invalidating the prompt cache.
4. **Confirm** — the prompt explicitly states the map is an *orientation layer,
   not ground truth*, and the agent must verify claims with `soul_grep`,
   `soul_find`, `navigate`, or `read` before asserting behavior.

The transferable insight is not the SQLite engine. It is the **protocol**:

| Protocol element | xdev's answer |
|---|---|
| Index a graph | LeanKG (already the memory backend; second process is allowed) |
| Rank what's important | LeanKG query ladder + `lsp` for precision; **no** PageRank port |
| Inject a snapshot | Opt-in `context.codeOutline`, capped; **off** by default |
| Keep it fresh without busting the cache | Frozen per-session outline; deltas are out of scope for v1 |
| "Orient, don't conclude" | Three rules in `SystemPromptBase` + tool descriptions |
| Same-turn self-check | #263 — diagnostics on the `edit`/`write` result |

## 0.3 The invariants that forbid the Soul Map port

Porting `repo-map.ts` into the agent binary would violate four things this repo
already decided, in writing:

1. **One CGO-free static binary** ([PRD §1](https://github.com/FreePeak/xdev/blob/main/docs/PRD.md) goal 1).
   tree-sitter is a C library; `ts-morph` is a TypeScript compiler. Both would
   either mean CGO or a shipped JS runtime.
2. **< 100 MB RSS** (M8 gate). A whole-repository graph plus a parser heap in
   the agent process is a second store, and the gate says a second *process* is
   fine, a second in-process store is not.
3. **Shell out over import** (design principle 6). `ast-grep`, `rg`, `git`,
   language servers — external, optional, probed at setup. A graph engine is the
   same rule at a larger scale.
4. **Minimal prompt** (design principle 1). A Soul Map render is thousands of
   tokens by construction; Empryo pays for it with a fat always-on system
   prompt and a fat intelligence core. xdev's answer to a wide surface is
   progressive disclosure, not a wide prompt.

A second, softer reason: **the maintenance cost is not the code, it is the
languages.** Empryo's map is 30+ languages of bespoke tree-sitter handling. LeanKG
already indexes this machine's Go and TS trees and can be improved there by one
owner instead of two.

## 0.4 What LeanKG gives us, and what it does not

Measured against the live server this decision is built on:

| Need | LeanKG | Source |
|---|---|---|
| Callers / callees / impact | `action=callers`, `action=callees`, `action=impact` + `args.depth` | LeanKG MCP `query` |
| Path between two elements | `action=path` + `args.to` | LeanKG MCP `query` |
| Exact element lookup | `action=exact` (L1), `fuzzy` (L2), `semantic` (L3) | LeanKG MCP `query` |
| Surrounding code | `action=context` | LeanKG MCP `query` |
| Typed precision (rename, diagnostics) | **not LeanKG** — `internal/lsp` | xdev |
| A ranked, budgeted outline for the prompt | **not guaranteed** — see §0.5 | — |

The missing row is why the outline in §0.5 is gated rather than assumed.

## 0.5 Why the outline is off by default

`context.codeOutline` defaults to `false`. Three reasons:

1. **There is no verified LeanKG endpoint that returns a token-budgeted outline
   today.** Inventing one means either blocking on a LeanKG feature or
   re-implementing ranking in xdev — which is the thing this decision refuses.
2. **A frozen outline in the system prompt is a cache liability.** Every
   re-render busts the prefix. Empryo handles this with an idle-TTL snapshot
   plus a delta channel; xdev has no delta channel, so a v1 outline is
   session-frozen and honest about being stale.
3. **The default prompt is a measured artifact.** `cmd/xdev/prompt_test.go`
   pins it under 1,000 tokens. An opt-in block keeps that gate meaningful for
   the configuration 99% of users run.

If LeanKG grows an outline endpoint, enabling this is a config change, not a
code change.

## 0.6 The failure contract

A dead LeanKG must be indistinguishable from an unconfigured one:

- `impact` / `code_query` return a one-line "code graph unavailable" and the
  session continues.
- The outline block is omitted silently after a single warning; it never
  retries per-turn.
- Memory already carries this contract for `hindsight`; the new tools copy it
  rather than inventing a new policy.

## 0.7 What this decision rejects

| Rejected | Why |
|---|---|
| Porting `repo-map.ts` (SQLite Soul Map) | Invariants 1–4 above |
| Embedded tree-sitter / ts-morph | CGO or a JS runtime |
| Always-on graph in the prompt | Busts the prompt budget and the cache |
| A first-class `memory: leankg` enum now | Option B needs the remote adapter generalized first (X6/X7) |
| Re-implementing PageRank in Go | LeanKG's ranking is the same job, already indexed |

## 0.8 The one runnable check

`internal/tool/verify_test.go` already pins the cheap end of the diagnostics
ladder. The protocol's other invariant — a dead graph server never breaks a
session — is pinned by the new `internal/codegraph` tests: a `httptest` server
that is closed (or never answers) must produce a capped error string, not a
hang and not a panic. If that test fails, this decision has been violated.