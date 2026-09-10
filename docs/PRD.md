# xdev — Product Requirements Document (lightweight Go coding agent)

**Product:** xdev · **Repo:** `FreePeak/xdev` · **Module:** `github.com/FreePeak/xdev` · **Binary:** `xdev` · **Data dir:** `~/.xdev/agent/`

Derived from the [omp/pi Go-rebuild architecture research](research/2026-09-09-omp-pi-architecture-go-rebuild.md) (2026-09-09). That research's proposed codename `adze` is **superseded by the repo name `xdev`**: every module path, binary name, and data dir below uses the product name. The session format stays compatible with omp's.

---

## 1. Product goals & non-goals

### Goals

1. **Single static CGO-free Go binary.** Cross-compiles to all six platforms omp supports, ~10–15 MB on disk, instant startup, no runtime install (research IV.0.2, Part V).
2. **Hard <100 MB RSS budget.** Realistic working target 30–70 MB; worst case must stay under 100 MB — roughly 3–8× lighter than a JS-runtime harness. Achieved by having no JS runtime, windowing the in-memory session to the post-boundary tail, bounding every queue, streaming tool output through fixed-size sinks, and keeping the terminal as the scrollback archive (research Parts III, IV.1).
3. **Port the proven pi/omp data model, not the code.** The JSONL session tree (append-only + mutable leaf pointer), the unified `AssistantMessageEvent` stream contract, context reconstruction, compaction, the OutputSink, and the frame-plan TUI are ported with identical semantics and — where possible — identical entry names, for interop with existing session tooling (research IV.0.1).
4. **pi's minimal philosophy.** <1,000-token system prompt including tool descriptions; exactly four core tools (`read`, `write`, `edit`, `bash`); YOLO by default — no permission theater; containment belongs to external sandboxing. (Primary-source check 2026-09-09: pi v0.85 actually ships 8 built-ins — `read`, `write`, `edit`, `bash`, `powershell`, `grep`, `find`, `ls` — with a measured ~460–510-token prompt and MCP only via the `pi-mcp-adapter` extension; xdev keeps the four-core-tool surface and ships grep/find/ls as rg/fd shell-outs, docs/research/parity-pi-internals.md.)
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
- No desktop control (`computer`), full DAP debugging, OMP-native `security_scan`, `tts`, or `generate_image` — per-OS native code or cloud-only dependencies with no daily-driver coding value (parity research: docs/research/parity-tools-providers.md).
- No embedded local tiny models (ONNX/MLX workers for titles/memory) — heavy native dependency; the online path only (research IV.10).
- No omp ecosystem services: `auth-broker`/`gateway`, `browser-relay`, `mnemopi` backend, `collab-web`, `metaharness`, `robomp`, `omp-stats`, `omptype`, `edit-benchmark` companion packages (parity research: docs/research/parity-tools-providers.md §E).
- No `sharpshooter` memory backend — niche; xdev ships the local two-phase memory pipeline instead (parity research: docs/research/parity-knowledge-ui.md).

## 2. Scope

### In scope

