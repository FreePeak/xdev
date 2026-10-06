# xdev vs omp — Deep Codebase Comparison

*Date: 2026-10-06 · Method: primary-source code reading (xdev Go source) + omp internals research docs (parity-omp-internals.md, 2026-09-09-omp-pi-architecture-go-rebuild.md, parity-pi-internals.md)*

---

## 1. Origin & Relationship

**xdev is a ground-up Go rebuild of omp's session/chat core.** The relationship is explicitly documented in xdev's PRD:

> *"Derived from the omp/pi Go-rebuild architecture research (2026-09-09). That research's proposed codename `adze` is superseded by the repo name `xdev`. The session format stays compatible with omp's."*

omp itself is a fork of [pi](https://github.com/earendil-works/pi) (formerly `badlogic/pi-mono`), a TypeScript coding agent by Mario Zechner. The lineage is:

```
pi (TypeScript, MIT) → omp / Oh My Pi (Bun + Rust N-API, fork) → xdev (Go, Apache-2.0, clean rebuild)
```

xdev ports omp's **proven data model** (JSONL session tree, unified event stream, context reconstruction, compaction, OutputSink, frame-plan TUI) while discarding the expensive parts: no JS runtime, no in-process plugin VM, no Rust N-API natives.

---

## 2. Runtime & Binary Footprint

| Dimension | **omp** | **xdev** |
|---|---|---|
| **Runtime** | Bun v18.1.14 (JIT-ed JS) | Go 1.25 (native, `CGO_ENABLED=0`) |
| **Binary** | `dist/cli.js` under Bun + npm install | Single static binary, ~20–22 MB per platform |
| **Memory** | Unbounded JS heap; `stats.db` = 77 MB, sessions = 1.4 GB on disk | Hard <100 MB RSS (`debug.SetMemoryLimit` backstop, `XDEV_MEMLIMIT` override) |
| **Natives** | Rust N-API layer (`@oh-my-pi/pi-natives`): grep, glob, AST, PTY, shell, diff, tokenizers, Git/Jujutsu, SIXEL, vector ranking | None — Go stdlib + subprocess shell-outs |
| **Cross-platform** | linux, macOS, windows (via Bun) | Same six-platform matrix (amd64 + arm64), each ~20–22 MB |
| **Startup** | JS runtime init + module loading | Instant (native binary) |
| **Supply chain** | npm install with pinned deps, shrinkwrap, `min-release-age=2` | `go build`, `govulncheck` in CI, single-binary releases with SHA-256 checksums |

**Key insight:** xdev's `internal/memlimit/memlimit.go` (99 lines) sets `debug.SetMemoryLimit` as the backstop. Every queue, buffer, cache, session window, and output sink is bounded. A runaway turn degrades into "compact now" instead of an OOM kill. omp has no equivalent — its JS heap grows until the OS kills it.

---

## 3. Source Code Scale

### xdev (Go) — 33 packages, ~105K lines (non-test)

| Package | Lines | Files | Role |
|---|---:|---:|---|
| `internal/tui` | 26,143 | 45 | Terminal UI (tcell, frame-plan, editor, markdown, mermaid) |
| `internal/agent` | 15,285 | 41 | Agent loop, compaction, subagents, hub, task, watchdog, retry, plan mode |
| `internal/tool` | 12,218 | 39 | Tool registry, 4 core tools + extended tools, OutputSink, approval |
| `internal/config` | 8,731 | 22 | Layered settings, credentials, models, approval, keychain, secrets |
| `internal/ai` | 7,351 | 21 | Provider layer, 8 wire adapters, SSE, tool converter, schema normalizer |
| `internal/session` | 3,591 | 13 | JSONL tree store, context reconstruction, blob store, import/export |
| `internal/memory` | 3,023 | 6 | Memory pipeline, hindsight backend, learn tool |
| `internal/lsp` | 2,178 | 7 | LSP client, manager, tool |
| `internal/serve` | 2,110 | 6 | Gateway (broker, relay, tokens, WebSocket) |
| `internal/collab` | 2,001 | 8 | Collaboration (host, guest, link, render, WebSocket) |
| `internal/dist` | 1,993 | 11 | Distribution (install, update, release, version) |
| `internal/browser` | 1,878 | 4 | CDP browser (launch, tool, WebSocket) |
| `internal/dap` | 1,842 | 7 | DAP debugging (client, protocol, session, tool) |
| `internal/theme` | 1,747 | 6 | Theming (palettes, symbols, colorblind, custom) |
| `internal/marketplace` | 1,359 | 5 | Marketplace (discovery, install, manifest, registry) |
| `internal/mcpclient` | 1,256 | 8 | MCP client (config, health, Gemini, server status) |
| `internal/stats` | 1,214 | 3 | Statistics (dashboard, rollup) |
| `internal/eval` | 1,208 | 5 | Eval kernel (persistent Python, jobs, kill) |
| `internal/computer` | 1,176 | 8 | Desktop control (macOS, X11, PowerShell backends) |
| `internal/websearch` | 1,067 | 2 | Web search (22-provider chain) |
| `internal/hooks` | 1,025 | 4 | Hooks (discover, interceptor, trust) |
| `internal/ext` | 953 | 2 | Extension subprocess protocol |
| `internal/share` | 820 | 5 | Sharing (crypto, serve, template) |
| `internal/acp` | 746 | 3 | ACP (framing, server) |
| `internal/imagegen` | 696 | 2 | Image generation (adapters) |
| `internal/tts` | 691 | 3 | Text-to-speech |
| `internal/typesafe` | 523 | 3 | TypeSafe judgment backend |
| `internal/rules` | 460 | 1 | Rule discovery and matching |
| `internal/oauth` | 337 | 1 | OAuth (PKCE S256, refresh, quota rotation) |
| `internal/fscache` | 328 | 1 | Filesystem-scan cache |
| `internal/skills` | 311 | 1 | Skill discovery and loading |
| `internal/rpc` | 201 | 1 | JSONL-over-stdio embedder contract |
| `internal/tiny` | 175 | 3 | Tiny stubs |
| `internal/logx` | 110 | 1 | Logging |
| `internal/protocol` | 108 | 1 | Protocol constants |
| `internal/memlimit` | 99 | 1 | Memory limit backstop |

### omp (TypeScript/Bun + Rust) — packages in npm tarball

| Package | Role |
|---|---|
| `@oh-my-pi/pi-coding-agent` | CLI wiring, session management, AGENTS.md hierarchy, slash commands, themes, editor, HTML export, headless JSON + RPC, OAuth, cost tracking |
| `@oh-my-pi/pi-agent-core` | Agent loop, tool execution, argument validation, event streaming, steering |
| `@oh-my-pi/pi-ai` | Unified multi-provider LLM API (4 wire protocols), streaming, tool calling with TypeBox schemas, thinking blocks, cross-provider context handoff, token/cost tracking |
| `@oh-my-pi/pi-tui` | Minimal terminal UI framework (differential rendering, synchronized output, editor, markdown) |
| `@oh-my-pi/pi-natives` | Rust N-API natives: grep, glob/fd, AST, PTY, shell, diff, tokenizers, Git/Jujutsu, SIXEL, vector ranking |
| `@oh-my-pi/pi-walker` | Shared FS-scan cache (process-local map, TTL 1s, max 16 entries) |
| `@oh-my-pi/pi-wire` | Wire protocol definitions |
| `@oh-my-pi/pi-utils` | Utilities |
| `@oh-my-pi/pi-catalog` | Model catalog (identity, family, capabilities) |
| `@oh-my-pi/hashline` | Hashline edit grammar |
| `@oh-my-pi/omptype` | ArkType-flavored schema validation |

---

## 4. Session & Persistence Layer

### 4.1 On-Disk Format

Both use the **same JSONL format** — this is a deliberate port, not a coincidence:

| Aspect | **omp** | **xdev** |
|---|---|---|
| **File layout** | `~/.omp/agent/sessions/<encoded-cwd>/<timestamp>_<sessionId>.jsonl` | `~/.xdev/agent/sessions/<encoded-cwd>/<timestamp>_<sessionId>.jsonl` |
| **Title slot** | Fixed-width 256-byte slot on line 1 | Same — `TitleSlotWidth` constant, `MarshalTitleSlot` pads |
| **Session header** | `type:"session"` with id, timestamp, cwd, title, parentSession | Same structure |
| **Entry model** | `{type, id (8-char), parentId, timestamp}` — append-only tree + mutable leaf pointer | Identical — `Envelope` struct in `entries.go:24-29` |
| **Entry types** | `message`, `thinking_level_change`, `model_change`, `service_tier_change`, `compaction`, `branch_summary`, `reset_boundary`, `custom` | Same + xdev additions: `goal_updated`, `checkpoint`, `schedule_change`, `branch` |
| **Blob store** | `~/.omp/agent/blobs/<sha256>` content-addressed | Same — `internal/session/blob.go` |
| **Import/export** | — | `xdev -from-claude` / `-from-codex` import transcripts from other harnesses |

### 4.2 Context Reconstruction

Both implement `buildContext` with identical semantics:

| Step | **omp** | **xdev** |
|---|---|---|
| Walk | `parentId` from leaf to root, cycle-bounded | Same — `context.go:29-46` |
| Reset boundary | Latest `reset_boundary` drops everything before | Same |
| Compaction | Summary message + entries after `firstKeptEntryId` | Same |
| Model resolution | Latest `model_change` in path wins | Same |
| Dangling tool calls | Neutralized (dropped from assistant message) | Same — plus `UnansweredToolCallNotice` for crash recovery |
| Branch summaries | Converted to user message | Same |

### 4.3 Durability Model

| Aspect | **omp** | **xdev** |
|---|---|---|
| **Append** | Synchronous in-memory + writer hand-off, no fsync | Same — `bufio.Writer + Flush` per append, `Options.StrictFsync` upgrades |
| **New sessions** | Memory-only until first assistant message or `ensureOnDisk()` | Same — `EnableAutoPersist` |
| **Persistence errors** | Latched and rethrown on later flush/close | Same — `Store` latches errors |
| **Rewrites** | Atomic stage-then-rename with EPERM-safe fallback, commit guard | Same discipline |
| **Indeterminate writes** | Fails closed (`SessionPersistenceIndeterminateError`) | Same — `RepairNotice()` reports what was discarded |
| **Crash forensics** | `session_exit` + `tool_execution_start` markers, synthetic aborted message | Same — `UnansweredToolCallNotice` + `ReplaySafety` func |
| **Size controls** | >500k-char strings truncated, images externalized to blob store, >8 MiB files load via streaming JSONL | Same — `maybeWindow()` drops entries before latest boundary |

### 4.4 Session Listing

| Aspect | **omp** | **xdev** |
|---|---|---|
| **Discovery** | 4 KiB prefix reads + bounded 32 KiB tail for lifecycle status, stat-keyed cached, parallel-scanned | Same — `listing.go` |
| **Unknown entry types** | — | `UnknownEntry` chain nodes keep parent chain walkable |

---

## 5. Agent Loop & Tool Execution

### 5.1 Agent Loop

| Aspect | **omp** | **xdev** |
|---|---|---|
| **Loop location** | `pi-agent-core/src/agent-loop.ts` | `internal/agent/loop.go` (~2,178 lines) |
| **Turn model** | One goroutine per turn, bounded tool worker pool | Same — `Agent.Run()` with `TurnHooks` interface |
| **Steering** | `steer`/`followUp`/`aside` channels, queued messages preserved | Same — `TurnHooks` interface with `OnStart`, `OnEvent`, `OnToolStart`, `OnToolEnd`, `OnMessageEnd`, `OnTurnEnd` |
| **Max turns** | Configurable | `DefaultMaxTurns = 200`, `MaxTurns=0` means unbounded |
| **Token budget** | — | Per-turn: `DefaultTurnTokenBudget = 5,000,000` (wrap-up prompt, session continues) |
| **Empty turn handling** | — | `EmptyTurnNudgePrompt` injected, `ErrEmptyTurn` after nudges exhausted |
| **Prompt continuation** | — | `PromptContinuationPrompt` — keeps interactive runs going without `/goal` |
| **Compaction** | 6 trigger paths incl. provider-native streaming compaction | 7 files: `compact.go`, `compact_async.go`, `compact_branch.go`, `compact_cli.go`, `compact_elide.go`, `compact_ladder.go`, `compact_snap.go` |
| **Retry/failover** | Auto-retry with backoff + provider failover + context promotion | `retry.go`, `failover.go`, `fallback_chain.go`, `fallback_recovery.go` |
| **Watchdog** | Per-delta constraint steering | `watchdog.go` + `advisor.go` |
| **Plan mode** | — | `planmode.go` |
| **Prewalk** | One-shot big→session-model handoff | `prewalk.go` |
| **Goal** | — | `goal.go` — session-scoped goal state |
| **Vibe** | — | `vibe.go` — vibe coding mode |
| **Keywords** | `ultrathink`/`orchestrate`/`workflowz` | `keywords.go` |
| **Offload** | — | `offload.go` |
| **Handoff** | — | `handoff.go` — session replaces live context with handoff document |
| **Schedule** | — | `schedule.go` — session-local reminders |
| **TTSR** | — | `ttsr.go` — TTS read-back merge |

### 5.2 Tool System

| Aspect | **omp** | **xdev** |
|---|---|---|
| **Builtin tools** | **29 builtins** + 3 hidden (`yield`, `goal`, `think`) | **4 core** (`read`, `write`, `edit`, `bash`) + extended tools registered but progressively disclosed |
| **Tool registry** | `BUILTIN_TOOLS: Record<BuiltinToolName, ToolFactory>` | `Registry` struct in `tool.go:52` — `tools map[string]Tool`, `snaps map[string]*fileSnapshot` |
| **Schema visibility** | All tools visible to model | Progressive disclosure: one-line index → `tool_search` → `tool_describe` → `tool_call` |
| **Edit modes** | 5 modes: `replace`, `patch`, `hashline`, `apply_patch`, `sloppy` | Hashline only (omp's hashline grammar ported) |
| **Edit schema** | Per-model variant resolution: `settings.getEditVariantForModel(model)` → env → setting → hashline default → per-model sloppy downgrade | Hashline with snapshot tags |
| **Bash** | `CRITICAL_BASH_PATTERNS` (172-227): `rm -rf /`, fork bombs, `mkfs`, `dd`, `shred`, `curl\|wget \| sh`, etc. | `bash.go` + `bashbg.go` (background execution) + `env.go` (env hardening) |
| **Grep caps** | 20 files × 20 matches / 200 single-file / 2000 total / 4 MiB native window / 30 s timeout | `search.go` — shells out to `rg` when present |
| **Glob caps** | 200-result cap + 5 s timeout | `search.go` — shells out to `fd` when present |
| **Approval** | `always-ask\|write\|yolo` + per-tool approval + `bash.patterns` | `approval.go` + `policy.go` — same tier model |
| **Output sink** | Fixed 32 KiB head + 32 KiB tail windows with truncation marker | `sink.go` — same |
| **File freshness** | — | `snapshot.go` — content hash + exact line text last shown to model |
| **Arg repair** | 7 repair passes with `__parseError` 512-char truncation | `argcoerce.go` |
| **Tool result** | `coerceToolResult` rejects malformed content arrays | `outcome.go` + `verify.go` |

### 5.3 Extended Tools (omp's 29 builtins vs xdev's approach)

| omp builtin | xdev equivalent | Status |
|---|---|---|
| `read`, `write`, `edit`, `bash` | `tool/read.go`, `tool/write.go`, `tool/edit.go`, `tool/bash.go` | **Ported** |
| `grep`, `glob` | `tool/search.go` (shells out to `rg`/`fd`) | **Ported** |
| `ast_grep`, `ast_edit` | `tool/ast.go` (shells out to `ast-grep` binary) | **Ported** |
| `lsp` | `internal/lsp/` (7 files, 2,178 lines) | **Ported** |
| `debug` (DAP) | `internal/dap/` (7 files, 1,842 lines) | **Ported** |
| `browser` (CDP) | `internal/browser/` (4 files, 1,878 lines) | **Ported** |
| `computer` | `internal/computer/` (8 files, 1,176 lines) | **Ported** |
| `eval` | `internal/eval/` (5 files, 1,208 lines) | **Ported** |
| `github` | `tool/github.go` + `tool/github_uri.go` | **Ported** |
| `web_search` | `internal/websearch/` (2 files, 1,067 lines) | **Ported** |
| `security_scan` | `tool/securityscan.go` | **Ported** |
| `checkpoint`, `rewind` | `tool/checkpoint.go` | **Ported** |
| `todo` | `tool/todo.go` + `tool/todo_session.go` | **Ported** |
| `ask` | `tool/ask.go` | **Ported** |
| `task` | `internal/agent/task.go` + `subagent.go` | **Ported** |
| `hub` | `internal/agent/hub.go` + `hubtool.go` + `hubnotice.go` | **Ported** |
| `memory_edit`, `retain`, `recall`, `reflect` | `internal/memory/` (6 files, 3,023 lines) | **Ported** |
| `learn`, `manage_skill` | `internal/memory/learn.go` + `internal/skills/skills.go` | **Ported** |
| `imagegen` | `internal/imagegen/` (2 files, 696 lines) | **Ported** |
| `tts` | `internal/tts/` (3 files, 691 lines) | **Ported** |
| `typesafe` | `internal/typesafe/` (3 files, 523 lines) | **Ported** |
| `inspect_image` | — | Not ported |
| `yield`, `goal`, `think` (hidden) | `internal/agent/goal.go` + `vibe.go` | **Partially ported** |

---

## 6. Provider / Wire Layer

| Aspect | **omp** | **xdev** |
|---|---|---|
| **Wire protocols** | 4 core: Anthropic Messages, OpenAI Completions, OpenAI Responses, Google GenAI | **8 adapters**: + Azure OpenAI Responses, OpenAI Codex Responses, Google Vertex, Gemini CLI |
| **SSE reader** | Hand-rolled over `net/http` | `internal/ai/sse/sse.go` + `partialjson.go` — 256-byte partial-JSON throttle with relaxed repair parser |
| **Unified events** | `AssistantMessageEvent` stream contract | Same — `ai/events.go` |
| **Tool calling** | TypeBox schemas | `internal/ai/toolconv.go` + `schema_normalize.go` |
| **Thinking blocks** | Cross-provider context handoff (Anthropic thinking → `<thinking>` text for OpenAI) | Same — `toolconv.go` |
| **Watchdogs** | First-progress + idle | `internal/ai/watchdog.go` |
| **Cache** | — | `internal/ai/cache.go` — prompt-cache identity per request |
| **Health check** | — | `HealthChecker` interface + `HealthCheckProvider` with custom probe URL |
| **Provider failover** | Auto-retry with backoff | `internal/agent/failover.go` + `fallback_chain.go` + `fallback_recovery.go` |
| **Model resolution** | `defaultModel` + `:effort` suffix + `modelRoles` alias table | `defaultModel` + `:effort` suffix (alias table removed 2026-09-17) |
| **Config** | `models.yml` per-provider | Same — `internal/config/models.go` |
| **Auth** | CLI → models.yml → OAuth → /login → env + `.env` layering | Same — `internal/config/credentials.go` + `oauth.go` |
| **OAuth** | Claude Pro/Max + Codex (PKCE S256, refresh, quota rotation) | Same — `internal/oauth/oauth.go` (337 lines) |
| **Keychain** | — | `internal/config/keychain.go` — reads macOS Keychain, never writes |
| **Install identity** | — | `SetInstallIdentity` — per-install UUID for Claude `metadata.user_id` / Codex installation id |

---

## 7. Extension / Plugin Architecture

This is the **most fundamental architectural difference**:

| Aspect | **omp** | **xdev** |
|---|---|---|
| **Model** | In-process TypeScript extension system | **Subprocesses speaking JSONL on stdio** |
| **Foreign code** | Loaded in-process (plugin VM) | **Never loaded in-process** |
| **Isolation** | None — extensions share the JS heap | Full process isolation — one bad extension can't crash the agent |
| **Timeout** | — | Per-event timeout (5 s default) + SIGKILL |
| **Handshake** | — | Capability exchange: tools, commands, event subscriptions, declarative renderers |
| **Event flow** | Direct function calls | `Frame` struct: `hello` → `capabilities` → `event` → `response`/`action`/`log` |
| **Policy** | — | `tool_call` may block/revise **fail-closed**; `tool_result` may patch |
| **Runtime actions** | — | `steer`, `followUp`, `aside`, `register_provider` |
| **Code size** | — | `internal/ext/ext.go` — 953 lines, 2 files |

**Why it matters:** omp's in-process plugin VM means a buggy extension can corrupt the agent's heap, cause OOM kills, or introduce security vulnerabilities. xdev's subprocess model means a hung extension is killable — the one thing an in-process plugin VM cannot offer. The tradeoff is IPC overhead (JSON serialization per event), but for a coding agent where tool calls are already network-bound, this is negligible.

---

## 8. TUI & User Experience

| Aspect | **omp** | **xdev** |
|---|---|---|
| **TUI framework** | `pi-tui` (TypeScript) — differential rendering, synchronized output | `tcell/v2` (Go) — frame-plan port from omp |
| **Frame plan** | `TerminalFramePlan { history?, viewport }`, active/settled/committed block states, history retirement batches, DSR-anchor resize recovery | Same — ported with identical semantics |
| **Editor** | pi-tui editor: path completion, bracketed paste, multi-line, IME-safe cursor tail | `internal/tui/editor.go` — same feature list |
| **Markdown** | Minimal rendering | `internal/tui/markdown.go` + `mermaid.go` |
| **Themes** | Live reload, dark/light auto | `internal/theme/` (6 files, 1,747 lines) — 66-token JSON themes, palettes, colorblind, symbols |
| **Status line** | — | `internal/tui/statuspill.go` — model, session, tokens (in/out/cache/think), cost, context window, decode speed, TTFT |
| **Tab strip** | — | `internal/tui/tabstrip.go` — multi-session tabs with status glyphs or numbers |
| **Hub roster** | — | `internal/tui/hubroster.go` — agent status/model/activity/cost |
| **Diff view** | — | `internal/tui/diff.go` + `diff_split.go` |
| **Welcome screen** | — | `internal/tui/welcome.go` |
| **Ask overlay** | — | `internal/tui/askoverlay.go` |
| **Tool highlights** | — | `internal/tui/toolhl.go` |
| **Trajectory** | — | `internal/tui/trajectory.go` |
| **Tree selector** | — | `internal/tui/tree.go` |
| **Message menu** | — | `internal/tui/msgmenu.go` |
| **Sticky scroll** | — | `internal/tui/sticky.go` |
| **Paste** | — | `internal/tui/paste.go` |
| **Selection** | — | `internal/tui/selection.go` |
| **Clipboard** | — | `internal/tui/clipboard.go` + `clipboard_image.go` |
| **TUI size** | `pi-tui` package (smaller) | **26,143 lines across 45 files** — the largest package in xdev |

---

## 9. Config & Credential System

| Aspect | **omp** | **xdev** |
|---|---|---|
| **Layering** | defaults ← global ← project ← CLI overlays | Same — `schema defaults ← ~/.xdev/agent/config.yml ← .xdev/config.yml ← --config overlays` |
| **Merge semantics** | Objects deep-merge; scalars and arrays replaced wholesale | Same — `settings.go:26-32` |
| **Unknown keys** | — | Rejected (typo reported, not ignored) |
| **Malformed files** | — | Preserved as `.broken-<stamp>`, load fails loudly |
| **Credentials** | `agent.db` (SQLite) | `credentials.json` (0600, re-verified on every read: mode + nlink == 1) |
| **Keychain** | — | `keychain.go` — reads macOS Keychain, never writes; unsatisfiable reference is an error |
| **Secrets** | — | `secrets.go` — reversible `$$HASH$$` placeholders |
| **Env framework** | process env → project `.env` → agent `.env` | Same — `internal/config/env.go` |
| **Project safety** | — | `reposafe.go` — repository is not a configuration authority |
| **Private files** | — | `privatefile_unix.go` + `privatefile_windows.go` |
| **Config CLI** | — | `xdev config list|get|path|set|reset` |
| **Settings size** | — | `settings.go` — 2,300+ lines, 22 files |

---

## 10. Subagent & Hub Architecture

| Aspect | **omp** | **xdev** |
|---|---|---|
| **Subagent model** | Same session core, restricted tool sets, structured `outputSchema`, `yield` tool, artifact handoff | Same — `internal/agent/subagent.go` |
| **Fork vs fresh** | Context-inheriting fork vs fresh agent | Same |
| **Stall detection** | With retry escalation | Same |
| **Hub** | Agent Hub TUI roster (status/model/activity/cost, steer, kill) | `internal/agent/hub.go` + `hubtool.go` + `hubnotice.go` + `internal/tui/hubroster.go` |
| **Task agents** | Markdown+frontmatter discovery, first-wins merge, spawn policy, depth guard | `internal/agent/discovery.go` + `catalog.go` + `task.go` |
| **Agent chain** | — | `internal/agent/chain.go` |
| **Mailbox** | — | `internal/agent/mailbox.go` |
| **Proc group** | — | `internal/agent/proc.go` + `proc_group_unix.go` + `proc_group_windows.go` |
| **Schedule** | — | `internal/agent/schedule.go` |

---

## 11. Memory & Skill Systems

| Aspect | **omp** | **xdev** |
|---|---|---|
| **Memory backends** | `hindsight` (bank over HTTP), `mnemopi` (SQLite), `sharpshooter` (friction-gated decision files) | `hindsight` only (mnemopi removed 2026-09-19, sharpshooter deferred to M15) |
| **Memory pipeline** | Extraction → session-model consolidation → `MEMORY.md` + `learned.md` | `internal/memory/pipeline.go` + `memory.go` + `hindsight.go` + `hindsight_tools.go` |
| **Memory tools** | `memory_edit`, `retain`, `recall`, `reflect` | Same — `internal/memory/hindsight_tools.go` |
| **Learn tool** | Dynamic approval (write iff skill payload / local backend) | `internal/memory/learn.go` |
| **Skills** | `SKILL.md` discovery, `skill://` protocol, `/skill:` commands, managed skills | `internal/skills/skills.go` (311 lines) |
| **Skill caps** | 64 KB cap, name regex, description sanitizer, symlink refusal, authored-shadow guard | Same |
| **Rules** | `RULES.md`, `.cursor/rules/*.mdc`, `.windsurf/rules`, `.clinerules`, `.agent/rules`, Copilot instructions | `internal/rules/rules.go` (460 lines) |
| **Context files** | `AGENTS.md` hierarchy (global → project), `@path` imports, 32 KB cap | Same — `internal/agent/prompt.go` |
| **Recall** | Ranked against last user turn only, cached 60 s, capped at 1024 tokens, labelled "not authoritative" | Same |

---

## 12. RPC & Embedder Contract

| Aspect | **omp** | **xdev** |
|---|---|---|
| **RPC mode** | JSONL-over-stdio | Same — `internal/rpc/server.go` (201 lines) |
| **Ready frame** | Advertises protocol versions + frame limits | Same — `protocol.Ready{Protocol, FrameLimit}` |
| **Frame limit** | 1 MiB; v2 chunked reassembly to 64 MiB | Same — `protocol.FrameLimit` |
| **Commands** | `prompt`/`steer`/`follow_up`/`abort`/`new_session`/`state`/`set_model`/... | Same — `Handler` interface |
| **Inbound correlation** | Id-tagged commands with correlated responses | Same |
| **Dispatch** | — | Bounded goroutine pool (`maxInflightDispatch = 8`), replies never block read loop |

---

## 13. Additional xdev Subsystems (No omp Equivalent)

| Package | Lines | Role |
|---|---:|---|
| `internal/serve` | 2,110 | Gateway (broker, relay, tokens, WebSocket) — for embedding xdev as a service |
| `internal/collab` | 2,001 | Collaboration (host, guest, link, render, WebSocket) — multi-agent collaboration |
| `internal/dist` | 1,993 | Distribution (install, update, release, version) — self-updating binary |
| `internal/share` | 820 | Sharing (crypto, serve, template) — E2E-encrypted session sharing |
| `internal/acp` | 746 | ACP (Agent Communication Protocol) — framing, server |
| `internal/marketplace` | 1,359 | Marketplace (discovery, install, manifest, registry) |
| `internal/fscache` | 328 | Filesystem-scan cache — the one omp concept worth porting from Rust |
| `internal/typesafe` | 523 | TypeSafe judgment backend (System One over HTTP) |

---

## 14. Pros & Cons per Tool

### omp — Pros

| Pro | Detail |
|---|---|
| **Full-featured out of the box** | 29 builtin tools covering the entire coding workflow — no shell-outs needed |
| **Rust N-API natives** | grep, glob, AST, PTY, diff, tokenizers, Git/Jujutsu all run in-process at native speed |
| **In-process extensions** | Zero IPC overhead for plugins; direct function calls; shared heap for fast data exchange |
| **Mature ecosystem** | npm distribution, marketplace, sharpshooter memory, friction-gated decision files |
| **Sidecar SQLite** | FTS5 prompt history, fast search, structured stats (77 MB stats.db) |
| **5 edit modes** | `replace`, `patch`, `hashline`, `apply_patch`, `sloppy` with per-model variant resolution |
| **Provider-native streaming compaction** | 6 trigger paths including provider-native streaming compaction |
| **Supply-chain hardening** | Pinned exact deps, `min-release-age=2`, shrinkwrap, lifecycle-script allowlist |

### omp — Cons

| Con | Detail |
|---|---|
| **JS runtime overhead** | Bun JIT baseline + GC spikes; no hard memory budget |
| **Unbounded memory** | JS heap grows until OOM kill; `stats.db` = 77 MB, sessions = 1.4 GB |
| **In-process plugin risk** | A buggy extension can corrupt the heap, cause OOM kills, or introduce security vulnerabilities |
| **No process isolation** | Extensions share the JS heap — one bad plugin can crash the entire agent |
| **Slow startup** | JS runtime init + module loading before the agent is ready |
| **npm supply chain** | Dependency install at runtime; supply-chain attack surface |
| **4 wire protocols** | Fewer provider options than xdev's 8 |
| **Permission prompts** | `always-ask\|write\|yolo` with per-tool approval — friction for autonomous runs |
| **No hard resource bounds** | Queues, buffers, session windows are unbounded |

### xdev — Pros

| Pro | Detail |
|---|---|
| **Single static binary** | ~20–22 MB per platform, CGO-free, cross-compiles to 6 platforms |
| **Hard <100 MB RSS budget** | `debug.SetMemoryLimit` backstop; every queue, buffer, cache, session window bounded |
| **Instant startup** | Native binary — no JS runtime init |
| **Process isolation for extensions** | Subprocess JSONL with per-event timeout + SIGKILL; a hung extension is killable |
| **8 wire adapters** | Anthropic, OpenAI Completions, OpenAI Responses, Google GenAI, Azure, Codex, Vertex, Gemini CLI |
| **Self-updating binary** | SHA-256 verified releases, `xdev update` with atomic rename |
| **YOLO by default** | No permission theater; containment belongs to external sandboxing |
| **Minimal prompt** | <1,000 tokens including tool descriptions; progressive tool disclosure |
| **Bounded everything** | Backpressure is a feature; a runaway turn degrades into "compact now" |
| **Inspectable sessions** | Full user control over what enters model context; post-processable JSONL |
| **Supply-chain hardening** | `govulncheck` in CI, single-binary releases with SHA-256 checksums |
| **Keychain integration** | Reads macOS Keychain, never writes; unsatisfiable reference is an error |
| **Session import** | `-from-claude` / `-from-codex` import transcripts from other harnesses |

### xdev — Cons

| Con | Detail |
|---|---|
| **4 core tools** | vs omp's 29 builtins — extended tools require shell-outs or subprocesses |
| **Subprocess IPC overhead** | JSON serialization per event for extensions (negligible for network-bound tool calls) |
| **No Rust natives** | grep/glob/AST shell out to `rg`/`fd`/`ast-grep` binaries when present |
| **Hashline-only edit** | One edit mode vs omp's 5 modes with per-model variant resolution |
| **No sidecar SQLite** | Tiny rollup table only; no FTS5 prompt history |
| **Fewer memory backends** | `hindsight` only; mnemopi removed, sharpshooter deferred to M15 |
| **Smaller TUI ecosystem** | 26K lines vs omp's mature pi-tui; some features deferred |
| **Deferred features** | Marketplace, sharpshooter, embedded local models, desktop control deferred to M15 |
| **No provider-native streaming compaction** | 7 compaction files but no provider-native streaming compaction |
| **Younger project** | Less battle-tested than omp's mature codebase |

---

## 15. Design Philosophy Comparison

### omp's Philosophy

1. **Full-featured out of the box** — 29 builtin tools, in-process extensions, Rust natives for performance
2. **JS runtime as foundation** — Bun-first, TypeScript everywhere, Rust N-API for heavy lifting
3. **In-process everything** — extensions, plugins, and tools all share one process
4. **Sidecar databases** — SQLite for auth, history, models, stats (77 MB stats.db, 1.4 GB sessions)
5. **Permission prompts** — `always-ask|write|yolo` with per-tool approval

### xdev's Philosophy

1. **Minimal prompt** — <1,000 tokens including tool descriptions; frontier models are RL-trained to act as coding agents
2. **Four core tools** — `read`, `write`, `edit`, `bash` are the job; everything else has to earn its place
3. **YOLO by default** — no permission theater; containment belongs to external sandboxing
4. **Extensions are processes** — never load foreign code in-process; subprocess JSONL with per-event timeout + SIGKILL
5. **Bounded everything** — queues, buffers, session windows, output sinks have hard limits; backpressure is a feature
6. **Inspectable sessions** — full user control over what enters model context; post-processable JSONL
7. **One static binary** — no JS runtime, no in-process plugin VM, no install-time toolchain

---

## 16. Summary: The Tradeoff

| What you gain with xdev | What you lose |
|---|---|
| 22 MB binary vs JS runtime + npm install | 29 builtin tools → 4 core + progressive disclosure |
| <100 MB RSS hard budget vs unbounded JS heap | In-process extensions → subprocess IPC overhead |
| Instant startup vs JS runtime init | Rust natives → Go stdlib + shell-outs |
| Process isolation for extensions | Sidecar SQLite databases → tiny rollup table only |
| 8 wire adapters vs 4 | Some omp ecosystem services (marketplace, sharpshooter) deferred |
| Self-updating binary with SHA-256 verification | 5 edit modes → hashline only |
| YOLO by default vs permission prompts | Provider-native streaming compaction |
| Minimal prompt (<1,000 tokens) | |
| Bounded everything | |

**The bottom line:** xdev is omp's session core, agent loop, and tool philosophy rebuilt as a single static Go binary with a hard resource budget. It trades omp's breadth of built-in features and in-process extensibility for lightweightness, process isolation, and bounded everything. The feature-parity matrix in xdev's PRD tracks the gap: CORE features are ported or in progress, NICE features are demand-driven, and a small set (marketplace, sharpshooter, embedded local models) is explicitly deferred to M15.
