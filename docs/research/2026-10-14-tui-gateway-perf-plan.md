# xdev performance review — TUI lag, CPU, stalls, process leaks, gateway

**Date:** 2026-10-14  
**Product:** xdev · **Repo:** FreePeak/xdev  
**Scope:** TUI lag (~2 fps feel), high CPU, stall dumps, stale/orphan processes, gateway.  
**Method:** live process snapshot (2026-10-14 ~11:20), **325** `tui-stall-*.txt` dumps under `~/.xdev/agent/dumps/`, source on `main` / installed **v0.4.300**, prior RCAs (#284 / 2026-09-15 slow-session RCA, compact #635–#651).  
**Status:** research + fix plan for later implementation sessions — not a shipped code change, with one exception: **§2 RC-T4's cheap wins landed on `perf/tui-fps` (2026-10-14)** — the ASCII `width()`, the byte-index `wrapLine`/`cutAt` and the byte-compare `webURLStart`, measured at 8.7×/10×/15× in `internal/tui/render_bench_test.go` and recorded in `docs/PRD.md`. Every other section below is still open.

---

## 0. Executive summary

| Layer | Symptom you feel | Dominant cause (today) | Severity |
|--------|------------------|-------------------------|----------|
| **TUI loop** | 2 fps, freeze, keys lag after scroll | Single-threaded `handleKey`→`draw`→`Show`; `paint()` holds `a.mu` across full paint; mouse `expandBox` deadlocks under that lock; long transcripts still expensive per frame | **P0** |
| **TUI idle/CPU** | xdev never fully idle while “running” | 33 ms ticker (~30 fps) always armed; while `st.Running`, **every tick draws** (spinner) | **P0** |
| **TUI stalls** | Screen frozen, CPU 0% or spinning | ≥5 s no beat → dump; main often `select` (false-ish after sleep) **or** `Mutex.Lock` / tty `Show` | **P0** |
| **Process leaks** | Many MCP / `--print` children, hot Mac | Historical dual live runs per session + MCP per worker; **#648 fixed on main** — still need prune/reap/ops | **P1** |
| **Gateway** | Background heat / log spam | Daemon itself cheap; **workers** are full `xdev --print` + MCP; Telegram poll errors retry hot; **Hermes gateway still scheduled** | **P1** |
| **Browser/Chrome** | Extra CPU/RAM | Auto headless Chrome + **153 MB** private profile; user Chrome renderers separate | **P2** |
| **Disk/state** | Slow resume, clutter | 160 bg job dirs (25 MB), 325 dumps (6 MB), 141 sessions (~805 MB) | **P2** |

**Already fixed on main (and in v0.4.300)** — do not re-diagnose as open bugs:

- Compact auto-latch flap (#635–#638): latch fed its own compact row count → mode flip every frame, hundreds of re-renders + toasts.
- Thinking box ignoring `Expanded` / click freeze (#640).
- Cap must not write `compactFullRows` (#638/#651).
- Idle welcome sheen + anim cadence + **one live bg run per session** (#648).
- Welcome paint-and-sit (#649).

**Live snapshot (2026-10-14):** gateway ~0% CPU / 15 MB; 3 interactive `xdev` ~0.5–2% idle, ~75–90 MB RSS each, each with `atlassian-mcp` + `db-mcp-server`; no hot orphan `--print` army in that snapshot (earlier #648 measurement: 21+20 MCP + 8 orphans + 379 MB). Stall dumps still appearing **that day** (9 on 2026-10-14) → remaining P0 is **not** “missed compact fix.”

---

## 1. Evidence snapshot

### 1.1 Stall dumps (`~/.xdev/agent/dumps/`)

- **325** files total; peak days 2026-10-01 (103), still **34/day** on 10-12/13, **9** on 10-14.
- Recent main-goroutine states (sample of 40): `select` 20 · `runnable` 12 · `Mutex.Lock` 6 · `syscall` 2.
- Recent signature mix (80 dumps): almost all stacks include `theme.Watch` (background FS watch — **noise**, not the stall); real signals = **tty write / Show / draw**, **mutex + expandBox + mouse**, agent loop / bash still alive underneath.
- Beat lag typically **6–10 s** (watchdog threshold 5 s); historical dumps went to **minutes** on blocked tty write.
- Hot code paths in stacks: `internal/tui/app.go`, `agent/loop.go`, `tool/bash.go`, `agent/mailbox.go`, MCP `jsonrpc2` readers (always-on children).

### 1.2 Architectural smoking guns (source)

```text
Run loop:  33ms ticker + keyq + dirty
  event  → handleKey → drainKeys(keyBurst) → draw() → beat
  dirty  → draw()
  tick   → if running || animate || stuck → draw()   // running ⇒ full 30fps paint

draw():
  paint()           // Lock a.mu for ENTIRE paint (defer Unlock)
  markComposerDirty()
  scr.Show()        // sync tty flush — can block minutes (historical)
  maybe Sync() on dropped frame

Agent stream:
  OnEvent → paint(func(){ AppendAssistant })  // contends for same a.mu
  poke() → non-blocking dirty signal
```

`internal/tui/rowindex.go` documents the long-session cost: pre-index work measured **7.8 ms/frame @ 85k rows** at 30 fps while running = sustained core slice + “2 fps” feel when frames exceed budget.

Mouse path still stalls on lock:

```text
handleMouse → expandBox → a.mu.Lock   // while paint()/other holder keeps mu
// dumps 2026-10-13 10:0x: goroutine 1 [sync.Mutex.Lock] in expandBox
```

Key locations (v0.4.300 / main tip at review time):

| Concern | Location |
|---------|----------|
| UI loop + 33ms tick | `internal/tui/app.go` `Run()` (~2946+) |
| `draw` / `paint` / `Show` | `internal/tui/app.go` `draw()` (~4705), `paint()` (~4763) holds `a.mu` |
| Stall watchdog | `internal/tui/stall.go` (`stallAfter` 5s) |
| Row index / long session | `internal/tui/rowindex.go` |
| Stream hooks | `cmd/xdev/tui.go` `tuiHooks.OnEvent` |
| Bg one-live-run | #648 / bg spawn path |
| Gateway spawn | `internal/gateway/gateway.go` `spawn` / `turn` |
| Browser launch | `internal/browser/launch.go` |

### 1.3 Process / gateway / disk

| Item | Observation |
|------|-------------|
| `xdev gateway run` | Idle, Setsid workers, lease per chat — **good design**; log full of Telegram `no route to host` / i/o timeout |
| `ai.hermes.gateway` | LaunchAgent **spawn scheduled** (active count 0 at review) — competing bot stack risk if it comes up |
| Interactive xdev ×3 | Each owns MCP pair; multi-session = multi-MCP tax |
| `~/.xdev/agent/bg` | **160** job dirs; almost all finished; prune exists (`xdev bg prune`) but not automatic enough |
| Browser profile | **153 MB** under `~/.xdev/agent/browser/profile` |
| Sessions | ~**805 MB** / 141 files |
| Stall dumps | **6 MB** / 325 files — should rotate harder |

### 1.4 Prior RCA still true

See [2026-09-15-xdev-slow-session-rca.md](2026-09-15-xdev-slow-session-rca.md) (#284):

1. UI freeze → agent event delivery blocked on `a.mu` → stream idle watchdog → synthetic “cut off” user turn → extra turns / felt slowness.
2. Watchdog must measure **SSE liveness**, not UI consumer liveness (#283 direction).
3. Holding work across `Show()` is fatal on full tty buffers / sleep / tmux.

---

## 2. Root-cause catalog (fix targets)

### RC-T1 — UI loop is the throughput ceiling (P0)

- One goroutine owns input + layout + paint + tty flush.
- Any slow frame stalls keys, mouse, quit chord (partially mitigated: quit while `wedged`).
- **Fix theme:** producer/consumer split
  - **UI thread:** input, state mutate, schedule frame.
  - **Render thread (or tick-only):** build frame plan / cell buffer under short lock or lock-free snapshot.
  - **Flush thread / async writer:** `Show` with timeout + drop-coalesce (already have `DeadlineScreen` / `frameDropped` — extend so **blocked Show never holds `a.mu` and never stops beat**).

**Acceptance:** p95 frame &lt; 8 ms at 5k transcript rows; blocked tty cannot stop `beat()` &gt; 1 s; agent stream continues during UI stall.

### RC-T2 — `paint()` holds `a.mu` for the whole frame (P0)

- Comment says state reads must not hold across `Show()` — good — but **paint itself** is the long critical section.
- Agent `AppendAssistant` / tool live boxes / mouse `expandBox` all serialize behind it → mutex stalls + stream backpressure.

**Fix:**

- Snapshot immutable view (blocks meta + rowIndex window) under lock &lt; 100 µs.
- Render from snapshot without `a.mu`.
- Publish row hit-maps after paint with short lock.
- `expandBox` / selection only touch flags + `markDirty`, never wait on full paint.

### RC-T3 — 30 fps paint while turn running (P0 CPU + lag)

- `tick` still `33 * time.Millisecond`.
- `#648` slowed **animation** and bounded sheen; **`if running { draw every tick }` remains**.
- Streaming already coalesces via `dirty` + `livePaint` 100 ms — spinner does not need 30 fps full transcript paint.

**Fix:**

- Spinner-only dirty region **or** anim cadence `animEvery` for running HUD (e.g. 7–10 fps).
- Full transcript paint only on: input, dirty content, resize, livePaint budget, toast.
- Optional: stop ticker entirely when `!running && !animate && keyq empty` (park on `dirty`+key only).

**Acceptance:** idle 0.0–0.2% core; running turn without tool spam &lt; 5% core; no full-tree walk more than ~10 Hz.

### RC-T4 — Long transcript / markdown cost per frame (P0/P1)

- `rowindex` is the right design; verify it’s on the hot path for **all** paints (no fallback full walk).
- Streaming assistant + markdown/highlight invalidation can still thrash stamps.
- Thinking / tool collapse helps; productize stronger “core lines only” on long sessions (hide thinking, code diffs) with expand — beyond current thinkBox/tool tiers.

**Fix:**

- Hard **transcript row cap** (e.g. 5 000) with head drop or session-file-backed scrollback (terminal is archive — PRD).
- Default compact: collapse `KindThinking`, large diffs, aged tools; one-line summaries; Ctrl+O / click expand (extend #640).
- Memoize markdown by content hash; never re-lex settled blocks.
- Bench gate: frame cost test / `internal/tui/render_bench_test.go` (landed on `perf/tui-fps`) — land as CI budget (ties PRD #118).

### RC-T5 — Mouse expand vs paint deadlock / long lock wait (P0)

- Repeated dumps: `expandBox` blocked on `a.mu` during click.
- User experience: click thinking/tool box → multi-second freeze (reports + #640 partial).

**Fix:** with RC-T2; add test `TestExpandBoxNeverWaitsOnPaint` (trylock / ordering).

- Coalesce mouse drags; don’t lock per wheel notch beyond `keyBurst` pattern.

### RC-T6 — Stall watchdog noise vs signal (P1)

- `pollSkip` 30 s helps sleep false positives; still many `select` dumps with beat 6 s while loop is merely between events.
- Re-read `watchStall`: ensure beat on **idle select wait** is refreshed (waiting for event is healthy).
- Cap dump retention (e.g. last 50 or 7 days) inside `xdev gc`.
- On wedge: already bounded restore + exit — keep; add metrics counter in stats.

### RC-T7 — UI ↔ stream coupling (P0 correctness)

- Still the #284 chain if UI blocks agent callbacks.
- Ensure `OnEvent` never calls into paint under agent goroutine holding progress; only enqueue deltas (lock-free ring / coalescing buffer); UI drains on its cadence.
- Stream idle watchdog armed from **SSE read**, not from UI consumption.

### RC-A1 — Stale / duplicate workers & MCP fan-out (P1)

- **Fixed:** `spawnBg` one live owner per session (#648).
- **Still open:**
  - Auto **`bg prune`** on start / timer / `xdev gc`.
  - Kill MCP children when parent session ends (process group — verify `SysProcAttr` on MCP launch).
  - Gateway `spawn` should share same “one live run per session” guard (gateway uses `--print --resume`, not only bg registry).
  - On TUI exit: tear down MCP, browser, hub children with deadline + SIGKILL.
  - `xdev ps` / doctor: list orphans (MCP without parent, print without lease).

### RC-A2 — Subagent / task / eval child storms (P1)

- Live sample showed repeated short-lived decision-loop / restricted children — CPU spikes.
- Bound concurrent children; reuse; ensure cancel kills process groups (`internal/tool/kill_*.go`).

### RC-A3 — Bash background jobs (P2)

- Registry + prune exist; default age may leave 160 dirs forever.
- Reap finished; delete logs; never leave process group alive after session quit.

### RC-G1 — Gateway worker cost (P1)

- Each Telegram turn = new `xdev --print` + full tool/MCP stack unless flags strip them.
- **Fix plan:**
  - Worker flags: `--no-mcp` or allowlist; reuse warm worker per chat (long-lived print RPC) instead of cold spawn.
  - Enforce single worker per chat (lease exists — verify /stop + crash clears AttachWorker).
  - Setsid is good; on daemon stop, signal all workers.
  - Backoff on poll errors (exponential + jitter); don’t log every failure at info.

### RC-G2 — Dual gateways (P2 ops)

- Disable or park `ai.hermes.gateway` if xdev owns the bot (409 conflicts already handled in xdev — still wasteful).
- Document single owner of `TELEGRAM_BOT_TOKEN`.

### RC-B1 — Chrome headless (P2)

- Idle timeout stop exists; verify it fires; delete/caches profile size; one browser per machine not per session.
- Don’t attach to user’s busy Chrome for automation if it multiplies renderers.

### RC-D1 — Disk / memory budgets (P2)

- Sessions 805 MB vs PRD &lt;100 MB RSS — RSS OK-ish per process; disk not.
- Window in-memory transcript; GC dumps; compact old sessions; bg prune.

---

## 3. Phased fix plan (implementation sessions)

### Phase 0 — Baseline & harness (½ day)

1. Script: sample `ps` + MCP children + bg live + stall count → baseline appendix or update this doc.
2. Land frame bench with budgets:
   - empty idle draw p50 &lt; 0.5 ms
   - 2k rows settled p50 &lt; 2 ms
   - 5k rows p50 &lt; 5 ms
   - running spinner-only path p50 &lt; 1 ms without full md
3. Pty drive: idle CPU 30 s, streaming fake tokens, wheel burst (extend keyburst tests).
4. Confirm installed binary = main tip after each ship.

### Phase 1 — Stop the bleeding (CPU + freeze) (2–3 days) — **ship first**

| ID | Change | Files (likely) | Done when |
|----|--------|----------------|-----------|
| P1.1 | Running turn: do **not** full-draw every 33 ms; spinner/HUD sub-fps | `internal/tui/app.go` Run tick | idle/run CPU targets |
| P1.2 | Park ticker when fully idle | `app.go` | 0 tty bytes idle |
| P1.3 | Snapshot paint; **never** hold `a.mu` during layout+md+Show | `app.go` paint/draw | expandBox dumps gone |
| P1.4 | Event coalescing ring for assistant/tool deltas | `cmd/xdev/tui.go` hooks, `app.go` | stream survives UI stall |
| P1.5 | Show deadline + beat even if flush blocks | `tty_deadline.go`, `stall.go` | no multi-minute dump without exit |
| P1.6 | Tests: mutex ordering, frame budget, idle CPU | `*_test.go` | CI red on regress |

### Phase 2 — Long-session FPS (2 days)

| ID | Change | Done when |
|----|--------|-----------|
| P2.1 | Enforce transcript window (config `tui.maxRows`, default 5000) | mem + frame flat vs turn count |
| P2.2 | Default collapse thinking / diffs / aged tools; toggle expand | UX “core lines only” |
| P2.3 | Settled block render immutable; md hash memo | no re-lex |
| P2.4 | Scroll/virtualization audit vs rowIndex | 10k history still smooth scroll |
| P2.5 | Compact mode latch regression suite stays green | no flap dumps |

### Phase 3 — Process lifecycle (1–2 days)

| ID | Change | Done when |
|----|--------|-----------|
| P3.1 | Gateway + bg share `LiveOwner(session)` | impossible dual resume |
| P3.2 | MCP/browser/hub in one process group; kill on session end | `pgrep -P $xdev` empty after quit |
| P3.3 | Auto `bg prune` + dump retention in `xdev gc` / start | bg dirs &lt; N |
| P3.4 | `xdev doctor perf` — orphans, stall rate, RSS, MCP count | operable |
| P3.5 | Subagent concurrency cap + group kill | no child storms |

### Phase 4 — Gateway (1–2 days)

| ID | Change | Done when |
|----|--------|-----------|
| P4.1 | Poll backoff + error rate limit in log | quiet log offline |
| P4.2 | Warm worker or `--minimal` print profile (no MCP unless needed) | worker RSS ↓ |
| P4.3 | Stream-to-Telegram without unbounded buffer | UX + mem |
| P4.4 | Ops: single bot owner; Hermes disabled when xdev gateway on | no 409 storms |

### Phase 5 — Browser & host (½–1 day)

| ID | Change | Done when |
|----|--------|-----------|
| P5.1 | Prove idle browser stop; cap profile; one launcher lock | no orphan Chrome helpers from xdev |
| P5.2 | Doc: user Chrome CPU ≠ xdev unless browser tool used | clear RCA |

### Phase 6 — Product metrics & PRD (½ day)

- Update `docs/PRD.md`: TUI frame budget, process budget, gateway worker model, status of #284/#648.
- Stats: stall count, dropped frames, injected continuation turns (hide from “user” metrics).
- Issue rollup: one epic “perf-tui-gateway” with Phase 1–5 checkboxes.

---

## 4. Suggested epic breakdown

```text
Epic: perf-runtime-2026-10
├── Issue A: TUI frame scheduler (P1.1–P1.2)          ← biggest CPU win, small diff
├── Issue B: lock-free snapshot paint (P1.3–P1.5)   ← biggest lag/stall win
├── Issue C: stream/UI decouple + watchdog (P1.4, RC-T7)
├── Issue D: transcript cap + collapse UX (P2.*)
├── Issue E: process ownership MCP/bg/gateway (P3.*)
├── Issue F: gateway worker/poll (P4.*)
└── Issue G: browser + gc + doctor (P5.*, P3.4)
```

Prefer **A then B** in one worktree `fix/perf-tui-loop`; E/F in `fix/perf-process-gateway`.

---

## 5. Verification protocol (every fix PR)

1. **Unit:** frame bench + latch + expandBox + keyburst + bg live owner.
2. **Pty:** isolated `XDEV_AGENT_DIR`; tmux; 200-turn fixture or replay.
3. **CPU:** `ps -o %cpu,rss,etime` idle 30 s → 0%; streaming → bound; after quit → no children.
4. **Stalls:** no new `tui-stall-*.txt` in 30 min soak; or only laptop-sleep with `pollSkip`.
5. **Gateway:** fake Telegram test suite + one real poll with backoff.
6. **Never** claim done on compile-only — drive real binary.

---

## 6. What not to do

- Don’t rewrite TUI in another framework.
- Don’t “fix” compact latch again without a failing flap test.
- Don’t add more 30 fps animations.
- Don’t spawn per-turn MCP in gateway workers.
- Don’t ignore Hermes/xdev dual ownership.
- Don’t hold `a.mu` across `Show` or markdown lex.

---

## 7. Quick ops actions (before code)

These reduce heat **today** without a PR:

1. `xdev bg prune` (and cron / launchd weekly).
2. Quit extra interactive `xdev` sessions (each = MCP pair).
3. `launchctl bootout gui/$(id -u)/ai.hermes.gateway` if unused.
4. Trim `~/.xdev/agent/dumps` older than 7 days; run `xdev gc` if available.
5. Stay on **≥ v0.4.300** (compact + idle + one-live-run).
6. If Telegram offline, stop gateway to silence retries: `launchctl bootout gui/$(id -u)/ai.xdev.gateway`.

---

## 8. Priority matrix

| Priority | Item | Impact | Effort |
|----------|------|--------|--------|
| **P0** | Scheduler: no full paint @ 30 fps while running; idle park | CPU | S |
| **P0** | Snapshot paint / short lock; async Show | Lag, stalls, stream kills | M |
| **P0** | Stream decouple + SSE-side watchdog | Extra turns, “slowness” | M |
| **P1** | Transcript cap + collapse UX | Long-session 2 fps | M |
| **P1** | MCP/process group lifecycle + gateway single flight | Stale procs | M |
| **P1** | Gateway poll backoff + lean workers | Background heat/logs | S |
| **P2** | Browser idle/profile; dump/bg GC; doctor | Hygiene | S |
| **P2** | Frame budget CI (#118) | Regress shield | S |

---

## 9. Handoff for the fix session

- **Branch tip at review:** installed **v0.4.300**; compact/idle/one-live-run already on main.
- **Worktree seed:** `.worktrees/perf-tui-fps` holds the landed benches (`internal/tui/render_bench_test.go`) and the three cheap wins of RC-T4; use them as the baseline for the incremental render.
- **Primary files:** `internal/tui/app.go` (`Run`, `draw`, `paint`, tick), `internal/tui/rowindex.go`, `internal/tui/stall.go`, `cmd/xdev/tui.go` (hooks), `internal/tool/bashbg.go`, bg spawn under `cmd/xdev`, `internal/gateway/gateway.go`, `internal/browser/launch.go`, `internal/ai/watchdog.go`.
- **Evidence dir:** `~/.xdev/agent/dumps/tui-stall-*.txt` (especially 2026-10-13 mutex/Show clusters).
- **When shipping code:** update `docs/PRD.md` status; keep this research doc linked from the PRD research section.

---

## 10. Related docs

- [2026-09-15-xdev-slow-session-rca.md](2026-09-15-xdev-slow-session-rca.md) — UI stall → stream kill → extra turns (#284)
- [2026-10-06-tui-refactor-omp-opencode-grok.md](2026-10-06-tui-refactor-omp-opencode-grok.md) — TUI refactor research
- [2026-10-10-gateway-telegram.md](2026-10-10-gateway-telegram.md) — gateway design
- [long-running-session-context.md](long-running-session-context.md) — long session survival
- PRD: `docs/PRD.md` — &lt;100 MB RSS, frame budgets (#118), bounded everything

---

*Last updated: 2026-10-14 (initial research write-up from live stall dumps + process snapshot + main@v0.4.300)*
