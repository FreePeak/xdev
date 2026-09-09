# xdev — Product Requirements Document (lightweight Go coding agent)

**Product:** xdev · **Repo:** `FreePeak/xdev` · **Module:** `github.com/FreePeak/xdev` · **Binary:** `xdev` · **Data dir:** `~/.xdev/agent/`

Derived from the [omp/pi Go-rebuild architecture research](research/2026-09-09-omp-pi-architecture-go-rebuild.md) (2026-09-09). That research's proposed codename `adze` is **superseded by the repo name `xdev`**: every module path, binary name, and data dir below uses the product name. The session format stays compatible with omp's.

---

## 1. Product goals & non-goals

### Goals

1. **Single static CGO-free Go binary.** Cross-compiles to all six platforms omp supports, ~10–15 MB on disk, instant startup, no runtime install (research IV.0.2, Part V).
2. **Hard <100 MB RSS budget.** Realistic working target 30–70 MB; worst case must stay under 100 MB — roughly 3–8× lighter than a JS-runtime harness. Achieved by having no JS runtime, windowing the in-memory session to the post-boundary tail, bounding every queue, streaming tool output through fixed-size sinks, and keeping the terminal as the scrollback archive (research Parts III, IV.1).
3. **Port the proven pi/omp data model, not the code.** The JSONL session tree (append-only + mutable leaf pointer), the unified `AssistantMessageEvent` stream contract, context reconstruction, compaction, the OutputSink, and the frame-plan TUI are ported with identical semantics and — where possible — identical entry names, for interop with existing session tooling (research IV.0.1).
4. **pi's minimal philosophy.** <1,000-token system prompt including tool descriptions; exactly four core tools (`read`, `write`, `edit`, `bash`); YOLO by default — no permission theater; containment belongs to external sandboxing (research I.2, IV.0.3).
5. **Extensions are processes, not in-process code.** Never load foreign code in-process; extensions are executables speaking JSONL on stdio with per-event timeout + SIGKILL (research IV.0.4, IV.8).
6. **Bounded everything.** Queues, buffers, session windows, and output sinks have hard limits; backpressure is a feature, not a bug (research IV.0.5).
7. **Inspectable sessions.** Fully documented, post-processable JSONL; the harness gives the user full control and inspection of what enters the model context (research I.2.5).

### Non-goals

