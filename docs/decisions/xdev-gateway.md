# Decision: the `xdev gateway` — Telegram-only background agent bridge

*Date: 2026-10-10 · Milestone: M15 tail (services) · Basis: [research/2026-10-10-gateway-telegram.md](../research/2026-10-10-gateway-telegram.md) · Status: **A adopted — worker-process per turn***

**Decision in one line:** `xdev gateway` is a small foreground/daemon process that
owns a Telegram long-poll loop, maps each chat to one xdev session, and runs each
turn as a **detached worker process** of the same binary — reusing the `--bg`
detach machinery that already exists instead of rebuilding an agent inside the
daemon. Memory is the existing remote backend pointed at a live **LeanKG**
server, requiring no new backend code at all.

---

## 1. Context

The repo ships three loopback services today (`internal/serve`:
auth-broker, auth-gateway, browser-relay) and a detached-print mode (`--bg`,
`internal/dist/proc_unix.go`). What is missing is the one thing the user asked
for: drive xdev from a phone, in the background, with its memory growing over
time. Hermes solved exactly this in Python; Prime Agent re-solved the process
model in Rust and measured what it buys.

---

## 2. Options

| Option | Shape | Verdict |
|---|---|---|
| **A — Worker process per turn** | The daemon holds no agent state. Each inbound message spawns `xdev print` detached (like `--bg`), collects its log, chunks and sends the reply. Session id is passed with `--resume`. | **Recommended.** Smallest daemon possible; Prime Agent's isolation lesson for free; a wedged provider is a `stop`d process, not a daemon crash. |
| B — In-process agent | Port the print-mode agent build into a long-lived goroutine per chat. | Rejected: duplicates `print.go`'s ~400-line wiring in a second place; a stuck turn takes the daemon down. |
| C — RPC mode per chat | `xdev rpc` child per chat, JSONL over pipes, stream events. | Better streaming, far more protocol surface (frames, ids, cancellation) than a phone chat needs. Rejected for now; the upgrade path if event streaming is wanted. |

---

## 3. Design

### 3.1 Process shape

```
xdev gateway            daemon: getUpdates loop, per-chat lease, worker spawn
                              │
                              ├── getSession(chatID) → session id (map on disk)
                              ├── spawn: xdev print --resume <id> "<text>"   (detached)
                              └── tail <id>.log → reply → sendMessage (4096 chunks)
```

One lease per chat id (`acquire(chat)` / `release(chat)`), fail-closed on a
busy chat: the reply is "still working on the previous one — /stop to cancel".
That is Hermes' `TurnLeaseRegistry` behavior, ~60 lines of Go.

### 3.2 The three surfaces

| Surface | Command |
|---|---|
| run | `xdev gateway` (foreground), `xdev gateway start` (background), `stop`, `status`, `logs` |
| install | `xdev gateway setup` — writes the token, allowlist, memory URL into `~/.xdev/agent/config.yml` under a new `gateway:` group, then offers launchd (macOS) / systemd (Linux) |
| status | `xdev ps` reports it as a `gateway` daemon row |

### 3.3 Config (new `gateway:` settings group)

