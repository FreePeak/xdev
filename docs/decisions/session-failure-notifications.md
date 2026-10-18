# Session-failure notifications

**Status:** adopted (2026-10-18) · **Issue:** #267

## 0.1 The decision in one line

xdev notifies the human when a session fails by emitting a terminal-native
notification (OSC 777 → 9 → BEL ladder) and/or an in-TUI toast, and by
optionally running a user-configured command with the event as JSON on stdin.
This replaces the earlier design where stderr under the alt screen was the
only failure signal — which was silently painted over by the TUI.

## 0.2 Why this is even on the table

User report (2026-09-16): "the mcp server error in the chat textbox/notification
should not show in the chat textbox, it should be a notification for ~2 min".
stderr under the alt screen is not a notice, it is damage — the TUI paints over
it. A session that fails while the human is looking elsewhere is a run that was
silently wasted.

## 0.3 What xdev does

xdev ships a single notification stack with two independent feeds:

1. **Feed A (subagents)** — wired into the Hub's `AddSettleListener`. Every
   subagent (job) that settles with `failed` or `schema-mismatch` emits a
   notification. The feed deduplicates by `(job_id, status)` and tracks
   `errored` IDs so a later success settle of the same id does not re-notify.
   Focus gate: if the terminal has reported focus and the window is currently
   focused, the alert is suppressed; unknown or blurred focus notifies.
   Human-facing part routes through `App.Toast(ToastError, ...)`. The
   `notifications.subagents` axis (default ON) gates this feed.

2. **Feed B (detached workers)** — wired into `finalizeBgStatus` in
   `cmd/xdev/bg.go`. When a detached worker exits non-zero, a notification
   fires once per `(id, status)`. The body names the log path so the user can
   `xdev bg list` and then inspect. The `notifications.errors` axis (default
   ON) gates this feed.

Both feeds share the same `internal/notify` dispatcher:

- Bounded buffered channel (default 64) + off-thread dispatcher.
- Dispatcher never blocks the caller; `Send` drops when full, `Close` drains
  with a deadline.
- Every transport call is recovered and bounded (default 2 s emit timeout,
  3 s close timeout). A panicking or blocking transport is logged and dropped;
  the dispatcher and the session survive.
- Transports: OSC 777 → 9 → BEL ladder (terminal-native, no dependency), and
  command transport (user command, event as JSON on stdin, bounded timeout).
- Sanitizer: every emitted string is stripped of C0/C1 control bytes, ANSI
  escape sequences collapsed, newlines collapsed to spaces, length capped
  (TitleLimit=80, BodyLimit=240). No model-controlled text can inject a
  terminal escape.
- F8: non-interactive runs get `NewOSCSink(nil)` → inert.

## 0.4 Configuration axes (F2)

| Axis | Default | Meaning |
|------|---------|---------|
| `notifications.errors` | ON | Failed turn alerts |
| `notifications.agent` | OFF | Success turn alerts (must earn the default) |
| `notifications.subagents` | ON | Subagent failure alerts (D5: diverges from opencode, which stays quiet here) |
| `notifications.sound` | OFF | Terminal bell (inert until sound transport lands) |
| `notifications.command` | empty | User command (empty = no command transport) |

All axes are `*bool` so "unset" ≠ "explicitly false". The merge layer skips
nil pointers, so a partial config file changes only the keys it names. Every
read goes through an `On` accessor (`NotificationsErrorsOn`, etc.) which
returns the documented default when the pointer is nil.

## 0.5 Divergence from opencode (D5)

opencode does not notify on subagent failures. xdev deliberately does: a
swallowed subagent failure resurfaces later as a wrong answer whose cause was
already known and already discarded. Default ON, user can opt out with
`notifications.subagents: false`.

## 0.6 Terminal-native OSC, not osascript (D4)

The transport uses the terminal's own notification escapes (OSC 777 → 9 → BEL
ladder). No `osascript`, `notify-send`, or `terminal-notifier`. Those write to
the machine running xdev, which over SSH or tmux is the wrong machine by
definition. A TUI must notify through the terminal it owns. The OSC ladder
picks the strongest escape the environment claims: kitty/VTE/gnome/rxvt get
OSC 777; iTerm2/WezTerm/mintty get OSC 9; Apple Terminal and unknown
environments fall back to BEL. A non-interactive run gets a nil writer → inert.

## 0.6 Testing (F1, F3, F6)

- `TestSanitizeDefeatsEscapeInjection`: ESC, BEL, CSI, OSC, embedded newlines,
  NUL → output is single-line, no control bytes.
- `TestPanickingSinkDoesNotStopLaterEvents`: a sink that panics does not crash
  the process and later events are delivered.
- `TestBlockingSinkIsDroppedAndDoesNotStallDelivery`: a transport that blocks
  past the timeout is dropped, later events still arrive.
- `TestSendNeverBlocksTheCaller`: full queue drops and returns promptly.
- `TestOSCSequencePrefixes`: 777, 9, BEL wire format pinned.
- `TestFeedAlertsOnlyOnFailureStatuses`: table over all six `JobInfo.Status`
  values asserting which alert.
- `TestFeedDeduplicatesRepeatedSettles`: same `(id, status)` settles once.
- `TestFeedStaysSilentOnSuccessAfterFailure`: failed→completed alerts once.
- `TestFeedFocusGate`: unknown focus notifies; focused suppresses; blurred
  notifies.
- `TestFeedDisabledByAxisDoesNothing`: axis off = no sink work.

All pass. `go test ./internal/notify/... ./internal/config/... ./internal/agent/... -count=1` passes.
The pre-existing `TestCompactionMethodNamesVocabulary` failure (expects
`vocabulary` without `microcompact`) is unrelated to this change.

## 0.7 Files changed

- `internal/notify/notify.go` — Event, Sink, Dispatcher, sanitizer, OSC/command transports
- `internal/notify/feed.go` — Feed A (hub settle) + Feed B (detached workers)
- `internal/notify/osc.go` — OSC 777 → 9 → BEL ladder + `DetectOSC`
- `internal/notify/command.go` — command transport (stdin JSON, bounded timeout)
- `internal/notify/sanitize.go` — ANSI/control/newline hygiene + caps
- `internal/notify/*_test.go` — unit tests for F1, F3, F4, F6
- `internal/config/settings.go` — `NotificationSettings` group + accessors + merge arms
- `internal/config/settingskeys.go` — key registry for `xdev config`
- `cmd/xdev/configcmd.go` — `notifications.*` keys in the CLI surface
- `cmd/xdev/tui.go` — Feed A wiring: `AddSettleListener`, `scr.EnableFocus`, `app.OnEvent`
- `internal/tui/app.go` — `focused`/`focusSet` fields + focus tracking in `handleKey`
- `internal/tui/toast.go` — `ToastError` constant used by feed A
- `cmd/xdev/bg.go` — Feed B wiring in `finalizeBgStatus`
- `docs/decisions/session-failure-notifications.md` — this ADR
- `docs/PRD.md` — footer stamp update

## 0.8 Verification

```bash
go test ./internal/notify/... ./internal/config/... ./internal/agent/... ./cmd/xdev/... -count=1
# All pass (pre-existing config test failure is unrelated)

go build ./cmd/xdev/... ./internal/notify/... ./internal/config/... ./internal/agent/... ./internal/tui/...
# Builds successfully

gofmt -l internal/ cmd/
# Clean
```

---

*Adopted 2026-10-18 · Replaces "stderr under alt screen" design*