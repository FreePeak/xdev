# xdev TUI — interactive audit (mouse + keybindings)

**Date:** 2026-09-28
**Binary under test:** `fa37024` (origin/main tip, includes #454's mouse fixes), built to `/tmp/xdev-tui-qa/xdev`
**Method:** the real `xdev tui` driven inside a tmux PTY at 120x40 and 46x24, with
raw SGR-1006 mouse bytes (`ESC [ < b ; col ; row M|m`) and named keys injected
through `tmux send-keys`. Live model turns against the local onegw gateway. Each
finding that is a code-level claim was also reproduced as a Go test against
`internal/tui` (every test file created for this audit was deleted afterwards —
the worktree is clean, nothing was committed).

**Reproduction environment note (matters for anyone re-running this):**
`tmux -f /dev/null` hands the pane `TERM=dumb`, which silently degrades every
tcell colour to reverse video and makes the whole TUI look broken for reasons
that are not xdev's fault. A real `tmux.conf` with
`set -g default-terminal 'tmux-256color'` and **`NO_COLOR` unset in the TUI's
own env** is required for any visual finding to mean anything. tcell's
`Colors()` returns 0 when `NO_COLOR` is set, and every themed colour then renders
as SGR 7.

---

## Confirmed issues

### 1. `C-r` is advertised by `/hotkeys` and dispatches nothing — history recall is keyboard-only via `Up`

`/hotkeys` prints `history-prev   C-r`. The keymap resolves `C-r` to
`history-prev` (verified with the live event shape a real terminal sends:
`Key=82 Ctrl+R, rune='r', mods=ModCtrl`). But:

- `Editor.HandleKey` has no `KeyCtrlR` case (`internal/tui/editor.go`),
- `app.go`'s action switch has no `case "history-prev"` — neither of its two
  dispatch tables lists it.

So the chord resolves to an action id nothing handles, and the editor ignores
the key. Reproduced live (empty composer + non-empty history, three prompts sent):
`Up` recalls `third` → `second` → `first`; `C-r` recalls nothing. Reproduced as
a unit test through `handleKey`: `C-r` recalled `""`, `Up` recalled `"third"`.

`history-next` is worse in the table sense: it is listed as `—` (correctly
unbound), so the table is honest there, but the paired `C-r` is not.

**Fixed** (`internal/tui/app.go`, one case in the second action switch):
`case "history-prev"` now calls `a.ed.HistoryPrev()` — the method that already
existed (editor.go:215) and was called by nothing. Pinned by
`TestHistoryPrevChordRecalls` and `TestHistoryPrevChordMatchesUpArrow`, both of
which fail on the unfixed tree. Verified live: `C-r` walks `charlie → bravo →
alpha`.

### 2. A click in the transcript columns fires the sidebar's action

fa37024 made the sidebar "a window of its own": `dockAt(x, y)` claims only
`x >= width-dockCols` (78 at 120 cols). But the press branch in
`internal/tui/selection.go` calls

```go
if path, act := a.dockRowAt(x, y); path != "" {   // selection.go:330
```

and `dockRowAt` **answers by row, never by x** (dock.go:889-906). So a press at
`x=40` — deep in the transcript — on a screen row the sidebar also paints opens
that file's diff overlay or runs that row's action.

Reproduced live on fa37024: the sidebar's `FILES` row `/tmp/xdev-tui-qa/probe2.txt`
painted on row 4; a click at col 48 (30 columns left of the panel) opened the
diff. Reproduced as a test: `dockAt=false`, `dockRowAt → path="foo.txt"`, and
the press opened the overlay for `foo.txt`.

Note the same guard is *present* on the two lines above (`thinkBoxAt`,
`msgArmed`) and missing on this one — the diff-overlay leg of #454 rewrote the
branch and left it out.

**Fixed** at the seam, not at each caller: `dockRowAt` now starts with
`if !a.dockAt(x, y) { return "", "" }`, so the column test lives with the
function that answers for the panel and both call sites get it. Both that guard
and `dockOn` are one line; the `d == nil` check that followed is now redundant
(`dockAt` already requires the panel) and was dropped. Pinned by
`TestDockRowAtAnswersOnlyInsideThePanel` (fails on the unfixed tree), and
verified live on the rebuilt binary: a click at col 99 opens the diff, the same
screen row clicked at col 59 does not.

### 3. A held drag parked on the bottom edge at the tail is a dead gesture

The edge auto-scroll (`selEdgeTick`) can only move the viewport toward older
rows when parked at the *top* edge. At the bottom edge while `follow` is pinned
to the tail — which is where every turn leaves the viewport — `ScrollDown` has
nowhere to go, the tick reports no work, and a user trying to drag-select past
the live edge gets nothing: no scroll, no selection extension, no indication.
Verified live: parked the pointer on the last transcript row for 11 s with the
button held; `▲n▼n` never changed. Verified as a test: `edge=1, offset=0,
follow=true, tickMoved=false`.

The top edge works correctly (`edge=-1 → offset 0→3`), so this is a one-sided
gap, not a broken mechanism.

### 4. The settings overlay's category tabs are keyboard-only

`Tab` / `Shift-Tab` cycle categories; the mouse handler
(`handleSettingsOverlayMouse`, settings.go:316) compares `y` against
`bodyStart` and never inspects the tab row. A click on `reasoning` / `ui` /
`appearance` does nothing — verified live, and as a test (`activeCat` unchanged
after a click on the painted tab strip). Every other overlay in the app
(picker, ask card, ledger, hub) answers the mouse on its tabs or rows; the
settings panel was the one that did not.

**Fixed.** The painter now builds the strip's layout **once** and publishes it:
`settingsOverlayState.tabCells` carries each category's label columns, filled by
the same walk that builds the string, and the hit-test reads those columns
instead of re-deriving them. The two used to compute the layout independently —
the painter by concatenation, the hit-test by arithmetic — and disagreed about
the leading space and the separator width, which is why an early arithmetic
hit-test put every click one tab to the left. Pinned by
`TestSettingsOverlayTabClickSwitchesCategory` and
`TestSettingsTabColumnsMatchThePaint` (the latter asserts each published span
contains its own label in the painted row). Verified live: clicking `reasoning`,
`ui`, `appearance`, `tools`, `model` each switches the active category, and the
selection resets to the top of the new list.

One caveat for a re-run: the active label paints UPPERCASED, so a category's
columns move by one when it becomes active. A live test must re-read the
strip's geometry after each click rather than computing it once.

### 5. A click on a read-only settings row is a silent no-op

`settingsOverlayAction` runs Enter's action on any clicked row. For a row with
`Editable: false` (`Theme`, `Approval mode`, `Model`, `Memory`, `Advisor`,
`Color-blind mode`) that is nothing, and the app says nothing either. A user
clicking `Theme` gets no feedback at all — no notice, no shake, no reason.

**Fixed.** `settingsOverlayAction` now answers a read-only row with the
composer's notice slot rather than silence: `Theme is read-only — `xdev config
set theme <value>`` naming the key that CAN change it. A notice, not a system
block, because this is a gesture's answer and a transcript block would scroll
the user out of the panel they are standing in. Pinned by
`TestSettingsOverlayReadOnlyClickExplainsItself`; verified live — the notice
appears on the composer's info divider the moment the row is clicked.

### 6. `NO_COLOR` degrades the entire TUI to reverse video

Not an xdev bug per se, but the failure mode is severe and xdev's own README
screenshot tooling documents it. With `NO_COLOR` set in the environment,
tcell's `Colors()` returns 0 and every themed foreground/background pair
collapses to SGR 7 (reverse). Reproduced live: with `NO_COLOR=1`, the
`/hotkeys` table, the slash dropdown, the dock and the status row all painted
as reverse-video blocks — the selected dropdown row was visually
indistinguishable from the unselected ones. The dropdown's own highlight
(`BgHighlight`) becomes invisible, so the only remaining affordance is the
2-space gutter indent.

The fix is one line in `cmd/xdev/tui.go` before `tcell.NewScreen()`: unset
`NO_COLOR` for the screen (tcell already has a `TCELL_TRUECOLOR` escape hatch;
the same reasoning applies to the no-color hint) or document it loudly in
`/hotkeys`. Right now nothing tells the user why their TUI looks wrong.

### 7. `C-c` / `C-d` quit with an unsent draft in the composer, with no warning

Both chords map to `quit`, and `quit` when idle calls `onQuit()` directly —
no check on `a.ed.Text()`. Reproduced live: typed `important unsent prompt`,
pressed `C-c`, the app exited; the text was not in the composer, not in
history (`C-r` and `Up` both came up empty after resume). The Esc stash
(`escDraft`) that exists for exactly this class of accident is not consulted on
the quit path.

### 8. Terminals narrower than ~3 rows collapse to a single garbled line

Resizing to 20x6 and then 30x1 (both while a turn was streaming): at 2 rows the
composer, the status row and the transcript all vanish; at 1 row the frame is
`❯/tmp/x…q18s │ ⚡ 111.9 t/s` — status-row fragments painted over the composer.
The app survives (typing still works once restored), but the degraded frames are
unreadable and there is no `resize` guard like the one `drawPicker` has
(`w < 24` closes the modal rather than leaving it invisible while owning the
keyboard).

### Status

Fixed in this branch, each with a test that fails without the change: **1**
(`C-r`), **2** (the sidebar column), **4** (the settings tabs), **5** (the
read-only row). Still open, with the evidence above: **3** (bottom-edge drag at
the tail — a one-sided gap in a mechanism that works at the top), **6**
(`NO_COLOR` — needs a decision on whether the TUI honours it or overrides it),
**7** (`C-c` on an unsent draft), **8** (the sub-3-row terminal).

---
---

## Verified working (no action)

- **Wheel scroll** — `▲n▼n` tracks it exactly, 3 rows per notch, direction-correct, and it survives a redraw.
- **Scrollbar drag** — the thumb tracks the pointer, the selection is not started by the grab, the clipboard is untouched.
- **Drag select + copy** — document-row anchoring works, the copy notice (`Copied 181 chars`) rides the composer divider and expires on its own.
- **Edge auto-scroll at the TOP** — arms and scrolls from the tick alone.
- **Slash dropdown** — fuzzy filter, `Up`/`Down`/`Tab`/`Enter`, correct truecolor theming (with `NO_COLOR` unset).
- **Model picker** — filter-as-you-type with a live `n/m` counter, mouse click on a row, wheel moves the selection, `Enter` commits. (Typing a space is dropped from the filter — `if r != ' '` at app.go:1524 — so `open code` becomes `opencode`; a `kilo-auto` query works, a query needing the space does not. Minor.)
- **Message menu** — a click on a user row opens it; `jump` → the read-only surface, `copy` → the real clipboard, `revert`/`fork` → the tree rewind.
- **Trajectory ledger** — click on the sidebar row opens it, the wheel moves the selection, a click opens the inspector.
- **Thinking box focus** — clicking a box aims the wheel at it, the border goes bold, the wheel scrolls its own window.
- **Keyboard:** `Enter`, `C-j` (0x0A newline), `C-u`, `C-l`, `C-o` (expand), `F5` (retry, does not clobber a draft), `A-m`, `A-a`, `A-t`, `A-s`, `S-Up`/`S-Down`, `PgUp`/`PgDn`, `Home`/`End`, `Shift-Tab` (cycles the session mode `default` → `auto` → `plan`; the reasoning toggle that used to hold the chord is now `thinking-toggle`, user-bindable), `Esc` double-tap rewind, `C-c` cancel-then-quit.
- **Resize** at 46x24 and 60x8 recovers cleanly; overlays do not trap the keyboard at 20x6.
- **Slash menu** `/trajectory` as the working keyboard route to the ledger.

---

## Method notes / traps

- `tmux -f /dev/null` → `TERM=dumb` in the pane → all colours collapse to SGR 7. Every "the theme is broken" observation from that setup is an artefact.
- `NO_COLOR` in the harness environment → same collapse, and it also makes `theme.CapabilityFromEnv()` report monochrome.
- SGR mouse bytes must use the **press form** (`M`, not `m`) for wheel: `\x1b[<64;c;rm` is delivered by tcell as `ButtonNone`, not `WheelUp`. A wheel test built on the release form silently tests nothing.
- `tmux send-keys M-t` etc. deliver the bytes a real terminal sends (`mods=ModAlt`); hand-built `tcell.NewEventKey(tcell.KeyCtrlR, 0, tcell.ModNone)` does **not** match what a terminal delivers and will make a live chord look broken when it is not.

---

*Last updated: 2026-09-28 (interactive audit + the four fixes it produced: `C-r` recall, the sidebar's column guard, the settings tab strip's click hit-test, and the read-only row's notice. Every fix is pinned by a test that fails on `fa37024`, and the binary was rebuilt and re-driven through the same pty harness for each one. Still open: the bottom-edge drag at the tail, `NO_COLOR`, quitting on an unsent draft, and sub-3-row terminals.)*
