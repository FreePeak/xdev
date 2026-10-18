# Plan: opencode as a supervised xdev backend service, with named workflow presets and a display-only sidecar pane

*Date: 2026-10-18 · Status: **plan only** (no code in this change) · Branch: `docs/plan-opencode-backend-service`*
*Trigger: request to raise xdev to a "high level" tooling surface — xdev renders, displays and orchestrates AI tools through a pre-configured workflow, while user tasks call Claude Code / opencode as backend services. Round scope (agreed): the opencode service surface first; the Claude Code wrapper is phase 2.*

Related:

- `docs/research/opencode-internals.md` (v1.18.28 inventory — **superseded in part**, see §1.0)
- `docs/decisions/xdev-gateway.md` (daemon/provider shape precedent)
- `docs/decisions/live-subagent-view.md` (live child rows — closest ADR to the sidecar)
- `docs/decisions/subagent-chains-and-panel.md` (strip above composer + Alt+B)
- `docs/plans/2026-10-14-open-subagent-session-in-main-tui.md` (format mirror for this document)
- `internal/tui/hubdock.go` (uncommitted WIP — the bottom-pane sidecar template)
- `scripts/tui-subagent-panel-drive.py` (existing TUI drive harness)

---

## 0. One-sentence outcome

Make xdev the surface that *drives* opencode — a supervised loopback backend child, a named-preset workflow selected from xdev config, and a read-only bottom pane showing that backend's live state — without importing opencode's transcript, its tools, or its system prompt into xdev's own context.

---

## 1. What opencode actually ships (primary source, verified 2026-10-18)

Everything in this section was read from the **installed** binary and its **running** server, not from the v1 research doc.

### 1.0 Version drift (fix the baseline first)

| Source | Version |
|---|---|
| `docs/research/opencode-internals.md` header | 1.18.28 |
| `opencode --version` on this machine today | **2.0.32** |

The v1 doc's storage, config and prompt sections are still directionally right, but the *process and API surface changed in v2* and that is exactly the part this plan depends on. Slice 0 exists to re-baseline it.

### 1.1 opencode already runs its own background server

`opencode serve --help` (v2.0.32):

```
USAGE
  opencode serve [flags]
FLAGS
  --hostname string
  --port integer
  --cors string     Additional allowed CORS origin (repeat for multiple origins)
  --service
  --stdio
```

Three modes, and they are materially different for xdev:

| Mode | Invocation | Ownership | Auth | Discovery |
|---|---|---|---|---|
| **stdio** | `opencode serve --stdio --port 0` | the spawning process | HTTP **Basic**, `opencode:<password>` | child prints exactly one JSON line on stdout: `{"url":"http://127.0.0.1:PORT"}` |
| **service** | `opencode serve --service` | opencode's own registration file | Basic, password persisted to `~/.config/opencode/service.json` | `opencode service status` prints the URL |
| **default** | `opencode serve` | foreground | Basic | prints `server listening on <url>` |

Verified from the shipped bundle (`strings` over `~/.opencode/bin/opencode`, the `cli.app` chunk):

- stdio mode emits `JSON.stringify({url:U})` and nothing else — the child never writes a log line to stdout in that mode.
- the child is spawned with `--stdio --port 0` and `env: {OPENCODE_PASSWORD: <random 32-byte base64url>}`.
- the parent kills it with `killSignal:"SIGTERM"`, `forceKillAfter:"3 seconds"`.
- `OPENCODE_PASSWORD` / `OPENCODE_SERVER_PASSWORD` pin the server password from the environment; `--stdio` **deletes both** from its own env, so the password cannot leak back out through a child endpoint.

`~/.config/opencode/service.json` on this machine today contains only:

```json
{ "password": "8j8AR3Y3lBSJjS2eZXZr_7hTL-oKVOmyamNsTwH73Zo" }
```

a generated 43-char secret. So in service mode the password is a *pinned* value in the user's config dir, not a per-launch random.

### 1.2 The API is a token-gated `/api/*` namespace

Probed against the live server on `127.0.0.1:49374`:

| Path | Result |
|---|---|
| `/api/session` | `401` `{"_tag":"UnauthorizedError","message":"Authentication required"}` |
| `/api/openapi.json` | `401` |
| `/openapi.json` | `401` |
| `/session` (no prefix) | `200` — but it is the **SPA** serving `index.html`, not the API |
| `/doc` | `200` — the OpenCode **web UI**, not an API doc |

