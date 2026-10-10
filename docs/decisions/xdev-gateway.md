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
- **No event streaming into the chat.** Print mode's reply arrives when the
  turn ends; Hermes' edit-message streaming is a nice-to-have that doubles the
  Bot API surface. `ponytail:` if this ships, it is a `sendMessage` per N
  seconds replaced by `editMessageText`, ~20 lines behind the existing sender.
- **No media.** Text in and out; a photo-upload path is a worker-side feature.
- **No session subprocess reuse across daemon restarts.** Workers are per-turn;
  the session *file* is the durable state, which is what makes this safe.

---

## 5. Files

| File | Role |
|---|---|
| `internal/gateway/telegram.go` | Bot API client: `getUpdates`, `sendMessage`, UTF-16 chunking |
| `internal/gateway/lease.go` | per-chat lease, fail-closed |
| `internal/gateway/session.go` | chat → session id map, one JSON file on disk |
| `internal/gateway/daemon.go` | the run loop |
| `internal/gateway/setup.go` | installer (config + launchd/systemd unit) |
| `internal/gateway/*_test.go` | httptest Bot API server, chunking, lease, allowlist |
| `cmd/xdev/gatewaycmd.go` | the `xdev gateway` subcommand |
| `internal/config/settings.go` | the `gateway:` group + merge arm |