- **Session core** — JSONL tree store: `EntryBase`/`Entry` union, fixed-width 256-byte title slot, append-only leaf-pointer tree, `buildContext` reconstruction, sha256 blob store, compaction (thresholds + overflow/incomplete recovery + context promotion), listing via 4 KiB prefix reads + stat-keyed cache, durability model (no-fsync appends, latched errors, atomic stage-then-rename rewrites, fail-closed on indeterminate writes).
- **Provider layer** — four wire adapters (Anthropic Messages, OpenAI Completions, OpenAI Responses, Google GenAI) hand-rolled over one shared SSE reader; unified `AssistantMessageEvent` contract; 256-byte partial-JSON throttle with relaxed repair parser; first-progress + idle watchdogs; model-host preconnect; bounded 1,024-event queue with delta coalescing; stop-reason maps + per-provider quirk tables; classified-error taxonomy (Flag bitmask + retriable kinds + overflow regexes, omp-context-resilience §3); failed-request dumps to disk; stalled-stream partial tool-call stubs are never executed (hermes pattern).
- **Agent loop** — one goroutine per turn; bounded tool worker pool (~4–8); steering channels (`steer`/`followUp`/`aside`, queued messages preserved, never discarded); persistence on `message_end` only; layered cancellation via `context.Context` chains; `todo` tool (9-op table, 5 statuses with `blocked`+`reason`, phased init) + TodoTracker loop engine (stop-time reminders max 3, mid-run nudge 12 mutations / 2 per cycle, blocked excluded) ported from omp v18; out-of-band user steering markers appended to tool results; `<untrusted_tool_result>` wrapping for web/browser/MCP output; per-result/per-turn tool-output budgets with disk spill (omp-todos-internals.md, hermes-internals.md).
- **TUI** — tcell + omp frame-plan port: `TerminalFramePlan { history?, viewport }`, active/settled/committed block states, history retirement batches, DSR-anchor resize recovery, alt-buffer overlays; pi-tui editor feature list (fuzzy path completion, bracketed paste, multi-line, IME-safe cursor tail).
- **Tools** — `read`/`write`/`edit`/`bash` exactly per pi's input schemas; omp hashline edit UX (`PUT N.=M:`, `MV`, `CUT`, `REM`) + line-numbered snapshots; five edit modes behind one `edit` tool (hashline/replace/patch/apply_patch/sloppy with per-model variant resolution); OutputSink ported byte-for-byte (fixed head+tail windows, artifact spill); bash env-hardening list; tier approvals (read-only/write/exec) + `bash.patterns` with conservative compound-command combination; grep/glob/find/ls via rg/fd shell-outs under omp's caps; file-freshness (modified-since-read) rejection; optional backgrounded bash; hidden tools (`yield`/`goal`/`think`) mounted behind activity gates (parity-omp-internals.md).
- **RPC** — JSONL-over-stdio embedder contract: `ready` frame advertising protocol versions + frame limits (1 MiB; v2 chunked reassembly to 64 MiB); id-correlated inbound commands (prompt/steer/follow_up/abort/new_session/state/set_model/…); outbound responses, session/agent events, extension-UI dialogs, host-tool calls, host-URI requests, subagent frames; headless/print modes share the same session core with no UI context.
- **MCP client** — official Go SDK (`github.com/modelcontextprotocol/go-sdk`), stdio/HTTP transports, `list_changed` notifications, per-server enable/disable, tool filtering (e.g. browser-automation servers filtered when a built-in browser exists). MCP stays optional — pi ships without it for a reason (context poisoning; CLI tools beat MCP when equivalents exist).
- **Ext subprocess protocol** — extension = any executable speaking JSONL on stdio: capability handshake (tools, commands, event subscriptions, renderers as declarative specs) → event frames (`tool_call` may block/revise fail-closed; `tool_result` may patch) → runtime action requests (steer/followUp/aside, register provider). Per-event timeout + SIGKILL.
- **Subagents** — same session core with restricted tool sets (no ambient MCP/extensions/LSP), structured `outputSchema` (permissive/strict), a `yield` tool, artifact handoff; child = same binary in `print`/`rpc` mode or in-process goroutine sessions with their own `SessionStore`; fork (context-inheriting) vs fresh agent types; stall detection with retry escalation; subagent JSONL + meta.json persistence with orphan auto-resume (claude-code-internals.md).
- **Model roles, providers, auth & config layering** — `modelRoles` record with `@role` aliases and `:effort` suffixes; per-provider `models.yml` (baseUrl/apiKey/auth/headers/discovery/overrides); credential resolution chain (CLI flag → models.yml → stored OAuth → /login key → env + `.env` layering); Claude Pro/Max + Codex OAuth (PKCE S256, refresh, quota-aware rotation); settings layering engine + `xdev config` CLI; env framework (process env → project `.env` → agent `.env`); approval modes always-ask|write|yolo + per-tool approval record + `bash.patterns` (docs/research/parity-tools-providers.md); role-selector grammar `provider/id:effort@upstream` with alias loop guard; durable credential-block table + quota rotation; failover chains; `promptCacheKey` = sessionId with fork inheritance cleared on model/thinking/tool overrides (omp-context-resilience §1).
- **Session UX** — slash commands (native markdown discovery project+user, quote-aware `$1..$n`/`$@`/`$ARGUMENTS` expansion, unknown-command fall-through); lifecycle `/new` `/fresh` `/clear` `/drop` `/fork`; `--continue`/`--resume` + picker + `switchSession` runtime; `/tree` `/branch`; context files (AGENTS.md/CLAUDE.md hierarchy, `@path` imports) + SYSTEM.md/APPEND_SYSTEM.md variants + `--system-prompt` flags; keybindings remap; `/dump` transcript (docs/research/parity-session-ux.md); session titles (`/rename`, ai-title small-model generation, custom > ai > first-prompt cascade, picker metadata); context-file prompt-injection scanning; three-tier stable→context→volatile prompt-cache assembly (claude-code-internals.md, hermes-internals.md).
- **Agent system** — task-agent discovery (markdown+frontmatter, first-wins merge), spawn policy (allowlist, depth guard, plan-mode read-only children), model precedence; `hub` tool (messaging, jobs, process supervision) + Agent Hub TUI roster; hooks event bus with fail-closed `tool_call`/`tool_result` interception; advisor/watchdog steering; prewalk big→`@smol` model handoff; magic keywords (`ultrathink`/`orchestrate`/`workflowz`); plan mode + `xd://propose`/`xd://resolve` finalization (docs/research/parity-agent-system.md); parent-owned `todo` policy (stripped from subagent tool sets except prewalk-armed children); cross-session messaging (mailbox dirs, SendMessage, InboxPoller idle delivery, accept/hold/off inbound policy) as NICE (claude-code-internals.md).
- **Knowledge & chrome** — memory backend seam + local backend (extraction → smol consolidation → MEMORY.md + learned.md) + `memory://` read seam; skills (SKILL.md discovery, `skill://` protocol, `/skill:` commands, managed skills); `learn` tool; theme engine (66-token JSON themes, vars, color detection, dark/light auto, symbol presets, live reload); TUI chrome (status-line/HUD segments, overlays, custom tool renderers) (docs/research/parity-knowledge-ui.md); frozen memory snapshots injected at prompt build (not per-turn) to protect prefix cache (hermes pattern).
- **Extended tools & ecosystems (demand-driven)** — eval kernel (py persistent NDJSON cells), notebook virtual text, `web_search` (22-provider chain with constraint relaxation), `github` tool, `ast_grep`/`ast_edit`, browser via chromedp, checkpoint/rewind (session-tree branch + report entry, NOT a git shadow repo), custom subprocess tools, MCP config extensions, LSP tool + `lsp-config`, secrets redaction, context promotion, deferred tool-catalog bridge (`tool_search`/`tool_describe`/`tool_call`), marketplace/plugin manager; v2 modes: vibe (director + workers), collab (E2E encrypted), multi-provider discovery, profiles (docs/research/parity-tools-providers.md).

### Out of scope (research IV.10)

- `stats.db`-scale sidecar databases — keep stats in a tiny rollup table.
- In-process extension loading — replaced by the subprocess protocol.
- Rust N-API natives — Go stdlib + subprocesses cover ~90%; the FS-scan shared cache is the one concept worth re-implementing (M7).
- Marketplace and composer-shape systems until demand exists (theme engine itself is now IN scope — M12 CORE; marketplace/plugin manager is the M13 tail).
- Built-in LSP server management — opt-in-lazy only.
- `go-git` — shell out to `git`.

### Feature-parity matrix

Classes per docs/research/parity-*.md scout reports (CORE = required for parity, NICE = demand-driven, SKIP = documented non-goal). Milestones reference the §4 roadmap.

