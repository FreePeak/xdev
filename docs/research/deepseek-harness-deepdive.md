# xdev ↔ DeepSeek Harness — feature deep-dive (2026-09-28)

**Subject:** [`github.com/deepseek-ai/deepseek-harness`](https://github.com/deepseek-ai/deepseek-harness) — "Everything is a Plugin", MIT, TypeScript/pnpm, 61 packages, Cordis plugin framework.
**Compared against:** xdev at `origin/main` (`b024693`, was `e1a6f5e` at read time), Go, one static CGO-free binary, 637 `.go` files, 185,950 lines under `internal/`+`cmd/`. **§0.4 records four rows revised after a concurrent session landed #449.**
**Method:** static read of both trees by 8 parallel readers. Every capability claim carries `path:line` on **both** sides or says "not found" explicitly. Code beats prose — where their `docs/` and `packages/` disagree, code wins and the drift is noted. No harness or test suite was executed. Read-only pass: nothing was written outside this document.
**Supersedes for dsh purposes:** [dsh-internals.md](dsh-internals.md) (2026-09-14) is now stale in several places; see §0.2 for the corrections. That document is kept for its `dsh`-naming-era citations and its contract-level invariants.
**Sister doc, same method:** [zcode-internals.md](zcode-internals.md) (2026-09-22).

---

## 0. How to read this

### 0.1 Verdict vocabulary

Every matrix row ends in one of five verdicts:

| Verdict | Meaning |
| --- | --- |
| `dsh-only` | dsh has it with a `path:line`; xdev has no counterpart (grep-verified, not inferred) |
| `xdev-only` | xdev has it; dsh has no counterpart found |
| `both-different` | both have the capability, mechanics differ materially |
| `both-same` | both have it, mechanics equivalent |
| `dsh-doc-only` | the claim rests on their docs, not on code read here |

`not found` in a cell means the reader grepped and did not find it — it is a negative finding, not an absence of proof. Read §Confidence for each dimension.

### 0.2 Corrections to `dsh-internals.md` (2026-09-14)

**dsh moved 2,635 commits** between the old study and this one (`git log --since=2026-09-14 --oneline | wc -l` in `deepseek-harness`). The 2026-09-14 doc read `dsh-*` npm package names; the prefix is gone (61 bare-named packages now, `ls packages | grep -c dsh` → 0). Its conclusions need these fixes:

1. **Turn-end typing: the doc says xdev is "PARTIAL". xdev has none at all.** Grep for `turn/start|turn/end|step/end|TurnEndReason` across `internal/session/` and `internal/ai/` returns zero hits. The doc's §1 item 4 comparison ("xdev: PARTIAL — the four `ai.StopReason` values are per-message") is wrong in xdev's favour-by-accident: dsh's `turn/end` carries a merge-extensible reason (`packages/core/session/src/types.ts:201-232,288-301`) and xdev persists no turn boundary whatsoever.
2. **Format maturity: dsh is at v4, xdev hardcodes v3.** `packages/core/session/src/types.ts:89` `SESSION_FORMAT_VERSION = 4`, with a published migration ladder `packages/session/session-format-v0-to-v1/` … `-v3-to-v4/`. xdev: `internal/session/store.go:1052` `Version: 3`, no version dispatch anywhere in `internal/session/`.
3. **"dsh specifies no concrete default approval behavior for shell/edit" — still true.** `packages/guard/` has no default policy; confinement lives in `packages/sandbox/` and is only reached by explicit mode selection.
4. **New since 2026-09-14: dsh has no terminal UI.** No `ink`, `@opentui/*` or `react-reconciler` dependency in any `package.json`; no package named `*tui*` or `*ink*`. Its interactive surface is a React 18 + Vite SPA (`apps/web/package.json:56-60`) and an Electron desktop shell (`apps/desktop/package.json:3`). xdev's tcell TUI is a capability dsh does not have in any form.
5. **The `ignorable` flag is no longer a differentiator by itself.** dsh is at v4 with a migration chain; xdev's blanket-preserve policy (`internal/session/entries.go:80-84` `ErrUnknownEntryType`, `store.go:205-218` materialize `UnknownEntry`) still has no way to express "this one is skippable", so the gap persists but the framing is now "flat v3 vs. versioned v4 with migrations".

### 0.3 Repo shape (measured, read-only commands)

| Axis | dsh | xdev | Evidence |
| --- | --- | --- | --- |
| Commits | 20,177 | 574 | `git log --oneline \| wc -l` per repo |
| Contributors | 75 | 5 | `git shortlog -sn --all \| wc -l` |
| Commits since 2026-09-14 | 2,612 | 218 | `git log --since=2026-09-14 --oneline \| wc -l` |
| Language | TypeScript, pnpm monorepo | Go, single module | `pnpm-workspace.yaml` vs `go.mod` |
| Source lines | 3,440,807 (19,766 ts/tsx) | 185,950 (637 go) | `find … -exec cat \| wc -l` excl. node_modules/dist/.worktrees |
| License | MIT | Apache-2.0 | `LICENSE` head |
| Distribution | ~100 npm packages + 6 bundles (`packages/bundle/{base,sdk-minimal,sdk-app,headless,acp-app,web-app}`) + Electron installers | one static binary (31 MB) + `xdev update` self-updater (`internal/dist/`) | |
| Build/test | pnpm + `Makefile` + `lefthook.yml` + 8 vitest configs | `scripts/` + one `release.yml` workflow | |
| TUI | **none** | tcell/v2 | grep verified both sides |
| Telemetry | OTel OTLP/HTTP ships and is egress-tested | **none, by policy** (0 matches for otel/opentelemetry/sentry/prometheus in `go.mod`) | `packages/telemetry/otel/src/transport.ts:1` |

### 0.4 Corrections from a concurrent xdev session (2026-09-28, #449 `perf/loop-cheap-wins`)

While this document was being written, another session did its own dsh read and landed
[`b024693` `perf/loop-cheap-wins`](https://github.com/FreePeak/xdev/pull/449), which changes four
rows in this file. Their analysis is in [2026-09-28-deepseek-harness-gap.md](2026-09-28-deepseek-harness-gap.md)
(105 lines, ~3.9k lines of their own `file:line` evidence) — a sibling document, not a competitor;
read both. What landed:

| #449 change | Where in this doc | Revised verdict |
| --- | --- | --- |
| `session.UnansweredToolCallNotice` — a rebuild-time stand-in for an unanswered call | §1.1, §1.2, §9.2 port 2 | `dsh-only` → **partly landed**; the persisted `tool/result` closer is still missing |
| `Agent.ToolTimeout` / `DefaultToolTimeout = 10m` | §4.1, §4.3, §7.1 | `dsh-only` → **landed** (run-level default, not a per-tool declaration) |
| `maxBatchItems = 32` | §3.1 | folded into the existing `xdev-only` concurrency-cap row |
| repeat-call detector (thresholds `[3,5,8]`, model-order counting) | §4.1, §7.1 | `dsh-only` → **both-same** |

**What this does not change.** The turn/step lifecycle is still absent: `b024693` leaves nine bare
`turn_end` emissions with no reason (`internal/agent/loop.go`), so port (1) in §9.2 stands unclaimed
and still gates port (2). Their gap doc says the same thing in §6: "Next three cheap wins, in order:
`concludeTurn`, the repeat-call detector, a named turn/step state machine with a `turn_end` reason" —
the repeat-call detector is now done, so the turn machine is the next one.

Their verdict ("xdev is not behind on the hard parts", §0 of the gap doc) and this document's
("dsh is deeper in durability and forensics", §9.1) are compatible, not contradictory: they are
comparing *mechanisms*, this document is comparing *forensic guarantees*. Where they differ on
mechanism, their read is deeper — `session.UnansweredToolCallNotice` is a strictly better answer to
"a wedged tool call left a hole" than this document's "xdev silently drops dangling calls", and
`ToolTimeout` closes a real hang the matrices here recorded as absent.

---

## 1. SESSION CORE

Persistence, tree/fork, resume, crash repair, format.

### 1.1 Capability matrix

| Capability | dsh (path:line) | xdev (path:line) | Verdict |
| --- | --- | --- | --- |
| Append-only contiguous seq-numbered event log | `packages/session/session-persistence/src/index.ts:118-119` "events are contiguous from seq 0 and never rewritten"; `storage-contract.ts:145-151` `assertContiguous` | `internal/session/store.go:534-641` `appendLocked`; entries carry `id`/`parentId` `internal/session/entries.go:21-29` | both-different |
| Physical file: plain JSONL | `packages/session/session-persistence-jsonl/src/format.ts:42` `logSuffix` → `.jsonl.zstd` or `.jsonl`; default zstd `src/index.ts:68` | `internal/session/listing.go:52-56` `<ts>_<id>.jsonl` | both-different |
| zstd-checksummed frame encoding of the log | `session-persistence-jsonl/src/index.ts:44-46` (zstd frame codec), `:77-81` header-frame assertion | not found | dsh-only |
| Fixed-width title slot on line 1 | not found (header is one JSON line: `format.ts:119` `toHeaderLine`) | `internal/session/entries.go` `MarshalTitleSlot`/`ParseTitleSlot`; doc `docs/reference/session-format.md:28-36` | xdev-only |
| Session header separate from the event log | `packages/core/session/src/types.ts:94-129` `SessionHeader` (version, id, createdAt, cwd, parentSession, isSeeded, origin, delegationDepth) | `internal/session/store.go:1051-1056`; `ParseHeader` `entries.go:702-712` | both-same |
| Format version + published migration ladder v0→v4 | `packages/core/session/src/types.ts:89` `SESSION_FORMAT_VERSION = 4`; `packages/session/session-format-v0-to-v1/`, `-v1-to-v2`, `-v2-to-v3`, `-v3-to-v4` | flat `Version: 3`, no migration code, no version dispatch (`internal/session/store.go:1052`; `fork.go:33`) | dsh-only |
| Linear log vs tree | linear, `seq`-addressed, no parent links | parent/child tree with mutable leaf: `internal/session/store.go:46-49`; `Branch` `:950-970`; `TypeBranch` marker `entries.go:74-77` | both-different |
| Fork = new session inheriting a prefix | `packages/core/session/src/fork.ts:21-30` `buildForkSeed` (prefix + `session/end-seed` + synthetic closers) | `internal/session/fork.go:15-74` `ForkSession` (verbatim file copy, rewritten header) | both-different |
| Fork lineage stored as | header `isSeeded` + numeric `inheritedEventCount` beside the log (`session-persistence/src/index.ts:66-83`) | header `parentSession` only (`internal/session/fork.go:35`); no inherited-prefix cut recorded | both-different |
| Inherited-prefix marker event | `packages/core/session/src/types.ts:403-427` `session/end-seed { inherited?: true }` | not found | dsh-only |
| In-session branch (move leaf, keep file) | not found (no branch/revert primitive; fork = new session) | `internal/session/store.go:950-970` `Store.Branch` + `TypeBranch` marker (`entries.go:74-77`) | xdev-only |
| Branch summary on rewind | not found | `internal/session/entries.go:123-128` `BranchSummaryEntry`; emitted by rewind `internal/tool/checkpoint.go:248-260` | xdev-only |
| Named checkpoints + model-facing `rewind` tool | not found in session core | `internal/session/entries.go:65-69` `TypeCheckpoint`; `internal/tool/checkpoint.go:186-264` `RewindTool` | xdev-only |
| Branch-point introspection | not found | `internal/session/store.go:920-948` `Branches()` | xdev-only |
| Context reset marker | compaction/shadow ops; `compaction/prune` `packages/compaction/compaction/src/types.ts:68-80` | `internal/session/entries.go:57,130-136` `reset_boundary`; honored `internal/session/context.go:78-90` | both-different |
| Compaction: summary + kept range | `packages/compaction/compaction/src/types.ts:24-72` (`compaction/start`/`summary`/`end`, `shadowedRange`, `shadowedSeqs`, `usage`, `model`) | `internal/session/entries.go:105-117` `CompactionEntry` (`summary`, `firstKeptEntryId`, `tokensBefore`, `method`) | both-different |
| Crash repair: synthetic closers appended to log | `packages/core/session/src/repair.ts:53-98` `openTurnClosers`, `:209-211` `interruptedTurnClosers`; synthesized in-memory on cold read `packages/session-query/session-query/src/cold-read.ts:55` | not found — an aborted turn "persists nothing past the prompt", inferred from the tail (`internal/session/status.go:77-97`) | dsh-only |
| Crash repair: torn physical tail | `session-persistence/src/index.ts:118-119`; `session-persistence-jsonl/src/index.ts:111-113` `tornTruncateTo`/`recoveredTail` | `internal/session/store.go:145-164` skip unterminated final line, record `tornTail`; `:172-181` `RepairNotice()`; cut-back on first append | both-different |
| Interrupted-turn recovery codes for unanswered tool calls | `packages/core/session/src/repair.ts:15-18` `TOOL_NOT_STARTED` / `TOOL_OUTCOME_UNKNOWN`, written back into the log on open | **landed in a cheaper form** — `session.UnansweredToolCallNotice`, a rebuild-time stand-in rather than a persisted closer (`internal/session/context.go:46-53`); not a `tool/result`, so the call still has no result block | both-different |
| Turn-end reason typing | `packages/core/session/src/types.ts:201-232` `TurnEndReasonMap` (completed/aborted/blocked/error/max-tokens/interrupted/forked), merge-extensible | no turn-end record; stop reason only inside `ai.Message` (`internal/session/status.go:44-45`) | dsh-only |
| Per-turn/per-step lifecycle events | `types.ts:288-301` `turn/start`,`turn/end`,`step/start`,`step/end` | not found (only `message` + `model_change` + custom) | dsh-only |
| Request envelope (model/tools/config) as durable log state | `types.ts:390-395` `request/header` (EpochHeader = config + adapterDefaults + tools), `request-header.ts:63-68` `foldRequestHeader` | only `model_change` carries a model string (`internal/session/entries.go:95-101`); tools/config are process state | dsh-only |
| Request route metadata logged | `types.ts:402` `request/context` (`RequestContext` `types.ts:254-263`) | not found | dsh-only |
| Why a header snapshot was appended | `types.ts:266-273` `RequestHeaderReason = 'initial'\|'resume'\|'change'\|'series'` | not found | dsh-only |
| System prompt on the surface, not in the header | `types.ts:247-250` `system?: never` + `system/message` event | `internal/session/context.go:8-10,14` system prompt passed in, not persisted | both-different |
| Forward-compat with unknown entry/event types | fail-closed unless `ignorable: true`: `packages/session/session-persistence/src/storage-contract.ts:74-80`; `packages/core/session/src/known-event-types.ts:22-82`; flag `types.ts:504-511` | preserve-and-continue: `internal/session/entries.go:80-84` `ErrUnknownEntryType`; `store.go:205-218` materializes `UnknownEntry` | both-different |
| Refuse rather than misread a newer log | `storage-contract.ts:74-80`; version gate `:46-53` | accepts foreign headers/entries silently (`internal/session/store.go:120-124`) | dsh-only |
| Silence unknown *required* events | dsh: refused as designed; xdev: preserved as designed | | both-same (opposite policies, both deliberate) |
| Surface (model-visible) vs log-only distinction | `types.ts:433-447` `SurfaceEventType`/`SurfaceEvent`; non-surface may not carry `surfaceOp` `types.ts:512-515`; query filter `packages/session-query/session-query/src/filters.ts:86-89` | implicit only: `buildContext` decides model-visibility per entry type (`internal/session/context.go`) | dsh-only |
| Cross-process write ownership | `packages/session/session-persistence-jsonl/src/lease.ts:1-27` flock/semaphore, no expiry by design | none; append is single-process, error latches `internal/session/store.go:57` | dsh-only |
| Durability barrier as an explicit op | `session-persistence/src/handle.ts:100-109` `flush()`; `packages/session/session-checkpoint-policy/src/index.ts:29-38,70-82` flush before model dispatch, before top-level tool body, before each step | bufio Flush per append, no fsync by default; `Options.StrictFsync` `internal/session/store.go:26-29,92-95` | both-different |
| Read handle / write handle split | `session-persistence/src/handle.ts:14` `SessionAccess = 'read' \| 'write'` | not found | dsh-only |
| Change token for read-model caches | `session-persistence/src/revision.ts`; `session-persistence-jsonl/src/index.ts:184-192` `fileRevision` (dev/ino/size/mtime/ctime) | `internal/session/listing.go:67-92` stat-keyed meta cache (listing only) | both-different |
| Listing sessions | `session-persistence/src/index.ts:201` `list()`; `stat()` `:194` | `internal/session/listing.go:63` `List(dataDir)` (first 4 KiB + last 32 KiB) | both-same |
| Listing/search with filters | `packages/session-query/session-query/src/filters.ts:18-24,45-90` (id/cwd/created-at/parent/availability; seq/time/type/surface/text) | not found (prefix-id resolution only `cmd/xdev/sessionops.go:112-119`) | dsh-only |
| Listing/search with filters (xdev side) | (see above) | `cmd/xdev/sessionops.go:112-119` prefix-id only | — |
| Session query: SQLite-indexed cold reads | `packages/session/session-query-sqlite/src/schema.ts`, `query.ts` | not found (blob store only, `internal/session/blob.go`) | dsh-only |
| Persistent projection checkpoints | `packages/session/session-projection-cache/src/spec.ts` | windowed materialization is in-memory per open (`internal/session/store.go:71-88`) | dsh-only |
| Status badge for an interrupted session | from `turn/end` reason / synthesized `interrupted` (`types.ts:216-221`; `session-query/src/cold-read.ts:55`) | `internal/session/status.go:57-97` `ClassifyStatus` from a 32 KiB tail window | both-different |
| Cold read that never mutates the log | `session-query/src/cold-read.ts:31-56` | `Open` never rewrites (`docs/reference/session-format.md:127`) | both-same |
| Interop: import foreign transcripts | not found — no importer package; `claude`/`codex` hits are subagent providers (`packages/subagent/subagent-codex/src/run.ts`) and hook dialects (`session-format-v0-to-v1/src/payload-validation.ts:104`) | `internal/session/import.go:1-6,48-79`; `import_claude.go`, `import_codex.go`; CLI `cmd/xdev/sessionops.go:315,333` | xdev-only |
| omp interop (resume a real omp file) | n/a | `internal/session/interop_test.go`; contract `docs/reference/session-format.md:139-149` | xdev-only |
| omp-compatible session file naming | dsh: n/a | `internal/session/listing.go:52-56` | xdev-only |
| Blobs/attachments out of line | `packages/attachment/attachment-local/` | `internal/session/blob.go` sha256 content store | both-same |
| Delegation depth / subagent origin in header | `types.ts:113-129` `origin: 'subagent'`, `delegationDepth` | `internal/session/listing.go:24-26` `TitleSource == "subagent"` | both-different |

### 1.2 What dsh has that xdev lacks

- **Crash-repair closers written back into the log** — `packages/core/session/src/repair.ts:53-98`; on cold read synthesized in memory (`packages/session-query/session-query/src/cold-read.ts:55`). Lands in `internal/session/store.go` (new `repair.go` beside `entries.go`) + a turn-end/abort entry type in `entries.go`.
- **Persisted `tool/result` closers** for unanswered calls — `repair.ts:15-18,32-41`, appended to the log on open so the dangling call has a real result block. xdev instead substitutes `session.UnansweredToolCallNotice` at rebuild (`internal/session/context.go:46-53`, landed in #449): the model is told the outcome is unknown, but the transcript still carries an assistant tool call with no matching result. Closing that last gap needs a turn boundary (below) to know *which* calls were open.
- **Typed turn/step lifecycle + `TurnEndReason`** — `types.ts:201-232,288-301`. xdev has no turn boundary; lands in `entries.go` (new types) and `status.go` (replace the tail sniff with a real reason read).
- **Durable request envelope** (`request/header` = config + adapterDefaults + tool schemas; `request/context` route metadata; `RequestHeaderReason`) — `types.ts:240-273,390-402`, `packages/core/session/src/request-header.ts:21-68`. xdev logs only `model_change`; lands in `entries.go` + `context.go`.
- **Fail-closed forward compat with an `ignorable` escape hatch** — `storage-contract.ts:74-80`, `known-event-types.ts:22-82`, `types.ts:504-511`. xdev's blanket-preserve policy has no way to express "this one is skippable"; lands in `entries.go` + `store.go`.
- **Format version migration ladder v0→v4** — `packages/session/session-format-v{0-to-1,1-to-2,2-to-3,3-to-v4}/`, gated by `assertVersion` (`storage-contract.ts:46-53`). xdev hardcodes v3; lands in `entries.go` or a new `internal/session/migrate.go`.
- **Kernel-arbitrated cross-process write lease** — `session-persistence-jsonl/src/lease.ts:1-27`. Lands beside `internal/session/store.go` (the existing platform-split precedent is `links_unix.go`/`links_windows.go`).
- **Read/write handle split + `flush()` as the only crash-survival promise** — `session-persistence/src/handle.ts:14,83,97,100-109`; `packages/session/session-checkpoint-policy/src/index.ts:29-38,70-82`. xdev's barrier is an unconditional per-append bufio Flush.
- **Fork inherited-prefix cut** (`inheritedEventCount` + `session/end-seed`) — `session-persistence/src/index.ts:66-83`, `types.ts:403-427`, `fork.ts:21-30`. xdev's `ForkSession` copies the whole file and records only `parentSession`; lands in `entries.go` + `fork.go`.
- **Explicit surface vs log-only partition and shadowing** — `types.ts:433-447,512-515`; filter at `session-query/src/filters.ts:86-89`. xdev encodes this implicitly in `context.go`.
- **Queryable/searchable session index** — `session-query/src/filters.ts:18-24,45-90` + `session-query-sqlite/src/schema.ts`. xdev has `session.List` only; new package under `internal/` beside `session`.
- **zstd frame compression with an independently decodable first frame** — `session-persistence-jsonl/src/index.ts:44-46,77-81`. xdev writes raw JSONL.

### 1.3 What xdev has that dsh lacks

- **In-file entry tree with a mutable leaf + non-destructive branching** — `internal/session/store.go:46-49,950-970`; `entries.go:74-77`. dsh's fork always makes a new session; no code re-points a live log's head.
- **Named checkpoints + model-facing `rewind` that preserves abandoned turns on disk** — `internal/tool/checkpoint.go:186-264`; `entries.go:65-69`; summary replayed as a user message (`context.go:40-41`).
- **Branch summary as a first-class context artifact** — `entries.go:123-128`; `context.go:40-41`. dsh's fork closers are log-only, not model-visible.
- **Foreign transcript import (Claude Code, Codex)** — `internal/session/import.go:1-6,48-79`; CLI `cmd/xdev/sessionops.go:279-348`. Not found anywhere in dsh `packages/`.
- **Fixed-width title slot rewritten in place + rename-driven title updates** — `entries.go` `MarshalTitleSlot`/`ParseTitleSlot`; `store.go:425-442` (WriteAt at offset 0). dsh's header is one JSON line.
- **omp wire compatibility as a tested contract** — `internal/session/interop_test.go`; `docs/reference/session-format.md:139-149`.
- **Session-scoped durable facts that are not model history** — goal snapshot (`entries.go:60-63,223-228`) and schedule snapshot with O(active) retention (`entries.go:198-209`; `store.go:614-624`). dsh's equivalents are ordinary surface-visible events.
- **Windowed materialization with a re-indexed in-memory tail** — `store.go:71-88,280-314`; dsh's equivalent is a separate projection-cache package.

### 1.4 Notable divergences

**The two are not the same shape of thing.** xdev is a *tree of entries in one file*; dsh is a *linear event log with a derived surface*. xdev's `parentId` is the branching mechanism, so branching is free (`store.go:963-968` writes one marker line and moves a pointer; the abandoned path stays replayable on a later rewind). dsh has no in-place branch primitive at all. Conversely dsh's explicit surface/log-only split gives it cheap compaction bookkeeping and a queryable index; xdev encodes visibility implicitly in `buildContext` and has no index beyond a listing.

**Crash repair is the widest real gap.** dsh treats an interrupted turn as a first-class state: synthetic `tool/result` errors with retry guidance, a `step/end`, and a `turn/end { reason: { kind: 'interrupted' } }` appended to the log (`repair.ts:53-98`; `types.ts:216-221`). xdev's position is the opposite: an aborted turn has nothing after the prompt, and the store *infers* "interrupted" by sniffing a 32 KiB tail (`status.go:57-97`). The forensic difference: dsh can answer "what did the model actually say before it died" from the log; xdev can only say a turn was interrupted here.

**Turn boundaries are simply absent on the xdev side.** There is no turn/step event; the only stop-reason evidence is a field inside an assistant `ai.Message` (`status.go:44-45`). Everything dsh keys off a turn — fork closers, crash closers, `max-tokens` rolling up as the turn's reason, `aborted` with a `TurnEndCancelCause` (`types.ts:195-232`) — has no xdev counterpart. This is the single highest-leverage port (§7.1).

**Request envelope durability.** dsh logs the full `EpochHeader` (config, adapter defaults, assembled tool schemas) as `request/header` with `RequestHeaderReason` distinguishing first/resume/change/series, and folds it offline (`request-header.ts:63-68`). xdev persists a `model_change` entry carrying only a model string (`entries.go:95-101`); tools and call config live in process state. The "state-carry half" of issue `#220` is exactly this.

**Storage discipline.** dsh splits append (best-effort) from `flush` (the barrier) and puts semantic checkpoints in front of model dispatch, tool bodies, and step boundaries, fail-closed (`session-checkpoint-policy/src/index.ts:29-38,70-82`). xdev flushes on every append with no fsync unless `StrictFsync` (`store.go:26-29,92-95`) and has no cross-process lease.

**Format maturity.** dsh is v4 with a per-generation migration chain and a build-static catalog (`packages/session/session-format-catalog/src/index.ts`); xdev is a flat v3 with no dispatch, so any wire change is a breaking change. Conversely dsh's per-line overhead is heavier: header+event lines wrapped in zstd frames, where xdev's plain append-one-line JSONL with a torn-tail cut is simpler to inspect with `jq`.

**Interop is a clean xdev win.** dsh has no transcript importer (its `claude`/`codex` references are hook dialects and a Codex *subagent provider*). xdev imports both (`internal/session/import.go:1-6`).

### 1.5 Confidence + gaps

High confidence on everything cited — primary source files read on both sides. Weakest points:

- dsh `session-persistence-jsonl/src/index.ts` read only to ~line 200 plus greps (1725 lines); torn-frame recovery (`tornTruncateTo`, `recoveredTail`, `:111-113`) cited from declarations, not full control flow.
- dsh `packages/core/session/src/index.ts` (the `Session` class) not read; claims about live-session reconstruction rest on type declarations.
- dsh `packages/attachment/`, `packages/storage/`, `packages/document/` not read — the blob/attachment row is shallow on both sides.
- xdev `internal/session/entries.go:230-350` (CheckpointEntry/UnknownEntry shapes), `blob.go`, `blob_list.go` not read in full.
- Deliberately not read: dsh `docs/` entirely (code-only method, so no doc-drift findings from the session dimension); dsh `session-projection*`, `session-stats`, `session-turn-outline`, `session-log-export` beyond filenames.

---

## 2. CONTEXT

System prompt, context files, compaction, window management, prompt caching, spill.

### 2.1 Capability matrix

| Capability | dsh (path:line) | xdev (path:line) | Verdict |
| --- | --- | --- | --- |
| System prompt as an ordered, plugin-registrable section registry with centrally allocated orders | `packages/core/system-prompt/src/index.ts:125` `SECTION_ORDERS` (30 named slots), `:405` `class SystemPrompt extends Service`, `:60` `order` field | not found — xdev builds one flat string, `internal/agent/prompt.go:225` `BuildSystemPrompt` | dsh-only |
| Third-party packages contribute prompt sections (MCP, LSP, jobs, workflow, ralph, goal, persona) | `packages/mcp/mcp-client/src/server-context.ts:32`; `packages/mcp/mcp-resources/src/index.ts:59`; `packages/lsp/tool-lsp/src/index.ts:103`; `packages/jobs/tool-jobs/src/index.ts:252` | xdev MCP tools inlined as a recap list, not a prompt section: `internal/agent/prompt.go:258` `BuildDeferredInd…` | both-different |
| `system-prompt/assemble` waterfall that can replace the whole prompt at assembly time | `packages/core/system-prompt/src/index.ts:31`; `complete?: boolean` at `:75` | not found | dsh-only |
| AGENTS.md + CLAUDE.md candidate pair per directory | `packages/context/agent-instructions/src/config.ts:12` `DEFAULT_INSTRUCTION_FILE_CANDIDATES = ['AGENTS.md','CLAUDE.md']` | `internal/agent/prompt.go:321` falls back to `CLAUDE.md` only when AGENTS.md is absent | both-different |
| Local overlay instruction files (AGENTS.local.md / CLAUDE.local.md) | `packages/context/agent-instructions/src/config.ts:13` | not found | dsh-only |
| Ancestor-walk root→cwd ordering of instruction files | `packages/context/agent-instructions/src/config.ts:11` root marker `.git`; `files.ts:511` readScopeInstruction; per-dir scope keying `render.ts:105` | `internal/agent/prompt.go:329-339` walks to filesystem root, reverses chain | both-same |
| User-global instruction file outside the repo | `packages/context/agent-instructions/src/render.ts:98` `USER_GLOBAL_FILE = 'AGENTS.md'` under `$DSH_HOME` | `internal/agent/prompt.go:108` `userAgentDir()` as second search root (for SYSTEM.md only) | both-different |
| Instruction byte budget | `packages/context/agent-instructions/src/config.ts:24` required `maxBytes`; shipped 65536 (`snapshots/session/agent-instructions/cordis.yml:18`); per-file cap `config.ts:14` `DEFAULT_MAX_SOURCE_BYTES = 1_048_576` | `internal/agent/prompt.go:165` `MaxContextBytes = 32 << 10` (32 KiB total); no per-file cap | both-different (xdev half the budget, no per-file cap) |
| Byte-budget overrun behaviour | `render.ts:19` `COMPACT_AGENT_INSTRUCTIONS_INTRO` + `truncated[]` + `omitted[]` records; UTF-8-safe cut `render.ts:69` | truncates the accumulated string by byte, marker per import: `internal/agent/prompt.go:211` | both-different |
| `@path` import expansion in instruction files | not found | `internal/agent/prompt.go:174` `expandImports`, depth 5 (`maxImportDepth` `:168`), cycle-skipped, wrapped in `<file path=…>` | xdev-only |
| Live reconciliation: fs tool touches inject changed/removed instructions mid-session | `packages/context/agent-instructions/src/index.ts:74` `FILE_TOUCH_TOOL_NAMES = new Set(['read','write','edit'])`, `:343` `tools/result` handler | not found — xdev loads instruction files once at session start (`internal/agent/prompt.go:310`) | dsh-only |
| Compaction triggers | two — `pressure` at step boundary and `context-overflow` from a provider error code: `packages/compaction/compaction/src/index.ts:32`; `compaction-basic/src/index.ts:164,194,280-294` | five in one vocabulary — `threshold` (`internal/agent/compact.go:43`, fired `:411`), `overflow` (`internal/agent/loop.go:983`), `promo…` | both-different |
| Compaction algorithm count (mutually-selectable retention methods) | 1 engine (`compaction-basic/src/index.ts:113`) with a `summarize()` subclass hook; rest are separate post-processors | 5 ladder members, first that yields a retained context wins: `handoff` (LLM summarize), `snapcompact`, `shake`, `soft` — deterministic, no model call (`internal/agent/compact_ladder.go:25-31,43-46`) | xdev-only (breadth) / both-different (shape) |
| Extensible compaction-method registry | not found beyond the subclass hook | `internal/agent/compact_ladder.go:71` `RegisterCompactionMethod` | xdev-only |
| Async/backgrounded compaction | not found (summarization awaited inside the turn) | `internal/agent/compact.go:98-102` `Async`; backgrounded when handoff is first product (`compact_ladder.go:116`); impl `compact_async.go` | xdev-only |
| Per-model compaction policy override | `packages/compaction/compaction-basic/src/config.ts:81` `modelPolicies` exact provider/model overrides, `resolveTargetPolicy` `:118`; reserved completion tokens subtracted `index.ts:65` | one `CompactionConfig` per run, swapped wholesale on failover (`internal/agent/fallback_recovery_test.go:343`) | both-different |
| Token measurement | heuristic only, no real tokenizer on either side. dsh: `CHARS_PER_TOKEN = 4` + `BLOCK_OVERHEAD = 4` + `ROLE_OVERHEAD = 4` — `packages/llm/token-meter/src/estimate.ts:13,16,19`; tool-schema price `:99` | `charsPerToken = 4`, `+8`/message, thinking counted — `internal/agent/compact.go:33,143` | both-same (dsh prices the envelope; xdev prices the transcript) |
| Provider-reported usage overrides the estimate | `packages/llm/token-meter/src/index.ts:165-169` — usage reused only when `>=` estimate | `internal/agent/compact.go:164-174` `contextTokens` — same rule | both-same |
| Trigger threshold | `thresholdRatio 0.8`, `headroomTokens` 65536, `retainRatio 0.16` — `compaction-basic/src/config.ts:20,23,75` | `DefaultCompactionRatio 0.80` (`compact.go:20`), `DefaultReserveTokens 16384` (`:25`), reserve floored at 20% of window (`:113`), `DefaultKeepRecentTokens 20000` (`:27`) | both-same in spirit, different constants |
| Compaction validates the envelope can never overflow | `compaction-basic/src/config.ts:172-188` fails at config time when `contextWindow - reservedCompletionTokens` leaves no message budget | `internal/agent/compact.go:119` `threshold()` subtracts the reserve but never the model's `MaxTokens` — a large `MaxTokens` can overflow despite a "safe" threshold | dsh-only |
| Overflow handling | one retry (`maxOverflowRetries` default 1, `config.ts:106`), then surface the error | promote to a larger-window model first, then exactly one compaction, then hard error — `internal/agent/loop.go:983-1002` | both-different |
| Tool-pairing-safe cut point | `packages/compaction/compaction/src/tool-pairing.ts`; re-exported `compaction/src/index.ts:18`; region never includes surface node 0 `system/message` — `compaction-basic/src/region.ts:107-116` | `internal/agent/compact.go:187` `findCutPoint` — walks back to `keepRecent`, never below index 1, advances past `RoleToolResult` | both-different |
| Durable compaction transaction with start/end markers + concurrency guard | `compaction-basic/src/region.ts:66-70` `CompactionEntryState`/`compaction/start`; `assertNoActiveCompaction` from `index.ts:305` | `internal/session/entries.go:108` `CompactionEntry`; anchor rebuild `internal/session/context.go:92-99` | both-different |
| Manual compact with classified failure codes | `packages/compaction/compaction/src/index.ts:35-41` `ManualCompactionErrorCode`; `runMaintenance` `:85`; `packages/compaction/command-compact/src/index.ts` | `internal/agent/compact_cli.go`; `xdev compress` `cmd/xdev/main.go:123`; `--handoff` `main.go:177` | both-different (dsh serializes against driver turns) |
| Tool-result pruning as a model-free pass | `packages/compaction/compaction-tool-result-pruner/src/index.ts:44` `ToolResultPruner`, config `:49-53`, code-point-safe slicing `:99` | not found as a separate pass; `internal/agent/compact.go:187` `findCutPoint` handles tool pairing, not pruning | dsh-only |
| Image offload before compaction | `packages/compaction/compaction-image-offload/src/image-offload.ts` | not found | dsh-only |
| Per-model compaction policy (config granularity) | `compaction-basic/src/config.ts:81` `modelPolicies` keyed by exact provider/model; `resolveTargetPolicy` `:118`; reserved completion tokens subtracted `index.ts:65` | one `CompactionConfig` per run, swapped wholesale on failover (`internal/agent/fallback_recovery_test.go:343`); constants at `internal/agent/compact.go:20-27` | both-different |
| Spill oversized tool output to disk, model gets a locator + retrieval hint | `packages/spill/spill/src/index.ts:45` `SpillStore.saveText` (seam); policy `packages/spill/spill-policy/src/index.ts:49` `maxInlineTokens` with head/tail retain `retention.ts:48`; backend `packages/spill/spill-local/src/store.ts:108` `saveTextFile` (private root `:36`, per-session dir sha256-prefixed `:78`, collision-free name `:110`) | `internal/agent/offload.go:15` `ArtifactOffloader` interface exists, **no implementation** (`offload.go:11-14` says so); threshold `:28`; results >16 KiB land verbatim, `internal/tool/sink.go:33` truncates at 64 KiB head+tail | dsh-only (backend) / both-different (seam exists, substrate does not) |
| Prompt-cache / prefix-caching markers on the request | capability flags only, **no marker emission** — `packages/llm/llm-pi-ai/src/catalog.ts:145` `CACHE_CONTROL_FORMAT_GATE`, `:249` `cacheControlFormat: 'offer'`, `:270` `supportsExplicitPromptCacheMode: 'withhold'`, `:277` `supportsCacheControlOnTools: 'offer'` | markers emitted: `internal/ai/cache.go:48,52,112` up to 4 breakpoints (last tool, system blocks, rolling conversation tail, every 15th message behind it); `:219` latches them off on a gateway 400 | xdev-only |
| MCP tool-schema deferral out of the eager tool list | MCP contributes a prompt *section*; tools stay in the request — `packages/mcp/mcp-client/src/server-context.ts:32` | deferred catalog, model discovers via `tool_search`/`tool_describe`/`tool_call` — `internal/agent/prompt.go:258`, `internal/tool/catalog.go:26-28`, `internal/mcpclient/mcp.go:406` | both-different |
| Memory-pressure-aware context maintenance | not found | `internal/memlimit/memlimit.go:15` `DefaultLimitBytes = 100 MiB`, `:93` `Pressure()`, sampled as a trigger at `internal/agent/compact.go:54` | xdev-only |
| Idle trigger for compaction | manual idle compaction only; no automatic idle or heap trigger found | `internal/agent/compact.go:54` and `:420` | xdev-only |
| System-prompt override files | closest is the persona prefix/suffix sections — `packages/core/system-prompt/src/index.ts:179,182` | `SYSTEM.md` / `APPEND_SYSTEM.md` / `PERSONALITY.md`, project-then-user (`internal/agent/prompt.go:97`) | both-different |
| FS-scan cache (adjacent, not context-window) | not checked | `internal/fscache/cache.go:23-31` TTL 1 s, 16 entries, 200 ms empty revalidation | xdev-only |

### 2.2 What dsh has that xdev lacks

- **A real system-prompt registry.** `packages/core/system-prompt/src/index.ts:125` allocates 30 named section orders; every capability package claims a slot at load time (`mcp-client/src/server-context.ts:35`, `tool-lsp/src/index.ts:105`, `tool-jobs/src/index.ts:252`, `tool-workflow/src/index.ts:325`, `tool-ralph/src/index.ts:407`, `tool-goal/src/index.ts:191`, `persona/src/index.ts:65,71`). xdev builds one `strings.Builder` (`internal/agent/prompt.go:225`) and appends `# Project context` / tool recaps inline.
- **Live instruction reconciliation on file touch.** `packages/context/agent-instructions/src/index.ts:74,343` — a successful `read`/`write`/`edit` re-probes changed instruction scopes and emits a new baseline into the inbox. xdev loads the AGENTS.md chain once (`internal/agent/prompt.go:310`); an edited AGENTS.md mid-session is invisible.
- **Local overlay instruction files.** `config.ts:13` loads `AGENTS.local.md`/`CLAUDE.local.md` after the base files, deduped per directory. xdev's chain builder (`internal/agent/prompt.go:306-339`) knows exactly two names.
- **A shipped spill backend.** `packages/spill/spill-local/src/store.ts:108` plus the policy gate `packages/spill/spill-policy/src/index.ts:49` (`maxInlineTokens`, head/tail retain at `retention.ts:48`). xdev's `ArtifactOffloader` (`internal/agent/offload.go:15`) has no implementation.
- **Tool-result pruning as a model-free pass.** `packages/compaction/compaction-tool-result-pruner/src/index.ts:44` shrinks one oversized result by code-point-safe head+tail before any LLM call and prices what it removed. Nothing in `internal/agent/` does this; xdev's only lever is the unwired 16 KiB offload hook.
- **Image offload before compaction.** `packages/compaction/compaction-image-offload/src/image-offload.ts`. No xdev equivalent.
- **Per-model compaction policy granularity.** `compaction-basic/src/config.ts:81` lets a large-window sibling model get its own threshold/headroom/retain without disturbing the default; xdev swaps the entire `CompactionConfig` on failover.
- **Envelope validation at config time.** `compaction-basic/src/config.ts:172-188` fails loudly when the window minus reserved completion tokens leaves no message budget. xdev's `threshold()` (`internal/agent/compact.go:119`) never subtracts `MaxTokens`.

### 2.3 What xdev has that dsh lacks

- **Emitted prompt-cache markers.** `internal/ai/cache.go:48,52,112` places up to 4 breakpoints (last tool, system blocks, rolling conversation tail, every 15th message behind it); `:219` latches them off on a gateway 400. dsh's llm packages only declare capability flags and explicitly withhold explicit cache mode (`packages/llm/llm-pi-ai/src/catalog.ts:270`).
- **Deterministic offline compaction members.** `snapcompact`, `shake`, `soft` (`internal/agent/compact_ladder.go:25-31`) need no model call. dsh's only engine (`compaction-basic/src/index.ts:113`) always summarizes through the LLM.
- **A runtime-extensible compaction ladder.** `internal/agent/compact_ladder.go:71` `RegisterCompactionMethod` lets another package add a retention method without editing the ladder, validated against `config.CompactionMethodNames` (`internal/config/settings.go:48`).
- **Background compaction.** `internal/agent/compact.go:98` `Async` runs the handoff summarize off the turn and applies at the next boundary.
- **Memory-pressure and idle triggers.** `internal/agent/compact.go:54` and `:420`. dsh has manual idle compaction but no automatic idle or heap trigger.
- **Window promotion on overflow.** `internal/agent/loop.go:987` climbs to a bigger-window model before compacting, deliberately without arming the revert policy (`internal/agent/fallback_recovery.go:451`).
- **Instruction `@path` imports.** `internal/agent/prompt.go:174`, depth 5, cycle-safe.
- **System-prompt override files.** `SYSTEM.md` / `APPEND_SYSTEM.md` / `PERSONALITY.md`, project-then-user (`internal/agent/prompt.go:97`).

### 2.4 Notable divergences

**System prompt: registry vs. string.** dsh treats the prompt as an ordered registry assembled per turn through a `system-prompt/assemble` waterfall, where a plugin can declare its section `complete` and replace everything (`packages/core/system-prompt/src/index.ts:31,75`). xdev builds a `strings.Builder` once per session (`internal/agent/prompt.go:225`) and appends fixed headings. Consequence: in xdev, every new capability (memory backend, LSP section, plan-mode note) edits one function; in dsh it is a package that claims a slot.

**Compaction: algorithm ladder vs. algorithm + post-processors.** xdev's ladder picks one of five retention algorithms per boundary and the ladder owns the anchor, token count and recorded method (`internal/agent/compact_ladder.go:43-46,226`). dsh has one summarizer and pushes size reduction into orthogonal, independently-loadable services — pruner, image offload, spill policy — all reading the same log. xdev's shape is the right one for a single-binary no-plugin-VM runtime; dsh's is the right one for 61 independently-shipped packages.

**Measurement: dsh prices the request, xdev prices the transcript.** dsh's meter prices the *envelope* — `estimateToolsTokens` (`packages/llm/token-meter/src/estimate.ts:99`) and `estimateSystemMessage` (`:73`) are separate arms in a singleton service every consumer shares. xdev's `estimateTokens` (`internal/agent/compact.go:143`) walks message text + thinking + 8 chars per message and never sees the tool schemas. With ~30 eager tools plus MCP tools, that is a systematic under-count of the exact thing that fills the window.

**Spill: seam without substrate.** xdev built the interface first (`internal/agent/offload.go:15`, threshold `:28`) and left `Agent.Offload` nil, saying so in the comment (`offload.go:11-14`). dsh built the seam, the host backend, the retention policy and the multimodal path. Until xdev lands a backend, oversized tool output is truncated in memory (`internal/tool/sink.go:33`).

**Instruction budget is a hard string cut vs. an accounted budget.** xdev spends one 32 KiB pool across the whole chain and truncates byte-wise mid-file (`internal/agent/prompt.go:211`). dsh budgets 65536 per baseline *and* caps each file at 1 MiB, and reports which files were omitted vs. truncated (`render.ts:22-33`), with a UTF-8-corrected cut (`render.ts:69`).

### 2.5 Confidence + gaps

High confidence on every claim: each carries a `path:line` read directly. No verdict rests on prior art.

- **Not determined:** the exact dsh prompt-assembly *caller* — the registry and its consumers were read, not the agent-loop call site, so the position of assembly relative to tool-schema resolution is inferred.
- **Not determined:** whether dsh's `agent-instructions` budget of 65536 (`snapshots/session/*/cordis.yml:18`) is the shipped default or a snapshot-local override; `Config.maxBytes` is `z.number().required()` (`config.ts:42`) with no default, so the value comes from a preset that was not read.
- **Not determined:** dsh's spill retention/cleanup beyond the file list (`packages/spill/spill-local/src/cleanup.ts` exists, unread).
- **Deliberately not read:** `packages/util/` tokenizer (grep for tiktoken/js-tiktoken/gpt-tokenizer across `packages/**/src` returned only prose hits — confident no real tokenizer is vendored, package unread); xdev's `docs/XDEV_MAX_CONTEXT_TOKENS-before-after.md`; dsh `compaction-basic/src/summarizer.ts`.
- **Not determined:** either side's explicit "pin" semantics. Both pin only by construction (keep a suffix / exclude surface node 0); neither exposes an explicit pin policy.

## 3. SUBAGENTS

Delegation, fan-out, agent types, isolation, result delivery.

### 3.1 Capability matrix

| Capability | dsh (path:line) | xdev (path:line) | Verdict |
| --- | --- | --- | --- |
| Model-invoked delegation tool | `packages/subagent/tool-subagent/src/index.ts:379` `defineTool({ name: toolName … })`, default `subagent`, configurable per-instance | `internal/agent/task.go:85` `TaskToolName = "task"`; schema `internal/agent/task.go:195` | both-different |
| Auto-delegation heuristic (spawns without a model tool_call) | not found — grep for `auto-delegat\|autoDelegate\|heuristic.{0,20}delegat` over `packages/` returns nothing; delegation enters only through the registered tool's `execute` (`tool-subagent/src/index.ts:471`) | not found — the only `SpawnChild` call sites are `TaskTool.Execute`/`executeBatch` | neither |
| Named agent types: user-authored definition format | no definition files; a "type" is a configured `toolName` + `provider` pairing (`tool-subagent/src/index.ts:107-110`), so multiple named tools can coexist | markdown + YAML frontmatter, `AgentDefinition` (`internal/agent/discovery.go:23`); roots in `AgentDiscoveryRoots` (`internal/agent/discovery.go:116`) | dsh-only (authorable definitions) |
| Builtin agent types shipped | none — providers are `spawn`, `fork`, `acp`, `codex`, `claude-code`, `dsh-sdk`: transports, not personas | 5 embedded via `//go:embed agents/*.md` (`internal/agent/discovery.go:73`): `scout`, `reviewer`, `security-reviewer`, `sonic`, `task` | xdev-only |
| Per-spawn re-resolution of definitions (no restart) | not applicable (no files) | `internal/agent/task.go:143-159` `discoverAgents` mtime-fingerprint cache, advertised in the tool description `task.go:108` | xdev-only |
| Per-agent model / reasoning effort / tool allowlist / spawns policy | `agentOptions` overrides only on the provider config (`tool-subagent/src/index.ts:74`), route-merged `child-agent.ts:99`; `toolFilter` `tool-subagent/src/index.ts:87` | frontmatter `model`, `thinkingLevel`, `tools`, `spawns` (`internal/agent/discovery.go:29-38`); spawns policy enforced `internal/agent/task.go:395-404` | both-different |
| Context isolation: fresh child (no parent history) | `packages/subagent/subagent-spawn-in-process/src/index.ts:50` `inheritsParentContext = false` | `internal/agent/task.go:100` "its transcript never enters this conversation"; child store is its own `session.OpenMem` (`internal/agent/subagent.go:183`) | both-same |
| Context isolation: forked child seeded with parent history | `packages/subagent/subagent-fork-in-process/src/index.ts:48-55,72` (`completedTurnPrefix`, seed to last `turn/end`) | not found — no fork path in `internal/agent/`; `SpawnChild` always opens a fresh store (`internal/agent/subagent.go:183`) | dsh-only |
| Child sees the parent's context files / project policy | child joins the parent's preset composition + a delegation-scope statement (`child-agent.ts:200-219`); per-child persona shadows deployment persona | `childSystem` appends `LoadContextFiles(cwd)` to the definition prompt (`internal/agent/subagent.go:390-396`) | both-different |
| Child is told its scope is fixed | `SUBAGENT_DELEGATION_CONTEXT`, `child-agent.ts:172-176` | not found in `internal/agent/task.go` or `subagent.go` | dsh-only |
| Restricted child tool set | `toolFilter` deny/allow applied as scoped `tools.restrict()` in the child's creation window (`child-agent.ts:218`; capability-gated `packages/subagent/subagent/src/index.ts:649`) | `ChildTools` (`internal/agent/task.go:31`), per-agent `tools` filter `internal/agent/task.go:564`, missing names reported `task.go:593` | both-different |
| Structured result contract | `outputSchema` + scoped structured capture tool (`packages/subagent/subagent-in-process-driver/src/structured.ts`, imported `index.ts:39`) | `schema` + `strict` + `SubagentOutput` (`internal/agent/subagent.go:58`), minimal validator `subagent.go:413` | both-different (xdev validator is top-level only, self-documented) |
| Correction turn on schema mismatch | not found; a schema miss settles as `stopReason: 'error'` (`subagent-in-process-driver/src/index.ts:235`) | `internal/agent/subagent.go:62` `Strict` grants one correction turn, surfaced as `schema-mismatch` `internal/agent/task.go:620` | xdev-only |
| Explicit handoff tool the child must call | not found — the child's final assistant message *is* the result (`subagent-in-process-driver/src/index.ts:226`, rule in `assistant-output.ts:1-8`) | `yield` tool, auto-appended and exempt from approval (`internal/agent/subagent.go:92,214`) | xdev-only |
| Fan-out in one call (batch) | not found — one call = one child; parallelism only across separate background jobs (`tool-subagent/src/index.ts:526-560`) | `{context, tasks[]}`, `executeBatch` (`internal/agent/task.go:236`), schema `task.go:202` | xdev-only |
| Concurrency cap | job-registry managed, no in-package cap | `const maxBatchParallel = 8` semaphore (`internal/agent/task.go:230,246`) **and** `maxBatchItems = 32` bounding the batch *size* (`internal/agent/task.go:232-253`, #449) | xdev-only |
| Background dispatch returning an id | `jobs.start({ kind: 'subagent' … })` (`tool-subagent/src/index.ts:544`); continuable variant returns `subagentId` (`:530`) | `Hub.Start` → "started background job" (`internal/agent/task.go:510-517`) | both-same |
| Continue / steer a child later | `send_message` + `interrupt_agent` (`packages/subagent/tool-subagent-control/src/index.ts:28,74`), cold resume `continuation.ts:406` | hub-side `Hub.Send`/`Revive`/`Park` (`internal/agent/hub.go:444,479,498`); a session-level `send_message` tool exists for *sessions* (`internal/agent/mailbox.go:512`) but is not the child-control path | both-different |
| Enumerate children / descendants | `list_agents` with `children`/`descendants` scope (`tool-subagent-control/src/list-agents.ts:87,159-171`), durable `subagentCatalog` projection (`packages/subagent/subagent/src/catalog.ts:125`) | `Hub.Roster()` (`internal/agent/hub.go:269`) — operator/TUI only, not a model tool | dsh-only |
| Delegation depth cap | persisted, monotone `delegationDepth` in the header (`packages/subagent/subagent/src/depth.ts:28-36`), `maxDepth` per request (`types.ts:184`), default `1` (`subagent/src/index.ts:202`) | hard-coded `const maxDepth = 2` (`internal/agent/task.go:360`), in-memory `Depth int` (`task.go:62`), not persisted | both-different |
| Recursion granted to a child | the child's provider decides; depth is a runtime check | explicit re-grant of a child `TaskTool` when a definition lists `task` and depth allows (`internal/agent/task.go:456-479`) | xdev-only |
| Max turns | not a subagent-seam field; per-agent `maxTokens` inherited (`child-agent.ts:99`) | model-settable `max_turns` (`internal/agent/task.go:221`), default `DefaultSubagentMaxTurns = 30` (`internal/agent/subagent.go:24`) | xdev-only |
| Permission/approval inheritance | captured pre-await, appended to the child log as `approval/policy`, `permission/preset`, `sandbox/mode` with `source: 'delegation'` (`child-agent.ts:249,267-279`) | `Policy` + `Approve` copied into the child spec (`internal/agent/subagent.go:48-49`; `internal/agent/task.go:496-497`) | both-different (dsh reconstructable from the log; xdev runtime-only) |
| Delegated child pinned to never-approve | `child-agent.ts:172,235` — visibility, not authority, with an explicit statement so the child reports the limit rather than retrying | not found | dsh-only |
| Multi-backend providers (out-of-process) | 6 providers incl. ACP/Codex/Claude-Code/DSH-SDK, no-capabilities contract `packages/subagent/subagent/src/out-of-process.ts:1` | not found; `internal/agent/subagent.go:20-21` names out-of-process as future work | dsh-only |
| Child session durable & inspectable | `subagent/descriptor` event, version 3 (`packages/subagent/subagent/src/descriptor.ts:38,48`) | child JSONL under `DataDir/sessions` with `ParentSession` header (`internal/agent/subagent.go:190-193`), `titleSource "subagent"` marks it user-invisible to resume (`subagent.go:186-187`) | both-same |
| Child resumable as a conversation | yes for continuable children, via `startContinuable` + cold resume (`packages/subagent/subagent/src/continuation.ts:301,406`) | no: resume paths skip `titleSource "subagent"` (`internal/agent/subagent.go:186`); hub `Revive` restarts a run, not the transcript | dsh-only |
| Per-child reviewer/advisor | not found (in-process driver has no advisor hook) | `ChildAdvisor` (`internal/agent/task.go:81`), `attachChildAdvisor` `task.go:652` | xdev-only |
| Batch/multi-agent scripting | workflow seam runs a model-written script of subagents, `maxTotalAgents` — `docs/subsystems/workflow.md:5,13` (doc-only) | not found in `internal/agent/` | dsh-only |

### 3.2 What dsh has that xdev lacks

- **Fork backend: a child seeded with the parent's completed-turn prefix** — `packages/subagent/subagent-fork-in-process/src/index.ts:48,72`. Lands in `internal/agent/subagent.go` (seed the parent store before `session.OpenMem` at line 183) + a `Fork bool` on `SubagentSpec` and a frontmatter key in `internal/agent/discovery.go`.
- **Durable delegation depth** — `packages/subagent/subagent/src/depth.ts:28`, `child-agent.ts:50`. Lands in the session header + `internal/agent/subagent.go` (stamp on open) + `internal/agent/task.go:62` (replace the volatile `Depth`).
- **Continuable children + cold resume + model-facing `send_message`/`interrupt_agent`** — `tool-subagent-control/src/index.ts:28,74`, `subagent/src/continuation.ts:406`. Lands in `internal/agent/subagent.go` (drop the `yield`-only termination) + a continuable mode in `internal/agent/task.go`.
- **Model-facing child enumeration** — `tool-subagent-control/src/list-agents.ts:87`. Lands in `internal/agent/hubtool.go` (already fronts the hub to the model) + `internal/agent/hub.go:269`.
- **The fixed-delegation-scope statement injected into every child** — `child-agent.ts:172`. Lands in `childSystem` (`internal/agent/subagent.go:390`).
- **Model-selectable child LLM route** (`provider`/`model`/`reasoning_effort` per call + `list_subagent_models`) — `tool-subagent/src/index.ts:400-418`. Lands in the `task` schema (`internal/agent/task.go:195`) and `SubagentSpec` (`subagent.go:27`).
- **Out-of-process provider seam with a no-capabilities contract** — `packages/subagent/subagent/src/out-of-process.ts:1`. Lands as a second provider interface beside `SpawnChild` (`internal/agent/subagent.go:160`).
- **Workflow-scripted multi-agent fan-out with `maxTotalAgents`** — `docs/subsystems/workflow.md:5,13`. **Doc-only evidence; `packages/workflow/src` was not read.**

### 3.3 What xdev has that dsh lacks

- **Authorable named agent types with frontmatter** (model, effort, tool allowlist, `spawns` policy) — `internal/agent/discovery.go:23`; 5 embedded builtins `internal/agent/agents/*.md`; live re-discovery `internal/agent/task.go:147`.
- **Single-call batch fan-out `{context, tasks[]}` with a concurrency cap** — `internal/agent/task.go:236,230`.
- **Mandatory `yield` handoff with artifact list** — `internal/agent/subagent.go:92-118`.
- **Schema-mismatch correction turn** — `internal/agent/subagent.go:62`.
- **Per-child turn cap** (`max_turns`, default 30) — `internal/agent/task.go:221`; `internal/agent/subagent.go:24`.
- **Spawn-policy enforcement between named agents** — `internal/agent/task.go:395-404`.
- **Per-child advisor over the child's live transcript** — `internal/agent/task.go:652`.
- **A hub that is both a background dispatcher and an operator-facing inspector** (roster, transcript, inbox, wait/jobs/cancel) — `internal/agent/hub.go:99,269,342,371,394,574`.

### 3.4 Notable divergences

The two systems model delegation at different layers. dsh treats it as a *capability seam* with pluggable transports: the model calls a tool, the tool builds a `SubagentStartRequest`, and a named provider decides the child's world — fresh, forked, ACP, Codex, Claude Code. Everything a child needs to be reconstructable later (depth, persona, tool filter, permission posture, mode) is written into the child's own log. xdev has one hard-wired `SpawnChild` (`internal/agent/subagent.go:160`) plus a hub for background orchestration, with a richer *authoring* surface (5 named types, frontmatter, batch, advisor) and a thinner *provider* surface.

The one place xdev is architecturally ahead is result discipline: `yield` makes "the child finished" an explicit event rather than an inference from the last assistant message, and the batch shape gives real fan-out per turn. dsh's counterpart is the continuable/one-shot split, which buys resumability but requires the child to keep running and a second tool surface to steer it.

Permission inheritance is the same intent with a different guarantee. Both copy the parent's approval posture into the child; dsh additionally pins delegated children to `approvalPolicy: 'never'` with the rationale written into the child's prompt (`child-agent.ts:172,235`) — visibility, not authority — while xdev copies the live `Policy` + `Approve` closure (`internal/agent/subagent.go:48-49`), which is not reconstructable after the fact.

### 3.5 Confidence + gaps

High confidence on both spawn paths: `subagent.go`, `task.go`, `discovery.go` in full on the xdev side; the service, `types`, `depth`, `child-agent`, `catalog`, `list-children`, `descriptor`, `assistant-output`, the in-process driver, both in-process providers, `tool-subagent` and `tool-subagent-control` on the dsh side.

- **Not determined:** whether dsh has any auto-delegation. Grep found no code-level heuristic, which does not rule out a prompt-level one in `.agents/notes/` or deployment templates — not read.
- `continuation.ts` (largest file behind resumability) read by grep + outline; the cold-resume body at line 406 was not read verbatim. The "resumes from the descriptor" claim rests on `descriptor.ts:16-19` + `continuation.ts:406`.
- `packages/workflow` — doc-only citation, source unread.
- xdev `internal/collab/` not read; I did not establish whether it carries a second multi-agent path that would change the "one task tool" claim.
- Out-of-process providers (`subagent-acp`, `-codex`, `-claude-code`, `-dsh-sdk`) not read; their capability matrices are inferred from the `SubagentCapabilities` type, not verified per provider.

## 4. TOOL CALLS

Registration, discovery/deferral, schema, execution pipeline, concurrency, result shaping, errors.

### 4.1 Capability matrix

| Capability | dsh (path:line) | xdev (path:line) | Verdict |
| --- | --- | --- | --- |
| Single tool registry service | `packages/core/tools/src/index.ts:807` `class ToolRuntime extends Service` | `internal/tool/tool.go:43` `type Registry struct` | both-different |
| Register returns an unregister disposer | `packages/core/tools/src/index.ts:1063` `register(definition): () => void` | `internal/tool/tool.go:62` `func (r *Registry) Register(t Tool)` — panics on dup, no disposer | both-different |
| Declaration helper with typed args + validation | `packages/core/tools/src/schema.ts:554` `defineTool<…>` | `internal/tool/tool.go:31` `Parameters() json.RawMessage` — hand-written JSON Schema, no validator | dsh-only |
| Declared OUTPUT schema (canonical value) | `packages/core/tools/src/index.ts:213` `ToolOutputDefinition { schema; render; presentationMeta }` | `internal/tool/tool.go:14` `type Result struct { Text; Details; IsError }` | both-different |
| Model-facing schema projection whitelist | `packages/core/tools/src/index.ts:1282` `schemaOf()` keeps only name/description/parameters/deferLoading | `internal/tool/tool.go:166` `Registry.Defs()` builds `NamedDef` from the Tool | both-different |
| Renameable tool name | `packages/core/tools/src/schema.ts:580` `name: options.name` (load-time config) | `internal/tool/ask.go:29` `AskToolName = "ask"` const; `internal/mcpclient/mcp.go:299` `t.server + "_" + t.spec.Name` | both-different |
| Malformed-schema guard before wire | not found (lossless-JSON throw at `packages/core/tools/src/index.ts:1286`) | `internal/agent/loop.go:1364` `guardToolParams` | xdev-only |
| Deferred / progressive-disclosure catalog as a model-facing bridge | no `tool_search`/`tool_describe`/`tool_call` in `docs/tool-catalog.md` (66 headings, no such name) | `internal/tool/catalog.go:25-29` `ToolSearchName`/`ToolDescribeName`/`ToolCallName` | xdev-only |
| Provider-level deferred tool loading flag | `packages/llm/llm/src/content.ts:433` `tool.deferLoading === true`; `packages/llm/llm-deepseek/src/serialize.ts:164` `defer_loading: true` | not found (no `defer_loading` in `internal/ai`) | dsh-only |
| Tool add/remove as a developer message in history | `packages/core/session/src/surface.ts:397`; `packages/llm/llm/src/content.ts:391` `declarations.set(tool.name, { …, deferLoading: true })` | not found — xdev re-sends `Defs()` each request | dsh-only |
| Registry-change notification | `packages/core/tools/src/index.ts:208` `'tools/change'(): void` | not found | dsh-only |
| Scoped/per-agent tool visibility | `packages/core/tools/src/index.ts:1260` `schemas(scope?: ScopeKey)` | `internal/agent/subagent.go:169` builds a child `Registry` by copying `ChildTools` | both-different |
| Per-call concurrency classification | `packages/core/tools/src/index.ts:1303` `executionMode()`; only exact `true` is parallel | `internal/tool/caps.go:53` static `ConcurrentSafe` table, **not consulted by the executor** | both-different |
| Bounded parallel pool per assistant step | `packages/core/agent-loop/src/tool-calls.ts:132,200` rolling pool, `maxParallelToolCalls` default 10 (`packages/core/agent-loop/src/index.ts:335`) | `internal/agent/loop.go:1374-1389` semaphore, `MaxToolWorkers = 6` (`loop.go:207`), every call in a batch starts concurrently | both-different |
| Result order preserved despite concurrency | `packages/core/agent-loop/src/tool-calls.ts:33` per-group outcome, skipped calls appended on abort (`:241`) | `internal/agent/loop.go:1375` `out := make([]ai.Message, len(calls))` indexed write | both-same |
| Exclusive barrier splitting a batch | `packages/core/agent-loop/src/tool-calls.ts:204` reclassify; break on a non-parallel call | not found | dsh-only |
| Background execution seam | jobs plugin, `job_kill`/`job_list`/`job_output` (`docs/tool-catalog.md:2149-2187`) | `internal/tool/bashbg.go:3` `run_in_background=true` on bash; `internal/agent/hub.go:142` background subagents | both-different |
| Cooperative per-tool timeout | `packages/core/tools/src/index.ts:266` `timeoutMs?` + `packages/guard/timeout-policy/src/index.ts` | **landed** — `Agent.ToolTimeout` / `DefaultToolTimeout = 10 * time.Minute` bounds any call that declares none (`internal/agent/loop.go:209-217,292-294`, #449); cancel-grace remains `DefaultCancelGrace = 5s` (`loop.go:1393`) | both-different |
| Repeat-call reminder guard | `packages/guard/repeat-tool-reminder/src/index.ts:77` `detailedReminder(toolName, count, canonicalArguments)` | **landed** — counted in model order at the loop boundary after `runTools` joins, thresholds `[3,5,8]`, 500-char preview cap; `TestLoopNoticesARepeatedToolCall` (PRD footer 2026-09-28) | both-same |
| Pre-execute waterfall (hooks/permission/sandbox) | `packages/core/tools/src/index.ts:153` `'tools/pre-execute'` | `internal/agent/loop.go:1498` `applyPlanMode` + `:1510` `Policy.ReviewBash` + `:1530` `Policy.Decide` + `:1559` `Intercept.ToolCall`, inline in the loop | both-different |
| Around-dispatch waterfall (timeout/retry/metrics) | `packages/core/tools/src/index.ts:164` `'tools/execute'` | not found as a seam; only `a.Hooks.OnToolStart/OnToolEnd` (`loop.go:1467,1475`) | dsh-only |
| Post-execute waterfall that can rewrite/block | `packages/core/tools/src/index.ts:176` `'tools/post-execute'`; `PostToolDecision` at `:617` | `internal/tool/interceptor.go` post-result interceptors (found by name, not read) | both-different |
| Tool body | `packages/core/tools/src/index.ts:236` `execute(args, exec): Promise<unknown>` | `internal/tool/tool.go:36` `Execute(ctx, args) (Result, error)` | both-different |
| Content materialization hooks | `packages/core/tools/src/index.ts:246` `projectContent`, `:258` `finalizeContent` | not found | dsh-only |
| UI presentation hooks (pending + completed) | `packages/core/tools/src/index.ts:290` `presentCall`, `:298` `presentResult` | `internal/ext/ext.go:699` `Renderers()` keyed by tool name | both-different |
| Approval request → decision | `packages/core/tools/src/index.ts:1744` `await approval.request(…)`; outcomes `packages/interaction/user-approval/src/types.ts:32` `'allowed-once' \| 'rejected' \| 'cancelled' \| 'unavailable'` | `internal/agent/loop.go:1550` `a.Approve(call, reason) bool` | both-different |
| Approval absent ⇒ deny | `packages/core/tools/src/index.ts:1732-1735` | `internal/agent/loop.go:1550` `a.Approve == nil` ⇒ "refused by user" | both-same |
| "Allowed always" / session memory of a grant | `packages/interaction/user-approval/src/index.ts:211` "'allowed-once' is the only grant" | not found — bool callback, no remembered grant | both-same (neither) |
| Batched approval of several pending calls | not found in `packages/interaction/user-approval` or `packages/guard` | not found — `internal/tool/ask.go:74` `AskBatchSink` is model→user questions, not approvals | both-same (neither) |
| Tier-based approval model | permission modes live in `packages/guard` (not read) | `internal/tool/approval.go:11` `TierReadOnly/TierWrite/TierExec`; `approval.go:43` `Yolo` is the zero value | xdev-only |
| Bash interceptor can rewrite the command | not read on the dsh side | `internal/agent/loop.go:1524` `args = rewritten` | xdev-only |
| Result block model | `packages/core/tools/src/index.ts:302` `ToolResult { content: ContentBlock[]; isError; meta? }` | `internal/ai/types.go:36` `Block` = text/thinking/toolCall/image; `internal/agent/loop.go:1637` results are text-only | both-different |
| Output truncation of shell results | `packages/shell/tool-bash/src/render.ts:14` `[output truncated; full output: …]`; `packages/shell/pwsh-local/src/index.ts:130` `maxOutputBytes: 64_000` + spill | `internal/tool/sink.go:33` `NewOutputSink(32*1024, 32*1024)`, marker at `sink.go:30` | both-different |
| Surrogate-pair-safe truncation / viewport paging | `packages/shell/tool-bash-persistent/src/index.ts:62` `truncateWithoutSplittingSurrogatePair` | not found | dsh-only |
| Images/files in tool results | `docs/tool-catalog.md:1012` `read_image` → "durable attachment" | `internal/tool/imagegen.go:43` `generate_image`; blob store used by browser/imagegen (`cmd/xdev/print.go:1824`) | both-different |
| Failed call presented to model | `packages/core/tools/src/index.ts:1856-1893` normalized `isError:true` + rendered error content | `internal/agent/loop.go:1473` `Result{Text: "tool call denied: …", IsError: true}` | both-same |
| Failed call stays in history | `packages/compaction/compaction-tool-result-pruner` prunes; `packages/core/session/src/tool-history.ts` | `internal/agent/loop.go:1634` `toolResultMsg` always appends, `IsError` or not | both-same |
| Retry semantics of a failed tool call | none in the registry (guards can turn a failure into a `block` correction, `:1773-1791`) | none; `internal/agent/loop.go:1583` one execute, result returned | both-same (neither retries) |
| Cancellation codes | `packages/core/tools/src/index.ts:483,486` `TOOL_ABORTED`, `TOOL_ABORTED_BEFORE_DISPATCH` | `internal/agent/loop.go:1457` abandoned-call `IsError` text naming untracked side effects | both-different |
| Launch-time tool filtering | `packages/core/tools/src/index.ts:700` `ToolRestriction`; scope-restricted visibility | `internal/tool/tool.go:91` `Remove(names…) []string` + `cmd/xdev/print.go:1972` `applyToolFilter` | both-different |
| Extension/plugin tool contribution | `docs/tool-catalog.md:53` `plugin_manager` enables/disables tool plugins at runtime | `internal/ext/ext.go:717` `Register(reg, tools)` at load only | both-different |
| MCP tool contribution | `packages/mcp/mcp-client/src/tools.ts:390` `toolName: string` | `internal/mcpclient/mcp.go:299` `remoteTool.Name()` = `server_tool`; `mcp.go:445` auto-defer when `len(tools) > DeferThreshold` | both-different |
| MCP resource tools | `docs/tool-catalog.md:123-173` `list_mcp_resources`/`read_mcp_resource` | not found (only MCP tools) | dsh-only |
| `run_code` / PTC reserved transport | `docs/tool-catalog.md:524` `run_code`; `packages/core/tools/src/index.ts:1267` excluded from `sdkSchemas` | `internal/eval/tool.go:40` `eval` (Go expression eval, ordinary tool) | both-different |

### 4.2 Full tool inventory, side by side

dsh names and locations all come from `docs/tool-catalog.md`, which the file itself says is generated by booting each plugin (`docs/tool-catalog.md:8,10` scopes it to `packages/*/tool-*`). 66 schema sections → 63 distinct model-facing names (3 duplicated across mutually exclusive deployments). The registry *mechanics* were verified in code; the per-tool *list* was not re-derived from all 61 `src/tools.ts` files (out of budget). A tool contributed by a plugin outside `packages/*/tool-*` would not appear.

**dsh (63 names):** `plugin_manager`:53 · `list_mcp_resource_templates`:123 · `list_mcp_resources`:148 · `read_mcp_resource`:173 · `stagehand_{act,extract,navigate,observe,screenshot,tabs}`:203-380 (experimental) · `ask_user_question`:450 · `run_code`:524 · `exit_plan_mode`:572 · `bash`:599 + persistent variants · `job_{list,output,kill}`:2149-2187 · `read_image`:1012 · `web_fetch`:2674 · `present`:643 · `terminal_*`:1125 · `session_*` query tools:1781 · `schedule_update`:1545 · `skill`:1756 · `workflow`:2562 · `ralph` · `team_*`/`spawn_teammate`/`wait_agent` · `todo_write` · split goal tools (`create_goal`/`get_goal`/`update_goal`) · str_replace editor · cron/daily/weekly schedule tools.

**xdev, eager (always in the request):** `read` · `write` · `edit` (`tool/read.go`, `write.go`, `edit.go`) · `bash` (`tool/bash.go`) · `grep` + `glob` (`tool/search.go:50,330`) · `eval` (`eval/tool.go:40`) · `ask` (`tool/ask.go:29`) · `todo` (`tool/todo.go:68`) · `task` (`agent/task.go:85`) · `goal` (`agent/goal.go:421`, mode-registered `cmd/xdev/tui.go:1469`) · `schedule_{create,list,delete}` (`agent/schedule.go:18-20`; no `schedule_update`) · `propose` (plan mode only).

**xdev, deferred on demand** (`cmd/xdev/print.go:1946-1961`): `ast_grep` · `ast_edit` · `github` · `hub` · `send_message` · `inbox` · `checkpoint` · `rewind` — reached by the model through `tool_search` → `tool_describe` → `tool_call` (`internal/tool/catalog.go:26-28`).

**xdev, conditional / opt-in:** `lsp` (`--no-lsp`) · `context_notes` + `new_context` (experimental gate, `print.go:1936`) · `recall`/`retain`/`reflect`/`memory_edit`/`learn` (memory backend, `print.go:1671-1695`) · `advise` (`agent/advisor.go:306`) · `vibe_*` (`agent/vibe.go:31-35`) · `ext_*` (`ext/ext.go:719`) · `mcp_*` (deferred above a threshold, `internal/mcpclient/mcp.go:395-477`).

**xdev-only names:** `eval` · `tts` · `dap` · `computer` · `security_scan` · `typesafe` · `checkpoint`/`rewind` · `tool_search`/`tool_describe`/`tool_call` · `inbox` · `propose` · `context_notes`/`new_context` · `learn` · `memory_edit` · `advise` · `vibe_*` · `ast_edit`.

**dsh-only names with no xdev counterpart:** `present` · `cordis_inspect_*` · `run_code` · `stagehand_*` · `str_replace_editor` · `terminal_*` · `job_*` · `read_image` · `web_fetch` · `skill` · `workflow` · `ralph` · `team_*`/`spawn_teammate`/`wait_agent` · `session_*` query tools · `schedule_update` · split goal tools · `exit_plan_mode` (xdev uses `propose` + a reviewer instead).

### 4.3 What dsh has that xdev lacks

- **`isConcurrencySafe(args)` per-call classifier + exclusive barriers** — `packages/core/tools/src/index.ts:1303`, `packages/core/agent-loop/src/tool-calls.ts:204`. Lands in `internal/tool/tool.go` (Tool interface) + `internal/agent/loop.go:1374`.
- **`tools/execute` around-dispatch waterfall** (timeout, retry, metrics) — `packages/core/tools/src/index.ts:164`. Lands around `executeTool` (`internal/agent/loop.go:1425`).
- **Per-tool `timeoutMs` as a declared property** — `packages/core/tools/src/index.ts:266`. xdev's `Agent.ToolTimeout` is a run-level default (#449), not a per-tool declaration; the interface field would go in `internal/tool/tool.go:25` so a slow tool (a release build, a big test run) can declare its own bound rather than inherit 10 minutes.
- **Provider-side deferred loading (`defer_loading`) + tool-add/remove developer messages** — `packages/llm/llm-deepseek/src/serialize.ts:164`, `packages/llm/llm/src/content.ts:391`. Lands in `internal/ai/` tool defs + `internal/session`.
- **`tools/change` registry event** — `packages/core/tools/src/index.ts:208`. Lands in `internal/tool/tool.go` Registry.
- **Output-schema declaration + `projectContent`/`finalizeContent`** — `packages/core/tools/src/index.ts:213,246,258`. Lands in `internal/tool/tool.go:14` Result/Tool.
- **Unregister disposer from `register()`** — `packages/core/tools/src/index.ts:1063`. Lands in `internal/tool/tool.go:62`.
- **`presentCall`/`presentResult` as first-class model-tool metadata** — `packages/core/tools/src/index.ts:290,298`. Lands in `internal/tool/tool.go` + the renderer seam at `internal/ext/ext.go:699`.
- **Background job control tools** (`job_list`/`job_output`/`job_kill`) as a model surface — `docs/tool-catalog.md:2149-2187`. xdev has background bash but no model-readable job controller; lands in a new `internal/tool/jobs.go` beside `internal/tool/bashbg.go`.
- **Surrogate-safe truncation + scrollback paging for persistent shells** — `packages/shell/tool-bash-persistent/src/index.ts:62`. Lands in `internal/tool/sink.go:30`.
- ~~Repeat-tool-call reminder guard — landed in #449~~ (see §0.4).
- **`schedule_update`, `skill`, `workflow`, `session_*` query tools, `web_fetch`, `present`, `terminal_*`, `read_image`, MCP resource tools** — `docs/tool-catalog.md:1545,1756,2562,1781,2674,643,1125,1012,123-173`.

### 4.4 What xdev has that dsh lacks

- **A model-callable deferred-catalog bridge** (`tool_search`/`tool_describe`/`tool_call`) that re-enters the full call path — `internal/tool/catalog.go:25-29,202`; installed at `cmd/xdev/print.go:1962-1965`. dsh has no equivalent tool; its only deferral is a provider wire flag. This is the single largest xdev advantage for large tool sets — 8 built-ins plus everything from extensions and MCP stay off the prompt until asked for.
- **A guard that keeps one malformed tool schema from killing the whole request** — `internal/agent/loop.go:1364`.
- **A destructive-tier approval model with `Yolo` as the safe zero value** — `internal/tool/approval.go:11-48`.
- **A bash interceptor that can rewrite the proposed command before approval** — `internal/agent/loop.go:1508-1528`.
- **Abandoned-after-cancel results that name the untracked side effect** instead of reporting a clean cancel — `internal/agent/loop.go:1457`.
- **Tool-result redaction with placeholder expansion on inbound args** — `internal/agent/loop.go:1490,1653`.
- **The ACP/rpc tool-kind classification for every built-in** — `internal/acp/acp.go:272-278`.

### 4.5 Notable divergences

**Registration.** dsh's registry is a Cordis service with scope-keyed visibility, an unregister disposer, and a `tools/change` event; xdev's is a plain mutex-guarded map that panics on duplicates and supports only whole-registry removal at launch (`internal/tool/tool.go:62,91`). xdev's per-child scoping is a different mechanism — copy `ChildTools` into a fresh registry (`internal/agent/subagent.go:169`) rather than a scope key on one registry.

**Declaration.** dsh is fully type-inferred: `defineTool` converts a typed `parameters` spec to JSON Schema and runs `validateJsonSchemaValue` on every call, throwing `ToolArgsError` (`packages/core/tools/src/schema.ts:577-599`). xdev ships hand-authored `json.RawMessage` schemas and does no arg validation at the registry; invalid args surface as whatever the tool body decides. That is a real gap: a malformed argument set is caught by a provider 400 or by a tool's own parsing rather than at the boundary.

## 5. LIFECYCLE

Boot, run modes, turn driver, background work, shutdown, plugin lifecycle.

### 5.1 Capability matrix

| Capability | dsh (path:line) | xdev (path:line) | Verdict |
| --- | --- | --- | --- |
| Launcher owns only profile/boot flags; app owns its own flag family | `apps/cli/src/args.ts:5-11`, `packages/boot/cmdline/src/index.ts:5-15` | not found — one `flag.FlagSet` parses every run flag centrally (`cmd/xdev/main.go:145-235`) | both-different |
| `dsh <name>` abbreviates `--profile <name>` | `apps/cli/src/args.ts:13,201-203` | absent (`cmd/xdev/main.go:55-79` subcommands are literal) | dsh-only |
| Run modes: TUI | profile `tui` via `--profile` (`packages/boot/app-boot/src/profile.ts:179-195` lists acp/web/headless/sdk/sdk-minimal; tui is installation-owned default, `profile.ts:198-203`) | `cmd/xdev/main.go:106`, dispatch `main.go:457-481` | both-different |
| Run mode: one-shot print/headless | `packages/bundle/headless/src/startup.ts:38-53` (`[task]`, `--json`, `--session-id`) | `cmd/xdev/main.go:104-105`; `print.go:245` | both-different |
| Run mode: ACP over stdio | `packages/boot/app-boot/src/profile.ts:180-182` | `cmd/xdev/main.go:108`; `main.go:509-527` | both-same |
| Run mode: RPC / SDK server | `profile.ts:189-194` (`sdk`, `sdk-minimal` bundles) | `cmd/xdev/main.go:107`; `main.go:483-503` (JSONL stdio RPC) | both-different |
| Run mode: web / desktop | `profile.ts:183-185`; `apps/cli/src/args.ts:83-87` (desktop refused by CLI, Electron-owned) | absent | dsh-only |
| Run mode: long-lived HTTP services (auth broker / gateway / browser relay) | not found as a launcher mode | `cmd/xdev/main.go:76`; `main.go:439-444`; `internal/serve/serve.go:86-162` | xdev-only |
| Mode selection by a single flag as well as a subcommand | no — inner args go to the booted app verbatim (`packages/boot/cmdline/src/index.ts:24-25`) | `-mode print|tui|rpc|acp` (`cmd/xdev/main.go:176`) | xdev-only |
| One process serves several surfaces (stdin EOF → bounded exit) | `packages/boot/cmdline/src/index.ts:36-53` (`AppExit`, `AppReady`), `:112-120` | not found; each xdev mode is its own process that `os.Exit`s (`cmd/xdev/main.go:480,502,526`) | dsh-only |
| Profile layering: bundle list → `cordis.patch.yml` → `--patch` overlays | `packages/boot/app-boot/src/profile.ts:37-40`; `apps/cli/src/profile-boot.ts:1-9` | not found (xdev has `-config` overlays and `-profile` base relocation, `cmd/xdev/main.go:147,174`) | both-different |
| Plugin manager verb | `apps/cli/src/args.ts:14,192-197` (forwards to pnpm) | `cmd/xdev/main.go:128`; `main.go:568-570` (marketplace) | both-same |
| Config dump without booting (`--dump-config`, `--dump-default-config`, `--dump-config-schema`) | `apps/cli/src/args.ts:33-51,115-119` | not found | dsh-only |
| Agent loop: driver repeats turns | `packages/core/agent-loop/src/agent.ts:254` `while (await this.turn()) {}`, step loop `:407` | `internal/agent/loop.go:493` `for turn := 0; limit == 0 \|\| turn < limit; turn++` | both-same |
| Turn cap configurable, 0 = unbounded | not found — grep for `maxTurns\|maxIterations\|turnLimit` across `packages/**/src/**` matched only `packages/subagent/subagent-claude-code/src/run.ts:115`; `packages/core/agent-loop/src/constants.ts:6` caps only parallel tool calls (10) | `internal/agent/loop.go:125-127` `DefaultMaxTurns = 200`, `:271-272` `MaxTurns int`, `:366-368` `effectiveMaxTurns`; flag `cmd/xdev/main.go:159` | xdev-only |
| Per-turn token budget with wrap-up injection | not found | `internal/agent/loop.go:129-145` `DefaultTurnTokenBudget`, `TurnBudgetPrompt`; applied `:553-556` | xdev-only |
| Wall-clock cap on a run | not found in the loop | `cmd/xdev/main.go:196`; `cmd/xdev/launchflags.go:297-319` `parseMaxTime`, `:347` | xdev-only |
| Empty-turn nudge recovery | `packages/core/agent-loop/src/agent.ts:489` (finish `error`/`aborted` handling only) | `internal/agent/loop.go:152-161,198` (max 2 nudges), `:622` | xdev-only |
| Failover across models / fallback chain | not found in `agent-loop`; retries live in `packages/llm/llm-retry` (package present, not read) | `internal/agent/loop.go:244-251,797-933` (failover ladder + `retry.infinite`); `internal/agent/fallback_recovery.go`, `fallback_chain.go`, `failover.go` | xdev-only (dsh evidence partial) |
| Retry-forever mode | not found | `cmd/xdev/main.go:208-209`; `internal/agent/retry.go`; `internal/agent/retry_infinite_test.go:110-195` | xdev-only |
| One-shot model handoff mid-run (prewalk) | not found | `internal/agent/prewalk.go`; `cmd/xdev/main.go:164-166` | xdev-only |
| Schedule: `after` / `at` / `every` | `packages/schedule/schedule/src/domain.ts:440-505` | `internal/agent/schedule.go:17-24`; tools `schedule.go:22-24` | both-same |
| Schedule: daily / weekly with time zone | `packages/schedule/schedule/src/domain.ts:539-649` | not found | dsh-only |
| Schedule: cron | `packages/schedule/schedule/src/domain.ts:655-656` | not found | dsh-only |
| Schedule survives restart, durable + delivery-acknowledged | `packages/schedule/schedule/src/runtime.ts:124-140` (flush must acknowledge before commit); `packages/schedule/schedule/tests/recovery.spec.ts` | `internal/agent/schedule.go:104-120` hydrate from latest `ScheduleChangedEntry`; `:91` `ErrScheduleDelivery` keeps a due reminder | both-different |
| Background job registry with output ring and settlement | `packages/jobs/jobs/src/index.ts:1-60`; `packages/jobs/jobs-local/src/index.ts:4,35-38` | not found (background work is `bash_bg` + schedules) | dsh-only |
| Goal loop that auto-continues rounds | `packages/goal/goal-round-driver/src/index.ts:18-46,76` | `internal/agent/goal.go` (goal tool + `goal_updated` entries; `cmd/xdev/goal_wire_test.go:69`) | both-different |
| Workflow engine (scripted orchestration of agents) | `packages/workflow/workflow/src/index.ts:31-70` | not found | dsh-only |
| Session resume by id | `packages/bundle/headless/src/startup.ts:44` (`--session-id`) | `cmd/xdev/main.go:150,205-206`; `cmd/xdev/sessionops.go:114-138` | both-same |
| Session fork / branch | `packages/core/session/src/fork.ts` | `cmd/xdev/main.go:154`; `sessionops.go:349-364,479-491` | both-same |
| Import foreign transcript (Claude/Codex) | not found | `cmd/xdev/main.go:151-152`; `cmd/xdev/sessionops.go:279-348` | xdev-only |
| Session export / gallery / list verbs | not found as CLI verbs | `cmd/xdev/main.go:122`; `sessionops.go:140-258` | xdev-only |
| Crash recovery: repair open turns with synthetic tool results | `packages/core/session/src/repair.ts:14-20,53,209` | **partly landed (#449)** — `UnansweredToolCallNotice` at rebuild (`internal/session/context.go:46-53`); still no persisted `tool/result`, and no turn boundary to know which calls were open | both-different |
| Crash recovery: truncate torn tail, keep prefix | `packages/session/session-persistence-jsonl/src/index.ts:1364-1368` | `internal/session/store.go:773-802`; notice `store.go:168-172` | both-same |
| Turn machine as a *small* port, not a large one | 13 durable session events + 8 live extension points, `docs/architecture.md:109`; the state machine is ~2,490 lines of TS | `Agent.Run` is one flat function with nine bare `turn_end` emissions (`internal/agent/loop.go:562…721`) | dsh-only |
| Durability: fsync on append | `packages/session/session-persistence-jsonl/src/index.ts:1320-1338` | `internal/session/store.go:727,1075,441` | both-same |
| Bounded shutdown with forced exit after grace | `apps/cli/src/process-shutdown.ts:4` (5 s), `:52-77` | only in `serve`: `internal/serve/serve.go:182-191` `serveUntilSignal` | both-different |
| SIGINT escalation to force exit | `apps/cli/src/process-shutdown.ts:69-75` | not found outside `serve` | dsh-only |
| Heartbeat (agent-loop level) | not found; heartbeats are transport-level only — `packages/api/gateway/src/stream-server.ts:106-123` (WS ping), `packages/ssh/ssh/src/index.ts:283-289` | not found | neither |
| Extension/plugin lifecycle: subprocess JSONL, per-event timeout + SIGKILL | not the dsh model — in-process Cordis fibers: `packages/goal/goal-round-driver/src/index.ts:18-19` (`name`/`inject`), `packages/boot/cmdline/src/index.ts:36-64` (`ctx.appExit`, `ctx.appReady`) | `internal/ext/ext.go:2-6,35-42,244-292,448,484-495` | both-different |
| Plugin can reach the whole `ctx` (services, agents, sessions) | `packages/extensions/tool-cordis/src/api-catalog.ts:221,240`; `packages/schedule/schedule/src/runtime.ts:51,101,123-124` | extension protocol is capabilities + actions only (`internal/ext/ext.go:269`; `cmd/xdev/router_test.go:41-91`) | both-different |
| Repository hook / plugin trust gate | `packages/boot/app-boot/src/compatibility-preflight.ts` (plugin admission preflight) | `internal/hooks/trust_test.go:42,87,123`; `cmd/xdev/main.go:68-69` | both-different |
| API gateway / controller surface for remote clients | `packages/api/gateway/src/index.ts:198` plus `session-controller`, `job-controller`, `terminal-controller`, `settings-controller`, `account-controller`, `remotes` | `internal/serve/`: `broker.go:70`, `gateway.go`, `relay.go`, `ws.go` — credential/relay only, no session/job controllers | both-different |
| goal/schedule/todo/stats/usage/memory CLI verbs | not found (schedules/goals are tools + UI: `packages/goal/goal/src/*`, `packages/client/ui-schedule/package.json`) | `cmd/xdev/main.go:60-61,117-119` (`memory`, `stats`, `usage`) | xdev-only |
| `update` / `setup` / `bench` / `say` / `gc` / `trust` / `cleanse` verbs | not found | `cmd/xdev/main.go:125,133-135,112,68-69,124` | xdev-only |

### 5.2 What dsh has that xdev lacks

- **Profile-first boot with layered patch composition** — `packages/boot/app-boot/src/profile.ts:37-40`, `apps/cli/src/profile-boot.ts:81-88`. Lands in `cmd/xdev/main.go` + a new `internal/profile` (xdev's `-profile` only relocates the base dir, `cmd/xdev/main.go:174`; `config.SetProfile` at `main.go:263`).
- **Config dumps that never boot the tree** — `apps/cli/src/args.ts:33-51,115-119`. Lands as a new `xdev dump-config` verb in the `subcommands` map (`cmd/xdev/main.go:55-79`).
- **Multiple long-lived surfaces in one process** with `AppReady`/`AppExit`/stdin-EOF lifetime — `packages/boot/cmdline/src/index.ts:36-53,112-120`. Lands in `internal/rpc` + `cmd/xdev/rpc.go`; today each xdev mode `os.Exit`s (`cmd/xdev/main.go:480`).
- **Bounded, escalating shutdown** (5 s grace, then force; second signal forces) — `apps/cli/src/process-shutdown.ts:4,52-77`. Lands as a generalization of `internal/serve/serve.go:182` plus TUI/print signal handling.
- **Open-turn repair with synthetic tool results on resume** — `packages/core/session/src/repair.ts:14-20,53,209`. Lands in `internal/session/` next to the torn-tail repair at `store.go:773-802`.
- **Cron, daily, and timezone-aware weekly schedules** — `packages/schedule/schedule/src/domain.ts:539-656`. Lands in `internal/agent/schedule.go:17-40` (currently only `after/at/every`, floor 300 s, 100 active).
- **Background job registry with bounded output ring and cause-tagged settlement** — `packages/jobs/jobs/src/index.ts:47-60`, `packages/jobs/jobs-local/src/index.ts:4,35-38`. Lands in a new `internal/jobs` + a tool beside `internal/agent/schedule.go`.
- **Workflow engine** with `workflow/start|phase|log|agent-start|agent-end` events — `packages/workflow/workflow/src/index.ts:31-70`. Lands in a new `internal/workflow`.
- **Web and desktop surfaces** — `packages/boot/app-boot/src/profile.ts:183-185`, `apps/cli/src/args.ts:83-87`. Lands in a web profile concept; xdev has no HTTP session surface beyond `internal/serve/ws.go`.
- **Remote controller API** (session/job/terminal/settings controllers over the gateway) — `packages/api/gateway/src/index.ts:198` + the sibling `packages/api/*-controller` packages. Lands in `internal/serve/` if xdev wants a control plane rather than credential relay.

### 5.3 What xdev has that dsh lacks

- **A user-facing turn cap and a wall-clock cap on a run** — `internal/agent/loop.go:125-127,271-272,366-368`; `cmd/xdev/main.go:159,196`. No equivalent in `packages/core/agent-loop` (`constants.ts:6` caps parallel tool calls only). This is xdev's clearest safety property on this dimension.
- **Per-turn token budget with a wrap-up prompt instead of a hard stop** — `internal/agent/loop.go:129-145,553-556`.
- **Model failover ladder + fallback chains with credential rotation, and `retry.infinite`** — `internal/agent/loop.go:244-251,797-933`; `cmd/xdev/main.go:208-209`.
- **Prewalk: one-shot model handoff mid-run after the first edit** — `internal/agent/prewalk.go`; `cmd/xdev/main.go:164-166`.
- **Empty-turn nudge recovery (max 2)** — `internal/agent/loop.go:198,622`.
- **Foreign transcript import and continuation** (Claude Code, Codex) — `cmd/xdev/main.go:151-152`; `cmd/xdev/sessionops.go:279-348`.
- **Long-lived local services as first-class verbs**: `auth-broker`, `auth-gateway`, `browser-relay` — `cmd/xdev/main.go:76`; `internal/serve/serve.go:86-162`.
- **Broad operator CLI surface** — `stats`, `usage`, `memory`, `search`, `gallery`, `render`, `compress`, `cleanse`, `gc`, `commit`, `worktree`, `token`, `trust`, `ps`, `completions`, `setup`, `update`, `say`, `bench`, `connect` (`cmd/xdev/main.go:100-139`). dsh's CLI is five launcher verbs plus whatever each profile's app plugin parses.
- **Subprocess extension boundary with a hard per-event timeout and SIGKILL** — `internal/ext/ext.go:35-42,448,484-495`. Isolation that in-process plugins do not get.

### 5.4 Notable divergences

**Boot model.** dsh's launcher is deliberately ignorant: it parses `--profile/--patch` and hands everything else to the tree as a `cmdlineArgs` snapshot (`apps/cli/src/args.ts:5-11`; `packages/boot/cmdline/src/index.ts:24-25`); any injected app plugin can claim it and print its own help (`cmdline/src/index.ts:10-15`). xdev has one `flag.FlagSet` with ~70 flags parsed before any subcommand is known (`cmd/xdev/main.go:145-235`). Consequence: dsh can ship a new surface as a new bundle without touching the launcher; xdev must edit `main.go` and the usage table (guarded by `cmd/xdev/usage_test.go`).

**Run modes.** dsh unifies modes as a "bundle list" (`packages/boot/app-boot/src/profile.ts:179-195`): a mode is a set of Cordis rows, and the same lifecycle services (`appExit`, `appReady`, stdin EOF) apply to all. xdev's modes are hand-wired call sites in one `main` (`cmd/xdev/main.go:428-527`), each rebuilding its own `printOptions` struct by hand — note how `acp` drops the session fl… (same lifecycle services vs. per-mode wiring).

**Agent loop.** Both are `while(turn)` drivers, but the bounding philosophy is opposite. xdev: a turn count (default 200, 0 = unbounded, `loop.go:125-127`), a per-turn token budget that *injects a wrap-up user message and keeps going* (`loop.go:145,553-556`), an optional wall clock (`cmd/xdev/launchflags.go:297-319`), and a retry/failover ladder that can run forever (`loop.go:836-904`). dsh: `while (await this.turn()) {}` with no bound found in the loop package. For a harness whose plugins are in-process, an unbounded loop is a wedge risk; for a harness with process isolation per extension it is less so — but the model bill is the same on both.

**Background work.** dsh has three distinct layers: durable time-based schedules with flush-acknowledged delivery (`packages/schedule/schedule/src/runtime.ts:124-125` — a reminder is marked delivered only after the session store acknowledges), durable jobs with an in-memory bounded output ring (`packages/jobs/jobs-local/src/index.ts:4,35-38`), and scripted workflow runs (`packages/workflow`). xdev has two: durable schedules (`internal/agent/schedule.go`, with `ErrScheduleDelivery` keeping a due reminder) and background bash. Neither has a "this result is a job the model can poll" surface; dsh does, as tools (`job_list`/`job_output`/`job_kill`, §4.3).

**Shutdown and crash.** dsh's session log fsyncs each append and truncates a crash tail (`packages/session/session-persistence-jsonl/src/index.ts:1320-1338,1364-1368`), and it additionally *repairs* open turns on open, substituting synthetic tool results so a resumed conversation has no dangling calls (`packages/core/session/src/repair.ts:14-20,53,209`). xdev fsyncs (`internal/session/store.go:727`) and truncates the torn tail (`store.go:773-802`) but leaves a dangling tool call to be silently dropped at context build (`internal/session/context.go:35-39`).

**Plugin lifecycle.** Opposite trust models. xdev extensions are child processes speaking JSONL with a 5 s default per-event timeout, a 10 s handshake floor, and SIGTERM→SIGKILL termination (`internal/ext/ext.go:35-42,244-292,484-495`); a hung extension cannot wedge the harness, and a plugin reaches only what its declared capabilities grant (`cmd/xdev/router_test.go:41-91`). dsh plugins are in-process fibers with access to the full `ctx`; a plugin *can* wedge the harness, and a plugin reaches everything (`packages/extensions/tool-cordis/src/api-catalog.ts:221`). dsh compensates with a sandbox package (§7); xdev compensates with process isolation.

### 5.5 Confidence + gaps

- **High confidence**: the xdev CLI surface, flags and dispatch order (read in full: `cmd/xdev/main.go`, `launchflags.go`, `print.go`, `sessionops.go`); the dsh launcher and profile model (`args.ts`, `profile-boot.ts`, `profile.ts`); xdev loop bounding constants (`internal/agent/loop.go`); dsh schedule runtime (`packages/schedule/schedule/src/runtime.ts` in full); both crash-tail paths.
- **Not determined**: whether dsh's agent loop has any turn or token bound. `packages/core/agent-loop/src/agent.ts` is 672+ lines and was read by grep plus three windows (`:46-370`, `:401-489`); a bound could live in a hook, in `packages/agent/*`, or in the app layer. Did not read `packages/core/agent/src/*`, `packages/llm/llm-retry/`, or their TUI app. The "xdev-only" verdicts on loop bounds are therefore "xdev has it, dsh's loop package does not" — not "dsh has none anywhere".
- **Doc drift checked**: `docs/agent-lifecycle.md` was not used as a source for any claim; where read (`:24-87`) it was consistent with `agent.ts`, and it is not cited in the matrix.
- **Not read**: dsh `packages/host/`, `packages/api/*-controller/` internals, `packages/preset/`, `packages/hooks/`, `packages/storage/`, `apps/desktop*/`, plugin-manager `operations.ts`/`tools.ts`; xdev `internal/tui/`, `internal/rpc/`, `internal/serve/ws.go` beyond a symbol grep, plus `internal/agent/hub.go`, `subagent.go`, `advisor.go`.

**Deferred discovery.** dsh pushes deferral to the model server (`defer_loading`) and to in-history developer tool-add/remove messages, so any tool can be made lazy in a deployment without a bridge tool. xdev solves it inside the harness with a three-tool bridge. dsh's approach is unavailable on non-DeepSeek routers; xdev's works everywhere but costs three tools in the eager set.

**Concurrency.** dsh reclassifies every pending call immediately before it starts and breaks the parallel group at the first exclusive call, so a batch of read-then-write serializes correctly (`packages/core/agent-loop/src/tool-calls.ts:204`). xdev launches all calls in a step at once behind a semaphore of 6 and never consults `ConcurrentSafe` (`internal/tool/caps.go:53` records the fact; `internal/agent/loop.go:1374` ignores it). A batch that mutates then reads can interleave.

**Result shaping.** dsh results are `ContentBlock[]` with tool-owned `presentationMeta` persisted on the event; xdev results are a single text string plus an opaque `Details any`, with a renderer-facing `Outcome` projection (`internal/tool/outcome.go:8`). dsh's images/files travel as content blocks and can be offloaded to durable attachments during compaction.

**Approvals.** dsh's channel returns a four-valued outcome with an explicit `cancelled` distinct from `rejected`, and `'allowed-once'` is the only grant (`packages/interaction/user-approval/src/index.ts:211`) — i.e. dsh has no "always allow" memory either. xdev collapses all of that to a bool plus a `Yolo`/`Write`/`AlwaysAsk` mode and a static tier table (`internal/tool/approval.go:34-43`). Both default to deny when no answerer exists.

### 4.6 Confidence + gaps

High confidence on both registration paths: `internal/tool/tool.go`, `internal/tool/catalog.go`, `cmd/xdev/print.go:1787-1977` and `packages/core/tools/src/index.ts` + `schema.ts` were read directly.

- The dsh tool inventory is from the generated `docs/tool-catalog.md`, not from reading all 61 `src/tools.ts`. Per your rule, code wins — treat the dsh *name list* as catalog-derived and the *registry mechanics* as code-verified.
- Not read: `packages/guard/` in full (only `repeat-tool-reminder` and `timeout-policy` by glob), `packages/fs/`, `packages/sandbox/`; xdev `internal/ext/ext.go` renderer semantics, `internal/tool/interceptor.go`, `internal/tool/policy.go`. "How approval is batched" on the dsh side is a negative finding from grep over two directories, not an exhaustive read.
- The claim that nothing consults xdev's `ConcurrentSafe` comes from grepping `internal/agent`; not every call site of `caps.go` outside that package was read.
- xdev `dap` and `computer` tool names inferred from constructor call sites (`cmd/xdev/print.go:1926,1827`) rather than a `Name()` grep hit.

## 6. THINKING FLOW

Reasoning capture, plan mode, todo tracking, goal loop, self-verification, the observable thinking surface.

### 6.1 Capability matrix

| Capability | dsh (path:line) | xdev (path:line) | Verdict |
| --- | --- | --- | --- |
| Reasoning captured as a first-class content block | `packages/llm/llm/src/types.ts:67-69` (`type: 'reasoning'`), `:139` (`'reasoning': ReasoningBlock`) | `ai.ThinkingBlock` (`internal/ai/openai_completions_test.go:184`); built in `internal/agent/loop.go:1219-1223` | both-same |
| Reasoning streamed as its own delta channel | `packages/llm/llm/src/types.ts:455` (`reasoning-delta`); assembled `packages/llm/llm/src/assembler.ts:63-64,112` | `internal/agent/loop.go:1264-1272` `EventThinkingStart/Delta`; flushed to `msg.Content` at `:1219-1225` | both-same |
| Delta-block invariant checked (no interleaved text/reasoning) | `packages/llm/llm/src/invariant.ts:54-55`; `assembler.ts:173-178` | not found | dsh-only |
| Reasoning replayed to the provider on the next request | `packages/llm/llm-deepseek/src/serialize.ts:31-34` (reasoning → `thinking` block with signature); `replay.ts:59-60` | `internal/ai/openai_completions.go:52-61` `openaiWireReasoning` (per-message `reasoning` alias), skip-placeholder `:217-219` | both-different |
| Redacted / encrypted reasoning blocks | `packages/llm/llm-deepseek/tests/stream.spec.ts:89` — `redacted_thinking` rejected with `UNSUPPORTED_CONTENT`; no translation in `translate.ts:52-78` | not found (grep over `internal/ai/` for `redacted`/`encrypted_reasoning` → none) | dsh-only (as an explicit rejection) |
| Reasoning effort as first-class config | `packages/core/agent/src/runtime-types.ts:31-32`; `packages/llm/llm-deepseek/src/config.ts:24,210-212` | `ai.ThinkingBudget{Tokens}` (`internal/ai/anthropic_test.go:210`); level switch via `internal/tui/app.go:1576-1615` | both-different (xdev: token budget + named levels; dsh: per-adapter effort id) |
| Reasoning-only turn treated as a non-answer | `packages/compaction/compaction-basic/tests/compaction-basic.spec.ts:1635-1636` | `internal/agent/compact_elide.go:170-186` | both-same |
| Plan mode as distinct logged state | `packages/plan/plan-mode/src/index.ts:46-55` (`plan/mode` session event), `:137-169` (projection, `stateVersion: 3`) | `internal/agent/planmode.go:28-71` — in-memory `PlanMode` struct, mutex-guarded, **no session event** | both-different |
| Plan mode replayable across resume/fork | `packages/plan/plan-mode/src/index.ts:9-13` (projection folds the session log) | not found (state is per-process; `Publish` at `internal/agent/planmode.go:513-518` is the only durable seam) | dsh-only |
| Plan mode entry/exit command | `packages/plan/plan-mode/src/index.ts:230-276` (`/plan [off\|message]`, queued-until-step-boundary) | `internal/tui/app.go:1131-1159` (`/plan` toggle, immediate) | both-same |
| Plan mode is a *hard* read-only guard | not enforced — `docs/subsystems/plan.md:5` and `packages/plan/plan-mode/src/index.ts:5-7` both say enforcement lives elsewhere; no tool-denial code in the package | `internal/agent/planmode.go:499` denies mutating tools with a pointer to `propose`; guard flag `:100-104` | xdev-only |
| Exit is gated on a user decision | `packages/plan/plan-mode/src/index.ts:294-354` (`exit_plan_mode` → `userQuestions.ask`: Approve / "Keep planning" with feedback returned to the model) | `internal/agent/planmode.go:502-507` `NewProposeTool` with host reviewer; `planOnly` `:50-59`; `yolo` `:44-46` | both-same |
| Exit tool always registered (stable catalog) | `packages/plan/plan-mode/src/index.ts:15-17,67` | `internal/agent/planmode.go:35-38` — propose tool exposed only while plan mode is live | both-different |
| Headless plan mode ends the run at the proposal | `packages/plan/plan-mode/src/index.ts:303-305` (throws if no user-questions channel) | `internal/agent/planmode.go:50-59,513` | both-same |
| Plan-mode prompt guidance section | `packages/plan/plan-mode/src/index.ts:217-225` (`plan:policy` section, deployment-owned) | `internal/agent/planmode.go:106-118` (host `note` on `PlanMode`) | both-same |
| Plan-mode transitions at a step boundary, not mid-turn | `packages/plan/plan-mode/src/index.ts:197-214` | immediate: `internal/tui/app.go:1133-1159` → `internal/agent/planmode.go:100-104` | dsh-only |
| Plan-review feedback loop ("keep planning" text) | `packages/plan/plan-mode/src/index.ts:343-348` — returned as a tool error so the model revises | `internal/agent/planmode.go:505` reviewer returns `(accept bool, note string)`; the note's return path not traced | dsh-only (likely) |
| Todo engine op surface | 1 op, whole-list replace: `packages/todo/tool-todo/src/index.ts:136-157` `todo_write` | 9 ops: `internal/tool/todo.go:83` `init,start,done,drop,block,unblock,append,view,rm` | xdev-only |
| Todo status set | 3: `packages/todo/tool-todo/src/index.ts:26` | 5 + phases: `internal/tool/todo.go:24-33,43-46` | xdev-only |
| Todo grouped into phases | not found (flat `TodoItem[]`, `packages/todo/tool-todo/src/index.ts:104-107`) | `internal/tool/todo.go:42-46` | xdev-only |
| Auto-promotion of earliest pending task | not found | `internal/tool/todo.go:498` `promoteInProgress`, described at `:3-7` | xdev-only |
| Model must keep it current | prose only: `packages/todo/tool-todo/src/index.ts:45-55` | prose `internal/tool/todo.go:72` + a mechanical gate — `internal/agent/prewalk.go:40-92` holds the model on the planning model until a todo list exists | both-different |
| Parallel-in-progress policy, deployment-chosen and enforced | `packages/todo/tool-todo/src/index.ts:37,96-98` (rejects >1 when not allowed) | not found | dsh-only |
| Todo persistence as a durable event + projection | `packages/todo/tool-todo/src/index.ts:123-134` (`todos` projection, cleared on `turn/start`, `stateVersion: 2`), `:199` `session.append('todo/write')` | `internal/tool/todo_session.go:20-25` (`user_todo_edit` custom entry), hydrate `:34-49` | both-same |
| Todo projection scoped to the standing plan, cleared on turn start | `packages/todo/tool-todo/src/index.ts:123-134` | `user_todo_edit` is an unbounded append chain (`internal/tool/todo_session.go:20-25`) | dsh-only |
| Todo rendered in the UI | `packages/client/ui-tool/src/client/tool/toolviews/plan-summary.ts:50-56`; `todo-row.tsx:42-44` | no todo renderer in `internal/tui`; the list reaches the TUI only through the plan-mode view (`cmd/xdev/tui.go:892-901,3204`) | dsh-only |
| Goal persistence | `packages/goal/goal/src/domain.ts:61-68` (`goal/change` session event), fold `:70-82`, revisioned refs | `internal/agent/goal.go:117-139` hydrates from `goal_updated` entries, last snapshot wins | both-same |
| Goal op surface | 3 tools / 5 actions: `packages/goal/tool-goal/src/index.ts:47-49` (`edit,pause,resume,complete,blocked`) | 6 ops: `internal/agent/goal.go:437` (`create,get,resume,evidence,complete,drop`) | both-different |
| Goal loop-bounding budget | rounds, not tokens: `packages/goal/goal-round-driver/src/prompt.ts:17` (`Round: n/maxGoalRounds`) | tokens: `internal/agent/goal.go:60-61,532` | both-different |
| Completion gated on evidence | not found in code; prompt prose only `packages/goal/goal-round-driver/src/prompt.ts:20-22` | hard gate: `internal/agent/goal.go:300-321` ("refusing an evidence-free completion") | xdev-only |
| Blocked requires a minimum number of rounds | `packages/goal/tool-goal/src/index.ts:33-40` (`blockedAfterConsecutiveRounds`, default 3) | not found | dsh-only |
| Authority gating on goal mutations | `packages/goal/tool-goal/src/authority.ts:100-118` (direct human or exact admitted goal round) | not found — `GoalTool.Execute` (`internal/agent/goal.go:447-501`) checks nothing about the caller | dsh-only |
| Goal revision + stale-revision errors | `packages/goal/goal/src/domain.ts:98` `GOAL_STALE_REVISION` | no revision on the goal (`internal/agent/goal.go:58-64`) | dsh-only |
| Cross-session arming: a resumed/forked goal is disarmed until rearmed | `packages/goal/tool-goal/src/index.ts:116-120` | not found | dsh-only |
| Automatic continuation rounds | `packages/goal/goal-round-driver/src/index.ts:76-119` (driver, fencing, disarm on stop `:437-455`) | `internal/agent/goal.go:188-210` — a hidden reminder in the *same* turn | both-different (dsh: new turns; xdev: reminder) |
| Side-channel reviewer/critic observing the run | not found (closest: `packages/workflow/tool-ralph/src/index.ts:157` tells the model to verify) | `internal/agent/advisor.go:25-38` (its own turn per feed, steers the primary `:198-201`) | xdev-only |
| Mid-stream interruption of thinking by a rule | not found | `internal/agent/ttsr.go:18-25`, matched against thinking deltas `internal/agent/loop.go:1270-1272` | xdev-only |
| Model handoff gated on having planned first | not found | `internal/agent/prewalk.go:19-18,40-66` | xdev-only |
| Reflection in the visible surface | reasoning blocks rendered in the trajectory: `packages/client/ui-trajectory/src/client/layout.ts:847`; `trajectory-event-projection.ts:95` | `internal/tui/app.go:573-599` `BeginThinking`/`AppendThinking`, toggleable, dropped entirely when off | both-same |

### 6.2 What dsh has that xdev lacks

- **Tool-delta invariant for reasoning/text blocks** — `packages/llm/llm/src/invariant.ts:54-55`; also `assembler.ts:173-178`. Would land in `internal/ai/` as a per-stream block-metadata validator invoked from the stream loop (`internal/agent/loop.go:1250-1300`).
- **Explicit `redacted_thinking` rejection** — `packages/llm/llm-deepseek/tests/stream.spec.ts:89`; xdev has no `redacted`/`encrypted_reasoning` handling anywhere. Lands in the content-block switches in `internal/ai/anthropic.go` / `openai_completions.go`.
- **Plan mode as durable, replayable logged state** — `packages/plan/plan-mode/src/index.ts:46-55,137-169`. xdev's `PlanMode` (`internal/agent/planmode.go:28-71`) is process-local; a resume loses the read-only guard. Lands in a new `internal/session/` entry kind + hydration in `internal/agent/planmode.go`.
- **Plan-mode transitions applied at a step boundary** — `packages/plan/plan-mode/src/index.ts:197-214`. xdev flips immediately (`internal/tui/app.go:1133-1159` → `internal/agent/planmode.go:100-104`). Lands in the loop's turn boundary (`internal/agent/loop.go`).
- **Plan-review feedback loop** ("Keep planning" text returned as a tool error) — `packages/plan/plan-mode/src/index.ts:343-348`.
- **Todo parallel-in-progress policy knob, enforced** — `packages/todo/tool-todo/src/index.ts:37,96-98`.
- **A standalone todo UI surface with per-row rendering and a done/total/active summary** — `packages/client/ui-tool/src/client/tool/toolviews/plan-summary.ts:50-56`; `todo-row.tsx:42-44`. xdev has no todo renderer in `internal/tui` (only `internal/tui/vibe.go:59` names the tool).
- **Todo projection scoped to the standing plan, cleared on turn start** — `packages/todo/tool-todo/src/index.ts:123-134`.
- **Goal authority gating** — `packages/goal/tool-goal/src/authority.ts:100-118` rejects goal mutations from a non-root agent or a turn without host-attested human input. xdev checks nothing about the caller.
- **Blocked-after-N-rounds floor** — `packages/goal/tool-goal/src/index.ts:33-40`; xdev lets `drop` close a goal from any turn.
- **Goal revision + stale-revision errors** — `packages/goal/goal/src/domain.ts:98`.
- **Goal disarm on resume/fork until a human rearms** — `packages/goal/tool-goal/src/index.ts:116-120`.

### 6.3 What xdev has that dsh lacks

- **A 9-op incremental todo engine with phases, block/unblock and auto-promotion** — `internal/tool/todo.go:83,24-33,498`. dsh has one whole-list replace call (`packages/todo/tool-todo/src/index.ts:136`).
- **A hard read-only plan-mode guard** — `internal/agent/planmode.go:499`. dsh's plan mode is explicitly soft guidance (`packages/plan/plan-mode/src/index.ts:5-7`).
- **Evidence-gated goal completion** — `internal/agent/goal.go:300-321`.
- **Token-budget goal accounting** — `internal/agent/goal.go:60-61,532`; dsh bounds rounds (`packages/goal/goal-round-driver/src/prompt.ts:17`).
- **Mid-stream rule interruption over thinking deltas** — `internal/agent/ttsr.go:18-25`; `internal/agent/loop.go:1270-1272`.
- **An advisor side-channel reviewer** — `internal/agent/advisor.go:25-38,198-201`.
- **Prewalk: model switch gated on a plan existing** — `internal/agent/prewalk.go:40-92`.
- **Junk-reasoning classification of an empty turn** — `internal/agent/compact_elide.go:170-186`; dsh's equivalent appears only in compaction tests (`packages/compaction/compaction-basic/tests/compaction-basic.spec.ts:1635`).
- **Reasoning replayed on a non-Anthropic wire via a `reasoning` message alias** — `internal/ai/openai_completions.go:52-61,217-219`. dsh has only the native DeepSeek/Anthropic thinking-block replay (`packages/llm/llm-deepseek/src/serialize.ts:31-34`).

### 6.4 Notable divergences

**Plan mode is a guard on one side and a prompt section on the other.** dsh's package docstring states that sandbox mode and approval policy enforce restrictions independently and that plan mode only injects a guidance section (`packages/plan/plan-mode/src/index.ts:5-7`); the docs page agrees (`docs/subsystems/plan.md:5`). There is no tool-denial code anywhere in `packages/plan/plan-mode/src/`. xdev denies mutating tools outright (`internal/agent/planmode.go:499`). If xdev ported dsh's plan mode it would have to keep the guard — the durable-state half is the port, the semantics are not.

**Persistence model.** dsh treats plan mode, todos and goals as session-log events with fold projections (`plan/mode`, `todo/write`, `goal/change`), so resume and fork restore them and every transition is replayable. xdev persists todos and goals as custom session entries with last-wins hydration (`internal/tool/todo_session.go:20-49`; `internal/agent/goal.go:117-139`) and keeps plan mode in memory only. Same outcome for todos/goals, strictly worse for plan mode.

**Continuation differs in kind, not degree.** dsh's goal round driver admits a *new turn* with a `renderGoalRoundPrompt` message attributed `source.kind === 'goal'` (`packages/goal/goal-round-driver/src/prompt.ts:12-25`; `packages/goal/goal/src/domain.ts:47-53`), with race fencing and lifecycle teardown. xdev injects a hidden reminder into the *same* turn at every turn boundary (`internal/agent/goal.go:188-210`). dsh's is interruptible and restartable; xdev's cannot be lost by a crash mid-loop but also cannot survive one cleanly.

**Reasoning re-feed is provider-shaped on both sides with different failure modes.** xdev carries thinking in the transcript and re-emits it; for OpenAI-completions upstreams it uses a non-standard `reasoning` key per assistant message (`internal/ai/openai_completions.go:52-61`) and falls back to a `(context elided)` placeholder for upstreams that ignore it (`:217-219`). dsh has a native path (`packages/llm/llm-deepseek/src/serialize.ts:31-34`) and *rejects* `redacted_thinking` outright (`tests/stream.spec.ts:89`) rather than guessing. Rejecting is the safer default; xdev's placeholder is the more compatible one.

**Todo semantics diverge in enforcement, not shape.** Both rely on prose to keep the list current; xdev additionally makes the list *mechanically* load-bearing by holding the planning model until one exists (`internal/agent/prewalk.go:51-63`). dsh pushes discipline into a deployment config flag enforced at the schema boundary (`packages/todo/tool-todo/src/index.ts:96-98`).

### 6.5 Confidence + gaps

High confidence on plan mode, todo and goal: the whole `src/` of `packages/plan/plan-mode`, `packages/todo/tool-todo`, `packages/goal/tool-goal`, plus `packages/goal/goal-round-driver/src/prompt.ts` and `authority.ts`; xdev's `planmode.go`, `goal.go`, `prewalk.go`, `todo.go`, `todo_session.go`, `advisor.go`, `ttsr.go`.

- **`packages/typert/` is not a thinking-flow package.** It is a code-generation/runtime-reflection layer: `packages/typert/registry/package.json:3` ("Runtime registry for generated package reflection and Zod schemas"); `registry/src/service.ts:443` (schemas, reflection, host invocations). No thinking/todo/plan/goal content. Excluded.
- **Not read**: `packages/goal/goal/src/runtime.ts` or `fold.ts`; `packages/core/agent-loop/src/` beyond grep hits on `reasoningEffort` (`agent.ts:561-572`) — whether the loop re-feeds assistant reasoning into the next request assembly was not verified beyond tracing the DeepSeek serializer; `packages/client/ui-trajectory/` (grep only, lines cited).
- **Could not determine**: whether dsh renders anything for `plan/mode` state in the desktop/web client — the wire view is `{active, pending}` (`packages/plan/plan-mode/src/index.ts:162-168`) and no consumer was found in the packages read. Whether xdev's plan-review `note` is fed back to the model on rejection: `internal/agent/planmode.go:505` receives it, but the propose tool's return path was not traced.

## 7. ACTION FLOWS

Hooks, extensions, MCP, LSP, sandbox, credentials, network, filesystem, git, delivery.

### 7.1 Capability matrix

| Capability | dsh (path:line) | xdev (path:line) | Verdict |
| --- | --- | --- | --- |
| Command hooks: JSON payload on stdin, exit-code contract | `packages/hooks/hook-protocol/src/runner.ts:67-105`; exit-2 = block w/ stderr reason `packages/hooks/hook-protocol/src/codec.ts:11,66-69` | `internal/hooks/hooks.go:341-358` (exit 2 blocks; other non-zero = warning) | both-same |
| Structured stdout vocabulary | `codec.ts:97-133` `continue`, `stopReason`, `systemMessage`, `decision`, `hookSpecificOutput` | `internal/hooks/hooks.go:6-9` — only `{"block":true,"reason":…}`, `{"input":…}`, `{"text":…}` | both-different |
| `hookSpecificOutput.permissionDecision` allow/deny/ask | `codec.ts:43-45,125-128` | not found (xdev has binary block/allow via `{"block":true}`) | dsh-only |
| Event-name guard on `hookSpecificOutput` (wrong `hookEventName` ⇒ discard) | `codec.ts:41-46,117-124` | not found | dsh-only |
| `updatedInput` (tool-arg rewrite) honored | `packages/hooks/hooks-claude-code/src/index.ts:181-182` — logged and **ignored** | `internal/hooks/interceptor.go:28-34` — honored, replaces args | xdev-only |
| `additionalContext` injected into the model turn | `hooks-claude-code/src/index.ts:199-200,257-269` | `internal/hooks/hooks.go:427-444` — `context` event replaces the system prompt | both-different |
| `systemMessage` surfaced | parsed `codec.ts:102-103`; **not surfaced** by the CC bridge `hooks-claude-code/src/index.ts:184-185` | not found | dsh-only (parsed, unexercised) |
| Two hook dialects (Claude Code + Codex) behind one protocol lib | `packages/hooks/hooks-claude-code/src/index.ts`; `packages/hooks/hooks-codex/src/index.ts` | one dialect (omp-shaped), `internal/hooks/hooks.go` | dsh-only |
| Per-hook timeout | `runner.ts:20` default 600 000 ms; per-hook override `runner.ts:74` | `internal/hooks/hooks.go:89` single 10 s default, no per-hook override | both-different |
| Hook runs through the credential-scrubbing shell executor | `runner.ts:68,2-5` | `internal/hooks/hooks.go` (own `exec`; no scrub seam found in the read range) | dsh-only |
| Durable paired `hook/invoked` + `hook/result` session events | `packages/hooks/hook-protocol/src/events.ts:75-104` (turn, point, handlerId, matcher, decision, exitCode, stderrSummary, durationMs) | `internal/hooks/hooks.go:65-66` emits only via `Emit`; no per-invocation durable record found | dsh-only |
| Workspace trust gate for repo-supplied hooks, per-hook digest | not found in `packages/hooks` | `internal/hooks/trust.go:1-19,40-50`; `internal/hooks/trust_test.go:42,87,123` | xdev-only |
| Hook discovery from project/user/extension dirs, merge precedence, name-keyed override | not found (bridges read `config.ts` per dialect) | `internal/hooks/hooks.go:91-119,182-225`; `internal/hooks/discover.go` | xdev-only |
| Matcher + `if: "Bash(git *)"` prefilter sharing the approval policy's matchers | not found | `internal/hooks/hooks.go:64-66,255-270` | xdev-only |
| Extensions as sandboxed in-VM plugins with a whitelist façade over `ctx` | `packages/extensions/cordis-host-runner/src/guard.ts:1-14`; VM host `cordis-host-runner/src/index.ts` | `internal/ext/ext.go:1-8` — extensions are **processes** over JSONL stdio, no in-process VM | both-different |
| Extension event timeout + SIGKILL | n/a (in-process) | `internal/ext/ext.go:36-42` | xdev-only |
| MCP stdio transport | `packages/mcp/mcp-client/src/transport.ts:33` | `internal/mcpclient/mcp.go:31-35` | both-same |
| MCP Streamable HTTP | `transport.ts:40-41` | `internal/mcpclient/mcp.go:37-38` | both-same |
| Legacy SSE transport | not found (`transport.ts:32-41` has only `stdio` \| `streamable-http`) | not found | both-same (absent) |
| MCP reconnect with exponential backoff + give-up | `packages/mcp/mcp-client/src/connection.ts:233-244` | `internal/mcpclient/health.go`; `server_status.go` (health probe; no reconnect loop found) | dsh-only |
| `notifications/tools/list_changed` auto re-sync | `connection.ts:263-269,296-304` | not found | dsh-only |
| MCP `tools/list` pagination limit | `packages/mcp/mcp-client/tests/fixtures/pagination-limit-server.ts` (client handling in `src/tools.ts`, unread) | not found | dsh-only (fixture-verified) |
| MCP sampling / roots | not found (grep for `sampling/messages`, `roots/list` over `packages/` → none) | not found | both-same (absent) |
| MCP elicitation | only as a Codex subagent wire passthrough: `packages/subagent/subagent-codex/src/wire.ts:614-617` | not found | dsh-only (narrow) |
| MCP resources/prompts as model tools | `packages/mcp/mcp-resources/src/tools.ts` | not found (grep `resources/` in `internal/mcpclient` → empty) | dsh-only |
| Per-tool / per-server call timeout | `packages/guard/timeout-policy/src/index.ts:55-60` (generic `TOOL_TIMEOUT` wrapper) | `internal/mcpclient/mcp.go:45-47,119-129,320-322` (120 s default, per-server override) | both-same |
| Per-server init timeout | not found | `internal/mcpclient/mcp.go:53-55,178-181` | xdev-only |
| MCP health endpoint + auto-start of a local server | not found | `internal/mcpclient/health.go`; `health_test.go:53-90` | xdev-only |
| MCP deferred-tool catalog behind a search threshold | not found | `internal/mcpclient/mcp.go:395-477` `DeferThreshold`, `deferredIndexChars` | xdev-only |
| LSP: language servers | provider generic/config-driven: `packages/lsp/lsp-stdio/README.md:28,42`; only a TypeScript e2e `lsp-stdio/tests/typescript-server.e2e.ts:23` | `internal/lsp/server.go:29-51` — gopls, rust-analyzer, typescript-language-server, pyright-langserver | xdev-only (breadth) |
| LSP: diagnostics | explicitly out of scope `packages/lsp/lsp/README.md:32,129`; server→client notifications ignored `lsp-stdio/src/connection.ts:248` | `internal/lsp/client.go:213-214,438-447` caches `publishDiagnostics`; `internal/lsp/tool.go:389-436` on-demand + `*` | xdev-only |
| LSP: document sync | transient `didOpen`→request→`didClose` per query `lsp-stdio/README.md:90` | on-demand open + cached diagnostics `internal/lsp/tool.go:218,425` | both-different |
| LSP: op set | definitions/references/implementations/hover only `packages/lsp/lsp/README.md:32` | + symbols, rename (report-only), code_actions (report-only), capabilities `internal/lsp/tool.go:63` | xdev-only |
| LSP: lazy start + idle shutdown | no idle policy found | `internal/lsp/manager.go:18-19,34-50` (5 min idle, `lsp.lazy`) | xdev-only |
| Sandbox: OS-level confinement | Seatbelt/Landlock/bwrap: `packages/sandbox/sandbox-local/src/profiles.ts:16-57`; Windows ACL + restricted token `packages/sandbox/sandbox-windows-acl/src/*` | not found (no sandbox package; approval policy only `internal/tool/policy.go:9-45`) | dsh-only |
| Sandbox: canonical writable-root allow-list shared across dialects | `packages/sandbox/sandbox/src/roots.ts:30-55` | not found | dsh-only |
| Sandbox escalation request surfaced to model/approval | `packages/interaction/user-approval/src/index.ts:73` (NEVER sentence names `sandbox_permissions`) | not found | dsh-only |
| Sandbox-unavailable structured error | `packages/sandbox/sandbox/src/index.ts:120-135` `SANDBOX_UNAVAILABLE` | not found | dsh-only |
| Approval policy deny > prompt > allow, per-tool + ordered globs | `packages/interaction/permission-presets/src/index.ts:2-3`; `user-approval/src/index.ts:12` (fail-closed on missing answerer) | `internal/tool/policy.go:9-45,59-70`; tiers `internal/tool/approval.go:34-43,55-70` | both-different |
| Compound-command conservative scanning | not found | `internal/tool/policy.go:12-29,87-90` `allowCompoundCommands` opt-in | xdev-only |
| Generic tool-call deadline guard | `packages/guard/timeout-policy/src/index.ts:25-60` | per-MCP timeouts only; no global wrapper found | dsh-only |
| Repeat-tool-call reminder guard | `packages/guard/repeat-tool-reminder/src/index.ts` | **landed** in #449 (loop boundary, model order) | both-same |
| Credential store: 0600 file + 0700 dir + mode audit | `packages/credentials/credentials-local/src/index.ts:116-143,690-691,763-764` | `internal/config/credentials.go`; `privatefile_unix.go`; `credentials_test.go:117` | both-same |
| OS keychain as a credential **source** | deferred, not shipped: `packages/credentials/credentials-local/README.md:200,212` | `internal/config/keychain.go:1-31` (read-only, `/usr/bin/security`, `keychain:` prefix) | xdev-only |
| OAuth authorization-code + PKCE | `packages/credentials/deepseek-account-platform/src/authorization-response.ts`; `protocol.ts` | `internal/oauth/oauth.go:1-5,70-77` | both-same |
| Secret redaction (reversible placeholders) | not found in the packages read | `internal/config/secrets.go:282-447` `Apply`/`Expand`/`ApplyValue`, hashed placeholder key; wired `internal/agent/loop.go:1490,1653` | xdev-only |
| HTTP fetch SSRF policy (public-IP validation, DNS pinning, NAT64 unwrap, proxy-aware) | `packages/web/web-fetch-http/src/network.ts:140-219`; `provider.ts:47-48` | `internal/websearch/websearch.go:30` (plain `net/http`; no address policy found) | dsh-only |
| Web fetch byte cap / truncation reporting | `web-fetch-http/src/provider.ts:185-197` | not found | dsh-only |
| Web search providers | `packages/web/web-search-{deepseek,exa,perplexity}/src/provider.ts`; registry `packages/web/web/src/index.ts:85-118` | `internal/websearch/adapters.go`; `websearch.go` | both-different |
| Browser control | via MCP, experimental: `packages/experimental/browser-use-playwright-mcp/src/index.ts:9-43`; chrome-devtools-mcp variant | native CDP: `internal/browser/cdp.go:3-10`, `launch.go`, `tool.go` | xdev-only (native) |
| Computer use (screenshot/click/type/scroll) | not found | `internal/computer/backend.go:29-35,46-52` | xdev-only |
| Image generation | not found | `internal/tool/imagegen.go:43`; `internal/imagegen/` | xdev-only |
| TTS | not found | `internal/tts/tts.go`; `tool.go`; `say.go`; hook event `ttsr_triggered` `internal/hooks/hooks.go:48` | xdev-only |
| Memory (recall/retain backends) | not found | `internal/memory/hindsight.go`; `mnemopi.go`; `learn.go`; `pipeline.go` | xdev-only |
| Session share (self-contained HTML + E2E-sealed loopback view) | not found | `internal/share/share.go:1-5`; `serve.go:11-30,131` | xdev-only |
| Webhook ingress → new Session | `packages/webhook/webhook/src/types.ts:37-51`; GitHub adapter `packages/webhook/webhook-github/src/handler.ts` | not found | dsh-only |
| Workspace-change capture / deliverable presentation | `packages/deliverables/workspace-changes/src/capture.ts:38-99`; `tool-present/src/index.ts` | `internal/tool/diff.go`; `checkpoint.go` | both-different |
| Message feedback (per-message, persisted) | `packages/feedback/message-feedback/src/index.ts:75-212` | not found | dsh-only |
| SSH execution / SSH filesystem | `packages/ssh/ssh/src/index.ts:17`; `fs-ssh`; `subprocess-ssh`; `sandbox-ssh` | not found | dsh-only |
| Persistent shell sessions (bash/pwsh) | `packages/shell/tool-bash-persistent`; `terminal/terminal-bash/src/session.ts` | `internal/tool/bashbg.go` | both-different |
| Plugin marketplace / installed-plugin roots | loader only: `packages/extensions/cordis-client-runner`; `host/plugin-inventory` | `internal/marketplace/discovery.go:8-34` (lowest-precedence plugin roots) | both-different |
| Managed `DSH_*` env registry for shell tools | `packages/shell/shell-env/src/index.ts:26-60` | not found | dsh-only |

### 7.2 What dsh has that xdev lacks

- **Any OS-level confinement of side-effecting tools.** Seatbelt (`packages/sandbox/sandbox-local/src/profiles.ts:51-57`), Landlock (`:30-35`), bwrap (`:16-23`), Windows ACL + restricted token (`packages/sandbox/sandbox-windows-acl/src/token.ts`, `grant.ts`, `path-boundary.ts`). xdev's only boundary is the approval policy (`internal/tool/policy.go:9-45`); the nearest xdev concept is `XDEV_AGENT_DIR` path relocation. Lands as a new `internal/sandbox` with per-platform profile builders.
- **A shared canonical writable-root derivation** — `packages/sandbox/sandbox/src/roots.ts:52-55`. That module exists specifically so the write-tool fence and the shell fence cannot drift; without it, any future xdev fence will drift.
- **`SANDBOX_UNAVAILABLE` structured failure and escalation prompts** — `packages/sandbox/sandbox/src/index.ts:120-135`; `packages/interaction/user-approval/src/index.ts:73`. xdev's `ApprovalMode` has no escalation concept (`internal/tool/approval.go:34-43`).
- **A generic tool-call deadline wrapper** — `packages/guard/timeout-policy/src/index.ts:25-60`. xdev's per-MCP timeouts are hand-rolled (`internal/mcpclient/mcp.go:119-129`); a wrapper belongs in `internal/tool/` around the registry's execute.
- ~~Repeat-tool-call reminder — landed in #449~~ (see §0.4).
- **MCP reconnect-with-backoff and `list_changed` re-sync** — `packages/mcp/mcp-client/src/connection.ts:233-244,263-269`. xdev has a health probe (`internal/mcpclient/health.go`) but no reconnect loop; lands in `internal/mcpclient/mcp.go`.
- **MCP resources/prompts exposed as tools** — `packages/mcp/mcp-resources/src/tools.ts`; xdev registers only remote tools (`internal/mcpclient/mcp.go:246`).
- **SSRF-hardened HTTP fetch** — `packages/web/web-fetch-http/src/network.ts:140-219`: public-IP validation, RFC 6052 NAT64 unwrapping, per-request DNS pinning. xdev's `internal/websearch/websearch.go:30` is a bare `net/http` import. Lands in `internal/websearch/`.
- **Webhook ingress creating a root Session** — `packages/webhook/webhook/src/types.ts:37-51`; `webhook-github/src/handler.ts`. xdev has `internal/serve/` (loopback services) but no signed inbound-event path; lands in a new `internal/webhook/` reusing `internal/serve/broker.go`.
- **Per-message feedback events** — `packages/feedback/message-feedback/src/index.ts:75-212` (`feedback/message-put` / `-delete`, version-checked). xdev has no feedback persistence; lands in `internal/session/entries.go` + `store.go`.
- **SSH execution and SSH-backed filesystem** — `packages/ssh/ssh/src/index.ts:17`; `fs-ssh`; `subprocess-ssh`; `sandbox-ssh`. xdev: none.
- **Credential-store mode audit with a `chmod 600` remediation message** — `packages/credentials/credentials-local/src/index.ts:116-143`. xdev has private-file helpers (`internal/config/privatefile_unix.go`) but no equivalent startup audit.
- **Durable per-hook invocation records** — `packages/hooks/hook-protocol/src/events.ts:75-104`. Lands in `internal/session/entries.go` + the hooks emit path (`internal/hooks/hooks.go:65-66`).
- **A managed `DSH_*` env registry for shell tools** — `packages/shell/shell-env/src/index.ts:26-60`; xdev has no env scrub/allowlist (grep over `internal/tool/` found none).

### 7.3 What xdev has that dsh lacks

- **Workspace trust with a per-hook content digest** — `internal/hooks/trust.go:1-19,40-50`. No equivalent gate found in `packages/hooks`.
- **Hook rewrite semantics dsh explicitly does not honor**: xdev replaces tool arguments from `{"input":…}` (`internal/hooks/interceptor.go:28-34`) and replaces the system prompt from the `context` event (`internal/hooks/hooks.go:427-444`). dsh's Claude bridge logs and discards `updatedInput` (`hooks-claude-code/src/index.ts:181-182`) and `systemMessage` (`:184-185`).
- **Hook `if:` prefilters sharing the approval policy's permission-syntax matchers** — `internal/hooks/hooks.go:64-66,255-270`. Worth porting *to* dsh: one matcher implementation for hooks and approval prompts.
- **Hook discovery + multi-source merge with name-keyed overrides and a `--hook` CLI** — `internal/hooks/hooks.go:91-119,182-225`.
- **Keychain as a read-only credential source** — `internal/config/keychain.go:1-31`; dsh defers an OS-keychain provider entirely (`credentials-local/README.md:200,212`).
- **Reversible secret redaction with hashed placeholder keys** — `internal/config/secrets.go:282-447`. dsh's scrub is at the shell-executor boundary (`packages/hooks/hook-protocol/src/runner.ts:2-5`), not a general log/payload redactor.
- **MCP deferred-tool catalog** — `internal/mcpclient/mcp.go:395-477`.
- **MCP local-server health endpoint and auto-start** — `internal/mcpclient/health.go`; `health_test.go:53-90`.
- **LSP breadth and diagnostics** — gopls/rust-analyzer/typescript/pyright with cached `publishDiagnostics` (`internal/lsp/server.go:29-51`; `internal/lsp/client.go:213-214`; `internal/lsp/tool.go:389-436`); dsh's `lsp` package is explicitly read-only and diagnostics-free (`packages/lsp/lsp/README.md:32`).
- **Native browser control over CDP** — `internal/browser/cdp.go:3-10`; dsh only wraps Playwright/chrome-devtools MCP (`packages/experimental/browser-use-playwright-mcp/src/index.ts:9-43`).
- **Computer use, image gen, TTS, memory, session share** — `internal/computer/backend.go:29-35`; `internal/tool/imagegen.go`; `internal/tts/`; `internal/memory/`; `internal/share/share.go:1-5`. No dsh counterpart among the ~200 enumerated `package.json` paths.
- **Compound-command approval semantics** — `internal/tool/policy.go:12-29,87-90`.

### 7.4 Notable divergences

**The security boundary sits in a different place in each repo.** dsh's boundary is a mode string (`read-only` / `workspace-write` / `danger-full-access`) resolved into four independent OS-enforcement dialects, with the writable-root meaning defined exactly once (`packages/sandbox/sandbox/src/roots.ts:52-55`) and parity pinned by test. xdev's boundary is a three-value `ApprovalMode` plus ordered rules (`internal/tool/policy.go:9-45`) — a *policy* boundary, not a *containment* boundary. Concretely: on xdev, approval being off (`Yolo`) removes the gate entirely; on dsh, it removes the prompt but the Seatbelt/Landlock profile still applies. That is the single most important structural difference in this document, and it interacts with §5.4: xdev can claim a strong boundary only because it isolates extensions by process, while dsh can only ship in-process plugins because it confines them by OS profile.

**Hook dialects versus hook surfaces.** dsh treats Claude Code and Codex as two wire protocols over one shared codec, paying a tax to keep them aligned. The codec itself is the interesting part: the `hookEventName` guard (`codec.ts:117-124`) stops a hook smuggling `permissionDecision` from one event into another — a security property xdev has no analogue for. xdev instead has a richer event vocabulary and no dialects.

**Both have redaction, at different layers.** dsh scrubs at the shell-executor boundary so every child process inherits a filtered environment (`packages/hooks/hook-protocol/src/runner.ts:2-5`). xdev redacts in-band, swapping secrets for keyed reversible placeholders and restoring them on the way in (`internal/config/secrets.go:397-447`). dsh protects the process table and disk; xdev protects the transcript, log, and tool payloads.

**LSP is the mirror image of every other dimension.** Everywhere else xdev is broader and dsh is architecturally deeper. Worth reading `packages/lsp/lsp/README.md:129` as a design input even where the capability is unwanted.

**Delivery surfaces barely overlap.** dsh has inbound (webhook → new Session) and diff presentation; xdev has outbound (E2E-sealed HTML over loopback), marketplace, and self-update. No shared concept to reconcile — each side is a separate port decision.

### 7.5 Confidence + gaps

- **High confidence** on the hooks and sandbox rows: `codec.ts`, `runner.ts`, `events.ts`, `profiles.ts`, `roots.ts` read in full; xdev `internal/hooks/hooks.go` through line 359 of 444 and `interceptor.go` in full.
- **Medium confidence**: `hooks-claude-code/src/index.ts` citations come from grep line hits, not a full read of the merge logic. `mcp-client/src/connection.ts` was read only at lines 230-309; the tools/apply/error-surfacing path in `src/tools.ts` was not read, so "dsh lacks error-surfacing parity" is **unverified**.
- **Explicitly not found on the dsh side**: any hook trust/approval mechanism, any secret-redaction module, any diagnostics support in `lsp/`, any SSE transport, any sampling or roots client, and no memory/image-gen/TTS/computer-use package among the ~200 `package.json` paths enumerated. Absence from that enumeration is weaker evidence than a grep hit — several bulk globs returned nothing.
- **Not read at all**: dsh `docs/subsystems/*.md` for this dimension's packages; `packages/ptc-runtime/ptc-runtime-node` (only `types.ts`); `packages/subprocess/*`; `packages/terminal/terminal` (only `terminal-bash`); `packages/fs/fs-sandbox` and `fs-observation-policy` — named in the `roots.ts` comment as an enforcement layer. If `fs-sandbox` fences writes in-process, the "xdev has no write fence" claim needs qualifying.

---

## 8. UI + TRANSPORT + DELIVERY SURFACE

How the agent is actually driven and observed.

### 8.1 Surfaces

| | dsh | xdev |
| --- | --- | --- |
| Terminal UI | **none** — no `ink`/`@opentui/*`/`react-reconciler` dependency in any `package.json`; no package named `*tui*`/`*ink*`; `packages/terminal/*` is PTY session management only | tcell/v2, hand-rolled cell painting; `internal/tui/app.go` ~4100 lines; `go.mod:6` |
| Web | real React 18 + Vite SPA: `apps/web/package.json:56-60`; ~80 UI packages under `packages/client/*` (`ui-primitives`, `ui-chat`, `ui-trajectory`, `ui-tool`, `ui-sidebar-terminal`, …) | none |
| Desktop | real Electron, not a shell: `apps/desktop/package.json:3` "Electron desktop shell for a bundled dsh runtime"; `electron ^44`, `electron-builder`, `electron-updater`, `@electron/notarize` (`:40-60`); full arm64+x64 packaging + signing + notarization (`:24-38`); companion host `apps/desktop-host/package.json:3` | none (no Electron/React/browser assets anywhere) |
| Headless | `packages/bundle/headless/src/startup.ts:38-53` | `cmd/xdev/print.go:245` |
| Loopback dashboard | gateway/web | `internal/stats/dashboard.go` on :3847 over `internal/stats/stats.go:3,39` |
| Share link | not found | `internal/share/share.go:1-5` (self-contained HTML, E2E-sealed loopback) |

**xdev `internal/tui/` (84 files) by ownership** — core frame/render: `render.go`, `app.go`, `blocks.go`, `rowindex.go`, `scroll.go`, `tree.go`; composer/input: `editor.go`, `paste.go`, `clipboard.go`, `clipboard_image.go`, `askoverlay.go`, `picker.go`, `suggest.go`, `keymap.go`, `selection.go`; content formatting: `markdown.go`, `links.go`, `diff.go`, `junk.go`; feature views: `settings.go`, `trajectory.go`, `dock.go`, `hubroster.go`, `welcome.go`, `vibe.go`, `memory_ops*`, `goal*`, `schedule.go`, `mcps*`, `msgmenu.go`, `commands.go`, `discovery.go`, `thinkbox*`, `stall.go`. Roughly half the directory is `*_test.go` — mouse, double-click-dock, wheel, hang, key-repeat are all first-class tested.

### 8.2 Transports and protocols

| Surface | dsh (path:line) | xdev (path:line) |
| --- | --- | --- |
| ACP (agent side) | `packages/acp/src/{index,codec,session,updates,model-control,mcp,content}.ts` + 11 spec files; runs as an ACP agent for editors via `packages/bundle/acp-app/` and `@agentclientprotocol/sdk 1.4.0` (`apps/cli/package.json:28,107`) | `internal/acp/{acp,server,framing}.go` — JSON-RPC 2.0 over stdio, `initialize` handshake reporting `AgentVersion` (`server.go:15`), one running turn per session (`server.go:17-28`), streaming `session/update` plus a blocking `Permission` request for approval (`server.go:31-38`), `session/cancel` off the read loop |
| Embedder RPC | `packages/sdk/{protocol,client,server}` — NDJSON JSON-RPC; client spawns `dsh` with named profiles + ordered cordis patches, server serves over stdio (`packages/sdk/README.md:12,29-31`); Python sibling at `python/sdk` | `internal/rpc/server.go:1-4` "the M6 embedder contract: a JSONL-over-stdio server"; verbs `Prompt`, `Steer`, `FollowUp`, `Abort`, `NewSession`, `State`, `SetModel` (`:21-29`); emits `ready` with `ProtocolVersion`+`FrameLimit` first (`:53`); bounded goroutine pool, mutex-serialized |
| Frame vocabulary | — | `internal/protocol/` (`Ready`, `State`, `FrameLimit`, `ProtocolVersion`), shared by rpc and tui; five front-ends at `cmd/xdev/main.go:429` |
| HTTP + WebSocket gateway | `packages/api/gateway/README.md:12,31,35` — two-sided Typert RPC over `/api` with `/api/remote.mux` WS multiplexing, cancellation, reconnection, forwarded host events; `./stream-protocol` for native Desktop callers (`:103`); plus `packages/api/{session,workspace,terminal,settings,job,account}-controller` | none for session control; `internal/serve/` is credential/relay only (`broker.go:70`, `gateway.go`, `relay.go`, `ws.go`) |
| SSH | `packages/ssh/{ssh,fs-ssh,subprocess-ssh,sandbox-ssh}`; the web GUI explicitly runs over SSH (`packages/bundle/web-app/README.md:14,62`) | none |
| Local HTTP services | not found as a launcher mode | `xdev serve {auth-broker,auth-gateway,browser-relay}` — three services in one trust domain, loopback-only with a per-install bearer token (`internal/serve/serve.go:2,17-27,60-63`); credential vault (AES-256-GCM) + usage reporting; forward proxy attaching broker-held credentials; CDP relay |
| Collab / multi-session | gateway `remotes` + `packages/client/connection` | `internal/collab/` — `ws.go`, `frame.go`, `chunk.go`, `host.go`, `guest.go`, `link.go`, `render.go`, `cmd.go`; guest-room pairing over WS with chunked frames; surfaced in the TUI as the hub roster (`internal/tui/hubroster.go`) |
| MCP | client (`dsh-mcp-client`, `dsh-mcp-resources` in `apps/cli/package.json:52,99`), not primarily a server | client only (`internal/mcpclient/`) |
| Bundles / profiles | `packages/bundle/{base,sdk-minimal,sdk-app,headless,acp-app,web-app}` — each a cordis patch layer defining a runnable profile | `-mode print|tui|rpc|acp` + subcommands (`cmd/xdev/main.go:176`) |

### 8.3 Observability

| | dsh | xdev |
| --- | --- | --- |
| Telemetry | **ships and emits.** OTLP/HTTP JSON log exporter with per-pipeline header/TLS isolation, keep-alive agent factory, per-pipeline URL: `packages/telemetry/otel/src/transport.ts:1-12`; siblings `session-log.ts`/`event-log.ts`; wired per-session via `packages/session/session-te…` | **none by policy.** grep over `go.mod` for `opentelemetry\|otel\|sentry\|prometheus` → 0 matches. Deliberate; the dep set is tiny (`go.mod:5-12`) |
| Product analytics | `packages/client/product-analytics` (desktop-only; web usage excluded by policy, `packages/bundle/web-app/README.md:10`) | none |
| Token metering | `dsh-token-meter` (`apps/cli/package.json:70`) — in-product meter | the compaction estimator only (§2.1) |
| Local stats | — | `internal/stats/stats.go:6-9` "No SQLite sidecar (PRD non-goals: no `stats.db`-scale accumulation)"; scan caps `DefaultMaxFiles = 2000` / `DefaultMaxBytes = 512 MiB` (`:36-37`); rollup cache; lean wire struct so peak memory is one line (`:10-13`) |
| Logger | — | `internal/logx/logx.go:1-2` minimal leveled stderr writer, default `LevelWarn` with `enabled = false` (`:25-30`), `Disable()` in print mode (`:40-45`); no sink, no export |
| Test/bench strategy | snapshots as the real strategy: `snapshots/web/*/*.expected.md` (turn-tail, steering, queue-actions, navigation-panes, feedback-release), `snapshots/session/**`; repo-level benchmark deps package (`pnpm-workspace.yaml:10`) | `internal/eval/` runs a kernel in a killable subprocess (`kernel.go`, `tool.go`, `jobs.go`, `kill_unix.go`/`kill_windows.go`); `scripts/{smoke.sh,bench,tui-shot.py}`; one CI workflow (`release.yml`) |

### 8.4 Distribution

| | dsh | xdev |
| --- | --- | --- |
| Artifacts | ~100 separately publishable npm packages + `apps/cli` public (`bin: dsh`, MIT, `files: lib/*.js`) + Electron installers (private) | one static binary (31 MB) per platform, CGO-free, cross-built for linux/macOS/windows on amd64+arm64 |
| Self-update | `electron-updater` for desktop | `internal/dist/{install,update,check,version,release,job,setup,bench,cmd,proc_unix,proc_windows}.go` — `xdev update [--channel stable\|canary] [--check] [--timeout D]` (`internal/dist/cmd.go:11`; channel semantics `internal/dist/release.go:19`), plus a twice-daily `xdev update job {install\|remove\|status}` background job (`cmd/xdev/main.go:133-134`, dispatched `main.go:559-561` via `runDist`) |
| Marketplace | `packages/extensions/cordis-client-runner`; `host/plugin-inventory` | `internal/marketplace/{registry,discovery,manifest,install,cmd}.go` — manifest-driven discovery + install, lowest-precedence plugin roots, with `roots_consumed_test.go` guarding which config roots it may write |
| Optional bundles | `packages/experimental/*` — `schedule-bundle`, `voice-input-bundle`, `speech-to-text(+sensevoice)`, `tool-agent-team`, `auto-review`, `webworker-runtime`/`webworker-packer`, `ptc-runtime-python`, `computer-use-cua-driver-native`, `inspector` | feature flags / subcommands inside the one binary |

### 8.5 Notable divergences

**The UI inversion is the largest single gap and it is not cheaply closable.** dsh is a product with three front-ends and no terminal; xdev is a terminal tool with a 4100-line hand-rolled TUI and no product surface. A dsh-class web client is a new product, not a feature; an xdev-class TUI for dsh would be a new package family. Neither is a port — both are builds. The realistic read: xdev's UI is not going to converge with dsh's, and the comparison's value on this dimension is bounded to *what the surfaces expose* (dsh's trajectory / plan-summary / todo renderers are worth reading as design inputs — §6.3), not to the surfaces themselves.

**Where the two overlap, they overlap well.** ACP agent-side, an embedder JSONL contract, and a loopback relay are present on both sides with the same shapes. xdev's `internal/rpc` verbs (`Prompt`, `Steer`, `FollowUp`, `Abort`, `NewSession`, `State`, `SetModel`) line up with dsh's SDK client/server pair closely enough that an embedder written for one has a path to the other.

**Observability is a values difference, not a capability gap.** dsh's OTel pipeline is a real exporter with tested egress (`packages/telemetry/otel/src/transport.ts:1-12`); xdev ships none and has said so deliberately (0 telemetry deps, bounded local stats, off-by-default logger). That is a decision to re-affirm, not a bug — the numbers to keep in view are `internal/stats/stats.go:6-9` (no accumulating `stats.db`) and `internal/logx/logx.go:25-30` (warn-level, off by default).

### 8.6 Confidence + gaps

High confidence on the dsh side: `package.json` manifests and `docs/` READMEs for the bundle, sdk, api, ssh, telemetry, and experimental families; the negative TUI finding is a grep across every `package.json` for three framework names plus a package-name glob, not a sample. High confidence on the xdev side: `go.mod` and `internal/{tui,acp,rpc,protocol,serve,collab,stats,eval,logx,dist,marketplace}/` read directly or symbol-grepped.

- Not read: dsh `packages/client/` beyond directory listings and entry points (155k lines — out of budget by design); `apps/desktop-host` internals; the `packages/api/*-controller` implementations. The controller row rests on the gateway README + package list.
- The TUI-absent finding is about *dependencies and packages*. If dsh has a terminal surface built on raw ANSI without a framework, the grep would not have found it; I did not read `apps/cli` deeply enough to exclude a hand-rolled terminal renderer. The claim is "no framework TUI and no TUI package", stated as such.

---

## 9. SYNTHESIS — what to port, what to keep, what to reject

### 9.1 The shape of the difference

**dsh is deeper in durability and forensics; xdev is broader in surface and safer on a runaway turn.** Every dimension below reduces to that sentence once you strip the feature lists.

| Dimension | dsh-only | xdev-only | both, different |
| --- | --- | --- | --- |
| 1 Session core | 12 | 6 | 16 |
| 2 Context/window | 8 | 7 | 9 |
| 3 Subagents | 9 | 8 | 9 |
| 4 Tool calls | 13 | 7 | 12 |
| 5 Lifecycle | 10 | 9 | 8 |
| 6 Thinking flow | 11 | 8 | 9 |
| 7 Action flows | 13 | 11 | 12 |
| 8 UI/transport/shape | 6 | 10 | 7 |

Counts are as first written; §0.4 moves four rows out of the `dsh-only` column, so the true dsh-only count is 8 lower and the `both-*` columns correspondingly higher.

dsh's depth comes from one decision — every fact is a typed, `seq`-numbered event in an append-only log, so "what happened" is a question the file answers. xdev's breadth comes from a different decision — one binary, process-isolated extensions, no plugin VM, so a new capability is a new Go package rather than a coordinated monorepo release.

### 9.2 The five highest-leverage ports, in dependency order

1. **Turn/step lifecycle + `TurnEndReason`** (§1.1, §1.2). xdev has no turn boundary at all. Nothing else in this list is cheap until this lands: crash repair, max-tokens-as-a-turn-reason, and a real status read all key off it. Today `internal/session/status.go:57-97` sniffs a 32 KiB tail to guess "interrupted". Evidence: `packages/core/session/src/types.ts:201-232,288-301`.
2. **Crash repair with synthetic closers** (§1.2, §5.1). **Partly landed in #449** — `session.UnansweredToolCallNotice` (`internal/session/context.go:46-53`) tells the model the outcome is unknown, but the dangling call still has no result block. The remainder needs (1): with a turn boundary you can append `step/end` + `turn/end{interrupted}` and a real `tool/result` carrying `TOOL_NOT_STARTED` / `TOOL_OUTCOME_UNKNOWN` on open. Evidence: `packages/core/session/src/repair.ts:15-18,53-98`.
3. **Durable `request/header`** (§1.2). Log the config + tool schemas with a `RequestHeaderReason`. Without it xdev cannot answer "which tools and which config produced this transcript" from the file — the exact gap `#220`'s census is about. Evidence: `types.ts:240-273,390-402`; `packages/core/session/src/request-header.ts:63-68`.
4. **OS-level sandbox with one canonical roots derivation** (§7.2). This is the one place where xdev is genuinely *behind* on safety rather than merely different. The `roots.ts` module exists specifically to stop the write-fence and the shell fence from drifting; porting the dialects without it is how you get a hole. Evidence: `packages/sandbox/sandbox/src/roots.ts:52-55`; `sandbox-local/src/profiles.ts:16-57`.
5. **A real system-prompt registry** (§2.2). 30 named section orders, plugin-claimable, with a `complete` flag. In xdev every new capability (memory backend, LSP section, plan-mode note) edits one function (`internal/agent/prompt.go:225`). Evidence: `packages/core/system-prompt/src/index.ts:31,125`.

Two more, if the budget allows: **cron/daily/weekly schedules** (§5.2 — xdev has only `after`/`at`/`every`, `internal/agent/schedule.go:17-24`) and **a model-readable job registry** (§4.3 — dsh's `job_list`/`job_output`/`job_kill`; xdev has background bash with no poll surface).

### 9.3 xdev advantages worth defending in the PRD

- **The deferred tool catalog** (`internal/tool/catalog.go:26-28`; installed `cmd/xdev/print.go:1962-1965`). dsh has no equivalent model-facing tool; its only deferral is a provider wire flag (`defer_loading`, `packages/llm/llm-deepseek/src/serialize.ts:164`) which is unavailable on non-DeepSeek routers. This is xdev's single largest structural advantage for a wide tool set.
- **Turn bounding** (§5.1). Turn count (default 200, `internal/agent/loop.go:125-127`), per-turn token budget that *injects a wrap-up and continues* (`:129-145,553-556`), wall clock (`cmd/xdev/launchflags.go:297-319`), and a retry-forever ladder. dsh's agent-loop constants file has exactly one knob — `DEFAULT_MAX_PARALLEL_TOOL_CALLS = 10` (`packages/core/agent-loop/src/constants.ts:6`) — over a bare `while (await this.turn()) {}` (`agent.ts:254`).
- **Evidence-gated goal completion** (`internal/agent/goal.go:300-321` refuses an evidence-free completion; dsh bounds rounds instead, `packages/goal/goal-round-driver/src/prompt.ts:17`).
- **Emitted prompt-cache markers** (`internal/ai/cache.go:48,52,112`; dsh declares the capability but withholds explicit cache mode, `packages/llm/llm-pi-ai/src/catalog.ts:270`).
- **The tool tree + rewind** (`internal/session/store.go:950-970`; `internal/tool/checkpoint.go:186-264`). dsh cannot branch a live log in place at all.
- **Four bounds landed in #449** — unanswered-call notice, per-call deadline, batch-size ceiling, repeat-call guard — none of which dsh has *all* of in one place either.
- **A hard read-only plan-mode guard** (`internal/agent/planmode.go:499`; dsh's plan mode is explicitly soft guidance, `packages/plan/plan-mode/src/index.ts:5-7`).
- **Telemetry-free by policy**, with bounded local stats (`internal/stats/stats.go:6-9`) and an off-by-default logger (`internal/logx/logx.go:25-30`). A decision, not a gap.

### 9.4 Reject, with reasons

- **In-process plugin VM** — already rejected in PRD §1 goal 5; this study re-confirms it. dsh can afford it only because `packages/sandbox/` provides OS containment. xdev's process isolation (`internal/ext/ext.go:35-42,484-495`) plus a future sandbox is the cheaper, safer combination.
- **dsh's UI surfaces** (§8.5) — a web SPA + Electron desktop is a new product, not a port. Not on any roadmap.
- **A database as the transcript** — dsh's `session-query-sqlite` index (§1.1) is the right answer to *search*, and the wrong answer to *record*. xdev's JSONL stays the record; if search is needed, a derived index beside the file is the port, not a replacement.
- **A dependency for every small thing** — dsh's `packages/util/` tokenizer grep found no real tokenizer either (§2.5); both sides estimate with `charsPerToken = 4`. There is nothing to port.
- **dsh's `defer_loading`** — provider-gated, unavailable on third-party routers. xdev's harness-level bridge already wins this (§9.3).

### 9.5 The one-paragraph version

Both harnesses are the same category and the same generation; they disagree about where a fact lives. dsh puts every fact in one append-only typed log and lets plugins share a process, which buys forensics and costs a security boundary it has to rebuild with OS profiles. xdev puts facts in a tree of entries with a mutable leaf, isolates plugins into processes, and pays for it with a thinner forensic story and no containment. The ports that pay for themselves are the ones that move dsh's *log discipline* into xdev's *tree* — turn boundaries, crash repair, a durable request envelope — and the one that pays for itself in safety is the sandbox. The ports that do not pay are the ones that turn xdev into a different product: dsh's web and desktop surfaces.

---

## 10. FOLLOW-UP WORK, IN ORDER

1. **Correct `docs/research/dsh-internals.md` §1 item 4** — it says xdev is "PARTIAL" on turn-end typing; xdev has none (§0.2 item 1). That is a factual error in a shipped research doc.
2. **Correct the `ignorable`-flag framing** in the same doc — dsh is at `SESSION_FORMAT_VERSION = 4` with a migration ladder; xdev is flat v3 (§0.2 items 2, 5).
3. **Add a "no TUI" line to `docs/research/harness-comparison.md`** — the DeepSeek Harness row currently implies a comparable surface; dsh ships React + Electron and no terminal at all (§8.1).
4. **Open issues for the five ports in §9.2**, in that dependency order, each citing the dsh `path:line` and the xdev file it lands in.
5. **Decide on the sandbox** — this is the only genuine safety gap, and it is a real decision (per-platform profiles, maintenance cost, whether `internal/ext` process isolation is considered sufficient). It should be an explicit PRD item, not an implicit one.
6. **Re-verify before acting** — dsh moved 2,612 commits in 13 days. Every `path:line` here is pinned to `deepseek-harness` at `21638c5631` (2026-09-27). Re-check any citation you intend to implement from.

---

*Last updated: 2026-09-28 (docs: DeepSeek Harness deep-dive). Read-only static comparison of `deepseek-ai/deepseek-harness` at `21638c5631` against xdev at `e1a6f5e`, across eight dimensions with `path:line` evidence on both sides. Supersedes the dsh-specific claims in [dsh-internals.md](dsh-internals.md) (2026-09-14), whose corrections are listed in §0.2. Nothing in this document was executed: no harness ran, no test suite ran, no file outside this one was created or modified.*
