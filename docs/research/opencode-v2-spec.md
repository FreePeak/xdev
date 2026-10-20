# opencode v2 — API surface, re-baselined from the shipped spec

*Date: 2026-10-18 · Source: the **installed** binary and its **live** loopback server · Replaces the version-stale parts of `opencode-internals.md`*

**Version: `opencode v2.0.32`.** `docs/research/opencode-internals.md` documents **1.18.28**. Its tool inventory, permission model, storage and compaction sections still hold; its **message model does not** — see §4.

This is the Slice 0 deliverable for `docs/plans/2026-10-18-opencode-backend-service-and-workflow-presets.md`.

---

## 1. Getting the spec

The spec is at the **root**, not under `/api`:

```
$ opencode api GET /openapi.json | wc -c
280292
```

| path | result |
|---|---|
| `/openapi.json` | **the spec** — OpenAPI 3.1.0, `info.title: "opencode HttpApi"`, `version 0.0.1` |
| `/api/openapi.json` | 404 |
| `/api/doc`, `/api/spec`, `/api/openapi` | 404 |
| `/doc` | the OpenCode **web UI** (HTML) |
| `/doc/openapi.json` | the **SPA shell** (HTML, `200`) |

Scale: **119 paths, 293 schemas.**

### 1.1 The spec declares no authentication

```json
"components": { "securitySchemes": {} }
"security": []
```

Not one operation declares a security requirement. A client generated from this spec **would send no credentials and get `401 UnauthorizedError` on every call.** Basic auth must be applied by hand. This is the single most likely way for a spec-driven client to fail silently against a working server.

### 1.2 The `200`-means-nothing trap, confirmed in the wild

`opencode api GET /doc/openapi.json` returns **`200` with `text/html`** — the SPA shell, not a spec. The web-UI handler routes `/api`, `/api/*`, `/auth/*`, `/openapi.json` to the API and serves `index.html` for everything else. Any client that infers success from a status code reads a typo as a healthy empty result. Enforce on content type, never on status.

---

## 2. Discovery (attach-only)

```
$ opencode service status
http://127.0.0.1:49374

$ opencode service get password
8j8AR3Y3lBSJjS2eZXZr_7hTL-oKVOmyamNsTwH73Zo

$ opencode service get url
Error: Unknown service config key: url

$ opencode service get          # no key
{}
```

There is **no single call** that yields url + auth + version. Compose `service status` + `service get password`. Both are sanctioned commands, so xdev never opens `~/.config/opencode/service.json` itself — which is the difference between attaching and rummaging.

Unknown `service get` keys error rather than returning empty, so key discovery is `service get` -> `{}` -> probe documented keys.

---

## 3. The event stream

```
event.subscribe    GET    /api/event
```

Declared SSE per `x-effect-stream: {"encoding": "sse"}`. **The declared envelope does not match what the server actually sends.** Captured from the live stream:

```
: heartbeat

data: {"id":"evt_14e1d058c001CLCjRQoJsL7tiB","created":1792311887244,
       "type":"session.reasoning.delta",
       "location":{"directory":"/Users/linh.doan/.config/opencode"},
       "data":{"sessionID":"ses_…","assistantMessageID":"msg_…",
               "ordinal":0,"delta":"Let me re-ground:"}}

data: {"id":"evt_…","created":…,"type":"session.tool.input.started",
       "location":{…},"data":{…},
       "durable":{"aggregateID":"ses_…","seq":2586,"version":1}}
```

Three places the spec is wrong:

| spec says | actually |
|---|---|
| the field is `event` | the field is **`type`** |
| `data` is `V2EventEncoded`, a JSON-encoded **string** | `data` is an **object**; no second parse needed |
| envelope is `{id?, event, data}` | it also carries **`created`**, **`location`**, **`durable`** |

So: do not generate a client from this operation's declared schema.

**Framing is bare `data:` lines**, not the `event:`/`id:` SSE fields — the identifier lives inside the JSON. Idle keepalive arrives as an SSE comment, `: heartbeat`.

**`durable` is the reconciliation key, and it is not always there.** In a 28-event sample, 18 carried it. Per session it is `{aggregateID, seq, version}`, with `seq` monotonic and `version` for idempotency. Events *without* it — every `*.delta` — are ephemeral and cannot be replayed. That is the shape of F2: a reconnect reconciles from `session.message.list`, not by replaying deltas, because the deltas that would fill the gap are gone.

### 3.1 Event types observed

Namespaced `server.*`, `project.*`, `session.*`, `shell.*`:

