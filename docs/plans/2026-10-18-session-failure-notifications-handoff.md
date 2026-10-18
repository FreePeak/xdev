# Handoff: session-failure notifications

**Date:** 2026-10-18
**For:** picking this up in a fresh session
**Status of the work:** **research done + plan written. Zero lines of product code written. Nothing committed.**
**Repo:** `/Users/linh.doan/work/harvey/freepeak/xdev` @ `main` · HEAD `d2fa6c1`

---

## 1. Read this first

### 1.1 The one artifact that matters

```
docs/plans/2026-10-18-session-failure-notifications.md
```

It is the deliverable so far, it is self-contained, and it carries all the evidence. **Read it in full before doing anything.** This handoff only adds: the state, the coordination risks, and what to do next.

### 1.2 The three-sentence version

xdev already detects background-session failure (`Hub.AddSettleListener`, `internal/agent/hub.go:313`) and already delivers it — but **in-band, to the model, never to the human** (`hubnotice.go:95`). OpenCode solves this with an `attention` subsystem that fires one deduped, focus-gated, per-axis-configurable notification on `session.execution.failed`. The plan copies OpenCode's semantics onto xdev's existing seam: one new `internal/notify` package, one settings group, two call sites, no new dependency, no second failure detector.

### 1.3 What the user actually asked for

> "Check my xdev already having the background agent service and be able to broadcast noti if there is session failed like opencode or not"

Answer given, and verified: **background agent service yes; broadcast-to-human no.** The plan closes that gap.

---

## 2. State, precisely

| Thing | State |
|---|---|
| Research (xdev + OpenCode) | **Done**, evidence-first, all claims file:line verified |
| Plan document | **Written** — `docs/plans/2026-10-18-session-failure-notifications.md`, ~10 sections |
| Plan committed | **No** — untracked (`??` in `git status`) |
| Product code | **None written** |
| Tests | **None written** |
| Branch | **None created** — sitting on `main` |
| Plan self-corrected | **Yes** — two claims were caught and fixed during review, see §5 |

### 2.1 The plan's shape (so you can skim-then-read)

- §0 outcome, §1 the problem, **§2 what OpenCode ships** (primary source), **§3 what xdev already ships**, **§4 eight decisions (D1–D8)**, §5 must-fix table (F1–F8), §6 six slices, §7 files touched, §8 verification checklist, §9 out of scope, §10 request→mechanism map.
- The two load-bearing sections are **§4** (decisions) and **§5** (must-fix). If you read only two, read those.
- §6 Slices 1–4 are the real work. Slices 5–6 are deferrable by design.

---

## 3. THE coordination risk — read before branching

**There is uncommitted, in-flight TUI work on `main` that touches the exact files this plan needs.**

```
git status --short   (on main, HEAD d2fa6c1)
 M .gitignore
 M cmd/xdev/sessionops.go          ← WIP
 M cmd/xdev/tui.go                 ← WIP  ★ this plan edits it (feed A, EnableFocus)
 M internal/tui/app.go             ← WIP
 M internal/tui/hubroster.go       ← WIP
 M internal/tui/keymap.go          ← WIP
?? cmd/xdev/sessionops_interrupted_test.go   ← WIP
?? docs/plans/2026-10-18-tui-revamp.md       ← the plan this WIP belongs to
?? docs/plans/2026-10-18-session-failure-notifications.md   ← THIS plan
?? internal/tui/hubdock.go         ← WIP (new)
?? internal/tui/hubdock_test.go    ← WIP (new)
```

`docs/plans/2026-10-18-tui-revamp.md` is the plan behind that WIP. Its scope is `internal/tui/`, `cmd/xdev/tui.go`, `internal/config` — **the same three areas this plan touches.**

**Consequences, in order of importance:**

