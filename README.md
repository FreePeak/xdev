# xdev

**xdev** is a lightweight coding-agent harness in Go: the session/chat core of [pi](https://github.com/earendil-works/pi) and Oh My Pi (omp) — provider streaming, the agent loop, JSONL session persistence, tool execution, and a terminal UI — rebuilt as a **single static, CGO-free binary** (~10–15 MB on disk) with a **hard <100 MB RSS budget**, roughly 3–8× lighter than a JS-runtime harness. It ports the proven pi/omp data model (append-only JSONL session tree, unified stream contract, context reconstruction, compaction, output sinks) while replacing the expensive parts: no JS runtime, no in-process plugin VM, no unbounded queues.

## Philosophy

- **Minimal system prompt.** Under 1,000 tokens including tool descriptions; frontier models are RL-trained to understand coding agents, and 10,000 tokens of instructions are dead weight. Project context comes from the standard `AGENTS.md` hierarchy (global + project), and the full prompt is user-replaceable.
- **Exactly four core tools:** `read`, `write`, `edit`, `bash`. These four are all you need for an effective coding agent — prompt plus tools fits under 1,000 tokens.
- **YOLO by default.** No permission theater. The read-data + execute-code + network trifecta cannot be contained by prompting or pattern rules; containment belongs to external sandboxing (containers, micro-VMs), not the agent. The harness documents sandbox patterns instead of building security theater.
- **Sessions are append-only JSONL trees with a mutable leaf pointer.** Nothing is ever mutated or deleted — branching moves a pointer, context is reconstructed by walking parent links, and the format is inspectable and post-processable with ordinary tools.
- **Bounded everything.** Queues, buffers, session windows, and output sinks are all bounded; backpressure is a feature, not a bug. A runaway turn degrades into "compact now" instead of an OOM kill.

## Status

**Pre-M0 — planning phase. xdev does not exist yet.** There is no binary, no install, and no code; what you see here is the blueprint. Work is tracked in the milestone roadmap below.

- [docs/PRD.md](docs/PRD.md) — product requirements and scope.
- [docs/research/2026-09-09-omp-pi-architecture-go-rebuild.md](docs/research/2026-09-09-omp-pi-architecture-go-rebuild.md) — the full architecture research and rebuild blueprint this project is based on.

## Planned module layout

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
  internal/store/      # pure-Go SQLite: history FTS, stats; blob store in stdlib
  internal/protocol/   # RPC v1/v2 wire types (compatible with omp's where possible)
```

Module path: `github.com/FreePeak/xdev`. Agent data lives under `~/.xdev/agent/`, with a session format compatible with omp's.

## Memory budget

Hard target: worst case **<100 MB RSS** (~30–70 MB estimated in normal use).

| Component | Budget | Technique |
|---|---|---|
| Go runtime + binary + idle GC heap | 8–15 MB | `GOGC` tuning, `debug.SetMemoryLimit` as hard backstop |
| Provider HTTP/2 connections + TLS | 3–8 MB | Shared `http.Client`, connection reuse, streaming bodies |
| Session entries in memory (windowed) | 10–30 MB | Materialize only the post-boundary tail; older entries stay on disk |
| In-flight assistant stream state | 1–3 MB | Only the current message materialized |
| Tool output sinks | <1 MB | Fixed head+tail windows, artifact spill |
| TUI frame buffers | 2–6 MB | Viewport-only frames; terminal owns scrollback |
| JSON decode buffers | 2–5 MB | `json.Decoder` per JSONL line; never unmarshal whole files |
| MCP child servers | 0 (agent RSS) | Child processes; agent holds pipes + tool descriptors only |
| **Total worst case** | **~30–70 MB (est.)** | Enforced via memory limit + bounded structures |

## Roadmap

| Milestone | Scope | Exit criterion |
|---|---|---|
| M0 | Skeleton + config + logger + memory-limit backstop | binary boots <15 MB RSS |
| M1 | Provider layer: 2 adapters (Anthropic + OpenAI Responses), SSE, watchdogs, unified events | streamed chat in `print` mode |
| M2 | Session core: JSONL tree store, buildContext, blob store, listing | resumes an omp-generated session file; context equivalent |
| M3 | Agent loop + 4 tools + OutputSink + env hardening + approvals | real coding tasks end-to-end |
| M4 | TUI: frame-plan renderer, editor, history retirement, resize | daily-drivable interactive mode |
| M5 | Compaction (threshold + overflow + promotion), retry/failover | 200k-token sessions survive |
| M6 | RPC mode (wire-compatible-ish) + subagents + MCP client | embedders can drive it |
| M7 | Ext subprocess protocol + FS-scan cache + AST shell-out | parity with omp daily workflow |
| M8 | Memory hardening audit, fuzzing, cross-platform builds, packaging | <100 MB RSS verified under worst-case transcript |

## Name

The repo and product name is **xdev**. The underlying research document explored the working codename **adze** (a small, sharp woodworking adze: light, precise, fast — it cuts exactly what you point it at); that proposal is superseded by the repository name.
