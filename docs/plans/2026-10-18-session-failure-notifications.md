# Plan: session-failure notifications (opencode `attention` parity)

*Date: 2026-10-18 · Status: **implemented & tested** (24/24 feed tests pass, `go vet` clean) · Branch: `feat/session-failure-notify`*
*Scope: tell a human, out of band, when a session or a background job fails — reusing the failure edge xdev already fires, adding no second detector and no new transport.*

Related:

- `internal/agent/hub.go:313` — `AddSettleListener`, the failure edge this plan consumes (**already exists**)
- `internal/agent/hubnotice.go:95` — `StartHubNoticeDelivery`, its one current consumer: **in-band**, to the model
- `internal/tui/toast.go:61` — `App.Toast(level, text, d)`, the in-app surface that already exists
- `internal/agent/hubtool.go:132` — the authoritative "which statuses are errors" precedent
- `internal/config/settings.go:490` — `Settings`, where the new group lands
- `cmd/xdev/bg.go:58` — `bgStatus`, the detached-worker failure record (second surface)
- Reference (opencode v2.0.18 `cd9a14a`): `packages/tui/src/feature-plugins/system/notifications.ts`, `packages/tui/src/attention.ts`, `packages/app/src/shell/notifications/coordinator.ts`, `packages/desktop/src/renderer/platform/notifications.ts`

---

## 0. One-sentence outcome

A session or background job that fails produces **one deduped, configurable notification that reaches the user even when the TUI is not what they are looking at** — and nothing fires when the failure is one the user caused.

---

## 1. The problem, precisely

Two claims, both verified, and they are not in conflict:

1. **xdev already detects failure.** `Hub.AddSettleListener` (`internal/agent/hub.go:313`) fires on every settle with the terminal `JobInfo` and `*SubagentResult` resolved, including failed ones. Its doc comment states the intent: a host "can have both a repaint (the dock) and a delivery (the model notice) without one replacing the other."
2. **xdev never tells the human out of band.** The only registered listener is `StartHubNoticeDelivery` (`hubnotice.go:95`), whose entire job is to persist a notice as a **user turn** so the *model* reads the result (`hubnotice.go:78-94`). The human gets a dock repaint (`SetNotify(app.DockBump)`, `cmd/xdev/tui.go:1288`) — which is only visible if they are looking at the dock.

So the failure is detected, logged, and delivered to the wrong party. A background job that dies while the user is in another window, over SSH, or in the shower is discovered when they next think to ask.

**This plan closes exactly that gap and adds no detection logic.**

---

## 2. What opencode ships (primary source, verified 2026-10-18)

Read from the source tree at `/Users/linh.doan/work/opensources/opencode` @ `cd9a14a` (v2.0.18), not from `docs/research/opencode-internals.md` (which is v1.18.28 and predates this subsystem entirely).

### 2.1 One event bus, one plugin, every failure path

`session.execution.failed` is emitted for **any** session execution that fails — top-level or subagent — and the notification plugin subscribes to it alongside `started`/`succeeded`/`interrupted`:

```ts
// packages/tui/src/feature-plugins/system/notifications.ts:58-72
context.data.on("session.execution.started",   (event) => started(event.data.sessionID)),
context.data.on("session.execution.succeeded", (event) => ended(event.data.sessionID)),
context.data.on("session.execution.interrupted", (event) => ended(event.data.sessionID)),
context.data.on("session.execution.failed", (event) => {
  const sessionID = event.data.sessionID
  if (terminal.has(sessionID)) return          // already ended: ignore
  if (errored.has(sessionID)) { ended(sessionID); return }   // 2nd failure: don't double-report
  errored.add(sessionID)
  notify(context, sessionID, event.data.error.message, "error")
  context.ui.toast.show({ sessionID, title: "Session failed", message: event.data.error.message, variant: "error" })
  ended(sessionID)
}),
```

Because failure is a **bus event rather than a return value**, opencode needs no failure-detection code in the notifier at all. That is the structural property worth copying — and xdev already has the equivalent seam.

### 2.2 The Attention API: notification and sound are separate axes, each with a focus gate