1. **Do not commit, stash, revert, or `git checkout --` any of that WIP.** It is the user's active work, not yours (absolute rule: treat unfamiliar changes as potential user work).
2. **Do not start Slice 3 or Slice 4 on top of it without asking.** Both edit files mid-flight in another plan. Either wait for the revamp to land, or branch from a commit that includes it, or coordinate with the user first. **Ask the user which** — this is the first question to resolve.
3. Both plans write to `internal/config` (revamp: settings; this: `NotificationSettings`). Settings changes are additive and should merge cleanly, but verify.
4. Slice 1 (`internal/notify/`, a brand-new package) and Slice 2 (settings) are **safe to start immediately** — no collision. That is why the slices are ordered this way.

---

## 4. What not to re-research (already verified)

Do not spend a fresh session re-deriving these. Full evidence is in the plan; this is the index.

### 4.1 xdev — the seam and the gap

| Fact | Location |
|---|---|
| `Hub.Start` — background job service; ids `hub-N` | `internal/agent/hub.go:194` |
| **`AddSettleListener(fn func(JobInfo, *SubagentResult))`** — the dispatch hook, additive by design | `internal/agent/hub.go:313` |
| `SetNotify` — the dock repaint slot (already wired) | `internal/agent/hub.go:293`, used `cmd/xdev/tui.go:1288` |
| **The one existing listener — in-band, to the model** | `internal/agent/hubnotice.go:95` (`StartHubNoticeDelivery`), wired `cmd/xdev/tui.go:2650` |
| Failure statuses: `failed`, `schema-mismatch` (authoritative predicate) | `internal/agent/hubtool.go:132` |
| Full status set: `yielded\|completed\|schema-mismatch\|failed` (+`running`,`canceled`) | `internal/agent/subagent.go:132`, `hub.go:60` |
| Roster collapses mismatches into display `"failed"` — **do not reuse this enum for alerting** | `internal/agent/hub.go:379-381` |
| Dedupe precedent, key `(id, status)`, with the reason | `internal/agent/hubnotice.go:109-116` |
| In-app toast surface (`ToastError`, already exists) | `internal/tui/toast.go:25-32,61` |
| **Second failure surface: detached `--bg` workers, no push at all** | `bgStatus` `cmd/xdev/bg.go:58-71`; emit point `finalizeBgStatus` `bg.go:407`; installed `cmd/xdev/print.go:443`; only reader `xdev bg list` `bg.go:720` |
| **No OS-notification code exists anywhere** — every `osascript` hit is screen control | `internal/computer/backend_macos.go:10`, `clipboard_image.go:90` |
| No hook event for this — `KnownEvents` is session/turn/tool-scoped, so config alone cannot do it | `internal/hooks/hooks.go:41-51` |
| **TUI is tcell v2** (`v2.13.10`), **not bubbletea** | `go.mod:6` |
| Screen init + the capability calls `EnableFocus` sits beside | `cmd/xdev/tui.go:362` (`NewScreen`), `:372` (`EnableMouse`), `:376` (`EnablePaste`) |
| No focus events enabled anywhere today | no `EnableFocus`/`EventFocus` outside tests |

### 4.2 OpenCode — what to copy

Local source: **`/Users/linh.doan/work/opensources/opencode`** @ `cd9a14a` (**v2.0.18**).

> Note: `docs/research/opencode-internals.md` in this repo documents **v1.18.28** and **predates this subsystem entirely** — it does not mention `attention`, the notifications plugin, or `session.execution.failed`. Do not use it as the source here.

| Fact | Location |
|---|---|
| The plugin: subscribes to `session.execution.{started,succeeded,interrupted,failed}` | `packages/tui/src/feature-plugins/system/notifications.ts:44-73` |
| `AttentionNotifyOptions`: `notification:{when}` and `sound:{name,when}` are **separate axes** | `packages/plugin/src/tui/context.ts` |
| Focus gate + global kill switch + never-throws + message hygiene | `packages/tui/src/attention.ts:129-181` (skip logic `:78-83`, focus state `:94-102`, sanitize `:56-63`) |
| Sound names are a closed enum incl. `subagent_done` | `packages/tui/src/config/v1/index.tsx:9-17` |
| **Subagents get `notification: false` — sound only** | `notifications.ts:11-17` |
| Two-tier dedupe (in-memory claim + Web Locks + localStorage) | `packages/app/src/shell/notifications/coordinator.ts:11-93` |
| Desktop notify: `new Notification(...)`, skips when focused | `packages/desktop/src/renderer/platform/notifications.ts:4-21` |
| **Semantics pinned by test** (dedupe per run; error suppresses following done; subagent sound-only) | `packages/tui/test/cli/cmd/tui/notifications.test.ts:149-160, 255-281, 283-298` |
| Settings categories `notifications.{agent,permissions,errors}` | `packages/app/src/settings/model.tsx:444-452` |

