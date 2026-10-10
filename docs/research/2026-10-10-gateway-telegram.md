# Research — Telegram gateway design across Hermes Agent and Prime Agent (Rust)

Research date 2026-10-10. Sources: `hermes-agent` checkout at
`/Users/linh.doan/work/harvey/freepeak/hermes-agent`, the live HERMES deployment
on this machine (`~/.hermes/.env`), and the Prime Intellect engineering post
[Rewriting Prime Agent in Rust](https://www.primeintellect.ai/blog/prime-agent-rust)
(2026-10-09). Purpose: design decisions for the `xdev gateway` (Telegram-only,
background daemon, LeanKG memory backend).

---

## 1. Hermes Agent — Telegram gateway

### 1.1 Platform adapter seam

`gateway/platforms/base.py:1917` defines `BasePlatformAdapter(ABC)`; a
platform must implement:

- `connect(*, is_reconnect=False) -> bool` (`:2724`)
- `disconnect() -> None` (`:2730`)
- `send(chat_id, content, reply_to=None, metadata=None) -> SendResult` (`:2734`)
- `get_chat_info(chat_id) -> dict` (`:4857`)

Defaults provided by the base: `edit_message`, `delete_message`,
`create_handoff_thread` (`:2742-2756`).

`plugins/platforms/telegram/adapter.py:514` `TelegramAdapter`:

- **Inbound:** python-telegram-bot long polling
  `app.updater.start_polling(allowed_updates=Update.ALL_TYPES, drop_pending_updates=...)`
  (`:1789`). A webhook mode exists but polling is the default.
- **Outbound:** `send()` (`:3618`) → `format_message` + `truncate_message(..., 4096, utf16_len)`
  (`:3674`) → `_send_chunks` (`:3705`). `MAX_MESSAGE_LENGTH = 4096` (`:520`),
  length counted in UTF-16 code units (`message_len_fn -> utf16_len`, `:576`).
  Streaming delivery uses `edit_message` (`:3819`) with overflow continuation
  split (`:3960`).
- **Rate limits:** flood-control is fail-closed — `flood_control:<wait>`
  SendResult, inline-wait capped at 5 s (`_FLOOD_INLINE_WAIT_CAP_SECONDS = 5.0`
  `:189`), per-chat cooldown window (`_record_send_flood_cooldown` `:5514`),
  per-chat send lock for ordering (`:5487`).
- **Auth/allowlist:** `TELEGRAM_BOT_TOKEN` required
  (`plugins/platforms/telegram/plugin.yaml:13`, `gateway/config.py:399`).
  Optional: `TELEGRAM_ALLOWED_USERS`, `TELEGRAM_ALLOW_ALL_USERS`,
  `TELEGRAM_HOME_CHANNEL`, `TELEGRAM_ALLOWED_CHATS`.
- **Media:** ext→kind routing at `base.py:43-52`; large images pre-compressed
  to JPEG q85/max-1600px above 1 MB (`adapter.py:536-538`); caption cap 1024.

### 1.2 Runtime loop

- **Session keys:** `build_session_key` `gateway/session.py:685` —
  `<ns>:<platform>:<chat_type>[:chat_id][:thread_id][:user]`. DMs are isolated
  per `chat_id`; threads shared unless `thread_sessions_per_user`.
- **Turn lease (per-chat serialization):** `gateway/turn_lease.py`
  `SessionTurnLeaseRegistry` (`:74`), `DEFAULT_MAX_LEASES = 512`,
  `DEFAULT_LEASE_WAIT = 5.0s` (`:21-25`), keyed by resolved session_id,
  fail-closed `TurnLeaseTimeoutError` (`:32-39`). Acquired before transcript
  load (`run_turn.py:60`).
- **Turn execution:** `GatewayTurnMixin._handle_message_with_agent`
  `gateway/run_turn.py:2144` → `_run_agent` `:2928`, with stream consumer,
  interrupt monitoring, inactivity watchdog (`run.py:2717`), turn timeout
  reaping (`run.py:2685`).
- **Daemon ops:** `gateway/systemd_notify.py` — optional `sd_notify`
  (`READY=1`, `WATCHDOG=1` fed at interval/2, `STOPPING=1` once, `:73-108`);
  `gateway/scale_to_zero.py` — `HERMES_SCALE_TO_ZERO` + `FLY_APP_NAME` +
  idle 2 min default (`:24-37`).

### 1.3 Setup/install

`gateway/config.py:399`, `hermes_cli/setup_platforms.py:12` validates the bot
token shape `^\d+:[A-Za-z0-9_-]{30,}$`; the wizard writes it to
`save_env_value("TELEGRAM_BOT_TOKEN", token)` (a `.env` file, never the repo).
`TELEGRAM_ALLOWED_USERS` is a comma-separated allowlist of Telegram numeric
user ids.

### 1.4 Slash commands

`gateway/run_busy.py:907-922` dispatches by name; the idle set includes
`status`, `help`, `version`, `stop`, `topic`, `whoami`. Minimal useful set for
xdev: `/help`, `/status`, `/new` (fresh session), `/stop` (abort running turn).

---

## 2. Prime Agent (Rust rewrite) — lessons

From [blog post](https://www.primeintellect.ai/blog/prime-agent-rust):

- **Modularity:** code split into nine crates with a **one-way dependency
  graph**; largest source file ~2,500 lines (down from ~15,000).
- **Isolation:** each session runs in its **own worker process under a small
  supervisor** — a failure in one session leaves the others running, and
  sessions persist on disk so clients reattach after a restart.
- **Platform abstraction:** transport, process control and file locking sit
  behind platform-specific interfaces (Windows support = implementing traits).
- **One protocol definition:** client, daemon and workers compile against the
  same message types.
- **Measured wins over TypeScript:** ~14× faster cold start (736 ms → 52 ms),
  >80% less memory (1,130 MB → 237 MB process-tree RSS on a 10 MiB session).

Relevance to xdev: xdev is already a single static Go binary with bounded RSS
and the "extensions are processes" philosophy. The gateway adopts the one
lesson that fits: **a turn is a worker process**, so a wedged model call can
be killed without destabilizing the daemon, and session files on disk mean
turns survive daemon restarts.

---

## 3. xdev decisions that follow

| Concern | Hermes | xdev gateway |
|---|---|---|
| Inbound | `start_polling` | long-poll `getUpdates` with `timeout=50` (stdlib `net/http`) |
| Outbound | edit-message streaming | stream deltas into an outbound buffer; send completed message with 4096 UTF-16 chunking |
| Chunking | UTF-16 code units | same rule: count UTF-16 code units, split at max 4096, fence-aware |
| Allowlist | `TELEGRAM_ALLOWED_USERS` | same env/shape; numeric chat ids |
| Serialization | `TurnLeaseRegistry` | per-chat lease with fail-closed timeout (configurable) |
| Turn execution | in-process agent | **subprocess worker** (`xdev --bg`-style child), Prime Agent isolation lesson |
| Background | systemd/sd_notify | launchd plist (macOS) / systemd unit (Linux) via `xdev gateway setup` |
| Memory | hermes SQLite/FTS + MEMORY.md | `memory: hindsight` → **LeanKG** `--hindsight-compat` (zero new backend code) |