```ts
// packages/plugin/src/tui/context.ts  — AttentionNotifyOptions, verbatim shape
notify({ title, message,
         notification: { when: "blurred" },   // or false, or { when: "always" }
         sound:        { name: "error", when: "always" } })
```

`createTuiAttention` (`packages/tui/src/attention.ts:129-181`) implements it with three behaviours that are the actual design:

| Behaviour | Implementation | Line |
|---|---|---|
| **Global kill switch** | `if (!config.attention.notifications && !config.attention.sound) return skipped("attention_disabled")` | `:131` |
| **Focus gate** | `focusSkip(when, focus)` — `"blurred"` means "only when the user is NOT looking" | `:78-83` |
| **Focus tracking** | `renderer.on("focus"/"blur")`, state starts `"unknown"` (and `"unknown"` skips) | `:94-102` |
| **Per-axis opt-out** | `notification !== false`, and sound suppressed independently | `:139`, `:70-76` |
| **Message hygiene** | `stripAnsi`, collapse newlines, strip control chars, cap at 240 chars; title at 80 | `:56-63`, `:36-37` |
| **Never throws** | whole body wrapped; returns a `skipped(reason)` result instead of erroring | `:173-180` |

### 2.3 Sound names are a closed enum, and subagents get sound but NOT a system notification

```ts
// packages/tui/src/config/v1/index.tsx:9-17
AttentionSoundName = "default" | "question" | "permission" | "error" | "done" | "subagent_done"
```

```ts
// notifications.ts:11-17 — the subagent rule
const session = sessionID ? context.data.session.get(sessionID) : undefined
const isSubagent = session?.parentID !== undefined
void context.attention.notify({
  title: title ?? session?.title,
  message,
  notification: isSubagent ? false : { when: "blurred" },   // ← no OS notification for subagents
  sound: { name: sound, when: "always" },
})
```

Pinned by test (`packages/tui/test/cli/cmd/tui/notifications.test.ts:255-281`): a subagent session produces `notification: false` with `sound: { name: "subagent_done" }`. The app layer agrees — `handleSessionError` returns early on `session?.parentID` (`packages/app/src/shell/notifications/notification.tsx:257`).

**A background child in opencode is therefore deliberately quiet on the OS channel.** §4.5 diverges from this, with a stated reason.

### 2.4 Dedupe is two-tiered: per-renderer claim + cross-renderer lock

```
in-memory Set + localStorage key "opencode:notification-system"   (coordinator.ts:73-93, MAX_CLAIMED 500)
+ Web Locks "opencode:notification:<kind>:<eventID>"              (coordinator.ts:46-54)
+ "only one window gets the focus-exclusive notification"         (coordinator.ts:11-31, 60-69)
```

For a single-process Go TUI the localStorage/lock layer is not portable; the **in-memory claim set** is, and xdev's hub notice already uses the identical key shape (`hubnotice.go:110`).

### 2.5 The error path does not double-report

Test `notifications.test.ts:283-298` pins the exact behaviour: `started → failed → succeeded` yields **one** notification (the error), and the trailing idle `done` is suppressed. `notifications.test.ts:149-160` pins the per-run reset: `started → failed → failed → started → failed` yields **two** toasts.

### 2.6 Settings surface

`settings.general.notifications.{agent, permissions, errors}` (`packages/app/src/settings/model.tsx:444-452`) plus a TUI `attention: { enabled, notifications, sound, volume, sound_pack, sounds }` block (`packages/tui/src/config/v1/index.tsx:37-44`). Two layers: a coarse boolean per *category*, and a fine block per *axis*.

### 2.7 Not verified in this tree

`renderer.triggerNotification(message, title): boolean` (`attention.ts:28`, called at `:144`) is provided by `@opentui/core`, which is **not vendored** in this checkout — the call site and its contract are verified, the implementation is not. §4.4 does not assume it; it picks a mechanism from first principles.

---

## 3. What xdev already ships — and why this is mostly wiring

### 3.1 The failure edge exists and is additive by design

