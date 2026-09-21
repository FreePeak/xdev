<p align="center">
  <img src="assets/brand/xdev-logo.png" alt="xdev" width="520">
</p>

<p align="center">
  A lightweight coding agent in Go: one static, CGO-free binary that reads your repo, edits it, runs it, and remembers the session.
</p>

<p align="center">
  <a href="https://github.com/FreePeak/xdev/releases/latest"><img src="https://img.shields.io/github/v/release/FreePeak/xdev?label=release&color=blue" alt="latest release"></a>
  <img src="https://img.shields.io/badge/platform-linux%20%C2%B7%20macos%20%C2%B7%20windows-blue" alt="linux, macOS, windows">
  <a href="https://go.dev"><img src="https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white" alt="Go 1.25"></a>
  <a href="./LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue" alt="license: Apache-2.0"></a>
  <a href="./CONTRIBUTING.md"><img src="https://img.shields.io/badge/PRs-welcome-brightgreen" alt="PRs welcome"></a>
</p>

<p align="center">
  <img src="assets/screenshots/welcome.png" alt="The xdev terminal UI: the block-art wordmark over the session menu, composer and status line" width="860">
</p>

## What it is

`xdev` is a terminal coding agent and a harness you can embed. It carries the
session/chat core of [pi](https://github.com/earendil-works/pi) and Oh My Pi
(omp) — provider streaming, the agent loop, JSONL session persistence, tool
execution, a terminal UI — rebuilt in Go with two disciplines the JS versions
gave up:

- **One binary, ~20–22 MB per platform.** No JS runtime, no in-process plugin
  VM, no install-time toolchain. `CGO_ENABLED=0` cross-builds for linux, macOS
  and windows, each on amd64 and arm64.
- **A hard <100 MB RSS budget.** `debug.SetMemoryLimit` is the backstop
  (`XDEV_MEMLIMIT` overrides it) and every queue, buffer, cache, session window
  and output sink is bounded, so a runaway turn degrades into "compact now"
  instead of an OOM kill.

Sessions are append-only JSONL trees with a mutable leaf pointer. Nothing is
rewritten: branching moves a pointer, context is reconstructed by walking parent
links, and the file stays inspectable with ordinary tools — `jq`, `grep`, git.
The format is omp-compatible (`xdev -from-claude` / `-from-codex` import a
transcript from another harness and continue it).

## Install

**Linux / macOS** — one command. It resolves the newest release for your
platform, verifies its SHA-256 against the release manifest, and installs to
`~/.local/bin`. Re-running it is how you upgrade.

```bash
curl -fsSL https://raw.githubusercontent.com/FreePeak/xdev/main/scripts/install.sh | sh
```

**Windows** — download `xdev_windows_amd64.exe` (or `_arm64`) from
[the latest release](https://github.com/FreePeak/xdev/releases/latest) and put it
on your `PATH`.

**From source** (needs Go 1.25+):

```bash
git clone https://github.com/FreePeak/xdev && cd xdev
CGO_ENABLED=0 go build -o xdev ./cmd/xdev
```

Installer knobs — the same names `xdev update` reads: `XDEV_UPDATE_REPO`,
`XDEV_UPDATE_API`, `XDEV_INSTALL_DIR`.

## First run

```bash
xdev setup      # creates ~/.xdev/agent/, writes a starter config.yml, prints next steps
xdev            # the TUI
```

xdev speaks eight wire formats through `~/.xdev/agent/models.yml` —
`anthropic-messages`, `openai-completions`, `openai-responses`,
`azure-openai-responses`, `openai-codex-responses`, `google-generative-ai`,
`google-vertex`, `gemini-cli` — so any OpenAI-compatible gateway works. Point a
provider at it:

```yaml
# ~/.xdev/agent/models.yml
providers:
  xdev-server:
    baseUrl: ${XDEV_SERVER_URL}/v1
    apiKey: ${XDEV_SERVER_KEY}        # gateway key (ONEGW_KEYS on the host)
    api: openai-completions
    models: [{ id: free, name: Free, contextWindow: 200000 }]
defaultModel: xdev-server/free
```

Or: `export XDEV_SERVER_URL=http://<gateway-host>:8080`, then
`xdev connect xdev-server --set-default` and `export XDEV_SERVER_KEY=...`.

Claude Pro/Max and Codex subscriptions work through the browser OAuth flow:

```bash
xdev login claude     # or: xdev login codex
```

Two hundred more hosts — the subscription and gateway providers opencode and omp
list — connect with one command instead of a hand-written block:

```bash
xdev connect                  # the providers a credential is ready to use
xdev connect --list           # the whole catalog (~200 hosts), with its state
xdev connect deepseek         # write that provider into models.yml
xdev connect openrouter --key sk-or-...   # ...and store the key (0600)
```

`/connect` in the TUI opens the same catalog as a picker. Each connected row
keeps its credential as a `${VAR}` reference in models.yml — the variable the
host documents — so the config file stays safe to paste into an issue; a key
passed with `--key` goes to `credentials.json` instead. Every connected
provider turns on model discovery, so hosts publish their own newest ids
without a config edit. The catalog is a generated snapshot of
[models.dev](https://models.dev) (`scripts/gen_connect.go`), and the same
command refreshes nothing at runtime — it answers offline.

## Use it

```bash
xdev                                            # the TUI (a bare call on a TTY)
xdev "add pagination to /users, then run the tests"
xdev -continue "now cover the empty page"       # latest session in this cwd
xdev -resume 01a0 "pick up where we left off"   # by session-id prefix
xdev -model xdev-server/free "…"                # explicit provider/model
xdev < prompt.txt                               # headless: the prompt on stdin
xdev rpc                                        # JSONL-over-stdio, for embedders
xdev acp                                        # ACP server on stdio, for editors
```

`xdev --help` lists every launch flag. The flags an omp user expects
(`-p/-c/-r/-e`, `--approval-mode`, `--models`, `--provider`, `--add-dir`,
`--no-prewalk`, `--plugin-dir`) are accepted with the same meanings;
[docs/parity-delta.md](docs/parity-delta.md) records every deliberate
difference.

## Terminal UI

The look is Grok CLI's (GrokNight/GrokDay, and 66 named theme tokens you can
remap in JSON with live reload). What matters for daily work:

| Keys | Action |
|---|---|
| `PgUp` / `PgDn`, `Ctrl+B` / `Ctrl+F` | page through the transcript |
| `Home` / `End` | jump to the top / back to live |
| `Shift+↑` / `Shift+↓` | line up / down |
| `↑` / `↓` | walk the composer's visual rows, then recall prompt history |
| `Ctrl+R` | previous prompt |
| `Alt+M` / `Alt+A` / `Alt+T` | model picker / agent hub / session tree |
| `Alt+S` / `Ctrl+T` | context dock: cycle shown/hidden/auto / fold its sections |
| `Alt+,` | settings panel: live-edit the session's settings (Enter changes the row under the cursor) |
| `Shift+Tab` | ask the model to stop reasoning ⇄ let the model role decide (`/thinking`) |
| `Ctrl+O` | expand the newest tool result or thinking box |
| `Esc` | idle: clear the draft → `Esc` again brings it back → `Esc` opens the session tree (running: cancels the turn) |
| `Ctrl+C` | quit (`Esc` cancels the running turn first) |

Every chord is remappable in `~/.xdev/agent/keybindings.yml`; `/hotkeys` shows
the live map.

Scrolling never fights the stream: a scrolled viewport stays put while output
arrives, and `▲ n ▼ n` shows how much is hidden. Mouse selection covers the
whole screen and survives a scroll — hold the drag at the transcript's top or
bottom edge and it keeps scrolling while you select, and the scrollbar — the
transcript's right edge, wherever the context dock leaves it — drags like any
other. Shift+drag hands the gesture back to the terminal's native selection.
A boxed row copies as its text: the output and the label on the top rule
(`╭─ bash ───╮` copies as `bash`), never the frame or the pad inside it.

Thinking renders like a tool result: a rounded box whose top border carries the
state (`⠹ Thinking…` while it streams, `Thought for Xs` when it settles) and
whose body is a fixed 12-row window. The wheel always scrolls the transcript
until you *click* a reasoning box: the clicked box draws a bold border and then
the wheel over it scrolls its own window, so long reasoning stays readable
without the transcript sliding along with it. A click anywhere else — or
`Ctrl+O`, which expands the newest boxed block to every row — hands the wheel
back to the transcript.

Display and request are separate switches. The box above is *display*
(`showThinking`, `/settings showThinking on|off`); `Shift+Tab` (or `/thinking
off`) is the *request*: the next turn goes out with no reasoning budget at all,
and the toggle flips between "off" and "auto", where "auto" hands the decision
back to the model role's `:effort` (`@slow:high`, or the persisted `thinking`
key). `/thinking low` pins one rung for the rest of the session and writes it to
the global layer.

The **context dock** (`Alt+S`) is a fixed 42-column panel right of the
transcript, opencode's sidebar shape: no box, just a surface of its own carrying
the session's title in the top slot, then the pending plan, the task list, the
files this session changed (their `+N`/`-N` counts flush right) and the session
footer — the working set, kept beside the stream instead of scrolling behind it.
Section headings are a bold name with the dim count appended. It auto-closes
below 120 columns, never takes focus from a picker or a question card, and
rebuilds only on events (plan published, tool finished, agent settled), never
per frame. A pending plan is resolved where plans have always been resolved —
`/plan off` approves, any typed prompt is revision feedback — and `/plan show`
reprints the document in the transcript.

**Slash commands** dispatch at input-submit and never reach the model:

| Command | Action |
|---|---|
| `/new` `/fresh` `/clear` `/drop` | start over, rotate provider state, reset context in place, delete the session file |
| `/resume [id]` `/fork` `/branch` `/tree` | session picker, fork, entry switch, tree navigator |
| `/rename <title>` `/dump` `/export [path]` `/share` `/collab` | title, export to markdown/HTML, share an E2E-encrypted view |
| `/model [ref]` `/connect [name]` `/theme <name>` `/settings [overlay]` `/hotkeys` | model, provider catalog, theme and display control (`/settings overlay` — or `Alt+,` — opens the settings panel; `/settings sidebarMode auto\|show\|hide` pins the dock) |
| `/thinking [off\|auto\|minimal\|low\|medium\|high]` | request-side reasoning for the next turn (bare reports; `on` = `auto`) |
| `/goal <objective>` `/plan` `/prewalk` `/handoff` `/advisor` `/vibe` | run modes: name the session's objective and start on it (bare `/goal` shows it, `/goal complete\|drop` closes it), read-only research, model handoff, background reviewer, director mode |
| `/memory` `/skill:<name>` `/hub` `/tasks` `/join <link>` | knowledge, skills, the subagent roster, background jobs, joining a shared session |
| `/help` `/quit` | every command, and an exit that prints the `--resume` line to get back |

Lifecycle commands refuse while a turn is running. **Custom commands** are
markdown files: drop one in `.xdev/commands/` (project) or
`~/.xdev/agent/commands/` (user; project wins), optionally with `name:` and
`description:` frontmatter, and the body is a prompt template with quote-aware
`$1..$n`, `$@`, `$@[start:length]` and `$ARGUMENTS` expansion. An unknown
`/foo` falls through to the model as ordinary text.

## Tools

Four tools are the core — `read`, `write`, `edit`, `bash` — and they are what
the system-prompt budget is spent on. The rest of the surface is registered but
progressively disclosed: the model sees a one-line index and pulls a schema with
`tool_search` → `tool_describe` → `tool_call`, so the long tail costs almost
nothing per turn.

The rest of the surface: `grep`/`glob` (the host's `rg` is used when present),
`eval` (a persistent Python kernel; a `.ipynb` reads and edits as cell blocks),
`ast_grep`/`ast_edit`, `lsp`, `debug` (DAP over stdio: dlv, debugpy, lldb-dap),
`browser` (CDP: attaches to a Chrome you started, else launches one on a
private profile and closes it again after 5 idle minutes — `browser.autolaunch:
false` keeps it attach-only, `browser.idleExit: <seconds>` tunes the idle
exit, `0` keeps the browser for the whole session),
`web_search`, `github`, `security_scan`, `computer`, `tts`, `generate_image`,
`checkpoint` / `rewind`, `todo`, `ask`, `task` + `hub` + `send_message` /
`inbox` for subagents and cross-session mail, and the memory and skill tools.
MCP servers and subprocess extensions register into that same registry, so they
ride the same approval policy, hooks and output sinks as the built-ins.

## Configuration & credentials

Everything lives under `~/.xdev/agent/` — `config.yml`, `models.yml`,
`credentials.json`, `sessions/`, `agents/`, `commands/`, `themes/`, `hooks/`,
`skills/` — layered
schema defaults → user file → `<project>/.xdev/config.yml` → `-config` overlays,
and readable with `xdev config list|get|path`. Only the user layer is editable
through `xdev config set`; project files stay hand-written, because a repository
that can set your settings is how leaks start.

### What is loaded into the prompt, and when

Two kinds of file reach the system prompt on **every** turn, and a third does not.
Put a convention in the first two if it must hold every time; recall is a ranked
search over the last message and will miss.

| Surface | Paths | When it applies |
|---|---|---|
| Context files | `~/.xdev/agent/AGENTS.md` **first**, then `AGENTS.md` (or `CLAUDE.md`) walking root→cwd, plus any `--add-dir` root and `@path` imports | all of it, every turn, authoritative — 32 KB cap |
| Rules | `<repo>/RULES.md` and `~/.xdev/agent/RULES.md` (native, `alwaysApply`), `.cursor/rules/*.mdc`, `.windsurf/rules`, `.clinerules`, `.agent/rules`/`.agents/rules`, Copilot instructions, installed plugins | matching rules, every turn; first source to claim a name wins |
| Skills | `<cwd>/.xdev/skills/`, `~/.xdev/agent/skills/`, `~/.xdev/agent/managed-skills/`, plus `skills.customDirectories` | by model invocation or `/skill:<name>` |
| Slash commands | `<cwd>/.xdev/commands/*.md`, `~/.xdev/agent/commands/*.md` | when you type `/<name>`; project wins a collision |
| Subagents | `<cwd>/.xdev/agents/`, `~/.xdev/agent/agents/` | when dispatched by the `task` tool |
| **Recall (memory guidance)** | the configured backend (`memory: hindsight` → a bank over HTTP) | **ranked against the last user turn only, cached 60 s, capped at 1024 tokens, and labelled "not authoritative"** |

So a "never do X" rule that only exists as a memory row is not policy: it competes
for 4 KB with everything else the bank knows, and the block tells the model to
weigh it. Write it to `AGENTS.md` (habits that span repos) or `RULES.md` (must
apply even where a same-named rule exists). The full measurement behind this is in
[docs/decisions/standing-conventions-location.md](docs/decisions/standing-conventions-location.md).

`credentials.json` is written `0600` and **re-verified on every read** — mode
`0600` and `nlink == 1`, because a write-time chmod says nothing about the file
now. A file that fails either check is refused with the repair named, never
silently trusted.

To keep a long-lived token off disk on macOS, store it in the Keychain yourself
and point a provider at it:

```yaml
providers:
  openai:
    apiKey: keychain:dev.xdev.credential.openai/linh   # service[/account]
    api: openai-completions
```

xdev **reads** such an item and never writes one — `/usr/bin/security` accepts a
secret either as an argument (visible in `ps` to every user on the machine) or
through a prompt that silently truncates at 128 bytes. An unsatisfiable
`keychain:` reference is an error naming the reference; xdev will not quietly
use a different credential. `XDEV_DISABLE_KEYCHAIN=1` turns the lookup off.
[Rationale with measurements](docs/decisions/keychain-credential-source.md).

A repository is not a configuration authority: its hooks run only once you trust
the workspace (`xdev trust`). [SECURITY.md](SECURITY.md) is the reporting
policy; the [2026-09-15 audit](docs/SECURITY-AUDIT-2026-09-15.md) is public.

## Updating, checking, measuring

```bash
xdev update --check                  # is a newer release published for this channel?
xdev update                          # verify SHA256SUMS, then replace this binary
xdev update --channel canary         # pre-release tags (v0.2.0-canary.1)
xdev update job install              # a twice-daily release check (launchd / systemd)
xdev update job status               # what it last found, and whether it is installed
xdev bench --turns 5 --model onegw/free   # TTFT + decode p50/p95 through your provider
xdev stats --serve                   # usage dashboard over the local session store
xdev usage                           # which accounts are configured + what you spent locally
```

`xdev update` resolves the channel's newest release from GitHub
(`XDEV_UPDATE_REPO`, default `FreePeak/xdev`; `XDEV_UPDATE_API` for a mirror),
refuses a release with no `SHA256SUMS` entry, refuses when the running binary is
not writable — printing the exact `chmod u+w <path>` — and installs by writing a
temp file next to the target and renaming, so an interrupted update never leaves
a half-written binary. `--check` only reports.

Keeping up to date is opt-in and costs nothing to notice: `xdev update job
install` registers `com.freepeak.xdev.update-check` (launchd) or a `--user`
systemd timer running `xdev update --check` at 09:00 and 21:00. Each run leaves
one record in the data dir, and the next launch reads it — so a newer release
surfaces as a line on the welcome screen and on `xdev -p`'s stderr, never as a
network call in the way of your prompt. It never installs anything; `xdev
update` stays the act that does. With no job registered, the first
terminal-attached launch of a 12-hour window runs the check detached instead,
and `XDEV_UPDATE_CHECK=0` declines even that. `xdev update job remove`
unregisters it.

Release binaries are Developer ID-signed and notarized in CI when the `APPLE_*`
secrets are configured, and ship ad-hoc signed when they are not: a release
never fails for want of credentials. See
[`.github/workflows/release.yml`](.github/workflows/release.yml) (job
`macos-sign`) for the five secret names.

## Design principles

1. **A minimal prompt.** The budget is under 1,000 tokens including tool
   descriptions. Frontier models are RL-trained to act as coding agents; 10,000
   tokens of instructions are dead weight. Project context arrives through the
   standard `AGENTS.md` hierarchy (global → project, imports included), and the
   whole prompt is user-replaceable.
2. **Four core tools.** `read`, `write`, `edit`, `bash` are the job. Everything
   else has to earn its place in the request.
3. **YOLO by default — no permission theater.** Read-data + execute-code +
   network cannot be contained by prompting or pattern rules. Containment
   belongs to external sandboxing; the harness documents those patterns
   ([container & sandbox reference](docs/reference/container-and-sandbox.md))
   rather than performing security.
4. **Bounded everything.** Backpressure is a feature. Nothing grows silently
   with its input.
5. **Append-only state.** Sessions, transcripts and checkpoints are trees you
   can read; a fork is a pointer, not a copy.
6. **Shell out over import.** `git`, `rg`, `fd`, `ast-grep`, `gh`, `python3`
   and `/usr/bin/security` stay external — optional, probed at setup, never
   required. Six direct Go dependencies in `go.mod`; the binary stays CGO-free.

## How it compares

The full comparison — seven peer harnesses read from primary sources,
each with adopt / verify / **reject** verdicts — lives in
[docs/research/harness-comparison.md](docs/research/harness-comparison.md).

The summary and the method, in one line: every peer claim is backed by
`path:line` evidence and recorded in [docs/PRD.md](docs/PRD.md) §5; the
tension between pi's minimalism and omp's surface is named, not
glossed; and the open half of the comparison (what was studied but not
shipped) is listed with its issues rather than hidden.

## Repository layout

One binary (`cmd/xdev`), one internal package per concern: `ai` (the wire
adapters + the shared SSE/partial-JSON reader), `agent` (the loop, compaction
ladder, subagents, plan/goal modes, prompt assembly), `session` (the JSONL tree
store), `tool` (the registry, approvals, output sinks), `tui` (the frame-plan
renderer over tcell), `config` (layering + the credential chain), and `dist`
(`setup`/`update`/`bench`). The rest are the seams: `rpc`, `acp`, `protocol`,
`ext`, `mcpclient`, `memory`, `skills`, `rules`, `hooks`, `theme`, `stats`.

## Documentation

| Read | What it answers |
|---|---|
| [docs/PRD.md](docs/PRD.md) | scope, architecture, milestones, status — the source of truth |
| [docs/research/2026-09-09-omp-pi-architecture-go-rebuild.md](docs/research/2026-09-09-omp-pi-architecture-go-rebuild.md) | the architecture research and rebuild blueprint |
| [docs/reference/session-format.md](docs/reference/session-format.md) | the JSONL tree on disk |
| [docs/reference/extension-protocol.md](docs/reference/extension-protocol.md) | the subprocess extension handshake |
| [docs/reference/container-and-sandbox.md](docs/reference/container-and-sandbox.md) | running it contained |
| [docs/parity/cli.md](docs/parity/cli.md) · [docs/parity/tools.md](docs/parity/tools.md) | measured parity against the omp baseline |
| [docs/decisions/](docs/decisions/) | the recorded trade-offs, with evidence |
| [CONTRIBUTING.md](CONTRIBUTING.md) | the checks a PR must pass, and what will not be accepted |

## Status

Milestones M0–M15 are landed (2026-09-09 → 2026-09-12): the provider layer and
its eight transports, the session core, the agent loop and four tools, the TUI,
compaction and failover, RPC + subagents + MCP, the memory-hardening audit
(fuzzers, six-platform cross-builds), model resolution and auth, session UX, the
agent system (task agents, hub, hooks, advisor, prewalk, plan mode), memory and
skills, the extended tool set, and the v2 modes (vibe, collab, ACP, profiles,
`/export` + `/share`, goal mode). The distribution surface — the installer,
`xdev update` and the release pipeline — closed on 2026-09-14. 553 Go files,

What ships is [the release list](https://github.com/FreePeak/xdev/releases).
What is left is tracked in GitHub issues — this repo has no `TODO.md`, and the
[PRD](docs/PRD.md) milestone table is the status summary.

## Contributing

Start with [CONTRIBUTING.md](CONTRIBUTING.md). Issues are the task list; every
PR says what changed and why. There is deliberately no CI gate on a pull
request — you run the checks yourself before merging: `gofmt -l .`,
`go vet ./...`, `go test -count=1 ./...`, `go test -race -count=1 ./...`, and
the six-platform `CGO_ENABLED=0` cross matrix. Agent-authored PRs are welcome
and hold to the same bar: the human who submits owns every line.

## License

Apache-2.0 — see [LICENSE](LICENSE). Code of conduct:
[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md). Security reporting:
[SECURITY.md](SECURITY.md).

<details>
<summary>The name</summary>

The repository and product name is **xdev**. The research document that
preceded it explored the working codename **adze** — a small, sharp woodworking
adze: light, precise, fast, cutting exactly what you point it at. That proposal
is superseded by the repository name.

</details>