The web-UI handler routes `/api`, `/api/*`, `/auth/*` and `/openapi.json` to the API and hands **everything else** to the static asset server with an `index.html` fallback. Consequence for xdev: **a `200` from this server proves nothing** — a mistyped path returns the SPA shell. xdev must assert on the response `Content-Type` (and on the API's own error `_tag`), not on the status code alone.

The sanctioned authenticated client is the CLI:

```
opencode api <operationId | METHOD /path> [--data -d] [--header -H] [--param k=v] [--server URL] [--standalone]
```

Verified working:

```
$ opencode api GET /api/session
{"data":[{"id":"ses_eb24d9e1...","projectID":"7f0bb1e2...","agent":"build",
  "model":{"id":"space-bunny-free","providerID":"opencode","variant":"high"},
  "cost":0.00002816,
  "tokens":{"input":925544,"output":164503,"reasoning":20185,"cache":{"read":24303175,"write":0}},
  "outcome":"succeeded",
  "time":{"created":1792304898581,"updated":1792308768148,"idle":1792307971493,"viewed":1792307971493},
  "title":"xdev high-level upgrade with background services",
  "location":{"directory":"/Users/linh.doan/work/harvey/freepeak/xdev"}}]}
```

Note what a session row already carries: agent, model+variant, cost, full token accounting (input / output / reasoning / cache read+write), outcome, idle time, and the working directory. **That row is enough to render a useful sidecar with no transcript access at all** (§4.3).

Other subcommands that matter:

```
opencode service start | restart | status | stop | get | set | unset
opencode session  list | delete | export | import
opencode run      <message>
opencode api      <request>
opencode acp      Start an Agent Client Protocol server
opencode mini     Start the minimal interactive interface
```

Global flags on every subcommand: `--server <url>`, `--standalone`, `--auto`, `--continue/-c`, `--session/-s <id>`, `--prompt <text>`.

`opencode acp` is the Agent Client Protocol server — a third integration seam, deferred (§8).

### 1.3 opencode's context levers (this is the §4.3 mechanism)

Environment variables read by the shipped bundle, filtered to the ones that shrink what the model is told:

| Variable | Effect (from the bundle's server-config construction) |
|---|---|
| `OPENCODE_DISABLE_CLAUDE_CODE` | `claude: !OPENCODE_DISABLE_CLAUDE_CODE` — stops importing Claude Code assets |
| `OPENCODE_DISABLE_CLAUDE_CODE_SKILLS` | same, skills only |
| `OPENCODE_DISABLE_PROJECT_CONFIG` | `project: !OPENCODE_DISABLE_PROJECT_CONFIG` — stops loading `./opencode.json` |
| `OPENCODE_CONFIG_DIR` | points config at an alternate directory |
| `OPENCODE_CONFIG_CONTENT` | inlines config JSON, no file read |
| `OPENCODE_DISABLE_FFF` | disables the FFF (file/flow) subsystem |
| `OPENCODE_DISABLE_FILEWATCHER` | no watcher (startup cost, not prompt cost) |
| `OPENCODE_DISABLE_MODELS_FETCH` | no model catalog fetch at boot |
| `OPENCODE_TOOL_GUIDANCE` | tool-guidance surface in the prompt |
| `OPENCODE_DISABLE_AUTOUPDATE` | no self-update check |

Agent-level context control lives in the agent schema (`docs/research/opencode-internals.md` §11.1, `AgentConfig`): an agent carries its own `model`, `variant`, `prompt` (markdown frontmatter body) and `permission` ruleset — so a **preset can pin a cheap small model** without touching global config. This machine's global `small_model` is already `opencode-go/glm-5.3-flash`; a preset can reuse that shape.

The user's live `~/.config/opencode/opencode.json` shows the surface a preset has to coexist with: four MCP servers (three enabled), `"permission": "allow"` globally, a `small_model`, and `compaction.keep.tokens: 20000` / `buffer: 48000`.

---

## 2. What xdev already ships (source-verified at HEAD `0dbfa67`, branch `main`)

### 2.1 Background-service precedent — `internal/serve`

`internal/serve/serve.go` is a ready-made supervisor posture and should be extended, not replaced:

| Piece | Location | Reuse |
|---|---|---|
| service-name consts | `serve.go:53-56` (`auth-broker`, `auth-gateway`, `browser-relay`, `headless`) | add `serviceOpencode` |
| `IsService(name)` | `serve.go:96` | becomes true for `opencode` |
| `Dispatch(args, version)` | `serve.go:104` | one new case |
| `Options` (dataDir / logf / addr) | `serve.go:219-257` | as-is |
| `loopbackAddr` | `serve.go:291` | as-is — loopback-only is already enforced |
| `resolveToken` / `ensureToken` / `loadToken` | `serve.go:274`, `:322`, `:346` | xdev's **own** token, for xdev's **own** broker surface |
| `serveHTTP` / `readJSON` / `requireToken` / `tokenEqual` | `serve.go:482`, `:524`, `:428`, `:471` | as-is |
| `serveUntilSignal` | `serve.go:198` | as-is |
| `HeadlessRunner` / `RegisterHeadless` | `serve.go:62`, `:65` | the existing seam for "a service implemented elsewhere and injected" |

Inventory: `broker.go`, `gateway.go`, `relay.go`, `serve.go`, `ws.go`, `headless.go`, `headless_stream.go`, `tokens.go`, `proc_unix.go`, `proc_windows.go`, `web/`.

**Gap, stated plainly:** there is **no** `exec.CommandContext` anywhere in `internal/serve`. Every current service is an in-process HTTP server. Spawning opencode as a managed child is new work in this package.

### 2.2 Managed-child precedent elsewhere

`internal/tts/tts.go:108` already carries the exact shape this plan needs — a `Lookup func(name string) (string, error)` seam defaulting to `exec.LookPath` so tests can substitute a fake backend, plus `exec.CommandContext(ctx, bin, ...Args(chunk, req)...)` at `:194`. `internal/browser/launch.go:99-115` does the same for a browser. Neither is in `internal/serve`; the pattern should be copied, not imported.

### 2.3 Supervised-daemon CLI precedent

`cmd/xdev/gatewaycmd.go` (694 lines) is the shape for "a service xdev owns end to end": subcommand parse → run-lock → PID file → OS service unit on launchd/systemd at launch → `status` / `logs`. `internal/gateway/` holds the daemon, runstate, lease, session-map, setup and service-unit implementation.

### 2.4 The bottom-pane sidecar template already exists (uncommitted WIP)

`internal/tui/hubdock.go` (202 lines, **untracked — do not disturb**):

| Element | Line | Note |
|---|---|---|
| `hubDockMaxRows = 5` | `:36` | + "+N more" overflow row |
| `hubDockRefresh = 500ms` | `:42` | throttled roster read; `Roster()` walks every child |
| `HubDockToggle()` / `HubDockOn()` | `:47`, `:57` | Alt+H chord, sticky not modal |
| `hubDockRows()` | `:93` | geometry; returns 0 when it paints nothing |
| `hubDockBudget(top, cRows, h)` | `:110` | clamps so the composer never climbs into the tab strip |
| `drawHubDock(dockTop)` | `:122` | the painter |
| `hubDockMouse` | `:187` | click handler |

Its header comment already documents the three-surface split it exists beside: `/hub` roster (modal overlay, `hubroster.go`), the live strip (`agentspanel.go`, only while a child is RUNNING), and the always-available bottom dock. **The opencode sidecar is a fourth surface on the same seam.**

### 2.5 Config layering — where named presets must fit

`internal/config/settings.go`:

- `Settings` struct at `:490`; `Tui TuiSettings` at `:515`; `TuiSettings` at `:779`; `TabSettings` at `:801`; `defaultSettings()` at `:1133`.
- Layering: schema defaults ← `~/.xdev/agent/config.yml` ← project `.xdev/config.yml` ← `--config` overlay. Objects deep-merge; scalars and arrays replace. A malformed file is preserved as `.broken-<stamp>` and never silently dropped.
- The zero-skip template is `MnemopiConfig()` / `DefaultMnemopiSettings()` — field-by-field, skip zero, keep the shipped behaviour when a layer omits the key. `TabSettings` is the naming precedent: *"spelled exactly as opencode's own config schema spells it (read out of the installed binary, not translated)"*, including accepting-and-ignoring `layout` so a config pasted from opencode's `tui.json` stays loadable.

`internal/config/xdg.go`: XDG roots are **opt-in** via `xdev config init-xdg`. Absent that record, config and runtime state stay at the legacy `~/.xdev/agent`. `$XDEV_AGENT_DIR` outranks the record. `~/.xdev/agent/` exists and is populated.

### 2.6 What does not exist yet

`grep -i opencode` over `*.go` finds only provider-catalog naming (`internal/config/connect_catalog.go` → `opencode`, `opencode-go`) and parity comments (`cmd/xdev/sessionops.go:469`, `cmd/xdev/tabset.go`, `scripts/gen_connect.go`). **No opencode process integration exists in Go code today.**

`docs/plans/` contains exactly one document — the reference plan. `docs/decisions/` has 12 ADRs.

---

## 3. Root-cause seams: why "just wire opencode in" is not enough

### R1 — A `200` from opencode is not proof of success

The web-UI handler falls through to `index.html` for any non-`/api`, non-`/auth`, non-`/openapi.json` path. Any xdev client that checks status codes alone will read a mistyped path as a healthy empty result — silently, forever. `GET /global/health` looks like a health probe and is actually the SPA shell. (Verified: `curl -o /dev/null -w %{http_code}` returned `200` for `/session`, `/global/health` and `/doc`, and `text/html` for all of them.)

### R2 — opencode has its own service manager; xdev must not fight it

`opencode service start|restart|status|stop` and the self-registration loop in the bundle (re-checked every 5 s; the server **shuts itself down** if its registration file is replaced, or if the observed `id`/`version`/`url`/`pid`/`password` no longer match what it wrote) mean a naive "xdev spawns its own `opencode serve --service`" collides with an incumbent and can kill an unrelated server. The stdio mode (`--stdio --port 0`) has no registration file and no port collision at all.

### R3 — Two different secrets, easy to conflate

opencode's server password (Basic auth, `~/.config/opencode/service.json`, 43-char generated) is **not** xdev's `internal/serve` token (`serve.go:274-350`, `<service>.token` in the serve dir, `XDEV_SERVE_TOKEN` at `:76`). Collapsing them into one variable would either leak the opencode password into xdev config or break every other serve service.

### R4 — Context bleed is the default failure mode, not the exception

opencode loads Claude Code assets, project config, `~/.claude/skills/`, `~/.agents/skills/`, four MCP servers, a full tool registry and tool guidance into **its own** system prompt. If xdev drives opencode by *becoming* a client that mirrors that surface — or by pulling opencode's prompt/tool schemas into xdev to reimplement them — xdev pays the context cost that this round exists to avoid. The v1 research doc §3.6 already sizes the prompt; the levers in §1.3 are the mitigation.

### R5 — Version drift will silently break a hand-written client

Research covers 1.18.28; the installed binary is 2.0.32. A hand-transcribed response struct (or a hand-written `path` string) is a liability across that gap. `opencode api` already accepts an **OpenAPI operation id** as well as a raw path — the spec is available to the CLI, which means a generated/typed client is possible and a hand-rolled one is not required.

### R6 — No discovery surface for "which backend is up"

`opencode service status` prints a bare URL (`http://127.0.0.1:49374`) and `opencode service get` prints `{}`. xdev needs url + auth + version in one place to render a status line, and none of the three CLIs give it in one call.

### R7 — The TUI layer is mid-flight

`internal/tui/hubdock.go`, `hubdock_test.go`, `cmd/xdev/sessionops_interrupted_test.go` are untracked; `app.go`, `hubroster.go`, `keymap.go`, `tui.go`, `sessionops.go`, `.gitignore` are modified. Any pane work in this plan lands on top of a moving target. This is a scheduling constraint, not a design one.

---

## 4. Target behavior

### 4.1 Non-negotiables (keep)

1. **xdev never imports opencode's transcript, tools, or system prompt.** xdev sends a task, receives status/metadata. Full stop.
2. **Loopback only, always authenticated.** The child binds `127.0.0.1` with `opencode serve --stdio --port 0` — a kernel-assigned loopback port and Basic auth, both the child's own. Under D1 xdev never constructs a Basic header itself. If xdev ever exposes its own broker surface, it stays token-gated via `requireToken`.
3. **The sidecar is display-only.** No input forwarding, no key injection, no attach/launcher semantics. Same contract as `hubdock.go`.
4. **stdio mode is the default child mode.** No port allocation, no collision, no registration file, no fight with `opencode service`.
5. **No hardcoded opencode schema.** Client surface goes through `opencode api` (operation id or path) — decided in D1 — and never through a transcribed struct (§R5).
6. **The TUI WIP is not touched.** Pane work is gated behind Slice 4 (§5).

### 4.2 Must-fix table

| ID | Must fix | Where | Test |
|---|---|---|---|
| **F1** | A `200` response is only accepted when `Content-Type` is JSON **and** the body is not the SPA shell; API errors carry `_tag` (`UnauthorizedError`, `RouteNotFound`) and must be surfaced as errors, never as empty results | new opencode client | table test: `/session`, `/global/health` (HTML `200`) → error; `/api/session` `401` → error; `/api/session` JSON `200` → ok |
| **F2** | Child lifecycle is stdio mode only: spawn `opencode serve --stdio --port 0` with a fresh random `OPENCODE_PASSWORD`, read exactly one stdout JSON line, treat any second stdout byte as a protocol violation, `SIGTERM` then force-kill after 3 s | new supervised child | fake binary emitting one JSON line → resolves; emitting two → fails loudly; never `SIGKILL`s first |
| **F3** | opencode's Basic-auth password and xdev's serve token are separate values, separately sourced, separately named; the opencode password is generated per child and never written to xdev config | serve service | no password in any xdev settings file; password absent from child env after `--stdio` |
| **F4** | Context budget is a declared, testable contract per preset: `OPENCODE_DISABLE_CLAUDE_CODE[_SKILLS]`, `OPENCODE_DISABLE_PROJECT_CONFIG`, `OPENCODE_CONFIG_CONTENT`, tool-guidance off, and an agent pinned to a small model. The preset's budget is rendered in the preset picker so the cost is visible before launch | config + preset layer | preset struct round-trips; unset budget keys mean "shipped default", never "empty" |
| **F5** | Presets are named, selectable and launchable from xdev config, layered by the existing merge (schema defaults ← `~/.xdev/agent` ← project ← `--config`), with the zero-skip rule: a layer that omits a key keeps the shipped value | `internal/config/settings.go` | a partial project overlay changes only the keys it names |
| **F6** | `IsService("opencode")` is true, `Dispatch` routes it, and it is excluded from the browser/auth service set | `internal/serve/serve.go:96`, `:104` | `IsService` matrix; dispatch returns non-zero usage for a bad flag |
| **F7** | The sidecar shows only metadata the API already returns — session title, agent, model+variant, cost, token counts, outcome, idle time, cwd — and polls on a bounded cadence with a bounded row budget, exactly like `hubDockRefresh` / `hubDockMaxRows` | new pane | pane tests: no API transcript field is read; empty state paints zero rows |
| **F8** | Backend discovery yields url + auth + version in one call, degrading to "unknown" rather than to a wrong answer | discovery helper | no server running → `unknown`, not a fabricated URL |

### 4.3 The context-budget contract (the non-negotiable, spelled out)

The requirement is that the opencode integration stay context-light. Concretely, four rules:

1. **No mirroring.** xdev does not import opencode's tool schemas, permission rules, prompt templates or transcript format into its own model context. xdev's models see a *task string* and a *status struct*, both tiny and both defined by xdev.
2. **Presets declare a budget, and the budget is enforced by env, not by prompt.** Every lever in §1.3 is an environment variable or a config key — none of them is an instruction the model has to be told about in prose. Setting a budget costs zero prompt tokens.
3. **The default preset is the cheap one.** A preset that does not name a model resolves to the existing small-model shape (`small_model`, already `opencode-go/glm-5.3-flash` on this machine) rather than inheriting whatever the user last used interactively.
4. **The preset picker prints the budget before launch.** Model, tool-guidance state, MCP set, and skill-import state are shown as fields. A user who wants a heavier run can pick a heavier preset knowingly.

### 4.4 Named workflow presets — the shape

A preset is **not** a fixed built-in pipeline and **not** a pass-through of opencode defaults. It is a named, user-definable bundle declared in xdev config:

```yaml
# ~/.xdev/agent/config.yml   (legacy root; XDG root after `xdev config init-xdg`)
workflow:
  presets:
    - name: quick-fix
      description: One file, one model call, no MCP.
      backend: opencode
      prompt: >-
        Fix the failing test in {{.File}}. Do not touch anything else.
      model: { providerID: opencode-go, modelID: glm-5.3-flash }
      context:
        disable_claude_code: true      # OPENCODE_DISABLE_CLAUDE_CODE
        disable_project_config: true   # OPENCODE_DISABLE_PROJECT_CONFIG
        tool_guidance: false           # OPENCODE_TOOL_GUIDANCE
        mcp: []                        # explicit, not inherited
      permissions: ask                 # maps to opencode permission rules
      timeout: 5m

    - name: review
      description: Read-only review of a diff. Never edits.
      backend: opencode
      prompt: Review {{.Diff}} against the repo conventions.
      context:
        disable_claude_code: true
        mcp: []
      permissions: deny               # no edit, no write
```

Three properties, each of which is a rule rather than a hope:

- **Named and selectable** — a preset is looked up by name from config; the TUI lists what config declares. xdev ships *no* built-in preset; if the config declares none, the picker says so.
- **Selectable at launch** — `xdev workflow run <preset>` and a picker in the TUI. Both resolve through the same lookup.
- **Explicit about context** — `context:` is a required block on a preset that runs a backend, because §4.3 rule 2 says the budget is set by env, and env must come from somewhere declarative. An omitted key means "shipped default" under the zero-skip merge (`F5`), never "no levers set".

Config placement follows the existing layering exactly: schema defaults in `defaultSettings()` (`settings.go:1133`) → `~/.xdev/agent/config.yml` → project `.xdev/config.yml` → `--config`. Naming follows `TabSettings`: **spelled the way the underlying tool spells it**, with unknown-but-harmless keys accepted and ignored rather than rejected, so a config pasted from elsewhere stays loadable.

---

## 5. Work plan (implementation order)

### Slice 0 — Re-baseline opencode v2 (½ day, no product code)

The v1 research doc is one minor version behind and the process/API surface is what this plan rests on.

- Dump `opencode serve --help`, `opencode api --help`, `opencode service --help`, `opencode session --help` into `docs/research/`.
- Fetch the OpenAPI spec through the authenticated CLI and record its path, its auth model, and the operation ids for session list / session get / message send / event stream.
- Re-verify the stdio handshake: exactly one JSON line, Basic auth, SIGTERM-then-force-kill.
- Record which §1.3 env levers still exist in 2.0.32 (they were read from the binary today, but confirm against the spec, not the bundle).
- Update `docs/research/opencode-internals.md`'s version header and add a short "what changed in v2" note rather than rewriting the whole inventory.

**Exit:** a spec file and a note, both in-repo, both dated.

### Slice 1 — The opencode client (F1, F5, F8) — **CLI-first, see D1**

A new package (`internal/opencode`) that owns exactly one job: turn a task into an opencode session and report status, without leaking opencode's shape.

- Transport: shell out to `opencode api <operationId | METHOD /path>`, via the same `Lookup` seam `internal/tts/tts.go:108` uses, so tests substitute a fake binary. xdev does not hold opencode's password (D1).
- The F1 content-type + `_tag` gate applies to the CLI's **stdout**: an HTML SPA shell or a non-JSON body is an error, never an empty result. The CLI turns a rejected request into a non-zero exit and an error on stderr, so exit status is the first gate and the content check is the second.
- `Config`: model/variant and an optional `--server` override. Resolved by a discovery helper that returns `unknown` rather than guessing (F8).
- Discovery order: (a) explicit `--server`/config; (b) `opencode service status` for a URL; (c) `unknown`. Under D1 the password is the CLI's problem, so the discovery helper never reads `~/.config/opencode/service.json`.
- `ListSessions`, `SessionStatus`, `SendTask` — the minimum three. No transcript method. The absence of a transcript method is the enforcement mechanism for §4.1 rule 1.

**Exit:** unit tests green, including the HTML-`200` and `401` cases.

### Slice 2 — Workflow presets in config (F5, F4)

- `WorkflowSettings` + `WorkflowPreset` in `internal/config/settings.go`, following the `TabSettings` comment discipline.
- Zero-skip merge, `defaultSettings()` entries, and a `Preset(name)` lookup.
- Validation at load: unknown `backend`, unknown context key, or a preset missing `context` → a named load error, not a silent drop. (A malformed *file* is still preserved as `.broken-<stamp>` per existing behaviour.)
- A `xdev workflow list` / `xdev workflow show <name>` subcommand so presets are inspectable without the TUI — the TUI is not the only way to see the config.

**Exit:** a partial project overlay changes only the keys it names; a broken preset names itself in the error.

### Slice 3 — Supervised child + service surface (F2, F3, F6)

- `serviceOpencode = "opencode"` in `internal/serve/serve.go:52-56`; `IsService` (`:96`) and `Dispatch` (`:104`) extended.
- The child supervisor copies the `internal/tts/tts.go:108` `Lookup` seam so tests substitute a fake `opencode` binary — no network, no real server, in unit tests.
- stdio mode only (F2). Fresh random password per child; `OPENCODE_PASSWORD` in the child env; `SIGTERM`, force-kill after 3 s; on exit, reap and invalidate the endpoint.
- xdev's own broker surface (if any is exposed) is token-gated with the existing `ensureToken`/`requireToken`; the opencode password never enters that path (F3).
- `xdev serve status opencode` following `cmd/xdev/gatewaycmd.go`'s shape. **No** OS service unit for the child — stdio mode's whole point is that the child dies with xdev; a launchd unit would fight §R2.

**Exit:** fake-binary test proves one-JSON-line resolution, second-line rejection, and signal ordering.

### Slice 4 — The display-only sidecar pane (F7) — **gated**

Blocked until the TUI WIP (`hubdock.go` + friends) settles and the WIP is committed. Do not start earlier.

- A bottom pane on the `hubdock.go` template: sticky below the composer, Alt-key toggle, `hubDockRefresh`-shaped bounded poll, `hubDockMaxRows`-shaped row budget with "+N more", hit table + click handler for consistency.
- Content is metadata only (F7): title, agent, model+variant, cost, tokens, outcome, idle. Clicking a row runs the preset / opens the session the same way `hubdockMouse` does for a child — it does **not** stream opencode's transcript.
- Empty state paints zero rows (no "no background agents yet" banner — that is exactly the bug the reference plan's R2 documents).

**Exit:** TUI package tests green; a drive script in the style of `scripts/tui-subagent-panel-drive.py`.

### Slice 5 — Preset picker (F4 visibility)

- Picker lists presets from config with their declared budget fields.
- Launch dispatches through the Slice 3 child + Slice 1 client; the running preset shows in the Slice 4 sidecar.
- The picker is the only place the §4.3 rule 4 budget display lives.

---

## 6. Files likely touched (when implementing — not in this docs PR)

| File | Change | Slice |
|---|---|---|
| `docs/research/opencode-internals.md` | version header + v2 delta note | 0 |
| `docs/research/opencode-v2-spec.md` (new) | spec dump, auth model, stdio handshake, lever list | 0 |
| `internal/opencode/` (new) | config, discovery, transport with F1 gate, `ListSessions` / `SessionStatus` / `SendTask` | 1 |
| `internal/opencode/client_test.go` (new) | HTML-`200`, `401`, `_tag`, no-transcript-method | 1 |
| `internal/config/settings.go` | `WorkflowSettings`, `WorkflowPreset`, defaults, zero-skip merge, validation | 2 |
| `cmd/xdev/workflowcmd.go` (new) | `workflow list` / `show` / `run` | 2, 5 |
| `internal/serve/serve.go` | `serviceOpencode`, `IsService`, `Dispatch` case | 3 |
| `internal/serve/opencode.go` (new) | stdio child supervisor, `Lookup` seam, signals | 3 |
| `internal/serve/opencode_test.go` (new) | fake binary: one line / two lines / no line / signal order | 3 |
| `internal/tui/opencodedock.go` (new) | sidecar pane, metadata rows only | 4 |
| `internal/tui/opencodedock_test.go` (new) | geometry, budget, empty state | 4 |
| `scripts/tui-opencode-dock-drive.py` (new) | TUI drive | 4 |
| `docs/decisions/opencode-backend-service.md` (new) | ADR for the supervisor shape + the context contract | 3 |

**Constraints carried from the reference plan §6:** no new third-party dependency (opencode is reached through its own CLI and its own loopback server, both already on the machine); no second transcript renderer; no change to `SpawnChild`'s yield contract; **no edits to the in-flight TUI WIP files listed in §R7**.

---

## 7. Verification checklist (definition of done)

- [ ] `go test ./internal/opencode/ ./internal/serve/ ./internal/config/ ./internal/tui/ ./cmd/xdev/ -count=1` green
- [ ] `go build -o /tmp/xdev ./cmd/xdev` succeeds
- [ ] F1 table test passes: HTML `200` and `401` are errors, JSON `200` is success
- [ ] F2 test: fake opencode emits one JSON line → resolves; two lines → fails loudly; SIGTERM before any force-kill
- [ ] F3: grep the xdev config tree for the child password — zero hits
- [ ] F4: a preset with `context.disable_claude_code: true` results in that env var on the child; the picker prints the budget
- [ ] F5: a project overlay naming two keys changes exactly two keys
- [ ] F7: the pane test asserts no transcript-shaped field is read; empty state paints zero rows
- [ ] `xdev serve status opencode` reports unknown cleanly when no server is running
- [ ] Manual: pick a preset from the TUI, watch the sidecar fill, confirm zero transcript surface
- [ ] Manual: confirm the opencode session row that appears is the one the preset created
- [ ] ADR written; PRD footer stamped
- [ ] No `Co-Authored-By` trailers; pre-commit hooks pass
- [ ] No CI workflow added — this repo has `release.yml` only, and adding a test job is a separate decision

---

## 8. Out of scope (do not sneak in)

- **Claude Code as a backend** — phase 2, per the agreed round split. Do not start the wrapper here.
- **ACP integration** (`opencode acp`) — a third, genuinely different seam (structured protocol, not HTTP). Deferred.
- **Streaming opencode transcripts into xdev** — this is the §4.1 rule 1 violation. The sidecar is display-only by decision, not by omission.
- **Input forwarding to the backend** — same decision as the transcript ban; the pane is read-only.
- **An OS service unit for the opencode child** — stdio mode's parent-owns-lifetime model is the design (R2).
- **Migrating opencode's config into xdev's** — xdev owns xdev's config; presets *set env*, they do not import `opencode.json`.
- **Built-in presets** — presets come from the user's config. Shipping defaults would make "named workflow preset" mean "fixed pipeline", which is what was explicitly ruled out.
- **Multiple concurrent backends** — one opencode child per xdev process.
- **New CI workflow** — this repo has one release workflow and no test/lint job; adding one is its own decision.

---

## 9. Decisions

### 9.1 Decided

**D1 — CLI-first for the opencode client. Decided 2026-10-18.**

xdev reaches opencode through `opencode api <operationId | METHOD /path>`, not through a hand-written HTTP client to `/api/*`.

Why:

- The CLI already owns the spec and the auth. `opencode api` accepts an OpenAPI operation id (§1.2), so xdev never transcribes a path or a response struct — which is exactly the liability R5 identifies, given the doc is one minor version behind the binary.
- It sidesteps F3's collision risk: xdev never holds opencode's Basic-auth password on the stdio path at all, because the CLI process is the one that does.
- The F1 gate (§4.2) still applies, one layer down: xdev parses the CLI's stdout and must reject an HTML SPA shell or a non-JSON body rather than read it as an empty result.

Accepted cost: one subprocess per call. At preset granularity — a task every few minutes, not a keystroke — this is not a bottleneck, and it is the cheaper mistake than coupling to an undocumented HTTP surface across a version gap.

Consequences for the slices:

- Slice 1's `internal/opencode` transport shells out via the same `Lookup` seam `internal/tts/tts.go:108` uses, so tests substitute a fake `opencode` binary. No network, no live server, no token fixture.
- The stdio child (Slice 3) still needs spawning — it is how a preset gets a server to talk to. But the *client* goes through the CLI even when xdev owns that server.
- HTTP-direct stays available later as a performance optimization behind the same interface, once a spec-derived client exists. It is not pre-built.

### 9.2 Still open (needed before Slice 1)

2. **Does xdev ever attach to a user's existing `opencode service` server, or only run its own stdio child?** Recommendation: **stdio child only** in round 1, with attach as a later opt-in. Attach means xdev inherits another process's lifetime and its pinned password (R2, F3). Note D1 narrows this: with a CLI-first client, attach only changes *which server* the CLI is pointed at (`--server <url>`), which is a much smaller surface than holding the password.
3. **Is the preset `context:` block mandatory or defaulted?** Recommendation: **mandatory for backend presets** (F5) — a preset with no declared budget is exactly the naive setup this round exists to prevent. Pure-local presets (no backend) may omit it.
4. **Does the sidecar ship in round 1 or wait for the TUI WIP?** Recommendation: **wait** (Slice 4, gated). Landing a pane on top of uncommitted concurrent work is how both efforts break.

---

## 10. Mapping: user request → this plan

| Request | Where it lands |
|---|---|
| xdev renders / displays / orchestrates AI tools | §4 overall; orchestrating = Slice 1 + 2, displaying = Slice 4, rendering = the existing TUI layer |
| pre-configured workflow, selectable from xdev | §4.4 — named presets in `~/.xdev/agent/config.yml`, merged by the existing layering |
| user tasks call Claude Code / opencode as backend services | §5 Slice 1 + 3 (opencode only, this round) |
| opencode must stay context-light | §1.3 levers + §4.3 four-rule contract + F4 |
| TUI wrapper around the backend | §4.3-adjacent Slice 4, display-only, bottom pane |
| background service supervision | §5 Slice 3 — `internal/serve`, stdio child, no OS unit |
| don't disturb the TUI WIP | §R7 + §6 constraints + Slice 4 gate |
| get approval before editing code | This document is the deliverable; nothing is implemented |

---

*Plan written 2026-10-18 against xdev HEAD `0dbfa67` on `main`, working tree carrying the uncommitted TUI WIP (`M .gitignore`, `M cmd/xdev/sessionops.go`, `M cmd/xdev/tui.go`, `M internal/tui/app.go`, `M internal/tui/hubroster.go`, `M internal/tui/keymap.go`; `?? cmd/xdev/sessionops_interrupted_test.go`, `?? internal/tui/hubdock.go`, `?? internal/tui/hubdock_test.go`). opencode facts verified against the installed v2.0.32 binary and its live loopback server on 2026-10-18; `docs/research/opencode-internals.md` still documents v1.18.28 and is re-baselined by Slice 0.*