```
server.connected              data {}
project.updated
session.created               {sessionID, slug, version, projectID, location, subpath, title, agent}
session.step.started|streamed|ended
session.reasoning.started|delta|ended          delta carries {ordinal, delta}
session.tool.input.started|ended               {id, name}
session.tool.called                            {id, input, executed}
session.tool.success                           {id, content[], metadata, executed}
session.usage.updated                          {sessionID, cost, tokens{input,output,reasoning,cache}}
session.synthetic
session.compaction.started
shell.exited | shell.deleted
```

Payload shapes worth pinning:

- `session.tool.called.input` is the tool's parsed **object** input.
- `session.tool.success.content` is `[{"type":"text","text":"…"}]` — the same `Tool.Content` union as the REST message model.
- `session.usage.updated` is the live cost/token roll-up, and matches `Session.Info.tokens` exactly.
- `location.directory` on the envelope says **which project** an event belongs to, so one stream can serve several working directories.

**The CLI cannot carry this stream.** Verified:

```
$ opencode api GET /api/event          # a session was created mid-stream
bytes=0                                  # while connected
bytes=0                                  # after the process was killed
```

Control-plane calls through `opencode api` work fine; the event stream must go direct HTTP with Basic auth.

---

## 4. Message model — v2 changed this

v1 modelled content as a flat `parts` array on every message. **v2 does not.** Messages are a **typed union discriminated by `type`**, and the union is asymmetric:

### 4.1 User message — flat string, no blocks

```json
Session.Message.User: {
  "id", "metadata",
  "time":   { "created": number },
  "text":   string,                          ← a plain string, NOT a content list
  "files":  Prompt.FileAttachment[],
  "agents": Prompt.AgentAttachment[],
  "skills": Prompt.SkillAttachment[],
  "type": "user"
}
```

### 4.2 Assistant message — a content union

```json
Session.Message.Assistant: {
  "id", "metadata",
  "time":   { "created", "streamed"?, "completed"? },
  "type": "assistant",
  "agent":  Agent.ID,
  "model":  Model.Ref,
  "content": [ Text | Reasoning | Tool, ... ]
}
```

Content item variants:

| variant | shape |
|---|---|
| `Assistant.Text` | `{type:"text", text, state?}` |
| `Assistant.Reasoning` | `{type:"reasoning", text, state?, time{created,completed}}` |
| `Assistant.Tool` | `{type:"tool", id, name, executed, providerState?, providerResultState?, state, …}` |

### 4.3 Tool state — four states, not two

| state | required fields |
|---|---|
| `Streaming` | `status`, `input` (**a string** — partial JSON) |
| `Running` | `status`, `input` (object), `metadata` |
| `Completed` | `status`, `input`, `content[]`, `metadata` |
| `Error` | `status`, `input`, `error` (`Session.StructuredError`), `content[]`, `metadata` |