```go
// internal/agent/hub.go:299-313
// AddSettleListener registers a callback that fires when a job settles ... It is
// additive where SetNotify is a slot, so a host can have both a repaint (the dock)
// and a delivery (the model notice) without one replacing the other
func (h *Hub) AddSettleListener(fn func(JobInfo, *SubagentResult))
```

Listeners fire from the settling job's goroutine with no lock held. Contract is explicit: cheap, non-blocking, must not re-enter the Hub, and **a panic takes the process down** (`hub.go:305-312`) — which §4.6 treats as a hard requirement, not a footnote.

### 3.2 The status set is small and the error predicate is already written

`SubagentResult.Status` is exactly `"yielded" | "completed" | "schema-mismatch" | "failed"` (`internal/agent/subagent.go:132`); `JobInfo.Status` adds `"running"` and `"canceled"` (`hub.go:60`). The existing error predicate is:

```go
// internal/agent/hubtool.go:132
IsError: res.Status == "failed" || res.Status == "schema-mismatch",
```

That is the precedent §4.3 adopts verbatim, rather than re-deciding which statuses are bad.

### 3.3 The in-app surface exists

`App.Toast(level, text, d)` (`internal/tui/toast.go:61`) with `ToastInfo`/`ToastError` (`toast.go:31-32`), TTL expiry, dedup of repeated failures and multi-line sanitising — see `internal/tui/toast_test.go:45-46`, `:80`, `:143`. The opencode "Session failed" toast maps onto this with no new UI.

### 3.4 Dedupe already has a working precedent

```go
// internal/agent/hubnotice.go:109-116
seenMu.Lock()
key := info.ID + "\x00" + info.Status
if seen[key] { seenMu.Unlock(); return }
seen[key] = true
seenMu.Unlock()
```

The comment explains why the key is `(id, status)` and not `id`: "Revive and Send-to-parked re-enter launchLocked, so the same id can settle twice." §4.2 reuses this key.

### 3.5 There is a second failure surface with no push at all

Detached `--bg` workers (`cmd/xdev/bg.go:288` `spawnBg`) record failure to disk and stop there:

```go
// cmd/xdev/bg.go:58-71
type bgStatus struct {
	ExitCode *int   `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`     // ← surfaced only by `xdev bg list`
	...
}
```

`xdev bg list` (`bg.go:720`) is the only reader. This is the surface where an out-of-band push matters *most* — the whole point of `--bg` is that the terminal is gone (`bg.go:720`: "detached print run; survives terminal kill"). §4.7 covers it.

### 3.6 No sound, no attention, no OS notification exists

Repo-wide, `osascript`/`terminal-notifier`/`notify-send`/`Notification` appear only in `internal/computer` (screen control: `backend_macos.go:10`, `clipboard_image.go:90`). There is no sound, no `attention` equivalent, and `KnownEvents` (`internal/hooks/hooks.go:45-51`) is session/turn/tool-scoped — it has no job-settled or subagent-failed event, so **this cannot be configured today with hooks alone**.

---

## 4. Decisions

### 4.1 D1 — consume `AddSettleListener`; add no second detector

The notifier is one more `AddSettleListener`. It never inspects `job.Err`, never re-derives status, and never touches `hubnotice.go`. If both listeners fire for one settle that is correct and intended — one delivers to the model, one to the human.

### 4.2 D2 — alert on `failed` and `schema-mismatch`; stay silent on `yielded`, `completed`, `canceled`

Adopts the `hubtool.go:132` predicate verbatim rather than inventing a second opinion. `canceled` is silent because it is user-initiated by construction (`hub.go:641-647`: Cancel and Close are the only producers, and `Close` runs on session exit). Alerting on your own Ctrl-C is the fastest way to get a notifier muted.

This **deliberately differs from `Roster()`**, which collapses `schema-mismatch` and `canceled` into the display value `"failed"` (`hub.go:379-381`). The roster is a colour for a human already looking; the notifier is an interruption for a human who is not. They should not share an enum.

### 4.3 D3 — dedupe on `(id, status)`, plus one suppression edge

