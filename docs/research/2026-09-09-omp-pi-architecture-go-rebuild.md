# From pi and Oh My Pi (omp) to a Lightweight Go Coding Agent

**Deep architecture research + rebuild blueprint (session core in Go, <100 MB RAM)**

*Date: 2026-09-09 · Status: research complete, blueprint ready for planning*

---

## Table of contents

1. [Executive summary](#1-executive-summary)
2. [Part I — How pi is built (upstream)](#part-i--how-pi-is-built-upstream)
3. [Part II — How omp (Oh My Pi) is built](#part-ii--how-omp-oh-my-pi-is-built)
4. [Part III — Where the weight is: why a JS harness is heavy](#part-iii--where-the-weight-is)
5. [Part IV — The Go rebuild blueprint (working name: **adze**)](#part-iv--the-go-rebuild-blueprint)
6. [Part V — Go ecosystem mapping](#part-v--go-ecosystem-mapping)
7. [Part VI — Name proposal](#part-vi--name-proposal)
8. [Part VII — Milestones](#part-vii--milestones)
9. [Sources](#sources)

---

## 1. Executive summary

**pi** (github.com/earendil-works/pi, formerly `badlogic/pi-mono`) is a minimal, opinionated coding-agent harness in TypeScript by Mario Zechner. Its core bet: a <1,000-token system prompt, exactly four tools (`read`, `write`, `edit`, `bash`), a hand-rolled unified LLM layer speaking only four wire protocols, and an inspectable, well-documented session format. No permission system ("YOLO by default" — sandbox externally), no built-in todos, no MCP in core.

**omp / Oh My Pi** is a disciplined fork of pi-mono that runs on **Bun**, adds a large **Rust N-API natives layer** (grep/glob/AST/pty/shell/diff/tokenizers + a shared filesystem-scan cache), an **in-process TypeScript extension system**, **MCP integration**, **subagent/hub orchestration**, **compaction** (6 trigger paths incl. provider-native streaming compaction), an elaborate **append-only JSONL session tree** (id/parentId + leaf pointer), and a JSONL-over-stdio **RPC mode** with host tools.

The cost of that power is runtime weight: a JIT-ed JS runtime baseline, in-process extension loading, unbounded event queues, whole-session-in-memory entries, plus SQLite sidecar databases (on this machine: `stats.db` = 77 MB, sessions = 1.4 GB on disk).

**The plan proposed here:** rebuild the *session/chat core* — provider streaming, agent loop, session persistence, tool execution, TUI — in **Go as a single static binary**, faithfully porting pi/omp's proven data model (JSONL session tree, unified stream contract, context reconstruction, compaction, output sinks) while replacing the expensive parts: no JS runtime, no in-process plugin VM, extensions via a **subprocess JSONL capability protocol** (the exact pattern omp's RPC + host-tool mode already proves works), MCP via the official Go SDK, pure-Go (CGO-free) SQLite.

**Realistic memory target: 30–80 MB RSS** in normal use (est.), with a hard budget that keeps the worst case under 100 MB — roughly 3–8× lighter than a JS-runtime harness, with no GC spikes from a scripting-language heap.

**Proposed name: `adze`** — a small, sharp woodworking adze: light, precise, fast, cuts exactly what you point it at. Shortlist and rationale in Part VI.

---

## Part I — How pi is built (upstream)

### I.1 Project layout

pi is a TypeScript monorepo ("Pi Agent Harness", MIT). Core packages:

| Package | Role |
|---|---|
| `pi-ai` | Unified multi-provider LLM API: OpenAI Completions, OpenAI Responses, Anthropic Messages, Google GenAI + compatible endpoints (xAI, Groq, Cerebras, OpenRouter, Ollama, Bedrock…). Streaming, tool calling with TypeBox schemas, thinking/reasoning blocks, **cross-provider context handoff**, token/cost tracking. Runs in Node and the browser. |
| `pi-agent-core` | The agent loop: tool execution, argument validation, event streaming, steering. |
| `pi-tui` | Minimal terminal UI framework: differential rendering, synchronized output, editor with autocomplete, markdown rendering. |
| `pi-coding-agent` | The CLI wiring everything together: session management (continue/resume/branch), AGENTS.md context hierarchy, slash commands + markdown custom commands, themes with live reload, editor with fuzzy file search, image support, HTML session export, headless JSON streaming + RPC mode, OAuth for Claude Pro/Max, full cost/token tracking. |

Plus peripheral packages (`chord` app-composition runtime, `pi-telemetry`). Upstream ships npm packages plus standalone binaries built from release source, with notable **supply-chain hardening** (pinned exact deps, `min-release-age=2`, shrinkwrap for the CLI, lifecycle-script allowlist) — a practice worth copying regardless of language.

### I.2 Design philosophy (from the author's own post-mortem)

Mario Zechner's write-up ("What I learned building an opinionated and minimal coding agent", 2025-11-30) is effectively the spec for a lightweight harness:

1. **Minimal system prompt** (~few hundred tokens; <1,000 with tool descriptions). Frontier models are RL-trained to understand coding agents; 10,000 tokens of instructions are dead weight. Only AGENTS.md files (global + project) get appended; the full prompt is user-replaceable.
2. **Minimal toolset**: `read`, `write`, `edit`, `bash` (plus opt-in read-only `grep`/`find`/`ls`). "These four tools are all you need for an effective coding agent." Prompt+tools < 1,000 tokens.
3. **YOLO by default**: no permission system, because the read-data + execute-code + network trifecta cannot be contained by prompting or pattern rules; containment belongs to external sandboxing (containers/micro-VMs). pi documents three container patterns (Gondolin micro-VM extension, plain Docker, OpenShell) instead of building security theater.
4. **No built-in todos**: models get confused by tool-tracked state; task lists belong in files (TODO.md).
5. **Context engineering as the product**: the harness must give the user full control and inspection of what enters the model context, with a cleanly documented session format that can be post-processed programmatically.
6. **Hand-rolled LLM layer instead of the Vercel AI SDK / provider SDKs**: there are only **four wire APIs** that matter (OpenAI Completions, OpenAI Responses, Anthropic Messages, Google GenAI). Owning them gives full control, small surface, and cross-provider context handoff (Anthropic thinking traces become `<thinking>`-tagged text blocks for OpenAI, etc.), at the price of a quirk table per provider (Cerebras/xAI/Mistral reject `store`; Mistral/Chutes use `max_tokens`; reasoning fields differ; Google historically doesn't stream tool calls; usage timing differs per provider making cost tracking imperfect on abort).
7. **Benchmark sanity**: a minimal harness + a good model beats heavyweight harnesses on terminal-bench-style runs (Terminus 2, the "just give the model tmux" agent, ranks high with the same idea).

### I.3 The pi TUI model

`pi-tui` keeps a full scrollback of previously rendered lines, re-renders on demand with caching, and uses **differential line comparison + synchronized output** for near-flicker-free updates. Simple architecture: the transcript is a list of rendered components; the terminal is a viewport over them. (omp later refined this into the frame-plan/retirement model — Part II.6.)

---

## Part II — How omp (Oh My Pi) is built

omp is a fork of pi-mono with a **strict recurring upstream-merge discipline** (`porting-from-pi-mono.md`): patch ranges from a recorded sync commit, scope renames (`@mariozechner/*` / `@earendil-works/*` → `@oh-my-pi/*`), Bun-first conventions, regression-trap checklists, semantic porting steps for reworked code, and an explicit **intentional-divergences table** (UI components, auth storage moved to `bun:sqlite` with multi-credential round-robin + session affinity, capability-based discovery, `createTools(session)` tool factories, bun:test).

### II.1 Runtime shape

- **Runtime: Bun.** The CLI is `dist/cli.js` under Bun (`~/.bun/bin/omp` → `@oh-my-pi/pi-coding-agent/dist/cli.js`, v18.1.14 on this machine). No Node entry points; Bun APIs preferred (`Bun.spawn`, `bun:sqlite`, text-import embeds for prompts).
- **Heavy lifting in Rust N-API natives** (`@oh-my-pi/pi-natives`): grep, glob/fd, workspace walking, AST match/edit, code summaries, syntax highlighting, text layout, token counting, structured diffs, shell/PTY/process/file-lock primitives, clipboard, PDF/Markdown, SVG rasterization, in-process Git/Jujutsu ops, SIXEL, vector ranking. Distributed as per-platform optional-dependency leaf packages with version sentinels and a versioned cache dir; x64 AVX2 "modern" vs "baseline" variants.
- **Shared FS-scan cache** (`crates/pi-walker`): process-local map of full directory walks, keyed by (canonical root, full traversal options), TTL 1 s, max 16 entries, empty-result revalidation after 200 ms, explicit invalidation after every write/delete/rename. This is what makes glob/fuzzy-find/AST tools feel instant in big repos — **directly portable to Go**.
- **Sidecar SQLite stores** (all `bun:sqlite`): `agent.db` (auth/credentials), `history.db` (prompt history with FTS5, batched ~100 ms drains), `models.db`, `stats.db` (77 MB here), plus `~/.omp/agent/sessions/` (1.4 GB here) and a content-addressed `blobs/` store.

### II.2 Session core (the part you want to port)

Source of truth: omp's `session.md` + `packages/coding-agent/src/session/*`. This is the "chat session core" to convert to Go — port it **faithfully**; it is mature, crash-tested design.

**On-disk layout**

```text
~/.omp/agent/sessions/<encoded-cwd>/<timestamp>_<sessionId>.jsonl
~/.omp/agent/blobs/<sha256>                    # content-addressed image/large payloads
~/.omp/agent/terminal-sessions/<terminal-id>   # breadcrumbs: cwd + session path [+ fresh]
```

`<encoded-cwd>` bucketizes by canonical cwd (home-relative `-<path>`, tmp `-tmp-<path>`, else `--<abs>--`), so symlink aliases share a bucket. Session discovery reads only a 4 KiB prefix (plus a bounded 32 KiB tail for lifecycle status), stat-keyed cached, parallel-scanned — cheap listing without parsing whole files.

**File format**: JSONL, one JSON object per line. Physically begins with a **fixed-width 256-byte `title` slot** (rename/list never rewrite the body), then a `type:"session"` header (id, timestamp, cwd, title/titleSource, additionalDirectories, previousSessionFiles, providerPromptCacheKey, parentSession), then entries.

**Entry model**: every entry has `{type, id (8-char), parentId, timestamp}`. History is an **append-only tree + mutable leaf pointer**:

- Append creates one entry whose `parentId` = current `leafId`; it becomes the leaf.
- `branch(entryId)` moves only the leaf pointer — nothing is mutated or deleted.
- `resetLeaf()` starts a new root (`/clear` writes a payload-free `reset_boundary` marker).

Entry types: `message` (full `AgentMessage` incl. usage/cost), `thinking_level_change`, `model_change` (per-role model map), `service_tier_change`, `compaction` (summary + `firstKeptEntryId` + `tokensBefore`), `branch_summary` (context for abandoned branches), `reset_boundary`, `custom` (opaque, extension-owned via namespaced `customType`; core reserves lifecycle values like `tool_execution_start`, `session_exit`, `user_todo_edit`), `custom_message` (extension-injected content that *does* enter LLM context, with attribution), `label`, `title_change`, `ttsr_injection`, `credential_pin`, `session_init`, `mode_change`.

**Context reconstruction** (`buildSessionContext`): walk `parentId` from leaf to root (cycle-bounded), reverse, derive runtime state from the path (thinking level, per-role models, service tiers, mode), pick the emission boundary (latest `reset_boundary` or latest compaction: emit summary + entries from `firstKeptEntryId`), convert `message`/`custom_message`/`branch_summary` to model messages, and **drop dangling tool calls / unsafe aborted-error turns** so a resumed session never presents an interrupted turn as live.

**Durability model** — deliberately pragmatic:

- Appends are synchronous in-memory + writer hand-off, **no fsync** (covers software crashes, not power loss).
- New ordinary sessions stay memory-only until the first assistant message or `ensureOnDisk()` (no junk files for aborted starts).
- Full rewrites (migrations, moves, repair) are atomic stage-then-rename with an EPERM-safe fallback and a commit guard; if durability of a rewrite cannot be proven, the manager **fails closed** (`SessionPersistenceIndeterminateError`) rather than silently losing data.
- Persistence errors are **latched** and rethrown on later flush/close — a broken disk never becomes silent loss.
- Migrations (v1→v2→v3; current version 3) mark the file for a full rewrite at the next persistence op.
- Crash forensics: `session_exit` entries (with pending-tool-call reconstruction) + `tool_execution_start` markers let a resumed process diagnose the interrupted turn; the loader synthesizes an `aborted` assistant message when needed.
- Size controls: >500k-char strings truncated (with byte-exact exceptions for signed provider blocks); images externalized to the blob store as `blob:sha256:<hash>`; files >8 MiB load via streaming JSONL.

**Prompt history** is deliberately a *separate* SQLite/FTS5 subsystem — replay fidelity lives in JSONL, search UX in SQLite. Don't mix them.

### II.3 Provider layer (unified streaming)

The contract to port, from omp's `provider-streaming-internals.md`:

1. Every provider adapter maps its native SSE stream onto one event shape, `AssistantMessageEvent`: `start` → block lifecycle triplets (`text_start/delta/end`, `thinking_*`, `toolcall_*`), `image_end`, terminal `done{reason: stop|length|toolUse}` or `error{reason: aborted|error}`.
2. `AssistantMessageEventStream`: async iteration + `result()`; push-order delivery, **no batching/merging, no backpressure** (unbounded queue — a documented wart; Part IV fixes this).
3. Tool-call arguments stream as partial JSON; adapters accumulate `partialJson` and re-parse **throttled** (skip until ≥256 new bytes; the final parse at `toolcall_end` is authoritative), with a relaxed/repairing JSON parser so truncated deltas never crash mid-stream.
4. Stop-reason mapping tables per provider; errors split into model-completion semantics vs transport failure; provider-owned bounded retries (empty completions, malformed envelopes, capability fallbacks such as retrying without rejected strict-tool fields); Codex websocket→SSE fallback only before replay-unsafe output is emitted.
5. Lazy provider loading (thin routing eager, heavy built-ins behind wrappers) + **first-progress and idle watchdogs** on streams; model-host `fetch.preconnect()` fired during session setup to overlap DNS/TCP/TLS with prompt assembly (saves 100–300 ms on transcontinental hops).
6. `agentLoop` consumes events → mutates in-flight assistant state → emits `message_update`; `AgentSession` layers persistence, extension hooks, retry, compaction, streaming-edit guards. Cancellation is layered (request/adapter/loop/session; tool execution abortable independently, `AbortSignal.any`).

### II.4 Agent session semantics

- **Prompt lifecycle**: `prompt()` when idle starts a turn; while streaming, `steer` (interrupt), `followUp` (queue), `aside` (inject at the next agent step boundary without killing the in-flight tool batch), `nextTurn`. Queued messages are preserved, never thrown away.
- **Retry/maintenance**: auto-retry on error stop-reasons with heuristics; auto-compaction on context overflow (context promotion to a bigger model first), on `stopReason:"length"`, on threshold after turns, mid-turn, and idle. Compaction walks a configurable `methodOrder`; `handoff` generates a handoff doc via a side request that reuses the warm prompt cache; provider-native streaming compaction (v2) with a retained-message budget (64k tokens); `snapcompact` archives history as dense bitmap images.
- **Tool pipeline** (bash as the deep example): input normalization (leading `cd X &&` extraction), policy resolution (per-tool tiers: read-only/write/exec; `bash.patterns` allow/deny/prompt with conservative compound-command combination), optional interception (regex rules route misuse to dedicated tools), cwd validation, artifact allocation, PTY vs non-PTY selection (PTY only with UI + opt-in), **shell session reuse** (cached native shell keyed by shell/env/session; concurrent calls never share), **non-interactive env hardening** (pagers off, `GIT_EDITOR=true`, `GIT_TERMINAL_PROMPT=0`, `SSH_ASKPASS=/usr/bin/false`, `NO_COLOR=1`, `CI=true`, package-manager automation flags), direnv/devenv preflight, and an **OutputSink**: UTF-8-safe 50 KB rolling tail + optional 20 KB head with middle-elision marker, per-line 768-byte column cap, raw stream mirrored to an artifact file on overflow, truncation metadata (`[raw output: artifact://<id>]` footers the model can re-read).
- **Approvals are separate from interception**: approval decides *whether to run*; interception routes *to a better tool*. Neither is containment (pi's YOLO philosophy); real containment is external sandboxing.
- **Subagents/RPC**: task subagents get restricted tool sets (no ambient MCP/extensions/LSP), structured output schemas, a `yield` tool, artifact handoff. Hub coordinates peers via message-passing.

### II.5 Extensions

- **Extensions are in-process TS modules** loaded via native `Bun import()`. The factory registers tools/commands/shortcuts/renderers/providers during load; runtime actions throw until `ExtensionRunner.initialize` wires them. Events cover session lifecycle (cancelable pre-events), prompt/turn lifecycle, **tool_call/tool_result middleware** (blocking, fail-closed; revised inputs are revalidated through the whole pipeline), MCP notifications (bounded FIFO buffer), user-bash override, provider registration with usage fetchers.
- **Isolation is the weak point**: extensions run with full process trust; a raw `setTimeout` throw outside the managed timers tears down the whole session. Managed `ctx.setInterval/setTimeout` exist precisely to contain this. **This is the strongest argument against porting in-process plugin loading to Go** — see Part IV.
- File write/delete fallbacks: on EPERM/EACCES/EROFS from the shared write primitive, registered handlers may broker the bytes (sandbox write-broker pattern), with symlink-resolution subtleties handled (resolved `dst`, separate delete seam, `ENOENT` never diverted).

### II.6 TUI internals

omp's TUI refines pi-tui into an explicit **frame-plan contract**: each frame = mandatory chrome (editor/status/HUD/overlay) + a `TerminalFramePlan { history?: {id, rows}, viewport: rows[] }`. Transcript blocks live in three states: **active** (mutable, viewport-resident), **settled** (finalized, still live, reflows on resize until capacity pressure), **committed** (written into terminal scrollback, caches released). History is offered as *retirement batches* only under capacity pressure and acknowledged only after acceptance — **the terminal's native scrollback becomes the archive** (no app-side scrollback memory). Resize borrows the alt-buffer, recovers the anchor via a DSR (CSI 6n) cursor round-trip, and replays per mode (`rebuild`/`append`/`preserve`). Tool blocks collapse by row budget (0→hidden, 1→label line, 2→folded card, ≥3→full renderer). IME-safe cursor tails, synchronized output, destructive `resetDisplay()` reserved for session swaps. **This architecture ports well to Go and is memory-efficient by design.**

### II.7 RPC mode (the embedder contract)

JSONL over stdio: a `ready` frame advertises protocol versions + frame limits (1 MiB; v2 chunked reassembly to 64 MiB); inbound commands (prompt/steer/follow_up/abort/new_session/state/set_model/…) correlate by id; outbound frames are responses, session/agent events, extension-UI requests (dialogs round-trip to the host), **host-tool calls** (the host owns tools the agent may invoke), host-URI requests, subagent frames, command side channels. Official clients: TS `RpcClient`, Python `omp-rpc`. Headless/print/ACP modes share the same session core with no UI context (stubs).

### II.8 Memory subsystem (optional; keep optional)

Five backends (off/local/hindsight/mnemopi/sharpshooter). The local pipeline: two-phase LLM extraction/consolidation over persisted sessions (lease + heartbeat, secret redaction, caps), injecting a bounded summary + `learned.md` lessons at session start, plus generated skill playbooks; piggybacks on the model-role system. For a v1 Go tool: implement the `memory://` read seam + lessons file; skip the pipeline.

---

## Part III — Where the weight is

Why omp cannot stay under 100 MB, component by component (measured facts marked; rest is estimate):

| Component | Cost | Notes |
|---|---|---|
| Bun runtime + JS heap | ~40–90 MB RSS baseline (est.); grows with session size | JIT, GC heap, module graph. The single biggest line item; unreducible by app code. |
| In-process TS extensions | +heap, full process trust | Discovery + import of user TS at startup. |
| Whole-session-in-memory entries | grows with transcript | Entries kept in memory as JS objects; >8 MiB files only switch to streaming *loading*, not storage. |
| Unbounded event queues | spikes | "No hard backpressure… queued events can grow until completion" (documented). |
| Rust natives addon | tens of MB on disk; moderate RSS | Loaded eagerly at root import. |
| Sidecar DBs | 77 MB `stats.db` here; 1.4 GB sessions on disk | Disk, not RSS, but shows accumulation cost; `stats.db` is held open per session. |
| MCP servers, LSP servers, browser | child processes | Outside the agent's RSS but part of the session's real footprint. |

**Conclusion**: <100 MB RSS in Go is comfortably achievable *if* you (a) have no JS runtime, (b) window the in-memory session (context reconstruction needs only the post-boundary tail), (c) bound every queue, (d) stream tool output through fixed-size sinks, (e) keep the terminal as the scrollback archive (omp's own TUI design already does this).

---

## Part IV — The Go rebuild blueprint

### IV.0 Principles

1. **Port the data model, not the code.** The JSONL session tree, entry taxonomy, context reconstruction, unified stream contract, OutputSink, and frame-plan TUI are the crown jewels — port their *semantics* exactly (same entry names where possible for interop with existing tooling).
2. **Single static binary, CGO-free.** Cross-compiles to all six platforms omp supports, ~10–15 MB on disk, instant startup, no runtime install.
3. **Same philosophy as pi**: minimal prompt, four core tools, sandbox externally, everything inspectable.
4. **Extensions are processes, not code.** Never load foreign code in-process — the one lesson omp's extension runtime proves negatively.
5. **Bounded everything**: queues, buffers, session windows, output sinks. Backpressure is a feature, not a bug.

### IV.1 Target memory budget (<100 MB RSS, hard)

| Component | Budget | Technique |
|---|---|---|
| Go runtime + binary + idle GC heap | 8–15 MB | Standard; `GOGC` tuning, `debug.SetMemoryLimit` as hard backstop |
| Provider HTTP/2 connections + TLS | 3–8 MB | Shared `http.Client`, connection reuse, streaming bodies |
| Session entries in memory (windowed) | 10–30 MB | Materialize only the post-boundary tail (compaction/reset boundary); older entries stay on disk, loaded on demand for list/tree/export |
| In-flight assistant stream state | 1–3 MB | Only the current message materialized |
| Tool output sinks | <1 MB | Fixed head+tail windows, artifact spill (port omp's OutputSink exactly) |
| TUI frame buffers | 2–6 MB | Viewport-only frames; terminal owns scrollback (frame-plan model) |
| JSON decode buffers | 2–5 MB | `json.Decoder` per JSONL line; never unmarshal whole files |
| MCP child servers | 0 (agent RSS) | Child processes; agent holds pipes + tool descriptors only |
| **Total worst case** | **~30–70 MB (est.)** | Enforced via memory limit + bounded structures |

### IV.2 Module layout

```text
adze/
  cmd/adze/            # CLI entry: tui | print | rpc modes
  internal/ai/         # provider adapters (anthropic, openai-completions, openai-responses, google)
    internal/ai/sse/   #   shared SSE frame reader, unified AssistantEvent, partial-JSON w/ 256B throttle
  internal/agent/      # agent loop: steering channels, tool scheduling, cancellation layers
  internal/session/    # JSONL tree store, entries, buildContext, compaction, blob store, listing
  internal/tool/       # read/write/edit/bash + registry, approvals, OutputSink, env hardening
  internal/tui/        # frame-plan renderer over tcell
  internal/ext/        # subprocess extension protocol (JSONL capability handshake)
  internal/mcp/        # MCP client via official Go SDK
  internal/store/      # pure-Go SQLite: history FTS, stats; blob store in stdlib
  internal/protocol/   # RPC v1/v2 wire types (compatible with omp's where possible)
```

### IV.3 Session core port spec (the heart)

Port `session.md` semantics to Go types:

```go
type EntryBase struct {
    Type      string    `json:"type"`
    ID        string    `json:"id"`       // 8-char random
    ParentID  string    `json:"parentId"` // "" = root
    Timestamp time.Time `json:"timestamp"`
}

type Entry struct {
    EntryBase
    Message       *Message       `json:"message,omitempty"`
    Compaction    *Compaction    `json:"compaction,omitempty"`
    BranchSummary *BranchSummary `json:"branch_summary,omitempty"`
    Custom        *Custom        `json:"custom,omitempty"`
    CustomMessage *CustomMessage `json:"custom_message,omitempty"`
    ModelChange   *ModelChange   `json:"model_change,omitempty"`
    // ... one pointer per entry type; single-union decode
}
```

- **Store**: `SessionStore` with `leafID` and an append-only entry slice; writer = `bufio.Writer` + explicit `Flush()` per append (matches omp's no-fsync guarantee); optional strict-fsync mode flag for paranoid durability.
- **Title slot**: keep the 256-byte fixed-width slot — it's what makes listing O(prefix).
- **buildContext(entries, leaf)**: identical algorithm (walk-to-root, boundary selection, dangling-tool-call neutralization, unsafe-turn dropping). Pure function, unit-testable against pi/omp-generated session files. **Acceptance test: adze must resume an omp session file and produce an equivalent LLM context.** That one test pins the entire port.
- **Blob store**: sha256 content-addressed files; externalize images/data-URLs at the same thresholds; truncate >500k-char strings with the same byte-exact exceptions.
- **Compaction**: implement thresholds + overflow/incomplete recovery + context promotion first; `handoff` and streaming-v2 later (they are LLM prompt engineering, portable as-is).
- **Interchange**: same naming/layout conventions (`~/.adze/agent/sessions/...`), so exporters and session-analysis pipelines work unchanged.
- **Listing**: 4 KiB prefix reads + stat-keyed cache + bounded parallel scan, as omp does.

### IV.4 Provider layer port

- Four wire adapters, hand-rolled over `net/http` (HTTP/2 for free) + one shared SSE reader: `data:` frames, `[DONE]` sentinel for OpenAI, typed events for Anthropic. Map onto the unified event contract (II.3) verbatim.
- Partial-JSON tool args: same 256-byte throttle + relaxed repair parser (~200 LOC port).
- Watchdogs: goroutine per stream — first-progress timer + idle timer, reset on every event; expiry aborts into `error{aborted}`.
- Preconnect: fire a warm-up dial on model resolve; same 100–300 ms win on distant hosts.
- Stop-reason maps + quirk tables: port pi's tables (they encode years of provider annoyance).
- Cancellation: `context.Context` chains (session → agent → request); tool exec gets a merged derived context (Go has no `AbortSignal.any`; use a small `errgroup`-based merge helper).
- **Backpressure fix**: bounded event queue (e.g. 1,024 events). On overflow, coalesce consecutive `*_delta` events of the same block — rendering already coalesces visually; persistence only needs final messages. This removes omp's documented unbounded-queue wart without changing semantics.

### IV.5 Agent loop

- One goroutine per turn; tools run on a **bounded worker pool** (omp runs same-batch tools concurrently — keep that, cap ~4–8).
- Steering via `chan Steering{steer|followUp|aside}` consumed at step boundaries; queued messages preserved.
- Persistence only on `message_end` (matching omp); streaming text is never persisted mid-flight.
- Extension interception points (`tool_call`/`tool_result`) called synchronously through the ext-protocol bus with a **per-call timeout + kill switch**. omp fails closed on `tool_call` policy blocks — keep fail-closed for policy, but a *hung* extension must be killable, which in-process TS cannot do and a subprocess can.
- Subagents: same session core with restricted tool sets + `outputSchema` (permissive/strict enforcement) + a `yield` tool; child = same binary in `print`/`rpc` mode, or in-process goroutine sessions with their own `SessionStore`. Start in-process (cheap goroutines, shared provider pool), move out-of-process later if isolation matters.

### IV.6 TUI

- Base: **tcell** (raw, fast, low-alloc) with omp's frame-plan model on top — history retirement batches, active/settled/committed blocks, DSR-anchor resize recovery, alt-buffer overlays. (bubbletea is friendlier but its Elm event model adds allocation churn; for a lightweight tool, tcell + frame-plan port is the honest choice. Charm's `x/ansi` for width/ANSI helpers.)
- Editor: port pi-tui's feature list — fuzzy path completion (over the FS-scan cache port), bracketed paste, multi-line, IME-safe cursor tail.
- Rendering discipline from omp: every row exactly `width` visible cells; ANSI is zero-width; one trailing separator row per block; retirement batches acknowledged only after the write is accepted.

### IV.7 Tools

- `read`/`write`/`edit`/`bash` exactly per pi's input schemas (model familiarity is an asset). omp-style line-numbered snapshots + hashline anchors (`PUT N.=M:`, `MV`, `CUT`, `REM`) are a proven edit UX worth porting — pure string ops in Go.
- `bash`: port `executeBash` semantics. Shell-reuse caching is *less* needed in Go (process spawn ≈ 1 ms; start one-shot), but **port the env-hardening list and the OutputSink byte-for-byte**. PTY via `creack/pty` behind the same eligibility predicate (UI + opt-in + env escape hatch).
- Grep/glob/fd: v1 = shell out to `rg`/`fd` when present (pi's own answer), pure-Go fallback (`fs.WalkDir` + `regexp`, gitignore parsing); v2 = port the pi-walker shared scan cache (TTL, partition by traversal options, empty-result revalidation, invalidate-on-write).
- AST tools: **don't embed tree-sitter** (CGO + memory); shell out to the `ast-grep` binary if present. Preserves the CGO-free guarantee.
- Approvals: port the tier model (read-only/write/exec) + pattern rules + compound-command conservative combination; keep approvals *separate* from interception; sandbox externally (document container/seatbelt/bubblewrap recipes like pi does).

### IV.8 Extensions & MCP

- **Extension protocol**: an extension is any executable speaking JSONL on stdio: handshake (announce capabilities: tools, commands, event subscriptions, renderers as declarative specs) → event frames (`tool_call` may return block/revise; `tool_result` may patch) → runtime action requests (inject message: steer/followUp/aside; register provider). Timeout + SIGKILL per event. This mirrors omp's RPC + host-tool model, which already proves the pattern works for cross-language embedding.
- Capabilities omp loses vs in-process TS: custom renderers degrade to declarative specs (card, table, tree); deep TUI integration (composer shapes, autocomplete providers) is a v2 concern. Gains: isolation, language-agnostic, killable, no Bun dependency for users.
- **MCP**: official Go SDK (`github.com/modelcontextprotocol/go-sdk`) for stdio/HTTP transports; list_changed notifications; per-server enable/disable; keep tools *filtered* like omp does (browser-automation servers filtered when a built-in browser exists). MCP stays optional — pi ships without it for a reason (context poisoning; the MCP-vs-CLI benchmark favors CLI tools when equivalents exist).

### IV.9 Reliability engineering checklist (port omp's proven moves)

1. Append-only JSONL + leaf pointer; no destructive edits.
2. Latched persistence errors; fail-closed indeterminate-write handling; atomic stage-then-rename rewrites.
3. `session_exit` + `tool_execution_start` forensics; synthetic aborted assistant message on resume.
4. Auto-retry with backoff + provider failover + context promotion before compaction.
5. Stream watchdogs (first-progress + idle); bounded provider-owned retries; capability fallbacks.
6. Memory limit as process backstop (`debug.SetMemoryLimit`), so a runaway turn degrades into "compact now" instead of an OOM kill.
7. Supply-chain: pin deps exactly, `govulncheck` in CI, single-binary releases with checksums.

### IV.10 What NOT to port

- `stats.db` scale (77 MB here) — keep stats in a tiny rollup table.
- In-process extension loading (replaced by IV.8).
- Rust natives — Go stdlib + subprocesses cover 90%; the FS-scan cache is the one concept worth re-implementing.
- Marketplace/complex theme/composer-shape systems until there is demand.
- Built-in LSP server management can start as opt-in (launch `gopls`/`rust-analyzer` only when the `lsp` tool is used — omp's `lsp.lazy` default proves this is the right default).

---

## Part V — Go ecosystem mapping

| Need | Choice | Note |
|---|---|---|
| TUI | `tcell/v2` (+ `rivo/tview` if rich widgets wanted) | Low-alloc, mature; pair with omp frame-plan port |
| ANSI/width | `charmbracelet/x/ansi`, `mattn/go-runewidth` | Zero-width/IME correctness |
| CLI | stdlib `flag` or `spf13/cobra` | Prefer stdlib/`flag` for lightweight; cobra only if subcommand UX needed |
| LLM SSE | hand-rolled over `net/http` (HTTP/2 built in) | One shared reader + four adapters; nothing else needed |
| JSON repair (partial tool args) | port the relaxed parser | ~200 LOC; `tidwall/gjson` for fast paths if needed |
| SQLite (history FTS/stats) | `ncruces/go-sqlite3` (WASM, pure Go) or `modernc.org/sqlite` | CGO-free; `mattn/go-sqlite3` only if CGO acceptable |
| FTS5 | via the above (FTS5 included) | For prompt history |
| MCP | `github.com/modelcontextprotocol/go-sdk` | Official Go SDK |
| PTY | `creack/pty` | Thin, proven |
| Git | shell out to `git` (like omp) | No go-git needed for v1 |
| Grep/glob fallback | stdlib + `sabhiram/go-gitignore` | Shell out to `rg`/`fd` when present |
| Diff (for edit previews) | `pmezard/go-difflib` or `sergi/go-diff` | Structured diffs |
| Token counting | provider-reported usage primary; `pkoukk/tiktoken-go` only if local estimates needed | Estimating locally costs RAM; report usage instead |
| Markdown render (TUI) | `charmbracelet/glamour` (lazy-loaded) or minimal custom renderer | Glamour is heavier; pi renders minimal markdown |
| Testing | stdlib + `stretchr/testify` for table-driven provider tests | Match Go conventions |
| Fuzzing | `go test -fuzz` on session loader + JSON repair | Loader parses untrusted files |
| Memory proof | `runtime.MemStats` in a smoke harness; `GODEBUG=gctrace=1` in CI | Budget verification per IV.1 |

**Feasibility verdict**: every subsystem omp implements has a viable CGO-free Go counterpart; nothing requires a JS runtime. The largest honest risks are (a) TUI fidelity (IME, wide chars, resize) — mitigate by porting omp's frame-plan discipline and testing against real terminals (Ghostty/iTerm/alacritty/kitty); (b) provider quirk coverage — mitigate by porting pi's quirk tables + cross-provider context handoff tests; (c) matching omp's edit-tool UX — mitigate by accepting omp session files in the acceptance test.

---

## Part VI — Name proposal

**Recommended: `adze`** — a small, one-handed woodworking adze. Conveys light, sharp, precise, fast; four letters, one syllable, unclaimed in the CLI-tooling space (verified: no major coding tool named adze), good domain availability (`adze.dev`/`adze.sh`-style), types fast in a terminal, and the verb form ("adzed that file") reads naturally.

Shortlist:

| Name | Vibe | Risk |
|---|---|---|
| **adze** | light, sharp, hand tool | none found |
| `whittle` | carve away the excess; fits "lightweight omp" | longer; whittling is slow (wrong connotation?) |
| `svelte` | fast/light | taken (Svelte compiler) |
| `lathe` | precision tool | a lathe is heavy industrial — wrong |
| `chisel` | precision editing | common name collisions (Chisel HDL) |
| `foil` | light fencing blade | collision with the term "foil" in security |
| `spokeshave` | exactly what a coding agent does to code | too long |

Binary name `adze`, module path e.g. `github.com/<you>/adze`, data dir `~/.adze/agent/`, session format compatible with omp's.

---

## Part VII — Milestones

| M | Scope | Exit criterion |
|---|---|---|
| 0 | Skeleton + config + logger + memory-limit backstop | binary boots <15 MB RSS |
| 1 | Provider layer: 2 adapters (Anthropic + OpenAI Responses), SSE, watchdogs, unified events | streamed chat in `print` mode |
| 2 | Session core: JSONL tree store, buildContext, blob store, listing | **resumes omp-generated session file; context equivalent** |
| 3 | Agent loop + 4 tools + OutputSink + env hardening + approvals | real coding tasks end-to-end |
| 4 | TUI: frame-plan renderer, editor, history retirement, resize | daily-drivable interactive mode |
| 5 | Compaction (threshold + overflow + promotion), retry/failover | 200k-token sessions survive |
| 6 | RPC mode (wire-compatible-ish) + subagents + MCP client | embedders can drive it |
| 7 | Ext subprocess protocol + FS-scan cache + AST shell-out | parity with omp daily workflow |
| 8 | Memory hardening audit, fuzzing, cross-platform builds, packaging | <100 MB RSS verified under worst-case transcript |

---

## Sources

Primary (read in full during this research):

- omp harness docs via `omp://`: `porting-from-pi-mono.md`, `session.md`, `sdk.md`, `extensions.md`, `provider-streaming-internals.md`, `compaction.md`, `bash-tool-runtime.md`, `tui-runtime-internals.md`, `rpc.md`, `memory.md`, `fs-scan-cache-architecture.md`, `natives-architecture.md`
- pi repo README (github.com/earendil-works/pi, formerly badlogic/pi-mono)
- Mario Zechner, "What I learned building an opinionated and minimal coding agent" (2025-11-30, mariozechner.at)
- Mario Zechner, "MCP vs CLI: Benchmarking Tools for Coding Agents" (2025-08-15, mariozechner.at)
- Local install forensics: `~/.bun/bin/omp` → `@oh-my-pi/pi-coding-agent/dist/cli.js` (v18.1.14); `~/.omp/agent` store sizes (sessions 1.4 GB, stats.db 77 MB, blobs 2.6 MB)

Unverified (blocked): omp.dev marketing site (DNS not resolvable from this machine — ENOTFOUND); omp/pi source code beyond the docs (private/closed; the harness docs above are the authoritative published architecture record).