`Tool.Content` is a small union: **`Tool.TextContent` | `Tool.FileContent`**. (v1's `patch` part is gone as a top-level part type; patches now arrive as file content inside a tool result.)

### 4.4 Other message types

`Compaction` (`Running`/`Completed`/`Failed`), `System`, `Shell`, `Skill`, `Synthetic`, `Idle`, `Retry`, `ProviderState`, plus control messages `AgentSelected`, `ModelSelected`, `LocationSwitched`.

### 4.5 Every schema is strict

`additionalProperties: false` throughout. A decoder must tolerate unknown *message types* (they will be added) even though it need not tolerate unknown fields within a known type.

---

## 5. Operations this project needs

| operationId | method | path |
|---|---|---|
| `session.list` | GET | `/api/session` |
| `session.create` | POST | `/api/session` |
| `session.get` | GET | `/api/session/{sessionID}` |
| `session.remove` | DELETE | `/api/session/{sessionID}` |
| `session.message.list` | GET | `/api/session/{sessionID}/message` |
| `session.message.get` | GET | `/api/session/{sessionID}/message/{messageID}` |
| `session.prompt` | POST | `/api/session/{sessionID}/prompt` |
| `session.switchAgent` | POST | `/api/session/{sessionID}/agent` |
| `session.switchModel` | POST | `/api/session/{sessionID}/model` |
| `session.interrupt` | POST | `/api/session/{sessionID}/interrupt` |
| `session.fork` | POST | `/api/session/{sessionID}/fork` |
| `session.compact` | POST | `/api/session/{sessionID}/compact` |
| `session.background` | POST | `/api/session/{sessionID}/background` |
| `event.subscribe` | GET | `/api/event` |

Note the send operation is **`session.prompt`**, not "message send".

### 5.1 Surfaces that matter for "control", beyond prompting

| operationId | path | why it matters |
|---|---|---|
| `session.permission.list` / `.reply` | `/api/session/{id}/permission[/{requestID}/reply]` | opencode **asks the user for permission**; a wrapper that cannot reply is not in control |
| `session.form.list` / `.get` / `.reply` / `.cancel` | `/api/session/{id}/form…` | opencode can ask an interactive question mid-turn |
| `session.inbox.*` | `/api/session/{id}/inbox…` | queued deliveries (user, compaction, move, synthetic) |
| `session.interrupt` | `/api/session/{id}/interrupt` | the stop control |
| `session.diff` | `/api/session/{id}/diff` | what the turn actually changed |

`session.permission.*` and `session.form.*` were **not** in the v1 research doc. Both are blocking interactions — a session can sit waiting for a human, and a display-only wrapper would hang forever. Any control surface must render these and reply.

### 5.2 `session.create` request body

```json
{ "id"?, "parentID"?, "title"?, "agent"?, "model"?, "location"?, "metadata"?, "permissions"? }
```

- **`agent` is a create field** — the plan's D2 lever holds, and it is set at creation rather than switched after.
- **`permissions` is also a create field**, typed `Permission.Ruleset`. A second per-session context lever, alongside `agent`.
- `location` is `Location.PublicRef` = `{directory: AbsolutePath}`.
- **There are no query parameters on this operation.** An earlier probe that passed `--param directory=…` had it silently ignored, and the session was created against the server's own cwd. Directory must go in the body's `location.directory`.

### 5.3 `session.prompt` request body

```json
{ "id"?, "text", "files"?, "agents"?, "skills"?, … }
```

---

## 6. Session row (`Session.Info`)

The list response is `{data: Session.Info[], cursor}`.

```
Session.Info props: id, parentID, fork, projectID, agent, model, cost,
                    tokens, outcome, time, title, subpath, metadata,
                    permissions, revert, location
required:            id, projectID, cost, tokens, time, location
```

`tokens` is `TokenUsage.Info` (`input`, `output`, `reasoning`, `cache.{read,write}`).

This row already carries everything a roster or dock row needs — title, agent, model, cost, token accounting, outcome, idle time, directory — so **a session list needs no message reads at all**. Sample from the live server:

```json
{"id":"ses_eb24d9e1…","agent":"build",
 "model":{"id":"space-bunny-free","providerID":"opencode","variant":"high"},
 "cost":0.00002816,
 "tokens":{"input":925544,"output":164503,"reasoning":20185,"cache":{"read":24303175,"write":0}},
 "outcome":"succeeded",
 "time":{"created":…,"updated":…,"idle":…,"viewed":…},
 "title":"xdev high-level upgrade with background services",
 "location":{"directory":"/Users/linh.doan/work/harvey/freepeak/xdev"}}
```

---

## 7. Corrections to the plan this re-baseline forces

The plan (`docs/plans/2026-10-18-…md`) asserted that opencode content is "byte-identical" to xdev's `internal/ai` blocks. That is **half true and needs narrowing** before Slice 4 is built:

| side | claim | verdict |
|---|---|---|
| assistant `content[].text` | `{"type":"text","text":…}` == xdev `ai.TextBlock` wire shape | **correct** |
| assistant `content[].reasoning` | `.text` -> `ai.ThinkingBlock.Thinking` | **correct**, one key rename |
| assistant `content[].tool` | -> `ai.ToolCallBlock` + result; 4 states | **correct**, richer than v1 |
| **user message** | "parts map onto blocks" | **wrong** — `Session.Message.User.text` is a **flat string**, not a content list |

So the mapping is **asymmetric**: the assistant side is nearly free, the user side needs a one-line wrap into a single `TextBlock`. The conclusion "map, don't build a second renderer" survives, and is arguably strengthened — but it must not be implemented on the v1 mental model.

Two more implementation consequences:

1. **Basic auth is manual.** §1.1 — no spec-driven client will authenticate.
2. **`data` on every SSE event needs a second JSON parse.** §3.

---

## 8. Reproducing this

```sh
opencode --version                                  # v2.0.32
opencode service status                             # url
opencode service get password                       # basic-auth secret
opencode api GET /openapi.json > oc-openapi.json    # the spec
opencode api GET /api/session                       # Session.Info rows
opencode api POST /api/session -d '{"agent":"build"}' \
  --param directory=/some/path                     # WRONG: --param is ignored here
```

Prefer `opencode api <operationId>` over a raw path wherever the spec lists one.

---

*Verified 2026-10-18 against opencode v2.0.32 at `~/.opencode/bin/opencode` and the live server on `127.0.0.1:49374`. No opencode file was modified; discovery used only `opencode service status` and `opencode service get password`.*