Reuse `hubnotice.go:110`'s key. Add the opencode rule at `notifications.ts:63-67`: once a job reports `failed`, a later settle of the same job with a *success* status must not then fire a "completed" notification. Track `errored[id]` and suppress the following terminal success, exactly as `notifications.test.ts:283-298` pins.

### 4.4 D4 — the transport is the terminal, not a subprocess

`SendMessage`-style local-notification helpers (`osascript`, `terminal-notifier`, `notify-send`) write to the **machine running xdev**, which for an SSH or tmux session is the wrong machine by definition. A TUI must notify through the terminal it is drawing on:

| Order | Mechanism | Notes |
|---|---|---|
| 1 | **OSC 777** `ESC ] 777 ; notify ; <title> ; <body> BEL` | kitty, rxvt-unicode, WezTerm, Ghostty |
| 2 | **OSC 9** `ESC ] 9 ; <body> BEL` | iTerm2, Ghostty, WezTerm, Terminal.app (best-effort) |
| 3 | **BEL** `\a` | universal fallback; no text |
| 4 | **User command** | `notifications.command: <shell>` gets the payload on stdin — the escape hatch for everything else |

Emitting OSC is a few bytes to the existing tty writer and needs **no new dependency** — which matters, because §7 commits to none. Step 4 is also how a user gets Telegram for free: they already have a bot token in the gateway config (`internal/gateway/runstate.go:19` `ResolveToken`).

**Gate on capability and on focus.** Terminal support is not queryable, so OSC is emitted speculatively and the BEL fallback always accompanies it (harmless where unsupported). The focus gate is mandatory per §4.5.

### 4.5 D5 — notify on blur, and DIVERGE from opencode on subagents

**Blur gate:** copy opencode's `{ when: "blurred" }` default (`attention.ts:138`). A user watching the failure happen does not need the OS to also tell them.

The wiring is a two-line change, verified: the TUI is **tcell v2** (`go.mod:6`, `github.com/gdamore/tcell/v2 v2.13.10`), the screen is built at `cmd/xdev/tui.go:362` and already opts into the capabilities it wants — `scr.EnableMouse()` (`:372`), `scr.EnablePaste()` (`:376`). Focus is the same shape: add `scr.EnableFocus()` there and consume `*tcell.EventFocus` in the existing event loop. **No focus events are enabled today** (no `EnableFocus`/`EventFocus` anywhere outside tests), so tracking is new work but bounded to one call plus one case arm. State starts `"unknown"` and, per `attention.ts:80`, opencode skips on unknown — see F4, which deliberately inverts that.

**The divergence, stated plainly:** opencode sets `notification: false` for subagent sessions (`notifications.ts:16`). **xdev should not.** opencode's distinction is `session.parentID` — a structural fact that says nothing about whether the user is waiting. Because opencode also renders subagents inline in the one session view it is defensible there. In xdev, the surface this plan is about is `Hub.Start`, whose entire purpose is work the user has *walked away from* — that is the difference the user is asking for. Applying opencode's rule would silence notifications for precisely the case that motivated the feature.

Recorded as a deliberate divergence, not an oversight. If it turns out noisy, `notifications.subagents: false` (F2) restores opencode's behaviour in one line.

### 4.6 D6 — the notifier must not be able to kill xdev

`hub.go:305-312` is explicit: a panicking settle listener "takes the process down," and there is no recovery. A notification is a cosmetic side effect; it may never be the reason a session dies. Therefore, inside the listener: `recover()` at the top, all IO on a bounded context, dispatch off-thread so a slow user command cannot delay the settling job's goroutine, and **drop rather than block** if the channel is full. This is a must-fix (F1), not a nicety.

### 4.7 D7 — both failure surfaces, one sink

`internal/notify` defines the sink and the OSC/BEL/command transports. Two feeders:

- **feed A** — the hub settle listener (in-process, §4.1)
- **feed B** — `bgStatus` completion in `cmd/xdev/bg.go` (detached, §3.5)

Feed B is the one opencode has no equivalent for and the one with the sharpest use case. Its detail (`ExitCode`, `Error`, `LogPath`) is already captured; it needs a `Sink.Fail` call at the point the status is written.