| Feature | omp doc | Class | xdev milestone |
|---|---|---|---|
| Model roles (`modelRoles`, `@role` aliases, `:effort`) | models.md | CORE | M9 |
| Per-provider `models.yml` (baseUrl/apiKey/auth/headers/discovery/overrides) | models.md | CORE | M9 |
| Credential resolution chain (CLI → models.yml → OAuth → /login → env) | models.md | CORE | M9 |
| Claude Pro/Max + Codex OAuth (PKCE S256, refresh, quota rotation) | models.md | CORE | M9 |
| Wire transports v1: anthropic-messages, openai-completions, openai-responses, google-generative-ai | models.md | CORE | M1 + M9 |
| Wire transports v2: azure, bedrock, vertex, gemini-cli, codex-OAuth | models.md | NICE | M14 |
| Settings layering engine (defaults ← global ← project ← `--config`) + `xdev config` CLI | config.md | CORE | M9 |
| Env framework (process env → project `.env` → agent `.env`) | config.md | CORE | M9 |
| Approval modes always-ask\|write\|yolo + per-tool approval + `bash.patterns` | approval-mode.md | CORE | M3 + M9 |
| Secrets redaction (`secrets.yml`, reversible `$$HASH$$` placeholders) | secrets.md | NICE | M13 |
| Slash commands (native markdown discovery, `$1..$n`/`$@`/`$ARGUMENTS`) | slash-command-internals.md | CORE | M10 |
| CLI surface (modes, flags; `--continue`/`--resume`/`--fork`/`--print`/`--rpc`) | cli.md | CORE | M10 |
| Keybindings remap (`keybindings.yml`) + `/hotkeys` | keybindings.md | CORE | M10 |
| Lifecycle `/new` `/fresh` `/clear` `/drop` `/fork` | session-operations.md | CORE | M10 |
| `--continue` breadcrumb + `--resume` + picker + `switchSession` runtime | session-operations.md | CORE | M10 |
| `/tree` `/branch` selectors + labels/filters (data model: M2) | session-operations.md | NICE | M10 |
| Context files (AGENTS.md/CLAUDE.md hierarchy, `@path` imports) | context-files.md | CORE | M10 |
| Third-party context files (`.cursorrules` etc.) | context-files.md | NICE | M10 |
| SYSTEM.md/APPEND_SYSTEM.md + `--system-prompt`/`--append-system-prompt` | system-prompts.md | CORE | M10 |
| TITLE_SYSTEM.md/PERSONALITY.md | system-prompts.md | NICE | M10 |
| `/dump` text transcript | tui.md | CORE | M10 |
| `/export` HTML + `/share` E2E-encrypted | tui.md | NICE | M14 |
| Task agents (markdown discovery, first-wins merge, spawn policy, depth guard) | agent.md | CORE | M11 |
| Hub tool (messaging/jobs/processes) | agent-hub.md | CORE | M11 |
| Agent Hub TUI roster (status/model/activity/cost, steer, kill) | agent-hub.md | CORE | M11 |
| Agent Hub inspector | agent-hub.md | NICE | M11 |
| Hooks event bus (lifecycle events, fail-closed `tool_call`/`tool_result`, subprocess hooks) | hooks.md | CORE | M11 |
| Embedded-JS hooks | hooks.md | NICE | M11 |
| Advisor/watchdog (per-delta constraint steering, nit→aside/concern→interrupt/blocker→steer) | watchdog.md | CORE | M11 |
| WATCHDOG.md guidance + WATCHDOG.yml advisor roster | watchdog.md | NICE | M11 |
| Prewalk (one-shot big→`@smol` handoff, `--prewalk`, `/prewalk`) | prewalk.md | CORE | M11 |
| Magic keywords (`ultrathink`/`orchestrate`/`workflowz`) | prompt-extensions.md | CORE | M11 |
| Plan mode + `xd://propose`/`xd://resolve` finalization | plan-mode.md | CORE | M11 |
| Vibe mode (director + worker subagents, tiered fast\|good) | vibe.md | NICE | M14 |
| Collab (E2E AES-256-GCM WS relay, host-authoritative, guest replica) | collab.md | NICE | M14 |
| Memory local backend (extraction → consolidation → MEMORY.md/learned.md) | memory.md | CORE | M12 |
| `memory://` read seam + `/memory` view\|stats\|clear | memory.md | CORE | M12 |
| Mnemopi SQLite-FTS5 backend + `recall`/`retain`/`reflect`/`memory_edit` tools | memory.md | NICE | M13 |
| `learn` tool + managed skills | skills.md | CORE | M12 |
| Skills (SKILL.md discovery, `skill://` protocol, `/skill:` commands) | skills.md | CORE | M12 |
| Theme engine (66-token themes, vars, color detection, symbol presets, live reload) | theme.md | CORE | M12 |
| Status-line/HUD theming + spinner frames + overlays/ask picker | tui.md | CORE | M12 |
| Custom tool renderers (`renderCall`/`renderResult`) + Component contract | tui.md | NICE | M12 |
| Kitty inline images | tui.md | NICE | M13 |
| Eval kernel py (persistent NDJSON subprocess cells) | eval.md | CORE | M13 |
| Eval kernel js | eval.md | NICE | M13 |
| Notebook `.ipynb` virtual text | notebook.md | NICE | M13 |
| `web_search` (22-provider chain with constraint relaxation) | web-search.md | NICE | M13 |
| `github` tool via `gh` shell-out | github.md | NICE | M13 |
| `ast_grep`/`ast_edit` via `sg` binary | ast-tools.md | NICE | M13 |
| Browser (chromedp CDP) | browser.md | NICE | M13 |
| Checkpoint/rewind (branch pointers + report entry) | checkpoint.md | NICE | M13 |
| Custom tools (subprocess TS/JS modules) | custom-tools.md | NICE | M13 |
| LSP tool + `lsp-config` (lazy launch, rootMarkers autodetect) | lsp-config.md | NICE | M13 |
| Marketplace + plugin manager | marketplace.md | NICE | M13 |
| MCP config extensions (imports from claude/codex/gemini/cursor, `!command` secrets, per-server timeout) | mcp-config.md | CORE | M6 + M13 |
| `todo` tool (9 ops, 5 statuses, phased init, TodoTracker reminders; transcript-persisted) | todo harness doc (omp v18) | CORE | M3 |
| Session titles (`/rename`, ai-title generation, resolution cascade, picker metadata) | claude-code-internals.md | CORE | M2 + M10 |
| File-freshness check (modified-since-read rejection) | claude-code-internals.md | CORE | M3 |
| Backgrounded bash (`run_in_background` + output polling + kill) | claude-code-internals.md | NICE | M3 |
| Cross-session messaging (mailbox, SendMessage, InboxPoller) | claude-code-internals.md | NICE | M11 |
| Deferred tool catalog (`tool_search`/`tool_describe`/`tool_call` bridge) | hermes-internals.md | NICE | M7 |
| Context-file prompt-injection scanning | hermes-internals.md | NICE | M10 |
| `computer` desktop control | computer-use.md | SKIP | — (skipped) |
| Full DAP debug driver | debug.md | SKIP | — (skipped) |
| `security_scan` (OMP-native + Codex cloud) | security-scan.md | SKIP | — (skipped) |
| `tts` text-to-speech | tts.md | SKIP | — (skipped) |
| `generate_image` cloud providers | generate-image.md | SKIP | — (skipped) |
| Local ONNX tiny models (titles/memory workers) | local-models.md | SKIP | — (skipped) |
| `auth-broker`/`gateway` + `browser-relay` | auth-broker-gateway.md | SKIP | — (skipped) |
| User-facing companion packages (omp-stats, mnemopi, metaharness, robomp, …) | packages.md | SKIP | — (skipped) |
| `sharpshooter` memory backend | memory.md | SKIP | — (skipped) |

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
- **Visual target: the Grok CLI.** M4's look-and-feel target is xAI's Grok CLI (user requirement, 2026-09-09; primary evidence: the grok-build open-source sync — github.com/xai-org/grok-build `crates/codegen/xai-grok-pager-render/src/theme/{groknight,grokday}.rs` + `xai-grok-pager/src/views/prompt_widget/`, verified 2026-09-10; cross-checked against the official media.x.ai TUI screenshot by pixel sampling). Concretely:
  - **Theme slots** (superset of omp's 66-token system; all RGB-defined, quantized at startup to truecolor/256/16, `NO_COLOR` → monochrome): backgrounds `bg_base` `bg_light` `bg_dark` `bg_highlight` `bg_hover` `bg_terminal` `bg_visual`; accents `accent_user` `accent_assistant` `accent_thinking` `accent_tool` `accent_system` `accent_error` `accent_success` `accent_running`; text `text_primary` `text_secondary`; grays `gray_dim` `gray` `gray_bright`; prompt chrome `prompt_border` `prompt_border_active`; markdown `md_heading_h1..h3` `md_code` `md_code_bg` `md_muted` `link_fg`. 2026-09-10 status: base+md slots implemented in `internal/theme`.
  - **Reference palette (GrokNight, the default) — VERIFIED 2026-09-10 from grok-build source** (`groknight.rs`): bg ramp `#0a0a0a` terminal / `#141414` base / `#242424` highlight+user-band / `#1c1c1c` code bg; text `#e1e1e1` primary / `#c8c8c8` secondary; grays `#585858`/`#6c6c6c`/`#787878`; accents (TokyoNight Night) BLUE `#7aa2f7` CYAN `#7dcfff` GREEN `#9ece6a` MAGENTA `#bb9af7` ORANGE `#ff9e64` PURPLE `#9d7cd8` RED `#f7768e` TEAL `#1abc9c` YELLOW `#e0af68`; inline code `#3A95AB`; link `#7aa6da`; prompt border `#323237`/active `#505058`. **Semantic correction vs the old provisional guess:** `accent_user` is neutral `#c8c8c8` (FG_DARK) — user is NOT magenta; magenta belongs to assistant/thinking/running. GrokDay (`grokday.rs`): `#eeeeee`/`#dedede`/`#f5f5f5`, text `#262626`/`#444444`, MAGENTA `#7D4BC6`, RED `#CD3048`, TEAL `#0A8E70`, BLUE `#2F64D2`, BLUE1 `#0F87A2`, GREEN `#378E23`, borders `#c8c8cd`/`#a5a5af`.
  - **Grok composer anatomy (implemented):** rounded box `╭─╮│╰─╯` (border `prompt_border_active` when focused), `❯ ` prefix (accent_user, bold), placeholder "Build anything" (muted, unfocused states), model info line embedded in the bottom border (chrome_caption, gray_dim), blinking block cursor; box grows with wrapped input (inline-viewport grow pushes into scrollback).
  - **Scrollback layout** (`pager.toml`-shaped config): outer padding `outer_vpad=1` `hpad 2/2`, block padding `2/2`, left accent rail per block (`collapsed_accent_char` `❙`), scrollbar (`▼/▲` follow indicator, `dim_accent 0.5`), `sticky_headers` pinning user prompts, `expandable_indicator` `›` on foldable entries, `tab_width 4`, `line_under_last_entry` off.
  - **Block styling:** edit-diff blocks (indent, `hunk_separator "…"`, optional dual line numbers, bg light/dark variants, collapsed one-liner by default with `+N/−M` summary), tool blocks collapsed by row budget, thinking blocks dimmed (`dim_accent` blend), fenced code with syntax highlighting via bundled tmTheme mapping.
  - **Interaction contract:** simple mode (arrows + `Shift+Arrow` turn jumps, Space/letter refocuses prompt) and opt-in vim mode (`j/k`, `H/L` turns, `J/K` responses, `h/l` fold, `e` expand, `y` copy, `Enter`/`Ctrl+F` fullscreen viewer, `Ctrl+E` thinking folds); `Tab` prompt↔scrollback focus; blocking cards (permission/ask) own the keyboard with `Tab`/`Shift+Tab` row walk and layered `Esc`-steps-back semantics; `Esc` mid-turn = cancel panel (never kills in-flight tools silently — offers keep-running).
  - **Parity is layout + block styling + interaction + palette — NOT 1:1 behavior.** Grok features that fight tcell's low-alloc model or the <100 MB RSS budget are **config-off-by-default or dropped**: 30 fps accent-wave animation (off; static accent rail is the default rendering), mouse hover highlights (off), background blending/`bg_blend` runtime color generation (off — every color comes from the quantized theme table, never blended at draw time). If any later ship, they are opt-in and must re-justify against the memory budget in M8's audit.


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
| M0 | Skeleton + config + logger + memory-limit backstop | binary boots <15 MB RSS | **Complete (2026-09-09)** — 11.3 MB RSS at boot; `debug.SetMemoryLimit(100MB)` backstop | [#1](https://github.com/FreePeak/xdev/issues/1) |
| M1 | Provider layer: 2 adapters (Anthropic + OpenAI Responses), SSE, watchdogs, unified events, error-classifier taxonomy, failed-request dumps, partial-tool-call stubs never executed | streamed chat in `print` mode | **Complete (2026-09-09, MVP scope)** — 3 adapters (openai-completions, openai-responses, anthropic-messages) over the shared SSE reader + partial-JSON repair; streamed live against the local onegw gateway (watchdogs/retry are M5 hardening) | [#2](https://github.com/FreePeak/xdev/issues/2) |
| M2 | Session core: JSONL tree store, buildContext, blob store, listing | **resumes omp-generated session file; context equivalent** | **Complete (2026-09-09)** — interop test against the real omp herdr fixture passes (11 entries, dangling toolCall neutralized, leaf/branch restore); format omp-exact (256B title slot, v3 header, unknown mid-chain entries preserved as opaque nodes) | [#3](https://github.com/FreePeak/xdev/issues/3) |
| M3 | Agent loop + 4 tools + OutputSink + env hardening + approvals + todo tool (omp 9-op port) + arg-repair pipeline + file-freshness guard + backgrounded bash | real coding tasks end-to-end | **Complete (2026-09-09, MVP scope)** — write→edit→bash chains verified end-to-end via print mode; OutputSink head+tail windows, env hardening, process-group kills; approvals YOLO (tier model landed, `bash.patterns` deferred); todo tool/arg-repair pipeline/file-freshness guard/backgrounded bash deferred to [#16](https://github.com/FreePeak/xdev/issues/16) | [#4](https://github.com/FreePeak/xdev/issues/4) |
| M4 | TUI: frame-plan renderer, editor, history retirement, resize — **visual target: the Grok CLI** (GrokNight theme slots + accent-rail scrollback, folding, blocking cards; §3.5; parity = layout/styling/interaction/palette only — animation/hover/bg-blend off by default) | daily-drivable interactive mode that a Grok CLI user recognizes as the same product class | **Complete (2026-09-09 MVP; 2026-09-10 deep grok-build parity)** — 2026-09-09: GrokNight/GrokDay + slot vocabulary + quantization; streaming blocks with accent rail, tool status, dimmed thinking; editor with history; status line with token counters + spinner; Esc-cancel/Ctrl+C; OSC 12 cursor; live-verified in tmux. 2026-09-10: palette re-verified against grok-build source (`groknight.rs`/`grokday.rs` exact hex; accent_user corrected magenta→neutral `#c8c8c8`); markdown renderer for assistant blocks (headings h1-h3 colored bold, inline code, fenced-code bg band, `•` bullets, `│` quotes, `───` rules, underlined links, URL-hiding); composer as grok rounded box (`╭─╮│╰─╯`, `❯ ` prefix, model info in the bottom border); thinking → "Thought for Xs"; tool `◈`/`↳` rows; user-prompt `#242424` band; right-aligned timestamps; shortcuts bar with bold keys; tmux-verified truecolor SGR capture (rail magenta, headings blue/teal, code band). Deferred to #5: vim mode, blocking cards, history retirement batching, DSR resize recovery, box growth on wrap, NO_COLOR bandless fallback tuning | [#5](https://github.com/FreePeak/xdev/issues/5) |
| M5 | Compaction (threshold + overflow + promotion + method ladder), retry/failover + partial-stream turn recovery | 200k-token sessions survive | **MVP complete (2026-09-10)** — typed `ai.HTTPError` + err-class taxonomy (auth/transient/overflow/bad-request) at the wire source; transient pre-content retry ladder (500ms→8s, 25% downward jitter, post-content errors never replayed); threshold compaction (reserve 16384/15% floor, keepRecent 20000, chars/4 estimate floor, provider-usage override) + overflow→force-compact→retry-once; `buildContext` emits `EntryIDs` so compaction anchors `firstKeptEntryId` store-side; steering messages persisted (no rebuild loss). Tails: model-promotion ladder, multi-provider failover, partial-stream retain+continue (post-content errors surface as-is) | [#6](https://github.com/FreePeak/xdev/issues/6) |
| M6 | RPC mode (wire-compatible-ish) + subagents (yield contract) + MCP client + deferred tool catalog | embedders can drive it | Not started | [#7](https://github.com/FreePeak/xdev/issues/7) |
| M7 | Ext subprocess protocol + FS-scan cache + AST shell-out | parity with omp daily workflow | Not started | [#8](https://github.com/FreePeak/xdev/issues/8) |
| M8 | Memory hardening audit, fuzzing, cross-platform builds, packaging | <100 MB RSS verified under worst-case transcript | Not started | [#9](https://github.com/FreePeak/xdev/issues/9) |
| M9 | Model roles, providers, auth & config layering — `modelRoles` record (default/smol/slow/vision/plan/commit/tiny/task/advisor; `@role` aliases; `:effort` suffix); `models.yml` per-provider config; credential resolution chain (CLI `--api-key` → models.yml → stored OAuth → /login key → env + `.env` layering); wire-transport catalog completed to 4 v1 transports; Claude Pro/Max + Codex OAuth (PKCE S256, refresh, quota-aware rotation); settings layering engine (deep-merge objects, wholesale-replace scalars/arrays) + `xdev config` CLI; env framework (process env → project `.env` → agent `.env`); approval modes always-ask\|write\|yolo + per-tool approval record + `bash.patterns`; role grammar `@upstream` + alias loop guard; credential-block table + rotation; failover chains | Role aliases resolve across session/subagents; OAuth login works; config precedence tests pass | Not started | [M9](https://github.com/FreePeak/xdev/issues/10) |
| M10 | Session UX: commands, lifecycle, context files, prompts, keybindings — slash commands (native markdown discovery project+user, quote-aware `$1..$n`/`$@`/`$ARGUMENTS` expansion, unknown fall-through); lifecycle `/new` `/fresh` `/clear` `/drop` `/fork` (fork = new file + parentSession header); `--continue` breadcrumb (TTY/pane-keyed) + `--resume` + picker + `switchSession` runtime (hooks, rollback, model/tier restore); `/tree` `/branch` selectors + labels/filters; context files (AGENTS.md/CLAUDE.md hierarchy, depth dedup, `@path` imports); SYSTEM.md/APPEND_SYSTEM.md/TITLE_SYSTEM.md/PERSONALITY.md + `--system-prompt`/`--append-system-prompt`; keybindings remap + `/hotkeys`; `/dump`; `/rename` + title cascade (custom > ai > first prompt); `/todo` command surface + markdown round-trip; context-file injection scan; 3-tier prompt-cache assembly | Continue/resume/fork/branch/clear workflow works; custom markdown commands expand; AGENTS.md + imports reach context | Not started | [M10](https://github.com/FreePeak/xdev/issues/11) |
| M11 | Agent system: task agents, hub, hooks, advisor, prewalk, plan mode — task-agent discovery (markdown+frontmatter, first-wins merge project→user→extension→bundled, invalid skipped w/ warning); spawn policy (allowlist, `task.maxRecursionDepth=2` depth guard, plan-mode read-only children, blocked self-recursion); model precedence (task > frontmatter > parent); hub tool (messaging/jobs/processes) + Agent Hub TUI roster; hooks event bus (session/agent/turn/tool lifecycle; fail-closed interception; command-style subprocess hooks); advisor/watchdog (per-delta steering, nit→aside/concern→interrupt/blocker→steer, NFKC guard, cooldown); prewalk (big→`@smol` handoff gated on todo, `--prewalk`/`/prewalk`); magic keywords (ultrathink/orchestrate/workflowz); plan mode + `xd://propose`/`xd://resolve`; parent-owned todo policy; cross-session mailbox messaging (NICE) | Parent spawns scoped subagent steering back via hub; advisor steers on concern; prewalk switches model after first edit | Not started | [M11](https://github.com/FreePeak/xdev/issues/12) |
| M12 | Knowledge & chrome: memory, skills, theme, TUI — memory backend seam + local backend (two-phase: extraction → smol consolidation → MEMORY.md + learned.md; lease+heartbeat; bounded caps) + `memory://` read seam + `/memory`; skills (SKILL.md discovery native+custom+managed first-wins, `skill://` protocol, `/skill:` commands); `learn` tool (+ managed-skill create/update w/ authored-shadowing conflict); theme engine (**slot vocabulary = §3.5's Grok slot names — `bg_*`, `accent_user/assistant/thinking/tool/...`, `text_*`, `gray_*`, diff/markdown slots — the single vocabulary M4 and M12 share; omp's 66-token list maps onto these slots**; vars resolution, hex/256/terminal colors, truecolor/256 detection with quantization, auto dark/light via OS appearance + OSC 11, symbol presets, box-drawing overrides, live reload w/ last-good fallback; GrokNight + GrokDay shipped in M4, remaining built-ins + `auto` land here); TUI chrome (themed status-line/HUD + spinner, overlay/dialog components incl. ask picker, custom tool renderers) | Memory summary+lessons inject at session start; `/skill:` expands; theme live-reloads with all slots enforced | Not started | [M12](https://github.com/FreePeak/xdev/issues/13) |
| M13 | Extended tools & ecosystems — eval kernel (py subprocess NDJSON persistent-state cells); notebook `.ipynb` virtual text; `web_search` (22-provider chain with constraint relaxation); `github` tool via `gh`; `ast_grep`/`ast_edit` via `sg`; browser via chromedp CDP; checkpoint/rewind (branch pointers + report entry); custom tools via subprocess; MCP config extensions (imports, `!command` secrets, per-server timeout/enable); LSP tool + `lsp-config` (lazy launch, rootMarkers); secrets redaction (reversible `$$HASH$$`); context promotion; deferred tool catalog; marketplace/plugin manager | Eval cell persists state; web_search/github/ast-grep answer real queries | Not started | [M13](https://github.com/FreePeak/xdev/issues/14) |
| M14 | v2 modes & distribution polish — vibe mode (director session, `vibe_spawn`/`send`/`wait`/`kill`/`status` over worker subagents, tiered fast\|good); collab (E2E AES-256-GCM WS relay, host-authoritative, guest replica, view-only links); multi-provider command/skill/context discovery (claude/codex/opencode roots + toggles); profiles (user-base relocation); `/export` HTML + `/share`; goal mode; ask-dialog headless-timeout policy | Vibe session completes delegated multi-worker task; collab guest mirrors host session | Not started | [M14](https://github.com/FreePeak/xdev/issues/15) |

**Execution order:** M0→M7 as numbered; M9→M12 (the parity-CORE milestones) next; M13/M14 are demand-driven and can interleave after their CORE prerequisites. M8 (issue #9) re-runs as the **final release gate** once M9–M12 land, re-verifying the <100 MB RSS budget against the full feature set.

**Known gaps — resolved (2026-09-10, commit c96f3f2):**
1. **`/<command>` slash commands now work** (M10 #11 first slice): built-in registry (`/new` `/clear` `/drop` `/help` `/quit` + `/q`) routed at input-submit before any user block is created; markdown commands discovered from `<cwd>/.xdev/commands/*.md` + `~/.xdev/agent/commands/*.md` (project beats user, non-recursive, hidden/non-md skipped) with frontmatter `name:`/`description:`, quote-aware `$1..$n`/`$@`/`$@[start]`/`$@[start:length]`/`$ARGUMENTS` expansion and the no-placeholder blank-line append fallback; unknown `/foo` still falls through as literal prompt text. Lifecycle wired to the session store: `/new` swaps a fresh session file, `/clear` appends a durable `reset_boundary` and resets in place, `/drop` deletes the file and starts fresh — all refuse while a turn is in flight. Remaining M10 scope (fork/resume/picker, context files, keybindings, /dump) unchanged.
2. **TUI scrollback implemented** (P0 #17): a tail-anchored viewport scroll model (`internal/tui/scroll.go`) — offset counts lines above the live tail, follow semantics pin streaming output to the tail, scrolled position survives appends/shrinks, clamped at both ends. Keys: PgUp/PgDn, Ctrl+B/F, arrows (when not running), Home/End jump to top/live; `▲ n ▼ n` indicator when content hides. Contract-tested (scroll model table) plus an integration test through the real key path. Fold/expand of tool blocks remains M4 tail scope.
3. **Agent turn budget graceful** (P0 #18): `MaxTurns` const → `DefaultMaxTurns = 200` + `Agent.MaxTurns` field (0 = default) + `-max-turns` flag (0 = default). At the cap the agent persists a synthetic user wrap-up message ("turn budget reached — wrap up…") and runs exactly one final turn whose tool calls are deliberately left dangling (neutralized on rebuild); the run ends nil-error with a normal assistant status report instead of the fatal `exceeded N turns`. Runaway-loop protection intact (cap applies per run; unit tests cover wrap-up, default resolution, and bounded runaway loops).

**Milestone deltas from the second-wave research (2026-09-09, `docs/research/*-internals.md`):** M1 adds the classified-error taxonomy (Flag bitmask + retriable kinds + overflow regexes) and never-executed partial-tool-call stubs; M2 adds todo-state replay (backward scan preferring `user_todo_edit` entries) and the title-resolution cascade; M3 adds the `todo` tool (9 ops, 5 statuses incl. `blocked`+`reason`), the 7-pass tool-arg repair pipeline, file-freshness rejection, backgrounded bash, and out-of-band steering markers; M5 adds the compaction method ladder `[remote, handoff, shake, soft]` with 80% recovery band + dead-end parking, `TurnRecovery` partial-stream rewrite, and fork-on-compression lineage as the audited alternative; M6 adds the `yield` wrapper contract (retry budgets, 12k inline / 4k preview async-result split), `todoPhases` in `get_state`, and the deferred tool-catalog bridge; M9 adds the role-selector grammar `provider/id:effort@upstream` with alias loop guard, the durable credential-block table + quota rotation, and failover chains; M10 adds `/rename` + ai-title generation, context-file injection scanning, and the 3-tier stable→context→volatile prompt-cache assembly; M11 adds the parent-owned `todo` policy (stripped from subagent tool sets) and mailbox-style cross-session messaging (NICE).

## 5. Key decisions

- **Go single static binary, CGO-free** — cross-compiles everywhere, ~10–15 MB on disk, instant startup, no runtime install; it is the only way to meet the hard <100 MB RSS budget a JIT-ed JS runtime cannot (research Part III, IV.0.2).
- **Port the data model, not the code** — pi/omp's session tree, stream contract, context reconstruction, OutputSink, and frame-plan TUI are mature, crash-tested designs; porting their semantics (same entry names) buys interop with existing session tooling without carrying JS-era implementation weight (research IV.0.1).
- **Extensions as subprocesses with per-event timeout + SIGKILL** — omp's in-process TS extensions run with full process trust and a stray throw kills the session; a process boundary makes hung extensions killable and extensions language-agnostic, and omp's RPC/host-tool model already proves cross-language embedding works (research II.5, IV.5, IV.8).
- **Bounded queues with delta coalescing** — fixes omp's documented unbounded-queue wart without changing semantics: on overflow of the 1,024-event queue, coalesce consecutive `*_delta` events of the same block; rendering coalesces visually and persistence only needs final messages (research II.3, IV.4).
- **tcell over bubbletea** — bubbletea's Elm event model adds allocation churn; tcell is raw, fast, low-alloc, and omp's frame-plan model ports cleanly on top (research IV.6).
- **Pure-Go SQLite (`ncruces/go-sqlite3` or `modernc.org/sqlite`)** — keeps the CGO-free guarantee while providing FTS5 for prompt history; replay fidelity stays in JSONL, search UX in SQLite, never mixed (research Part V, II.2).
- **Shell out to `rg`/`fd`/`ast-grep`; no tree-sitter embedding** — embedding tree-sitter means CGO + memory; Go stdlib + subprocesses cover ~90% of the Rust natives; the one natives concept worth re-implementing in-process is the FS-scan shared cache (research IV.7, IV.10).
- **Product name xdev; research codename `adze` superseded** — the repo name (`FreePeak/xdev`) is authoritative: binary `xdev`, module `github.com/FreePeak/xdev`, data dir `~/.xdev/agent/`; session format remains compatible with omp's.
- **Prompt reality check (2026-09-09 primary sources)** — pi's real base prompt measures ~460–510 tokens over 8 built-ins; omp's full 29-tool slate costs ~12–15k tokens (hashline edit mode alone 5.3 KB). xdev ships the pi-minimal core plus an ESSENTIAL gate: the top-level tool schema carries only essential tools; discoverable tools are demoted behind `xd://` devices and search (parity-omp-internals.md §4).
- **Todo state rides the transcript** — todo phases persist as tool-result `details.phases` plus `user_todo_edit` custom entries (omp v18), replayed by backward scan on branch switch/rewind; NOT side files (Claude Code `tasks/`) and NOT a SQLite table (opencode) — the replayable transcript stays the single source of truth (omp-todos-internals.md §6).
- **Injected content is hostile until scanned** — AGENTS.md-class files and web/browser/MCP tool output pass prompt-injection threat scanning and `<untrusted_tool_result>` wrapping (hermes pattern) before entering context (hermes-internals.md).

### 5.1 Claude Code cross-check (adopt / verify / reject)

Claude Code v2.1.263 was studied independently of pi/omp — bundle forensics (verbatim system prompts + tool schemas from the installed binary), the official docs, and design analysis. Full evidence in §6. Verdicts that change or confirm the plan:

**Adopt:**
- **Microcompact before compaction** (M5) — clear old tool outputs first, summarize only as the second phase; plus a context-high-water warning in the status line and a `/context` per-category budget display.
- **Hooks exit-code contract** (M11) — CC's `PreToolUse` blocking semantics (exit 2 = block with stderr reason, JSON `updatedInput` mutation, two-stage matcher + `if` permission-syntax pre-filter) is the proven guardrail model; keep execution boring (`sh -c`, stdin JSON).
- **Subagent yield-only isolation as a contract test** (M11) — a subagent transcript NEVER streams into parent context; only the final yield does. Hub design already implies this; pin it with a test.
- **Plan mode + ExitPlanMode approval gate** (M14) — read-only mode, plan presentation, explicit approve flow. omp has no equivalent; cheap given the approval tiers.
- **AskUserQuestion** (M14) — structured mid-task clarification tool (options, multi-select, timeout→recommended) instead of prose questions; xdev's `ask` dialog is the TUI surface.
- **Checkpoint/rewind** (M13, promoted from SKIP-adjacent) — pre-prompt file snapshots + triple restore (code / conversation / both); CC's most-loved feature. Design JSONL turn-boundary markers from M0 so this stays cheap.
- **Think-budget keyword ladder** (M5) — `think` < `think hard` < `ultrathink` → provider reasoning budgets; one constant table.
- **CLAUDE.md as alias** (M10) — AGENTS.md is the standard, but loading `CLAUDE.md` as an alias keeps ecosystem compatibility; `@path` imports, hierarchy, lazy subdirectory loads.
- **Edit-tool semantics refinement** (M3) — CC's exact-string Edit with uniqueness + read-before-edit staleness gates complements the hashline UX; also adopt the benign-exit-1 command list and output-to-file overflow pattern for `bash`.
- **Background bash registry** (M3/M13) — `run_in_background`, `/tasks` listing, timeout→background move with explicit notice, output-file-as-result.

**Verify:**
- RESOLVED by primary-source research (2026-09-09): omp v18 ships a full `todo` tool (9 ops, 5 statuses, transcript-persisted) — the "file-based TODO.md" question is settled. Port omp's todo engine as CORE in M3 (see omp-todos-internals.md); TODO.md survives only as export/import surface in M10.
- Computed context-budget line in the system prompt (CC hardcodes ~90% of window) — inject the real usable-token number instead.
- CC's `auto` permission mode (classifier model reviews every action) — pluggable, off-by-default, only if sandbox integrations demand it.

**Reject (with reasons):**
- Full in-process permission-mode UI — xdev stays YOLO-inside-external-sandbox (pi/omp stance); thin mode concept only.
- LSP/AST/embedding-memory as built-ins — CC itself proves the "thin layer over the model" philosophy (grep/glob/file tools only); extended infrastructure stays opt-in (M13/MCP).
- Multi-agent fan-out without caps — CC's own research post cites ~15× token cost for orchestrator-worker research; keep hub depth/width caps (M11) and compaction budgets.

Net: Claude Code **confirms xdev's pi/omp-shaped minimalism** (it lacks LSP/AST/memory-DB too) while contributing five features omp lacks entirely: microcompact, ExitPlanMode gate, AskUserQuestion, checkpoints/rewind, and the hooks blocking contract.

## 6. Links

- [README.md](../README.md) — project overview.
- [docs/research/2026-09-09-omp-pi-architecture-go-rebuild.md](research/2026-09-09-omp-pi-architecture-go-rebuild.md) — full architecture research and rebuild blueprint (authoritative source for every design point summarized above).
- [docs/research/parity-agent-system.md](research/parity-agent-system.md) — task agents, hub, hooks, advisor, prewalk parity spec.
- [docs/research/parity-session-ux.md](research/parity-session-ux.md) — slash commands, lifecycle, context files, keybindings parity spec.
- [docs/research/parity-knowledge-ui.md](research/parity-knowledge-ui.md) — memory, skills, theme, TUI chrome parity spec.
- [docs/research/parity-tools-providers.md](research/parity-tools-providers.md) — tool inventory, model roles, auth, approval, LSP, plugins parity spec.
- [docs/research/claude-code/cc-bundle-forensics.md](research/claude-code/cc-bundle-forensics.md) — local Claude Code 2.1.263 install forensics (bundle, prompts, session JSONL).
- [docs/research/claude-code/cc-mainprompt.txt](research/claude-code/cc-mainprompt.txt) — verbatim main system prompt + tool descriptions extract (plus cc-compact.txt, cc-enterplan.txt, cc-gitstyle.txt, cc-explore.txt, cc-envvars.txt).
- [docs/research/claude-code/cc-official-docs.md](research/claude-code/cc-official-docs.md) — official-docs study (tools, hooks, subagents, skills, memory, permission modes, checkpoints, headless, MCP, settings, compaction) with per-feature verdicts.
- [docs/research/claude-code/cc-design-decisions.md](research/claude-code/cc-design-decisions.md) — design-decision analysis vs pi/omp minimalism with adopt/verify/reject mapping.
- [docs/research/parity-omp-internals.md](research/parity-omp-internals.md) — omp v18 primary-source: 29+3 tools with verbatim schemas, tool-call mechanics, system-prompt assembly (~12–15k tokens at full slate).
- [docs/research/parity-pi-internals.md](research/parity-pi-internals.md) — pi v0.85 primary-source: 8 built-ins with verbatim TypeBox schemas, measured ~460–510-token prompt, two-layer retry, MCP via proxy-tool pattern only.
- [docs/research/claude-code-internals.md](research/claude-code-internals.md) — Claude Code: session-rename flow (custom/ai-title cascade), inter-session communication (SendMessage/mailbox/InboxPoller/teammate frames), 19-tool inventory, hooks taxonomy.
- [docs/research/opencode-internals.md](research/opencode-internals.md) — opencode v1.18.28: 14 tools, string-replace edit with 9-strategy fuzzy recovery, SQLite parts storage, first-class plan mode, last-match-wins permissions.
- [docs/research/hermes-internals.md](research/hermes-internals.md) — Hermes Agent (Nous Research): 45+ tools, 61,810-char three-tier prompt cache, 8-stage error classifier, fork-on-compression lineage, tool_search progressive disclosure.
- [docs/research/omp-todos-internals.md](research/omp-todos-internals.md) — omp todo deep dive: 9-op contract, 5-status engine, transcript persistence, TodoTracker reminders, `/todo` markdown round-trip; the M3/M10 todo port spec.
- [docs/research/omp-context-resilience.md](research/omp-context-resilience.md) — omp model switching, buildContext/compaction method ladder, API-error taxonomy + failure-mode matrix, resume guarantees; the M5/M9/M10 resilience spec.

---

*Last updated: 2026-09-10 (known gaps documented: /<command> slash commands non-functional — M10 #11; TUI scrolling missing — P0 #17; agent 32-turn hard stop — P0 #18. All user-reported.)*