**Not verified:** `renderer.triggerNotification` is from `@opentui/core`, which is **not vendored** in that tree. The call site and contract are verified; the implementation is not. §4.4 of the plan therefore picks a transport from first principles rather than assuming OpenCode's.

### 4.3 The decisions the plan makes (D1–D8, one line each)

- **D1** consume `AddSettleListener`; add no second detector
- **D2** alert on `failed` + `schema-mismatch` only; `canceled` silent (user-initiated)
- **D3** dedupe `(id, status)`; a failed job never also announces success
- **D4** transport = **terminal OSC** (777 → 9 → BEL), never `osascript`/`notify-send` (wrong machine over SSH/tmux); plus a user `notifications.command` escape hatch
- **D5** blur gate, **and deliberately diverges from OpenCode**: xdev *does* notify for background children, because background work is work the user walked away from
- **D6** the notifier must not be able to kill xdev (`recover()`, off-thread, drop-never-block) — `hub.go:305-312` says a panicking listener takes the process down
- **D7** one sink, two feeders: hub settle (A) + `finalizeBgStatus` (B)
- **D8** top-level turn failure = feeder C, emitted from the existing `auto_retry_end` point, **gated**: drop it if not a single obvious site

---

## 5. Corrections already applied to the plan

Two claims were caught and fixed during review. Recorded so they are not re-introduced:

1. **TUI library.** The plan initially said focus tracking would use `tea.FocusMsg`/`tea.BlurMsg`. **Wrong** — xdev has no bubbletea. Verified: tcell v2 at `go.mod:6`. Corrected to `scr.EnableFocus()` at `cmd/xdev/tui.go:362-376` plus a `*tcell.EventFocus` case arm.
2. **Feed B emit point.** Cited as `print.go:441` (a comment line). Corrected to the real call: `finalizeBgStatus` (`bg.go:407`), installed by the deferred hook at `print.go:443`.

Both were fixed by editing the plan file. The plan as it stands on disk is accurate.

---

## 6. Next actions

### 6.0 Before any code — resolve these with the user

1. **The TUI WIP collision (§3).** Wait for `2026-10-18-tui-revamp` to land, branch on top of it, or work only in non-colliding slices? *This gates Slice 3.*
2. **Feeder C (D8): in or out?** Deferrable; the plan is complete without it.
3. **Sound (Slice 5): confirm deferral.** xdev has no audio subsystem; the portable options are all wrong over SSH. Plan defers it.
4. **Success notifications:** plan defaults them **off** (OpenCode has them on). Confirm.

### 6.1 Then, the first concrete step

**Slice 1 — `internal/notify`** (new package, zero collision risk, can start today):

- `Event{Kind, Title, Body, JobID, SessionID, At}`, a `Sink`, a bounded buffered channel + off-thread dispatcher (**F1**), `Close()` with a drain deadline.
- Transports: `osc.go` (OSC 777 → OSC 9 → BEL, plain byte strings — **no new dependency**), `command.go` (stdin JSON, bounded timeout), `toast.go` (delegate to `App.Toast`).
- `sanitize.go` implementing **F6**: strip ANSI, collapse newlines, strip control bytes, cap body 240 / title 80.
- Tests: sanitize against escape injection; a panicking transport and a blocking transport each leave the caller alive.

Reason this is first: it is the only slice that touches nothing in flight, and every other slice depends on it.

### 6.2 Then in order