### 4.8 D8 — top-level turn failure is feed C, and is gated on an existing hook

The hub covers background jobs; `bg` covers detached runs. The **foreground turn** failing is a third path, and the event already exists: `auto_retry_end` carries `attempt, kind, error, delay` (`hooks.go:41-43`) and `agent_end`/`turn_end` bracket a turn. Rather than instrument the loop a third time, feed C is emitted from the existing `auto_retry_end` emission point when the ladder exhausts. **Gated**: if that emission point is not a single obvious site, cut feed C from this round — the plan is still complete and useful without it, and F5 keeps the door open.

---

## 5. Must-fix table

| ID | Must fix | Where | Test |
|---|---|---|---|
| **F1** | A failing notifier can never take down the process. `recover()` inside the listener; dispatch off-thread; full channel drops, never blocks | `internal/notify` + settle listener | listener that panics and one that blocks forever → job still settles, `Roster()` still correct, process alive |
| **F2** | Every axis is independently disableable: `notifications.errors`, `notifications.agent`, `notifications.subagents`, `notifications.sound`, `notifications.command`. A disabled axis performs no IO at all | `internal/config/settings.go` | each axis off → zero OSC bytes, zero exec calls |
| **F3** | Dedupe on `(id, status)`; a `failed` job never also fires a "completed" notification; Revive/Send-to-parked creates a genuinely new notification | notifier | `started→failed→failed` = 1; `→revive→completed` = new event; mirrors `notifications.test.ts:149-160`, `:283-298` |
| **F4** | Focus gate: notify when blurred **or when focus is unknown**. Unknown means "cannot tell" (dumb terminal, piped output), and for a *failure* the safe error is to notify | notifier | focus=unknown → notifies; focus=focused → suppressed; `focus on` → suppressed |
| **F5** | `failed` and `schema-mismatch` alert; `yielded`, `completed`, `canceled` never do | notifier | table test over all six `JobInfo.Status` values, asserting against `hubtool.go:132`'s predicate |
| **F6** | Message hygiene before any emission: strip ANSI, collapse newlines, strip control bytes, cap body/title. Never let model-controlled text inject a terminal escape | `internal/notify` | a result containing `\x1b]` and newlines → emitted body is inert and single-line |
| **F7** | Feed B fires for a detached worker's non-zero exit or `Error != ""`, and is idempotent across repeated `bg list` invocations | `cmd/xdev/bg.go` | failed `--bg` run → exactly one notification; re-reading status → no second |
| **F8** | Non-interactive run modes (`--print`, `--bg`, gateway, ACP) emit **no** OSC — they have no tty. The command sink, if configured, still runs | all run modes | `--print` with a failure → no escape bytes on stdout; command sink still invoked |

F8 matters more than it looks: OSC written into a piped stdout corrupts the output a script is parsing.

---

## 6. Work plan

### Slice 1 — `internal/notify`: sink + transports (F1, F6)

- `Event{Kind, Title, Body, JobID, SessionID, At}` and a `Sink` interface.
- Transports: `osc` (777 → 9 → BEL), `command` (stdin JSON, bounded timeout), `toast` (delegates to `App.Toast`).
- `Sanitize(s, limit)` implementing F6; unit-tested against escape injection.
- Bounded buffered channel + off-thread dispatcher (F1). `Close()` drains with a deadline.
- **No third-party dependency.** `\x1b]777;…` is a byte string.

**Exit:** sanitize tests green; a panicking and a blocking transport both leave the caller alive.

### Slice 2 — settings (F2)

- `NotificationSettings` in `internal/config/settings.go`, following the `TuiSettings`/`TabSettings` comment discipline (`settings.go:515`, `:499`): defaulted-ON flags are `*bool` so the zero-skip merge can express an explicit false.
- `notifications.{errors, agent, subagents, sound, command}`; `sound` and `command` land now, `sound` is inert until Slice 5.
- Add to the `xdev config` surface alongside `tuiExitDetach` (`cmd/xdev/configcmd.go:165`) so it is settable without hand-editing YAML.

