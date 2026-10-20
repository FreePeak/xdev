# Plan: xdev as an attach-only wrapper around the user's opencode server

*Date: 2026-10-18 · Status: **plan only** (no code in this change) · Branch: `docs/plan-opencode-backend-service` · PR [#671](https://github.com/FreePeak/xdev/pull/671)*
*Supersedes the supervised-child design in the first revision of this document. Round scope: opencode only; the Claude Code wrapper is phase 2.*

Related:

- `docs/research/opencode-internals.md` (v1.18.28 inventory — **version-stale**, see §1.0)
- `docs/research/opencode-v2-spec.md` (the v2 re-baseline this plan's §2.5 and §3 were corrected against)
- `docs/plans/2026-10-14-open-subagent-session-in-main-tui.md` (format mirror; source of the "no second transcript renderer" constraint)
- `cmd/xdev/tabset.go` — **the multi-session mechanism this plan reuses, not rebuilds**
- `internal/ai/types.go` — **the block model this plan maps onto**
- `internal/agent/task.go:21` — the yield-only isolation precedent behind D3
- `docs/decisions/live-subagent-view.md`, `docs/decisions/xdev-gateway.md`

---

## Implementation status (as of PR #672, branch `feat/opencode-attach`)

| Slice | State |
|---|---|
| 0 — re-baseline opencode v2 | done — `docs/research/opencode-v2-spec.md` |
| 1 — control plane | done — `internal/opencode/client.go` |
| 2 — workflow presets | done — `internal/config/workflow.go`, `xdev workflow` |
| 3 — event stream | done — `internal/opencode/events.go` |
| 4 — part→block adapter | done — `internal/opencode/adapter.go` |
| 5 — sessions as tabs | done — `internal/opencode/bridge.go` |
| 6 — session roster | **data layer + CLI done** (`registry.go`, `xdev workflow sessions`); the **TUI dock pane is still gated** on the in-flight TUI WIP |

Three things implementation changed relative to this plan. All three are recorded where they were discovered rather than papered over:

1. **§2.5's "byte-identical wire shape" was half right.** v2 replaced v1's flat `parts` array with a `type`-discriminated message union, and a user message's text is a plain string rather than a content list. The assistant side still matches exactly. See §2.5 and the spec's §4.
2. **§3's declared event envelope is wrong in three places.** The live stream uses `type` (not `event`), an object `data` (not a JSON-encoded string), and adds `created`, `location` and `durable`. Do not generate a client from that operation's declared schema. See `docs/research/opencode-v2-spec.md` §3.
3. **Slice 5 needed no `tabset.go` change at all.** `tabset.open()` already takes a `*session.Store`, so a bridged session *is* a tab — a smaller change than the plan assumed, and one that kept the slice out of the TUI files that are mid-flight.

`R3` (no xdev model ever receives opencode transcript content) is enforced structurally: the adapter's only outputs are `session.Entry` values destined for a `Store`, and the client exposes no method that returns transcript content to a model-callable surface.

---

## 0. One-sentence outcome

Make xdev a realtime, multi-session window onto the opencode server the user already runs — attaching only, spawning nothing, owning nothing, and rendering opencode's messages through xdev's existing transcript model rather than a second renderer.

---

## 1. The governing principle

> Everything that belongs to opencode stays in opencode. xdev asks, listens, and displays. It does not spawn opencode, signal it, configure it, restart it, or read its files behind its back.

This is a **hard architectural constraint**, not a preference. It has teeth in four places:

| xdev must not | because |
|---|---|
| spawn `opencode serve` (stdio or service) | spawning is owning a lifecycle that isn't xdev's |
| send SIGTERM / SIGKILL, or call `opencode service start\|stop\|restart\|set` | same |
| edit `opencode.json`, `service.json`, or any `~/.config/opencode/*` file | their config is their territory |
| reimplement opencode's agent/permission/model resolution | their schema, their rules |

What xdev **may** do: run `opencode api`, `opencode service status`, `opencode service get password`, `opencode session list` — opencode's own commands — and speak HTTP to the loopback URL that `opencode service status` prints.

The first revision of this plan designed a supervised child (`opencode serve --stdio --port 0`, own the PID, SIGTERM then force-kill). That is deleted. §5 has no child slice.

---

## 2. What opencode actually ships (primary source, verified 2026-10-18)

Read from the **installed** binary (**v2.0.32**) and its **live** loopback server, not from the v1 research doc.

### 2.0 Version drift

`docs/research/opencode-internals.md` documents **1.18.28**; `opencode --version` is **2.0.32**. Slice 0 re-baselines.

### 2.1 The user already runs a server

```
$ opencode service status
http://127.0.0.1:49374

$ opencode service get password
8j8AR3Y3lBSJjS2eZXZr_7hTL-oKVOmyamNsTwH73Zo

$ opencode service get url
Error: Unknown service config key: url
```

`opencode service status` prints a bare URL. `opencode service get password` returns the Basic-auth password **through the sanctioned CLI** — so xdev never opens `~/.config/opencode/service.json` itself. That is the difference between "attaching" and "rummaging".

`opencode service get` (no key) returns `{}`. There is **no single call** that yields url + auth + version together — xdev composes them.

### 2.2 Two transports, and they are not interchangeable

| Plane | Transport | Verified |
|---|---|---|
| **Control** — create session, send message, read status | `opencode api <operationId \| METHOD /path>` | works; accepts an OpenAPI operation id |
| **Data** — live events, streaming messages, token/cost deltas | direct HTTP `<url>/api/event` + Basic auth | **required** — see below |

Verified control call:

```
$ opencode api POST /api/session -d '{"agent":"build"}' --param 'directory=...'
{"data":{"id":"ses_eb206e544ffe342uaINxXHqVtR","projectID":"d11f85e0...",
  "agent":"build","cost":0,"tokens":{"input":0,"output":0,"reasoning":0,
  "cache":{"read":0,"write":0}},"time":{...},"location":{"directory":"/Users/linh.doan"}}}
```

**Sessions carry an `agent` and a `directory`.** That is the per-session context lever — see §4.2.

### 2.3 `/api/event` streams; the CLI does not

Verified by differential against a known-bad path:

```
/api/nonexistent-xyz   exit=1   "HTTP 404 Not Found"      (fast)
/api/event             exit=124 (killed by timeout)      (streaming)
```

`/api/event` is a long-lived stream. But:

```
$ timeout 12 opencode api GET /api/event   # while a session was created
bytes=0                                       # while connected
bytes=0                                       # after the process was killed
```

**The CLI swallows the event stream entirely.** `opencode api` is fine for request/response and useless for realtime. This splits D1 (see §9.1): the data plane cannot go through the CLI.

### 2.4 The API is Basic-gated `/api/*`, and a `200` proves nothing

```
/api/session        401  {"_tag":"UnauthorizedError","message":"Authentication required"}
/api/openapi.json   401
/session            200  ...but Content-Type: text/html  ← the SPA shell
/doc                200  ...the OpenCode web UI
```

The web-UI handler routes `/api`, `/api/*`, `/auth/*`, `/openapi.json` to the API and serves `index.html` for **everything else**. A mistyped path returns `200` with HTML. Any client that trusts status codes reads a bug as a healthy empty result. Must-fix **F1**.

### 2.5 opencode parts are already close to xdev's blocks

`docs/research/opencode-internals.md` §4.1 (schema unchanged in v2 for these types):

| opencode message (v2) | key fields |
|---|---|
| `Session.Message.User` | `text` is a **plain string**, not a content list; plus `files`, `agents`, `skills` |
| `Session.Message.Assistant` | `agent`, `model`, and `content[]` of Text \| Reasoning \| Tool |
| `…Assistant.Text` | `{type:"text", text, state?}` |
| `…Assistant.Reasoning` | `{type:"reasoning", text, state?, time}` |
| `…Assistant.Tool` | `{type:"tool", id, name, executed, state}` |
| tool `state` | `Streaming` (input is a partial **string**) \| `Running` \| `Completed` \| `Error` |
| `…Compaction` | `Running` / `Completed` / `Failed` |

Compare `internal/ai/types.go`:

```go
type Block interface{ blockType() string }
type TextBlock struct{ Text string `json:"text"` }
// wire: {"type":"text","text":"..."}   <- identical to opencode's assistant text item
type ThinkingBlock struct{ Thinking string `json:"thinking"`; ... }
// opencode's reasoning item carries the same payload under the key "text"
type ToolCallBlock struct{ ... }  // id, name, arguments, partialArgs, streamIndex, intent
```

The mapping is **asymmetric**. Slice 0 corrected an earlier draft of this section that claimed it was uniform:

- **assistant side** — `{"type":"text","text":…}` is the same wire shape on both sides, so text and reasoning are a rename each, and tools carry *more* state than xdev's block.
- **user side** — `Session.Message.User.text` is a flat string. It needs a one-line wrap into a single `TextBlock`.

"Map, don't build a second renderer" survives this correction and is arguably strengthened. But it must not be implemented on the v1 `parts` mental model. Full detail, including two spec gotchas (the spec declares **no** security schemes, and SSE `data` is a JSON-encoded string needing a second parse), is in [`docs/research/opencode-v2-spec.md`](../research/opencode-v2-spec.md).

---

## 3. What xdev already ships — and the reason this is mostly reuse

### 3.1 `tabset` already solved multi-session

`cmd/xdev/tabset.go`, in its own words:

> tabset is the TUI's live session set: an ordered list of open sessions, one of them current. It replaces the one `store` variable plus the one process-wide `running` flag the TUI used to hold.
>
> The rule it exists to enforce: **a session that is not current KEEPS RUNNING.** A parked session's turn persists everything it produces and renders nothing (tuiHooks gates its App writes on `tuiSession.focused`), so its transcript is rebuilt from the store on switch-back.

```go
const tabSlots = 6
var errTooManyTabs = errors.New("too many open sessions (all slots busy)")
type tab struct {
    id, title string
    store     *session.Store
    running   bool
    cancel    ...
}
```

It is bounded (`tabSlots`), persisted to disk and reopened per-cwd, and explicitly modelled on opencode:

> opencode does the same thing one layer down: switching is route navigation (`session-tabs.tsx` select -> `route.navigate`) and the previous session's execution fiber is never torn down — only its events stop painting.

**So "show opencode sessions on the xdev TUI" is not a new feature. It is a new tab source.** Everything the user asked for — multiple concurrent sessions, switch between them, parked sessions keep running, survive restart — is already built and already shaped like opencode.

### 3.2 The transcript model is already the right target

`internal/session/store.go:38` `Store`, with `Entries() []Entry` (`:460`), `Append(Entry)` (`:538`), windowing (`maybeWindow` `:260`, `WindowStats` `:85`), auto-persist (`EnableAutoPersist` `:328`), and `Entry` as an interface discriminated by `Envelope` with `TypeMessage`, `TypeCompaction`, `TypeModelChange`, `TypeCustom`, …

`internal/tui/blocks.go` renders `*Block`. `internal/tui/render.go` handles declarative tool-output specs (card/table/tree) with **graceful degradation** — the documented best practice in this repo:

> specs DEGRADE GRACEFULLY: any parse failure falls back to the plain text the model already sees.

### 3.3 Yield-only isolation is existing policy

`internal/agent/task.go:21`: *"parent's context (PRD §5, yield-only isolation)"*, recorded in `docs/decisions/live-subagent-view.md:13` from omp's yield-only isolation.

D3 below extends an existing stance. It is not a new invention, and it is not fighting the architecture.

---

## 4. Decisions

### 4.1 D1 — split transport (revised 2026-10-18)

**Control plane: CLI-first.** `opencode api` owns the spec and the auth; xdev never transcribes a path or a response struct. Accepted cost: one subprocess per call, negligible at preset granularity.

**Data plane: direct HTTP.** `/api/event` with Basic auth, because the CLI provably drops the stream (§2.3). URL from `opencode service status`; password from `opencode service get password`. Neither touches opencode's files.

The CLI-first rationale holds where it was decided. It simply does not extend to streaming, and pretending otherwise would have produced a realtime feature that never fires.

### 4.2 D2 — the context budget is an **agent name**, not env

The first revision enforced the budget with `OPENCODE_DISABLE_CLAUDE_CODE`, `OPENCODE_DISABLE_PROJECT_CONFIG`, `OPENCODE_TOOL_GUIDANCE`. Those are read by the server **at boot**. Under attach-only, xdev cannot set them — ever, by design. That is the correct outcome, not a problem to route around.

What remains, verified in §2.2: **a session carries an `agent` and a `directory`.** An opencode agent's definition — prompt, model, variant, permission rules, MCP posture — lives in opencode's config (`~/.config/opencode/agents/*.md`, `.opencode/agents/<name>.md`).

So a preset pins an agent **by name**, and the leanness stays in opencode's files where it belongs:

```yaml
workflow:
  presets:
    - name: quick-fix
      backend: opencode
      agent: lean          # an agent opencode already defines; xdev never edits it
      directory: "{{.CWD}}"
      prompt: Fix the failing test in {{.File}}. Nothing else.
      context:             # MANDATORY on backend presets (D3)
        model: { providerID: opencode-go, modelID: glm-5.3-flash }
        mcp: []
```

xdev selects; opencode defines. If no agent matches, xdev says so rather than silently substituting.

### 4.3 D3 — three hard rules

**R1. Never a second renderer.** opencode parts are adapted into `internal/ai` blocks and appended to a `session.Store`. `blocks.go` renders them unchanged. This satisfies the constraint the reference plan set.

**R2. Attach only.** §1. No spawn, no signal, no `service start|stop|restart|set`, no config writes.

**R3. No xdev model ever receives opencode transcript content.** Transcript flows `adapter → session.Store → blocks.go → screen`, and terminates. This is yield-only isolation (`internal/agent/task.go:21`) extended from subagents to backend sessions.

R3 is the one that gets harder as this grows. With N realtime sessions and full control, "have xdev summarize what opencode did" is the obvious next feature and the exact thing that would void the round's premise. It is stated as a rule so it survives future pressure. **It can be relaxed by a new ADR, not by a feature request.**

---

## 5. Must-fix table

| ID | Must fix | Where | Test |
|---|---|---|---|
| **F1** | Every response is content-typed. HTML `200` and `401` are errors; only JSON is success. Never infer emptiness from a status code | transport | table test: `/session` (HTML 200) → error; `/api/session` 401 → error; JSON 200 → ok |
| **F2** | The event stream survives a reconnect: on stream drop, re-subscribe and reconcile from the last seen event id rather than silently losing output | stream client | drop mid-stream → reconnect → no gap, no duplicate |
| **F3** | Adapters are **lossless-forward**: an unknown opencode part type becomes a `TypeCustom` entry carrying the raw JSON, never dropped and never rendered as text | part→block adapter | unknown part type round-trips; no silent loss |
| **F4** | A backend preset without a `context:` block is a **named load error**, not a default | config | broken preset names itself |
| **F5** | Presets pin `agent` + `directory`; an unknown agent is reported, never silently substituted | config + client | missing agent → named error |
| **F6** | opencode sessions obey `tabSlots` and the parked-session rule — parked tabs keep receiving events into their store and paint nothing until focused | `tabset` | 7th tab refused while all busy; parked tab accumulates without painting |
| **F7** | No transcript read path exists that an xdev model can reach (R3). Enforced structurally, not by convention | package boundary | no method on the opencode client returns transcript content to a model-callable surface; a compile-time or boundary test asserts it |
| **F8** | Discovery degrades to `unknown` (no server / `service status` fails / password unavailable) — never a fabricated URL or an empty password | discovery | no server → `unknown` |

---

## 6. Work plan

### Slice 0 — Re-baseline opencode v2 (½ day, no product code)

- Dump `serve`/`api`/`service`/`session` help into `docs/research/`.
- Fetch the OpenAPI spec through the authenticated CLI; record operation ids for session create/get, message send, and the event schema for `/api/event` (including the event-id / resume semantics F2 needs).
- Confirm the part list in §2.5 still matches 2.0.32 and fix `docs/research/opencode-internals.md`'s version header.

**Exit:** spec + dated note in-repo.

### Slice 1 — Control plane (F1, F8)

`internal/opencode`:

- `Discover()` → `(url, password, ok)` from `opencode service status` + `opencode service get password`; `unknown` on any failure (F8). Never reads opencode's files.
- `CreateSession(agent, directory)`, `SessionStatus(id)`, `SendMessage(id, text)`, `ListSessions()` — all via `opencode api`.
- The F1 content gate on every response.

**Exit:** unit tests green, including HTML-`200` and `401`.

### Slice 2 — Workflow presets (F4, F5)

- `WorkflowSettings` + `WorkflowPreset` in `internal/config/settings.go`, following the `TabSettings` comment discipline (spelled the way the tool spells it; harmless unknown keys accepted, not rejected).
- Zero-skip merge; layering unchanged (`defaultSettings()` → `~/.xdev/agent/config.yml` → project → `--config`).
- `context:` mandatory on backend presets (F4); `agent` required (F5).
- `xdev workflow list|show|run` so presets are inspectable without the TUI.
- **No built-in presets.** Presets come from user config; shipping defaults would make "named preset" mean "fixed pipeline", which was ruled out.

**Exit:** a partial overlay changes only the keys it names; a bad preset names itself.

### Slice 3 — Event stream (F2)

- Subscribe to `<url>/api/event` with Basic auth. Bounded reconnect with reconciliation from the last event id (F2).
- `--print-logs` off; stderr drained and logged, never to the TUI.
- One stream for all sessions; events fan out to per-session stores. **One** stream, not one-per-tab — N tabs on one connection.

**Exit:** reconnect test shows no gap and no duplicate.

### Slice 4 — Part→block adapter (F1, F3)

`internal/opencode/adapter.go` → `[]session.Entry` over `internal/ai` blocks:

| opencode part | xdev block |
|---|---|
| `text` | `ai.TextBlock` — identical wire shape |
| `reasoning` | `ai.ThinkingBlock` (payload key `text` → `thinking`) |
| `tool` | `ai.ToolCallBlock` + result; `state.status` → pending/ok/error |
| `step-start`/`step-finish` | turn boundaries / `StopReason` |
| `file` | attachment, degraded rendering |
| `patch` | through the existing declarative renderer (`render.go`) |
| `compaction` | `session.TypeCompaction` — xdev already models this |
| anything else | `TypeCustom` with raw JSON (F3) |

Then `store.Append(...)` — the same path xdev's own sessions use. Windowing and auto-persist come free from `Store`.

**Exit:** adapter round-trips every part type; unknown types survive.

### Slice 5 — opencode sessions as tabs (F6)

- Extend `tabset` with an opencode tab source. `tab.store` is filled by the adapter, not by a local process.
- Parked-tab rule already implemented (tuiHooks gates on `tuiSession.focused`) — opencode tabs inherit it for free.
- Input: typing in a focused opencode tab sends via `SendMessage`. **This is the reversal** — the pane is no longer display-only.
- Respect `tabSlots = 6` (F6).

**Exit:** opencode tab renders, switches, survives restart; parked tab accumulates without painting.

### Slice 6 — Session roster dock

The `hubdock.go` bottom-pane pattern (`hubDockRefresh`, `hubDockMaxRows`, `hubDockBudget`, hit table, Alt-toggle) reused for a cross-session roster: title, agent, model, cost, tokens, outcome, idle.

- **Gated on the in-flight TUI WIP.** Do not start before `hubdock.go` and friends are committed.

---

## 7. Files likely touched

| File | Change | Slice |
|---|---|---|
| `docs/research/opencode-v2-spec.md` (new) | spec, auth model, event schema, part mapping | 0 |
| `docs/research/opencode-internals.md` | version header + v2 delta | 0 |
| `internal/opencode/` (new) | discover, control plane, F1 gate | 1 |
| `internal/config/settings.go` | `WorkflowSettings`, `WorkflowPreset`, validation | 2 |
| `cmd/xdev/workflowcmd.go` (new) | `workflow list|show|run` | 2, 5 |
| `internal/opencode/events.go` (new) | `/api/event` subscription, reconnect | 3 |
| `internal/opencode/adapter.go` (new) | part → `ai` block → `session.Entry` | 4 |
| `cmd/xdev/tabset.go` | opencode tab source | 5 |
| `internal/tui/` | input path for a focused opencode tab | 5 |
| `internal/tui/opencodedock.go` (new) | roster dock (gated) | 6 |
| `docs/decisions/attach-only-opencode.md` (new) | ADR: §1 principle + R3 | 5 |

**Not touched:** `internal/serve/` (no service surface), `internal/ai/types.go` (the model is already right), `session.Store` (already right).

**Constraints:** no new third-party dependency (opencode is reached via its own CLI + its own loopback server, both already on the machine); no second transcript renderer; no change to `SpawnChild`'s yield contract; **no edits to the in-flight TUI WIP**.

---

## 8. Verification checklist

- [ ] `go test ./internal/opencode/ ./internal/config/ ./internal/tui/ ./cmd/xdev/ -count=1` green
- [ ] `go build -o /tmp/xdev ./cmd/xdev` succeeds
- [ ] F1: HTML `200` and `401` are errors, JSON `200` is success
- [ ] F2: stream drop → reconnect → no gap, no duplicate
- [ ] F3: unknown part type survives as `TypeCustom` with raw JSON
- [ ] F4/F5: preset without `context:` names itself; preset with unknown `agent` names itself
- [ ] F6: 7th tab refused while all busy; parked opencode tab accumulates without painting
- [ ] F7: boundary test — no transcript-returning method reachable from a model-callable surface
- [ ] F8: no server running → `unknown`, not a fabricated URL
- [ ] Manual: `opencode service status` output matches the URL xdev shows
- [ ] Manual: preset run → session appears as a tab → messages stream in realtime → typing sends
- [ ] Manual: `grep` the opencode config tree — xdev wrote nothing to it
- [ ] ADR written; PRD footer stamped
- [ ] No `Co-Authored-By` trailers; pre-commit hooks pass
- [ ] No CI workflow added (`release.yml` only — a test job is a separate decision)

---

## 9. Out of scope

- **Claude Code as a backend** — phase 2, per the agreed split.
- **ACP** (`opencode acp`) — a third seam, structurally different. Deferred.
- **Spawning or supervising opencode** — R2. Deleted from the first revision.
- **Writing anything under `~/.config/opencode/`** — R2.
- **An xdev model reading or summarizing opencode transcripts** — R3. Relaxable only by ADR.
- **A second transcript renderer** — R1.
- **Built-in presets** — presets come from user config.
- **Editing opencode's agents, prompts, or permissions** — the preset selects; opencode defines.
- **New CI workflow.**

---

## 10. Mapping: request → mechanism

| Request | Mechanism | New code? |
|---|---|---|
| attach to existing server, don't touch their flow | §1 principle; discovery via `service status` + `service get password` | small |
| realtime metadata + messages + input/output | `/api/event` direct stream; one connection, fan-out | Slice 3 |
| control the opencode session | `SendMessage` on a focused tab; input forwarding | Slice 5 |
| spawn multiple opencode sessions via xdev workflow | `CreateSession(agent, directory)` per preset; `tabSlots` bound | Slices 2, 5 |
| show session data on the xdev TUI session | **existing `tabset`** — a new tab source, not a new mechanism | Slice 5 |
| don't rebuild the transcript | **existing `ai` blocks + `session.Store` + `blocks.go`** | Slice 4 |
| context-light | D2 (agent-by-name) + R3 (yield-only isolation) | §4.2, §4.3 |

Two of the seven rows above require no new mechanism at all. That is the main reason this revision is smaller than the first.

---

*Revised 2026-10-18 after codebase investigation; the supervised-child architecture of the first revision was deleted per the attach-only instruction. All opencode facts verified against the installed v2.0.32 binary and its live loopback server. Reuse claims verified at xdev HEAD `0dbfa67` on `main`: `cmd/xdev/tabset.go` (multi-session set, `tabSlots`, parked-session rule, opencode-shaped by design), `internal/ai/types.go` (block model), `internal/session/store.go` (`Store`, `Entry`, windowing, auto-persist), `internal/tui/blocks.go` (renderer), `internal/agent/task.go:21` (yield-only isolation). The in-flight TUI WIP was not modified.*