2. **Slice 2 — settings** (F2): `NotificationSettings` in `internal/config/settings.go`, `*bool` for defaulted-ON axes (the established pattern: `RenderMermaid` `settings.go:512`, `ShowThinking` `:629`, `SubagentPanel` `:636`), `xdev config` surface beside `tui.exitDetach` (`cmd/xdev/configcmd.go:164`, accessor `TuiExitDetachOn` `settings.go:1179`). *Minor collision risk — additive, verify the merge.*
3. **Slice 3 — feed A** (F3/F4/F5): one `AddSettleListener` in `cmd/xdev/tui.go` beside `:2650`, same `hubCtx`. **Gated on §6.0.1.**
4. **Slice 4 — feed B** (F7): emit from `finalizeBgStatus` (`bg.go:407`); include `LogPath` in the body.
5. Slices 5–6: optional.

---

## 7. Constraints that bind this work

From `AGENTS.md` / repo rules — all of these apply:

- **No `Co-Authored-By` trailer naming any AI**, ever, in this repo. The author is `linhdmn <mnhatlinh.doan@gmail.com>`. A commit with a Claude trailer is a broken commit.
- **One git worktree per feature** (mandatory workflow loop). Not yet created.
- **TDD** — this is an existing, running, large repo, so red-green at an agreed seam; not SDD.
- **DRY/SOLID; prefer deleting over adding.** The plan is deliberately mostly reuse — hold that line. If a slice starts growing a second detector or a second renderer, stop.
- **No new third-party dependency.** OSC is a byte string; that is the whole point of D4.
- **Evidence-first.** Every claim needs `file:line` + actual vs expected.
- **Do not touch the in-flight TUI WIP** (§3).

---

## 8. Verification commands

```bash
# the plan doc itself
sed -n '1,40p' docs/plans/2026-10-18-session-failure-notifications.md

# confirm the seam still exists (it is the foundation)
grep -n "func (h \*Hub) AddSettleListener" internal/agent/hub.go

# confirm the in-band consumer that this is NOT replacing
grep -n "func StartHubNoticeDelivery" internal/agent/hubnotice.go

# confirm the alert predicate precedent
grep -n 'schema-mismatch' internal/agent/hubtool.go

# confirm no notification code exists yet
grep -rn "osascript\|notify-send\|terminal-notifier" internal/ --include='*.go' | grep -v _test

# confirm tcell, not bubbletea
grep -n tcell go.mod

# after Slice 1+
go test ./internal/notify/ ./internal/config/ ./internal/agent/ ./internal/tui/ ./cmd/xdev/ -count=1
go build -o /tmp/xdev ./cmd/xdev
```

Full checklist (F1–F8 + manual): plan §8.

---

## 9. Open questions / known soft spots

- **OSC support is not queryable.** The plan emits OSC 777 speculatively with BEL always alongside, and gates on `notifications.command` for anything exotic. It does not promise the user's terminal shows it — the manual checklist asks them to verify in their actual terminal.
- **`schema-mismatch` alerting** may prove noisy in practice (a permissive-mode nudge, not a hard failure). It matches the existing `hubtool.go:132` predicate, which is the right default, but it is the first knob to re-tune if the user mutes the feature.
- **Feeder C coverage** depends on whether `auto_retry_end` (`hooks.go:41-43`) is a single obvious emission site. Unresolved on purpose (D8).
- **`--bg` dedupe across repeated `bg list`** (F7) needs care: the status file is read by more than one command, so the "already notified" marker needs a home. Probably a field on `bgStatus`.
- **Where the notification is suppressed during the TUI's own teardown** — a failing job while the TUI is exiting should not try to write to a dead tty. Bounded by `Close()`/drain, but worth an explicit test.

---

## 10. TL;DR for the next agent

1. Read `docs/plans/2026-10-18-session-failure-notifications.md` — it is complete and verified.
2. **Ask about the TUI WIP collision before touching `cmd/xdev/tui.go` or `internal/tui/`.**
3. Start `internal/notify` (Slice 1) — it collides with nothing.
4. Worktree per feature; TDD; no new deps; no AI co-author trailers.
5. Nothing is committed yet. The only artifact is the plan.