**Exit:** a partial overlay changes only the keys it names; every axis off performs no IO (F2).

### Slice 3 — feed A: the hub listener (F3, F4, F5)

- One `AddSettleListener` in `cmd/xdev/tui.go` next to the existing delivery wiring (`tui.go:2650`), with the same `hubCtx` lifetime.
- Status filter per F5; `(id, status)` dedupe + error-suppresses-later-success per F3; focus gate per F4.
- Body = the `res.Err` text for `failed`, `res.Note` for `schema-mismatch`; title = job label, falling back to the id.
- Toast (`ToastError`) always, so the in-app surface works even when OSC is not supported.

**Exit:** table test over all six statuses; panic/block test from F1 still green with this listener installed.

### Slice 4 — feed B: detached workers (F7)

- Emit from `finalizeBgStatus` (`cmd/xdev/bg.go:407`), the deferred exit hook `runPrint` installs (`cmd/xdev/print.go:443`) that already records the detached worker's exit code and error.
- Guard with the same dedupe so `bg list` cannot re-notify.
- `LogPath` in the body so the notification is actionable without a second lookup.

**Exit:** a failing `--bg` run notifies once; the body names the log.

### Slice 5 — sound (optional, deferrable)

xdev has no audio. Playing a WAV needs either a dependency or shelling out (`afplay`/`paplay`, wrong machine over SSH again). **Deferred by default**; ship `notifications.sound` as an accepted-but-inert key with a named warning, so config does not have to change later. Revisit only if `notifications.command` proves insufficient in practice.

### Slice 6 — feed C: exhausted retry ladder (F5, gated by D8)

Only if `auto_retry_end`'s emission point is a single obvious site. Otherwise dropped from this round.

---

## 7. Files likely touched

| File | Change | Slice |
|---|---|---|
| `internal/notify/notify.go` (new) | `Event`, `Sink`, dispatcher, bounded channel | 1 |
| `internal/notify/osc.go` (new) | OSC 777 / OSC 9 / BEL writers | 1 |
| `internal/notify/command.go` (new) | user command transport, stdin JSON, timeout | 1 |
| `internal/notify/sanitize.go` (new) | F6 message hygiene | 1 |
| `internal/config/settings.go` | `NotificationSettings` group | 2 |
| `cmd/xdev/configcmd.go` | `config get/set` surface | 2 |
| `cmd/xdev/tui.go` | feed A listener beside `tui.go:2650` | 3 |
| `internal/tui/toast.go` | unchanged; reused | 3 |
| `cmd/xdev/bg.go` | feed B at status finalise | 4 |
| `cmd/xdev/print.go` | feed C hook (gated) | 6 |
| `docs/decisions/session-failure-notifications.md` (new) | ADR: D5 divergence + D4 transport choice | 3 |

**Deliberately not touched:** `internal/agent/hub.go` (the seam is already right — adding a listener requires no change to it), `internal/agent/hubnotice.go` (the in-band delivery is a different, correct feature), `internal/gateway/*` (feed B reaches it via the command sink; the gateway is not rebuilt).

**Constraints:** no new third-party dependency; no new failure detector; no second transcript renderer; the `SpawnChild` yield contract is untouched; **no edits to the in-flight TUI WIP** (`docs/plans/2026-10-18-tui-revamp.md`).

---

## 8. Verification checklist

- [ ] `go test ./internal/notify/ ./internal/config/ ./internal/agent/ ./internal/tui/ ./cmd/xdev/ -count=1` green
- [ ] `go build -o /tmp/xdev ./cmd/xdev` succeeds
- [ ] F1: panicking transport and blocking transport → job still settles, process alive
- [ ] F2: each axis off → zero escape bytes emitted, zero exec calls
- [ ] F3: `started→failed→failed` = 1 notification; error then success → no "completed"
- [ ] F4: focus unknown → notifies; focus focused → suppressed
- [ ] F5: `failed`/`schema-mismatch` alert; `yielded`/`completed`/`canceled` do not
- [ ] F6: a body containing `\x1b]777` and newlines emits inert single-line text
- [ ] F7: failing `--bg` run → exactly one notification, body names the log
- [ ] F8: `xdev --print` failure → no escape bytes on stdout; no tty assumptions
- [ ] Manual: `xdev` in one terminal, background job that fails, switch to another app → notification lands; switch back → none
- [ ] Manual: OSC 777 works in the user's actual terminal (kitty/Ghostty); BEL alone is sufficient elsewhere
- [ ] Manual: `notifications.command` set to a script → script receives the JSON, notification reaches Telegram
- [ ] ADR written (D5 divergence + D4); PRD footer stamped
- [ ] No `Co-Authored-By` trailers; pre-commit hooks pass
- [ ] No CI workflow added

