<p align="center">
  <a href="https://github.com/FreePeak/xdev/actions/workflows/ci.yml"><img src="https://github.com/FreePeak/xdev/actions/workflows/ci.yml/badge.svg" alt="CI gate"></a>
  <a href="https://github.com/FreePeak/xdev/releases"><img src="https://img.shields.io/github/v/release/FreePeak/xdev?include_prereleases&label=release" alt="release"></a>
  <a href="./LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue" alt="license: Apache-2.0"></a>
  <a href="https://go.dev"><img src="https://img.shields.io/badge/Go-1.25-00ADD8" alt="Go 1.25"></a>
  <a href="./CONTRIBUTING.md"><img src="https://img.shields.io/badge/platforms-linux%20%C2%B7%20macos%20%C2%B7%20windows-blue" alt="linux, macos, windows"></a>
</p>

**xdev** is a lightweight coding-agent harness in Go: the session/chat core of [pi](https://github.com/earendil-works/pi) and Oh My Pi (omp) — provider streaming, the agent loop, JSONL session persistence, tool execution, and a terminal UI — rebuilt as a **single static, CGO-free binary** (~22 MB dev / ~29 MB installed with the SQLite-backed mnemopi memory backend linked in; a build without it is smaller) with a **hard <100 MB RSS budget**, roughly 3–8× lighter than a JS-runtime harness. It ports the proven pi/omp data model (append-only JSONL session tree, unified stream contract, context reconstruction, compaction, output sinks) while replacing the expensive parts: no JS runtime, no in-process plugin VM, no unbounded queues.

## Philosophy

- **Minimal system prompt.** Under 1,000 tokens including tool descriptions; frontier models are RL-trained to understand coding agents, and 10,000 tokens of instructions are dead weight. Project context comes from the standard `AGENTS.md` hierarchy (global + project), and the full prompt is user-replaceable.
- **Exactly four core tools:** `read`, `write`, `edit`, `bash`. These four are all you need for an effective coding agent — prompt plus tools fits under 1,000 tokens.
- **YOLO by default.** No permission theater. The read-data + execute-code + network trifecta cannot be contained by prompting or pattern rules; containment belongs to external sandboxing (containers, micro-VMs), not the agent. The harness documents sandbox patterns instead of building security theater.
- **Sessions are append-only JSONL trees with a mutable leaf pointer.** Nothing is ever mutated or deleted — branching moves a pointer, context is reconstructed by walking parent links, and the format is inspectable and post-processable with ordinary tools.
- **Bounded everything.** Queues, buffers, session windows, and output sinks are all bounded; backpressure is a feature, not a bug. A runaway turn degrades into "compact now" instead of an OOM kill.
- **Full-parity ambition.** Everything omp does daily — model roles, slash/custom commands, AGENTS.md hierarchy, subagents + hub, advisor watchdog, prewalk, memory, skills, themes/color — rebuilt with the same bounded-memory discipline.

## Status

**M5 through M15 landed (2026-09-12).** `xdev tui` is daily-drivable (streaming markdown-aware output, tool-call blocks, dimmed thinking, 66-token themes with box/spinner/HUD controls, session picker + tree selector, ask overlay) and the conversation survives turns, resumptions, forks, imports and compactions. The harness now carries the full parity surface: model roles + four wire transports + OAuth login, settings layering with `xdev config`, an approval policy with `bash.patterns`/compound handling and a fail-closed interceptor hook, a compaction method ladder (`threshold`/`overflow`/`promotion`/`snapcompact`/`shake`/`soft`/`handoff`) with `/handoff`, retry fallback chains with credential rotation, TTSR stream rules, goal mode, plan mode with `ask` + `xd://` devices, subagents with the hub roster (park/revive, process supervision), a cross-session mailbox, hooks, advisor/watchdog, prewalk, memory (local two-phase pipeline, mnemopi, Hindsight, sharpshooter), skills (`/skill:`, managed skills, `learn`), rules/rulebooks, context notes, and the extended tool set (eval kernel, web_search, github, lsp, checkpoint/rewind, browser, marketplace, deferred catalog, secrets redaction, MCP extensions, security_scan, computer, DAP, tts, generate_image). v2 modes are in: vibe, collab (`xdev join`), ACP, `/export`+`/share`, profiles/XDG, and the distribution surface (`update`/`setup`/`bench`). Verified on the integration branch: `go build ./...`, `go vet ./...`, gofmt clean, six-target CGO-free cross-builds, and the full `go test -count=1 ./...` suite green.

- [docs/PRD.md](docs/PRD.md) — product requirements and scope.
- [docs/research/2026-09-09-omp-pi-architecture-go-rebuild.md](docs/research/2026-09-09-omp-pi-architecture-go-rebuild.md) — the full architecture research and rebuild blueprint this project is based on.

## Quickstart (print mode)

```bash
go build -o xdev ./cmd/xdev

# ~/.xdev/agent/models.yml — OpenAI-compatible gateway (env-expanded):
# providers:
#   onegw:
#     baseUrl: http://127.0.0.1:8080/v1
#     apiKey: ${ONEGW_KEY}         # macOS alternative: keychain:dev.xdev.credential.onegw
#     api: openai-completions
#     models: [{ id: free, name: Free, contextWindow: 1000000 }]
# defaultModel: onegw/free

xdev "create a hello.py that prints hello world, then run it"
xdev -continue "now add tests"   # resume the latest session in this cwd
xdev -resume 01a0 "pick up where we left off"   # resume by session-id prefix
xdev -model onegw/dev "..."      # explicit provider/model
xdev tui                        # interactive mode (Grok-CLI look)
xdev tui -theme grokday         # light variant (default: auto)
# hard RSS backstop defaults to 100MB; set XDEV_MEMLIMIT to override
```

### Where secrets live

`~/.xdev/agent/credentials.json` (what `/login` stores) is written `0600` and
**re-verified on every read** — mode `0600` and `nlink == 1`, because a
write-time chmod says nothing about the file now. A file that fails either check
is refused with the repair named, never silently trusted. The data directory is
tightened to `0700` when a credential is saved.

To keep a long-lived token off disk entirely on macOS, store it in the Keychain
yourself (Keychain Access, `1password --read`, `op read`, or your own `security
add-generic-password -w` call) and point a provider at it:

```yaml
providers:
  openai:
    apiKey: keychain:dev.xdev.credential.openai/linh   # service[/account]
    api: openai-completions
```

xdev **reads** such an item and never writes one: `/usr/bin/security` accepts a
secret either as an argument (visible in `ps` to every user on the machine) or
through an interactive prompt that silently truncates at 128 bytes — enough for
an API key, not for a JWT. A `keychain:` reference that cannot be satisfied is an
error naming the reference; xdev will not quietly use a different credential.
`XDEV_DISABLE_KEYCHAIN=1` turns the lookup off. [Why, with measurements](docs/decisions/keychain-credential-source.md).

## Install (Linux / macOS)

One command — fetches the newest release for your platform, verifies its
SHA-256, and installs to `~/.local/bin`. Re-running it auto-updates the
installed binary to the newest release.

```bash
curl -fsSL https://raw.githubusercontent.com/FreePeak/xdev/main/scripts/install.sh | sh
```

Variables for power users (same names as `xdev update`):
`XDEV_UPDATE_REPO`, `XDEV_UPDATE_API`, `XDEV_INSTALL_DIR`.

## Onboarding, updates, benchmarks

```bash
xdev setup                           # data dir + starter config.yml + next steps
xdev update --check                  # is a newer release published for the channel?
xdev update                          # verify SHA256SUMS, then replace this binary
xdev update --channel canary         # pre-release tags (v0.2.0-canary.1)
xdev bench --turns 5 --model @smol   # TTFT + decode p50/p95 for the configured provider
```

`xdev update` resolves the channel's newest release from GitHub releases
(`XDEV_UPDATE_REPO`, default `FreePeak/xdev`; `XDEV_UPDATE_API` for a mirror),
refuses a release that carries no `SHA256SUMS` entry, refuses when the running
binary is not writable (printing the exact `chmod u+w <path>` hint), and
installs by writing a temp file next to the target and renaming — an
interrupted update never leaves a half-written binary. `--check` only reports.

`xdev setup` is the non-interactive onboarding pass: it creates
`~/.xdev/agent/` (plus `sessions/`, `agents/`, `commands/`, `themes/`), writes
a starter `config.yml` only when none exists, then prints the `models.yml`
template and which optional external tools (git, rg, fd, ast-grep, gh,
python3) it found. Re-running it never overwrites an existing file.

Release binaries are Developer ID-signed and notarized in CI when the
`APPLE_*` repository secrets are configured, and ship ad-hoc signed when they
are absent — a release never fails for want of credentials. See
`.github/workflows/ci.yml` (job `macos-sign`) for the five secret names and how
to produce them.

## Interactive mode

`xdev tui` is the Grok-CLI-styled interactive mode (GrokNight/GrokDay themes).

**Scrollback** — the transcript is fully scrollable; streaming output never
drags a scrolled viewport (the `▲ n ▼ n` indicator shows hidden rows):

| Keys | Action |
|---|---|
| PgUp / PgDn | half-page up / down |
| Ctrl+B / Ctrl+F | half-page up / down (emacs-style) |
| ↑ / ↓ | recall the previous / next prompt (once history exists) |
| Shift+↑ / Shift+↓ | line up / down |
| Home / End | jump to top / back to live |

**Slash commands** — dispatched at input-submit, never sent to the model:

| Command | Action |
|---|---|
| `/new` | fresh session file + cleared transcript |
| `/clear` | reset context in place (durable `reset_boundary`; history kept on disk) |
| `/drop` | delete the session file and start fresh |
| `/rename <title>` | title this session (a manual title beats the generated one) |
| `/resume [id]`, `/fork`, `/branch`, `/tree` | session picker, fork, and the tree navigator |
| `/model [@role\|ref]` | switch the active model or assign a role (Alt+M opens the selector) |
| `/goal [view\|create <objective>\|resume <objective>\|evidence <note>\|complete [notes]\|drop]` | drive the session objective and its token budget; bare `/goal` (or `check`/`status`/`get`) shows it. Creating one starts its first turn, and an active goal keeps working until it is completed, dropped, out of budget, or the turn ends (Esc) |
| `/plan`, `/vibe`, `/prewalk`, `/advisor`, `/handoff` | run-mode controls |
| `/memory`, `/skill:<name>`, `/settings`, `/theme`, `/hotkeys`, `/hub` (Alt+A), `/stats`, `/export`, `/share`, `/collab` | knowledge, chrome and sharing |
| `/help` | list every command |
| `/quit`, `/q` | quit |

Lifecycle commands refuse while a turn is running (Esc cancels first, Ctrl+C
quits). `xdev -h` lists the launch flags; the flags an omp user expects
(`-p/-c/-r/-e`, `--approval-mode`, `--smol/--slow/--plan-model`, `--models`,
`--provider`, `--add-dir`, `--allow-home`, `--yolo`, `--no-prewalk`,
`--plugin-dir`) are accepted with the same meanings, and
`docs/parity-delta.md` records every deliberate difference from the baseline.

**Custom markdown commands** — drop `*.md` files into `<cwd>/.xdev/commands/`
(project) or `~/.xdev/agent/commands/` (user; project wins on name
collisions). Optional frontmatter sets `name:`/`description:`; the body is a
prompt template with quote-aware argument expansion: `$1..$n`, `$@`,
`$@[start]`, `$@[start:length]`, `$ARGUMENTS`. Unknown `/foo` input falls
through to the model as ordinary text.

**Turn budget** — `-max-turns N` caps one run (default 200). At the cap the
agent wraps up gracefully with a status report instead of dying with an
error; say "continue" to resume.

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
| M9 | Model roles + provider/auth layer: roles (`@smol`/`:effort`), models.yml, credential chain, 4 wire transports, Claude/Codex OAuth, config layering + `xdev config` | role aliases resolve across sessions; OAuth login works; config precedence tests pass |
| M10 | Session UX: slash commands, lifecycle (`/new` `/fork` `--continue`), AGENTS.md hierarchy + imports, system-prompt files, keybindings | continue/resume/fork workflow works; custom markdown commands expand; AGENTS.md reaches context |
| M11 | Agent system: task agents, hub messaging/processes, hooks, advisor watchdog, prewalk, plan mode | parent spawns scoped subagent steered back via hub; advisor steers; prewalk switches model after first edit |
| M12 | Knowledge & chrome: memory backend + `/memory`, skills (`skill://`), theme engine, TUI chrome (status line, overlays) | memory summary injects at start; `/skill:` expands; theme live-reloads with all 66 tokens enforced |
| M13 | Extended tools: eval kernel, notebook, web_search, github, ast-grep, browser, checkpoint/rewind, secrets redaction, LSP, MCP extensions | eval cell persists state; web_search/github/ast-grep answer real queries |
| M14 | v2 modes & polish: vibe mode, E2E-encrypted collab, multi-provider discovery, profiles, `/export` `/share`, goal mode | vibe session completes delegated multi-worker task; collab guest mirrors host |

## License

Apache-2.0 — see [LICENSE](LICENSE). Security reporting: [SECURITY.md](SECURITY.md).

## Name

The repo and product name is **xdev**. The underlying research document explored the working codename **adze** (a small, sharp woodworking adze: light, precise, fast — it cuts exactly what you point it at); that proposal is superseded by the repository name.