| Key | Default | Meaning |
|---|---|---|
| `enabled` | false | master switch |
| `allowedChats` | (empty) | numeric chat ids; **empty = deny all** (fail-closed) |
| `allowedAll` | false | accept any chat (single-user box opt-in) |
| `workspace` | `~/work` | cwd the worker turns run in |
| `pollTimeout` | `50s` | long-poll timeout |
| `leaseWait` | `5s` | how long a second message waits for the chat's lease |
| `replyChunk` | `4000` | sendMessage chunk size in UTF-16 code units (Telegram's limit is 4096) |
| `model` | (session model) | model ref for the worker |

Secrets never enter `config.yml`: the bot token comes from
`TELEGRAM_BOT_TOKEN` (env) or `~/.xdev/agent/gateway.token` (0600), the same
split `internal/serve` uses.

### 3.4 Memory — LeanKG, zero new backend code

The gateway's workers are ordinary xdev runs, so they take the ordinary memory
settings. Pointing them at the already-running LeanKG REST listener on this box
(`--rest :9700 --hindsight-compat`, measured 2026-10-10:
`GET /v1/default/banks/omp/stats` → 233 entries) completes the objective with
no code:

```yaml
memory: hindsight
hindsight:
  apiUrl: http://127.0.0.1:9700
  bankId: xdev
```

This is Option A from [leankg-memory-backend.md](leankg-memory-backend.md),
already half-shipped. What this change adds is the **documentation of it as the
gateway's memory story** plus `xdev gateway setup` writing those keys so a
fresh install gets memory growth for free.

---

## 4. What this deliberately does NOT do (and why)

- **No webhook mode.** Long polling needs no public URL, no TLS, no cert.
- **Streaming into the chat — SHIPPED 2026-10-13.** The `ponytail:` above was
  the right forecast and the wrong size: the preview is `internal/gateway/stream.go`
  (`preview` + `runPreview` + `deliver`), plus `SendMessageID`/`EditMessageText`
  on the client. The worker's **stdout** is teed into a preview (`io.MultiWriter`
  with the existing ring buffer) and painted into ONE message: the first chunk
  `sendMessage`s it, later chunks `editMessageText` the same id at most every
  `gateway.streamIntervalMs` (default 1.5s — Hermes' `DEFAULT_STREAMING_EDIT_INTERVAL`
  is 0.8s, its turn runner uses 1.5s), and the finished answer is edited INTO
  that message with any overflow as follow-ups. stderr is untouched: it stays
  the diagnostics buffer and the place a model-switch notice is read from.
  - Why stdout and not the ring buffer: the old single buffer held both streams,
    so an answer could not be told from narration. Splitting them is what makes
    "paint only what the model said" true by construction.
  - A preview that outgrows 4096 UTF-16 units is CUT with a marker, never split:
    splitting moves the edit target, and the next tick re-edits the whole text
    into the new message — the duplication loop Hermes documents at
    `adapter.py:3880`. The finished answer is split properly, where there is no
    next tick.
  - One 429 ends the preview (`flooded`): Telegram's limiter is per chat and
    editing into it escalates the penalty. Hermes backs the interval off instead
    (`stream_consumer_transport.py:586`) — the upgrade path if this ever trips.
  - Three 400s are success, not failure: "message is not modified" (the text is
    already on screen), a deleted message, and one past the edit window.
  - `gateway.stream: false` restores the old behaviour exactly (one reply at the
    end); the preview is not even created, so no API call changes.
- **No message editing beyond the answer itself, but yes the typing bubble.**
  While a turn runs the daemon re-posts `sendChatAction` every 2s
  (`startTyping`, `gateway.go`), the same keepalive Hermes' gateway runs.
  Cheap (one small POST per 2s), non-fatal on failure, and it stops with the
  worker — `/stop` included. Shipped 2026-10-11.
- **No media.** Text in and out; a photo-upload path is a worker-side feature.
- **No session subprocess reuse across daemon restarts.** Workers are per-turn;
  the session *file* is the durable state, which is what makes this safe.

---

## 5. Files

| File | Role |
|---|---|
| `internal/gateway/telegram.go` | Bot API client: `getUpdates`, `sendMessage`/`SendMessageID`, `editMessageText`, `sendChatAction`, UTF-16 chunking |
| `internal/gateway/stream.go` | the answer preview: tee the worker's stdout, paint one message, finish it in place |
| `internal/gateway/chats.go` | per-chat lease, session pointer, model pin |
| `internal/gateway/gateway.go` | the run loop, the worker spawn, the reply path |
| `internal/gateway/setup.go` | installer (config + launchd/systemd unit) |
| `internal/gateway/*_test.go` | httptest Bot API server, chunking, lease, allowlist, preview |
| `cmd/xdev/gatewaycmd.go` | the `xdev gateway` subcommand |
| `internal/config/settings.go` | the `gateway:` group + merge arm |