- No JS/Bun runtime, no in-process plugin VM, no Rust N-API natives.
- No built-in permission/security system: YOLO by default; sandboxing documented externally (container/seatbelt/bubblewrap recipes, like pi).
- No `stats.db`-scale sidecar accumulation (77 MB on the reference machine) — a tiny rollup table only.
- No marketplace, complex theme system, or composer-shape system until there is demand (research IV.10).
- No tree-sitter embedding (CGO + memory); AST work shells out to the `ast-grep` binary when present.
- No in-tree Git library; shell out to `git` (no go-git for v1).
- No built-in LSP server management beyond opt-in-lazy launch: `gopls`/`rust-analyzer` start only when the `lsp` tool is used (omp's `lsp.lazy` default, research IV.10).

## 2. Scope

### In scope

- **Session core** — JSONL tree store: `EntryBase`/`Entry` union, fixed-width 256-byte title slot, append-only leaf-pointer tree, `buildContext` reconstruction, sha256 blob store, compaction (thresholds + overflow/incomplete recovery + context promotion), listing via 4 KiB prefix reads + stat-keyed cache, durability model (no-fsync appends, latched errors, atomic stage-then-rename rewrites, fail-closed on indeterminate writes).
- **Provider layer** — four wire adapters (Anthropic Messages, OpenAI Completions, OpenAI Responses, Google GenAI) hand-rolled over one shared SSE reader; unified `AssistantMessageEvent` contract; 256-byte partial-JSON throttle with relaxed repair parser; first-progress + idle watchdogs; model-host preconnect; bounded 1,024-event queue with delta coalescing; stop-reason maps + per-provider quirk tables.
- **Agent loop** — one goroutine per turn; bounded tool worker pool (~4–8); steering channels (`steer`/`followUp`/`aside`, queued messages preserved, never discarded); persistence on `message_end` only; layered cancellation via `context.Context` chains.
- **TUI** — tcell + omp frame-plan port: `TerminalFramePlan { history?, viewport }`, active/settled/committed block states, history retirement batches, DSR-anchor resize recovery, alt-buffer overlays; pi-tui editor feature list (fuzzy path completion, bracketed paste, multi-line, IME-safe cursor tail).
- **Tools** — `read`/`write`/`edit`/`bash` exactly per pi's input schemas; omp hashline edit UX (`PUT N.=M:`, `MV`, `CUT`, `REM`) + line-numbered snapshots; OutputSink ported byte-for-byte (fixed head+tail windows, artifact spill); bash env-hardening list; tier approvals (read-only/write/exec) + `bash.patterns` with conservative compound-command combination.
- **RPC** — JSONL-over-stdio embedder contract: `ready` frame advertising protocol versions + frame limits (1 MiB; v2 chunked reassembly to 64 MiB); id-correlated inbound commands (prompt/steer/follow_up/abort/new_session/state/set_model/…); outbound responses, session/agent events, extension-UI dialogs, host-tool calls, host-URI requests, subagent frames; headless/print modes share the same session core with no UI context.
- **MCP client** — official Go SDK (`github.com/modelcontextprotocol/go-sdk`), stdio/HTTP transports, `list_changed` notifications, per-server enable/disable, tool filtering (e.g. browser-automation servers filtered when a built-in browser exists). MCP stays optional — pi ships without it for a reason (context poisoning; CLI tools beat MCP when equivalents exist).
- **Ext subprocess protocol** — extension = any executable speaking JSONL on stdio: capability handshake (tools, commands, event subscriptions, renderers as declarative specs) → event frames (`tool_call` may block/revise fail-closed; `tool_result` may patch) → runtime action requests (steer/followUp/aside, register provider). Per-event timeout + SIGKILL.
- **Subagents** — same session core with restricted tool sets (no ambient MCP/extensions/LSP), structured `outputSchema` (permissive/strict), a `yield` tool, artifact handoff; child = same binary in `print`/`rpc` mode or in-process goroutine sessions with their own `SessionStore`.

### Out of scope (research IV.10)

- `stats.db`-scale sidecar databases — keep stats in a tiny rollup table.
- In-process extension loading — replaced by the subprocess protocol.
- Rust N-API natives — Go stdlib + subprocesses cover ~90%; the FS-scan shared cache is the one concept worth re-implementing (M7).
- Marketplace / complex theme / composer-shape systems until demand exists.
- Built-in LSP server management — opt-in-lazy only.
- `go-git` — shell out to `git`.

## 3. Architecture (HLD summary)

### 3.1 Module layout

```text
xdev/
  cmd/xdev/            # CLI entry: tui | print | rpc modes
  internal/ai/         # provider adapters (anthropic, openai-completions, openai-responses, google)
    internal/ai/sse/   #   shared SSE frame reader, unified AssistantEvent, partial-JSON w/ 256B throttle
  internal/agent/      # agent loop: steering channels, tool scheduling, cancellation layers
  internal/session/    # JSONL tree store, entries, buildContext, compaction, blob store, listing
  internal/tool/       # read/write/edit/bash + registry, approvals, OutputSink, env hardening
  internal/tui/        # frame-plan renderer over tcell
  internal/ext/        # subprocess extension protocol (JSONL capability handshake)
  internal/mcp/        # MCP client via official Go SDK
  internal/store/      # pure-Go SQLite: history FTS, stats rollup; blob store in stdlib
  internal/protocol/   # RPC v1/v2 wire types (compatible with omp's where possible)
```

### 3.2 Session core port spec (the heart)

- **On-disk layout:** `~/.xdev/agent/sessions/<encoded-cwd>/<timestamp>_<sessionId>.jsonl` (cwd-bucketized so symlink aliases share a bucket) and `~/.xdev/agent/blobs/<sha256>`. Listing reads only a 4 KiB prefix (plus a bounded 32 KiB tail for lifecycle status), stat-keyed cached, bounded parallel scan — never parses whole files.
- **File format:** JSONL, one JSON object per line. Physically begins with a **fixed-width 256-byte `title` slot** (rename/list never rewrite the body), then a `type:"session"` header (id, timestamp, cwd, title/titleSource, additionalDirectories, previousSessionFiles, providerPromptCacheKey, parentSession), then entries.
- **Entry model:** every entry has `{type, id (8-char), parentId, timestamp}`. History is an **append-only tree + mutable leaf pointer**: an append creates one entry whose `parentId` = current leaf and becomes the leaf; `branch(entryId)` moves only the leaf pointer — nothing is mutated or deleted; `resetLeaf()` starts a new root (`/clear` writes a payload-free `reset_boundary` marker).
- **Entry types:** `message` (full AgentMessage incl. usage/cost), `thinking_level_change`, `model_change` (per-role model map), `service_tier_change`, `compaction` (summary + `firstKeptEntryId` + `tokensBefore`), `branch_summary` (context for abandoned branches), `reset_boundary`, `custom` (opaque, extension-owned via namespaced `customType`; core reserves lifecycle values such as `tool_execution_start`, `session_exit`), `custom_message`. Go shape: `EntryBase` + one pointer per entry type (single-union decode).
- **Context reconstruction** (`buildContext(entries, leaf)`): walk `parentId` leaf→root (cycle-bounded), reverse, derive runtime state from the path (thinking level, per-role models, service tiers, mode), pick the emission boundary (latest `reset_boundary` or latest compaction: emit summary + entries from `firstKeptEntryId`), convert `message`/`custom_message`/`branch_summary` to model messages, neutralize dangling tool calls, drop unsafe turns. Pure function, unit-testable against pi/omp-generated session files. **Acceptance test: xdev must resume an omp-generated session file and produce an equivalent LLM context** — that one test pins the entire port (research IV.3).
- **Blob store:** sha256 content-addressed files; externalize images/data-URLs to `blob:sha256:<hash>` at omp's thresholds; truncate >500k-char strings with byte-exact exceptions for signed provider blocks; files >8 MiB load via streaming JSONL.
- **Compaction:** thresholds + overflow/incomplete recovery + context promotion first; `handoff` doc generation and provider-native streaming compaction later (LLM prompt engineering, portable as-is).
- **Durability model (deliberately pragmatic):** appends are synchronous in-memory + writer hand-off with **no fsync** (`bufio.Writer` + explicit `Flush()` per append; optional strict-fsync flag for paranoid durability); new ordinary sessions stay memory-only until the first assistant message or `ensureOnDisk()` (no junk files for aborted starts); full rewrites (migrations, moves, repair) are atomic stage-then-rename with an EPERM-safe fallback and commit guard — if a rewrite's durability cannot be proven, the manager **fails closed** rather than silently losing data; persistence errors are **latched** and rethrown on later flush/close; crash forensics via `session_exit` + `tool_execution_start` entries, with the loader synthesizing an `aborted` assistant message on resume.
- **Prompt history:** deliberately a *separate* SQLite/FTS5 subsystem — replay fidelity lives in JSONL, search UX in SQLite; never mixed.

### 3.3 Provider layer

- **Four wire adapters, one shared SSE reader**, hand-rolled over `net/http` (HTTP/2 for free): `data:` frames and `[DONE]` sentinel for OpenAI-family, typed events for Anthropic. Each adapter maps its native stream onto the unified event contract verbatim: `start` → block lifecycle triplets (`text_start/delta/end`, `thinking_*`, `toolcall_*`), `image_end`, terminal `done{reason: stop|length|toolUse}` or `error{reason: aborted|error}`.
- **Partial-JSON tool args:** adapters accumulate `partialJson` and re-parse throttled (skip until ≥256 new bytes; the final parse at `toolcall_end` is authoritative) with a relaxed/repairing JSON parser (~200 LOC port) so truncated deltas never crash mid-stream.
- **Watchdogs:** goroutine per stream — first-progress timer + idle timer, reset on every event; expiry aborts into `error{aborted}`. **Preconnect:** warm-up dial on model resolve (saves 100–300 ms on transcontinental hops).
- **Stop-reason maps + quirk tables** ported from pi (they encode years of provider annoyance); provider-owned bounded retries (empty completions, malformed envelopes, capability fallbacks such as retrying without rejected strict-tool fields).
- **Backpressure fix:** bounded event queue (1,024 events). On overflow, coalesce consecutive `*_delta` events of the same block — rendering already coalesces visually; persistence only needs final messages. This removes omp's documented unbounded-queue wart without changing semantics.
- **Cancellation:** `context.Context` chains (session → agent → request); tool execution gets a merged derived context (small `errgroup`-based merge helper; Go has no `AbortSignal.any`).

### 3.4 Agent loop

- One goroutine per turn; tools run on a **bounded worker pool** (same-batch concurrency kept from omp, capped ~4–8).
- **Steering** via `chan Steering{steer|followUp|aside}` consumed at step boundaries; `aside` injects at the next agent step boundary without killing the in-flight tool batch; queued messages preserved, never thrown away.
- **Persistence only on `message_end`** (matching omp); streaming text is never persisted mid-flight.
- **Extension interception** (`tool_call`/`tool_result`) called synchronously through the ext-protocol bus with **per-call timeout + kill switch**. `tool_call` policy blocks fail closed; a *hung* extension is killable — which in-process code cannot offer and a subprocess can.
- **Subagents:** same session core, restricted tool sets + `outputSchema` (permissive/strict enforcement) + `yield` tool; start in-process (cheap goroutines, shared provider pool), move out-of-process later if isolation matters.

### 3.5 TUI

- Base: **tcell** (raw, fast, low-alloc) with omp's frame-plan model on top: each frame = mandatory chrome (editor/status/HUD/overlay) + `TerminalFramePlan { history?: {id, rows}, viewport: rows[] }`; transcript blocks in three states — **active** (mutable, viewport-resident), **settled** (finalized, reflows on resize until capacity pressure), **committed** (written into terminal scrollback, caches released).
- **History retirement** offered as batches only under capacity pressure, acknowledged only after the write is accepted — **the terminal's native scrollback is the archive** (no app-side scrollback memory). **Resize** borrows the alt-buffer, recovers the anchor via a DSR (CSI 6n) cursor round-trip, and replays per mode (`rebuild`/`append`/`preserve`). Tool blocks collapse by row budget (0→hidden, 1→label line, 2→folded card, ≥3→full renderer).
- **Rendering discipline:** every row exactly `width` visible cells; ANSI is zero-width; one trailing separator row per block; synchronized output; IME-safe cursor tails.
- **Editor:** port pi-tui's feature list — fuzzy path completion (over the FS-scan cache port), bracketed paste, multi-line, IME-safe cursor tail. Width/ANSI helpers from `charmbracelet/x/ansi` + `mattn/go-runewidth`.

### 3.6 Tools

- `read`/`write`/`edit`/`bash` **exactly per pi's input schemas** (model familiarity is an asset). omp-style line-numbered snapshots + hashline anchors (`PUT N.=M:`, `MV`, `CUT`, `REM`) are the proven edit UX — pure string ops in Go.
- `bash`: port `executeBash` semantics; one-shot process spawn (~1 ms in Go, no shell-reuse cache needed) — but port the **env-hardening list** and the **OutputSink byte-for-byte** (fixed head+tail windows, artifact spill). PTY via `creack/pty` behind the same eligibility predicate (UI + opt-in + env escape hatch).
- Grep/glob/fd: v1 shells out to `rg`/`fd` when present (pi's own answer) with a pure-Go fallback (`fs.WalkDir` + `regexp` + gitignore parsing); v2 ports the pi-walker shared FS-scan cache (TTL, partition by traversal options, empty-result revalidation, invalidate-on-write).
- AST tools: **no tree-sitter embedding** (CGO + memory); shell out to the `ast-grep` binary if present — preserves the CGO-free guarantee.
- Approvals: port the tier model (read-only/write/exec) + `bash.patterns` allow/deny/prompt with conservative compound-command combination; approvals stay **separate** from interception (approval decides *whether to run*, interception routes *to a better tool*); real containment is external sandboxing (document container recipes like pi).

### 3.7 Memory budget (hard <100 MB RSS)

| Component | Budget | Technique |
|---|---|---|
| Go runtime + binary + idle GC heap | 8–15 MB | Standard; `GOGC` tuning, `debug.SetMemoryLimit` as hard backstop |
| Provider HTTP/2 connections + TLS | 3–8 MB | Shared `http.Client`, connection reuse, streaming bodies |
| Session entries in memory (windowed) | 10–30 MB | Materialize only the post-boundary tail (compaction/reset boundary); older entries stay on disk, loaded on demand for list/tree/export |
| In-flight assistant stream state | 1–3 MB | Only the current message materialized |
| Tool output sinks | <1 MB | Fixed head+tail windows, artifact spill (OutputSink ported exactly) |
| TUI frame buffers | 2–6 MB | Viewport-only frames; terminal owns scrollback (frame-plan model) |
| JSON decode buffers | 2–5 MB | `json.Decoder` per JSONL line; never unmarshal whole files |
| MCP child servers | 0 (agent RSS) | Child processes; agent holds pipes + tool descriptors only |
| **Total worst case** | **~30–70 MB (est.)** | Enforced via memory limit + bounded structures |

`debug.SetMemoryLimit` is the process backstop: a runaway turn degrades into "compact now" instead of an OOM kill.

### 3.8 Go ecosystem mapping

| Need | Choice | Note |
|---|---|---|
| TUI | `tcell/v2` (+ `rivo/tview` if rich widgets wanted) | Low-alloc, mature; pair with omp frame-plan port |
| ANSI/width | `charmbracelet/x/ansi`, `mattn/go-runewidth` | Zero-width/IME correctness |
| CLI | stdlib `flag`; `spf13/cobra` only if subcommand UX needed | Prefer stdlib for lightweight |
| LLM SSE | hand-rolled over `net/http` (HTTP/2 built in) | One shared reader + four adapters; nothing else needed |
| JSON repair (partial tool args) | port the relaxed parser (~200 LOC) | `tidwall/gjson` for fast paths if needed |
| SQLite (history FTS / stats rollup) | `ncruces/go-sqlite3` (WASM, pure Go) or `modernc.org/sqlite` | CGO-free; FTS5 included, for prompt history |
| MCP | `github.com/modelcontextprotocol/go-sdk` | Official Go SDK |
| PTY | `creack/pty` | Thin, proven |
| Git | shell out to `git` (like omp) | No go-git for v1 |
| Grep/glob fallback | stdlib + `sabhiram/go-gitignore`; shell out to `rg`/`fd` when present | v2: FS-scan cache port |
| Diff (edit previews) | `pmezard/go-difflib` or `sergi/go-diff` | Structured diffs |
| Token counting | provider-reported usage primary; `pkoukk/tiktoken-go` only if local estimates needed | Local estimates cost RAM |
| Markdown render (TUI) | `charmbracelet/glamour` (lazy-loaded) or minimal custom renderer | pi renders minimal markdown |
| Testing | stdlib + `stretchr/testify` for table-driven provider tests | Match Go conventions |
| Fuzzing | `go test -fuzz` on session loader + JSON repair | Loader parses untrusted files |
| Memory proof | `runtime.MemStats` smoke harness; `GODEBUG=gctrace=1` in CI | Budget verification per §3.7 |

Feasibility verdict (research Part V): every subsystem omp implements has a viable CGO-free Go counterpart; nothing requires a JS runtime. Top risks and mitigations: TUI fidelity (IME, wide chars, resize) → frame-plan discipline + tests against real terminals (Ghostty/iTerm/alacritty/kitty); provider quirk coverage → pi's quirk tables + cross-provider context handoff tests; edit-tool UX parity → the omp-session acceptance test in §3.2.

### 3.9 Reliability requirements (ported from omp, research IV.9)

1. Append-only JSONL + leaf pointer; no destructive edits.
2. Latched persistence errors; fail-closed indeterminate-write handling; atomic stage-then-rename rewrites.
3. `session_exit` + `tool_execution_start` forensics; synthetic aborted assistant message on resume.
4. Auto-retry with backoff + provider failover + context promotion before compaction.
5. Stream watchdogs (first-progress + idle); bounded provider-owned retries; capability fallbacks.
6. Memory limit as process backstop, so a runaway turn degrades into "compact now".
7. Supply-chain: pin deps exactly, `govulncheck` in CI, single-binary releases with checksums.

## 4. Milestones & status

| Milestone | Scope | Exit criterion | Status | Issue |
|---|---|---|---|---|
| M0 | Skeleton + config + logger + memory-limit backstop | binary boots <15 MB RSS | Not started | [#1](https://github.com/FreePeak/xdev/issues/1) |
| M1 | Provider layer: 2 adapters (Anthropic + OpenAI Responses), SSE, watchdogs, unified events | streamed chat in `print` mode | Not started | [#2](https://github.com/FreePeak/xdev/issues/2) |
| M2 | Session core: JSONL tree store, buildContext, blob store, listing | **resumes omp-generated session file; context equivalent** | Not started | [#3](https://github.com/FreePeak/xdev/issues/3) |
| M3 | Agent loop + 4 tools + OutputSink + env hardening + approvals | real coding tasks end-to-end | Not started | [#4](https://github.com/FreePeak/xdev/issues/4) |
| M4 | TUI: frame-plan renderer, editor, history retirement, resize | daily-drivable interactive mode | Not started | [#5](https://github.com/FreePeak/xdev/issues/5) |
| M5 | Compaction (threshold + overflow + promotion), retry/failover | 200k-token sessions survive | Not started | [#6](https://github.com/FreePeak/xdev/issues/6) |
| M6 | RPC mode (wire-compatible-ish) + subagents + MCP client | embedders can drive it | Not started | [#7](https://github.com/FreePeak/xdev/issues/7) |
| M7 | Ext subprocess protocol + FS-scan cache + AST shell-out | parity with omp daily workflow | Not started | [#8](https://github.com/FreePeak/xdev/issues/8) |
| M8 | Memory hardening audit, fuzzing, cross-platform builds, packaging | <100 MB RSS verified under worst-case transcript | Not started | [#9](https://github.com/FreePeak/xdev/issues/9) |

## 5. Key decisions

- **Go single static binary, CGO-free** — cross-compiles everywhere, ~10–15 MB on disk, instant startup, no runtime install; it is the only way to meet the hard <100 MB RSS budget a JIT-ed JS runtime cannot (research Part III, IV.0.2).
- **Port the data model, not the code** — pi/omp's session tree, stream contract, context reconstruction, OutputSink, and frame-plan TUI are mature, crash-tested designs; porting their semantics (same entry names) buys interop with existing session tooling without carrying JS-era implementation weight (research IV.0.1).
- **Extensions as subprocesses with per-event timeout + SIGKILL** — omp's in-process TS extensions run with full process trust and a stray throw kills the session; a process boundary makes hung extensions killable and extensions language-agnostic, and omp's RPC/host-tool model already proves cross-language embedding works (research II.5, IV.5, IV.8).
- **Bounded queues with delta coalescing** — fixes omp's documented unbounded-queue wart without changing semantics: on overflow of the 1,024-event queue, coalesce consecutive `*_delta` events of the same block; rendering coalesces visually and persistence only needs final messages (research II.3, IV.4).
- **tcell over bubbletea** — bubbletea's Elm event model adds allocation churn; tcell is raw, fast, low-alloc, and omp's frame-plan model ports cleanly on top (research IV.6).
- **Pure-Go SQLite (`ncruces/go-sqlite3` or `modernc.org/sqlite`)** — keeps the CGO-free guarantee while providing FTS5 for prompt history; replay fidelity stays in JSONL, search UX in SQLite, never mixed (research Part V, II.2).
- **Shell out to `rg`/`fd`/`ast-grep`; no tree-sitter embedding** — embedding tree-sitter means CGO + memory; Go stdlib + subprocesses cover ~90% of the Rust natives; the one natives concept worth re-implementing in-process is the FS-scan shared cache (research IV.7, IV.10).
- **Product name xdev; research codename `adze` superseded** — the repo name (`FreePeak/xdev`) is authoritative: binary `xdev`, module `github.com/FreePeak/xdev`, data dir `~/.xdev/agent/`; session format remains compatible with omp's.

## 6. Links

- [README.md](../README.md) — project overview.
- [docs/research/2026-09-09-omp-pi-architecture-go-rebuild.md](research/2026-09-09-omp-pi-architecture-go-rebuild.md) — full architecture research and rebuild blueprint (authoritative source for every design point summarized above).

---

*Last updated: 2026-09-09 (initial PRD derived from omp/pi Go-rebuild research)*
