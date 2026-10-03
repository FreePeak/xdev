# Empryo / SoulForge — how it makes an agent understand a codebase

*Reference: local checkout `~/work/harvey/freepeak/Empryo` (public **SoulForge v2**
core, `@proxysoul/soulforge` 2.20.25) · Date: 2026-10-02 · Status: **read; not
shipped** (disposition in
[decisions/code-graph-via-leankg-not-soulmap.md](../decisions/code-graph-via-leankg-not-soulmap.md)
and PRD §5)*

**Companion:** [2026-10-03-empryo-tui-parity.md](2026-10-03-empryo-tui-parity.md)
covers the other half of the same peer -- Empryo's terminal UI as a TUI reference.

## What was read

The tree at `Empryo/` is the **public SoulForge v2 core**, not Empryo v3. v3's
desktop app, morphs and newest surfaces are developed privately — the README
says so, and nothing here should be inferred about them from marketing alone.
Files cited below are SoulForge v2 paths in that checkout.

## The one-sentence version

The agent's understanding is not a dump. It is a **four-layer orientation
system**: index a graph, rank it for this turn, inject a *frozen* snapshot plus
*deltas*, and — this is the load-bearing part — teach the model in the prompt
that the map is **not ground truth** and must be confirmed with a tool before
any claim about behavior.

## Layer 1 — Index

`src/core/intelligence/repo-map.ts` (~5.3k lines) builds a SQLite graph under
the project data dir (`repomap.db`, WAL):

| Table | Holds |
|---|---|
| `files` | path, language, mtime, `symbol_count`, `pagerank` |
| `symbols` | name, kind, line/end_line, `is_exported`, signature |
| `edges` | import/dependency edges, weight, `is_type_edge` |
| `refs` | name references + resolved `source_file_id`, `import_source` |
| `calls` | call graph rows (caller symbol → callee) |
| `cochanges` | git "files that changed together" pairs |
| `external_imports` | package usage |
| `semantic_summaries` | one-line symbol blurbs (`ast` / synthetic / `llm`) |
| `shape_hashes`, `token_*` | clone detection (minhash + fragment postings) |
| trigram postings | packed substring search |

`scan()` is an ordered pipeline, not a walk:

```text
collect (capped, git-recency biased)
  → indexFile per file (tree-sitter)
  → resolveUnresolvedRefs → resolveIdentifierRefs
  → buildCallGraph → buildEdges
  → linkTestFiles → rescueOrphans
  → computePageRank → buildCoChanges
```

Edits mark files dirty and re-index on a debounce. Understanding is
**structural and topological** here, not type-level — types arrive later, from
LSP/ts-morph, only when a tool needs precision.

## Layer 2 — Rank

`computePageRank` runs weighted PageRank over the edge table with three
deliberate corrections:

- **type-edge down-weighting** (`is_type_edge` × 0.3) so a file that is only
  re-exported types cannot accumulate inbound rank from every consumer;
- **non-code exclusion** — json/md/yaml are tracked but excluded from the rank
  vector, so config cannot outrank source through dangling-node redistribution;
- **personalization** — mentioned / edited / open-editor files bias the teleport
  vector (0.7 uniform + 0.3 personalized), and dangling mass redistributes
  into that vector rather than uniformly.

`rankFiles` then applies turn-local signals PageRank structurally cannot see:

```text
score  = pagerank * 1000
       + 1                              if a neighbor of a context file
       + min(cochangeCount / 5, 3)      if it historically co-changes
       * 0.5  when >66% of its symbols are interface/type/enum/trait
       * 0.6  when a thin barrel (is_barrel && symbol_count < 10)
```

Context files always survive the filters — a mentioned file is never demoted.

`render()` fits the result to a token budget (`computeBudget` scales the budget
*down* as the conversation grows) and renders:

```text
src/services/auth/session.ts (→12)
  +SessionManager  Tracks active sessions, refresh tokens, expiry
  +SessionStore
```

`(→N)` is blast radius (inbound dependents), `+` is exported, `[NEW]` is a file
modified within 48h. Only exported symbols are rendered; files that would
render empty fall back to their exported constants.

## Layer 3 — Inject

This is the part that survives into xdev as protocol.

**The map is not in the static instruction prose.** `src/core/prompts/builder.ts`
builds the system prompt (family base prompt → `TOOL_GUIDANCE_WITH_MAP` → cwd /
project instructions) and says explicitly that the Soul Map is injected
*separately*, so it can change after edits without invalidating the cached
system prefix.

Two channels:

| Channel | Source | Lifetime |
|---|---|---|
| **Frozen snapshot** | `ContextManager.buildSoulMapSnapshot` → `src/core/context/soul-map-snapshot.ts` | idle TTL, not birth TTL — each `read()` bumps `lastAccessedAt`, so an hour of continuous use stays hot and 5 idle minutes expire it |
| **Delta** | `buildSoulMapDiff` → `<soul_map_update>` injected between turns | rebuilt when the changed-file set changes; skipped when the emitted string is identical to last time |

Edits append to the delta channel and **never** touch the frozen bytes
(`manager.ts`: "The frozen soulMapSnapshot is NOT touched — file edits append to
deltas only"). Providers without prompt caching get a live re-render instead,
because there is no cache to protect and a delta would duplicate it.

## Layer 4 — Confirm

The prompt text is explicit that the map is lossy:

> Absence from the map is NOT absence from the codebase — a missing symbol may
> be ranked out, not nonexistent. Before stating any claim about how the code
> behaves, confirm it with a soul tool (`soul_grep()` / `soul_find()` /
> `navigate()` / `read()`). The map tells you where to look; the tools tell you
> what's true.

and the prescribed workflow is a cost ladder:

```text
PLAN from the map (zero tool calls)
  → DISCOVER in parallel, only if the map does not answer
  → READ in one parallel batch, using the map's line numbers
  → EDIT (ast_edit for TS/JS, structural_edit elsewhere, multi_edit for raw text)
  → VERIFY (project: typecheck/lint/test)
```

`navigate` re-resolves paths from symbol names; `soul_query` composes
search → filter → deps → outline → read into **one** call specifically to avoid
a 4-round-trip grep loop. Memory supplies the other half — the prompt states it
as: *"Soul Map = what code IS; memory = WHY it got that way."*

## What is genuinely portable

| Layer | xdev equivalent | Status |
|---|---|---|
| Rank/orient protocol | prompt rules + deferred tools | shipped in this branch |
| Impact before high-fanout edits | `impact` over LeanKG | shipped in this branch |
| Same-turn self-check | #263 diagnostics ladder | shipped in this branch |
| Free structured compaction | working-state extractor | shipped in this branch |
| The graph itself | LeanKG, out of process | refused as in-process work |
| Frozen snapshot + delta channel | — | out of scope (needs a prompt-delta mechanism xdev does not have) |

## The tool surface, for reference

`soul_find` (fuzzy, PageRank-ranked), `soul_grep` (structured; can search inside
a dependency by name), `soul_analyze`, `soul_impact`
(`dependents` / `dependencies` / `cochanges` / `blast_radius`, with a grep
fallback when the map is not ready), `soul_query` (the staged pipeline),
`navigate` (LSP definitions/references/call+type hierarchy). Each is tagged
`[TIER-1]` in its description and the guidance puts Tier-1 before falling to
regex.

## Caveat

This is a read of **SoulForge v2's public core**. Anything about Empryo v3 —
the desktop app, morphs, cells — is not evidenced here.