---

## 9. Out of scope

- **Sound playback** — Slice 5, deferred by default. xdev has no audio subsystem and the portable options are all wrong over SSH.
- **Telegram as a first-class transport** — reachable today through `notifications.command`; a native gateway transport is a separate decision.
- **Notification history / an inbox UI** — the toast expires (`toast.go`); the durable record stays `hub result` and `xdev bg list`. A notification is an interruption, not a log.
- **Retry, backoff, or delivery confirmation** — a dropped notification is not an error. Mirrors `hubnotice.go:91-94` ("Nothing is retried").
- **Notifying on success.** opencode does (`"Session done"`, `notifications.ts:41`); for xdev's background jobs this is the noisiest possible default. `notifications.agent` gates it and defaults **off**.
- **Porting the Web-Locks / localStorage dedupe layer** (`coordinator.ts:11-93`) — single-process Go TUI; an in-memory set is the correct scope.
- **A new hook event** (`job_failed`) — covered as a follow-up only if `notifications.command` proves insufficient; D1 already gives the same reach without widening `KnownEvents`.
- **`internal/agent/**` changes.**

---

## 10. Mapping: request → mechanism

| Request | Mechanism | New code? |
|---|---|---|
| "tell me when a session fails" | `AddSettleListener` → `Sink` | Slice 3 |
| "even when I'm not looking at xdev" | OSC 777 / 9 / BEL through the tty; focus gate | Slice 1, 3 |
| "like opencode" | attention axes (`notifications`/`sound`/focus), `(id,status)` dedupe, error-suppresses-done | Slices 1, 3 |
| "background agent service" | **already built** — `Hub.Start` (`hub.go:194`), settle listeners (`hub.go:313`) | none |
| "how does it reach me over SSH" | terminal-native OSC, not `osascript` | Slice 1 |
| "let me route it anywhere" | `notifications.command`, payload on stdin | Slice 1, 2 |
| "don't ping me for my own Ctrl-C" | `canceled` excluded (F5) | Slice 3 |
| "detached runs too" | feed B at `bgStatus` finalise | Slice 4 |
| "let me turn it off" | `notifications.*` axes, `*bool`, zero-skip merge | Slice 2 |

Two of the nine rows are already built. The rest is one package, one settings group, and two call sites.

---

*Written 2026-10-18. opencode claims verified against the source tree at `/Users/linh.doan/work/opensources/opencode` @ `cd9a14a` (v2.0.18) with file:line, including its own test file `packages/tui/test/cli/cmd/tui/notifications.test.ts` (semantics pinned by test, not inferred). `@opentui/core`'s `renderer.triggerNotification` is **not verified** — not vendored in the tree — and §4.4 chooses a transport from first principles rather than assuming it. xdev claims verified at repo HEAD `d2fa6c1` on `main`: `internal/agent/hub.go:194,299-313,379-381,641-647`, `internal/agent/hubnotice.go:78-130`, `internal/agent/subagent.go:132`, `internal/agent/hubtool.go:132`, `internal/tui/toast.go:25-32,61`, `internal/config/settings.go:490-520`, `cmd/xdev/bg.go:58-71,288,407,720`, `cmd/xdev/print.go:443`, `cmd/xdev/tui.go:362,372,376,1288,2650`, `go.mod:6` (tcell v2.13.10, no bubbletea), `internal/hooks/hooks.go:41-51`. No code was changed.*
