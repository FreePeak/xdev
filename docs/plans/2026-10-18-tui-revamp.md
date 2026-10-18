# Plan: TUI revamp

**Date:** 2026-10-18
**Status:** Proposed
**Scope:** `internal/tui/`, `cmd/xdev/tui.go`, `internal/config`

Line numbers in this plan are anchored to `origin/main` at `d2fa6c1`. Re-check
them before acting on a stale checkout.

## Related

- `docs/research/2026-10-03-empryo-tui-parity.md` — parity gaps, R-* backlog
- `docs/research/2026-10-06-tui-refactor-omp-opencode-grok.md` — 8-phase refactor
- `docs/plans/2026-10-14-open-subagent-session-in-main-tui.md` — shipped subagent panel

## 1. Context

Since 2026-10-03 the TUI shipped ~30 commits: 40fps renderer (#663), keyboard
protocol (#658), settings overlay (#654), transcript window (#635), subagent
panel (#657/#668), effort ladder (#607/#611/#613), Rosé Pine (#604), serve
headless (#644). Of the parity backlog, R-THEME-1 shipped (8 palettes,
2026-10-03).

But the structural findings of both research docs are untouched: none of the
8 refactor phases landed, and `app.go` grew from 5,889 lines at the parity pass
(`5a38e44`, 2026-10-03) to 6,512 today. Two more gaps surfaced in Empryo source
review.

## 2. Current state

| File | LOC | Note |
| --- | --- | --- |
| `internal/tui/app.go` | 6,512 | `handleKey` is 767 of those lines |
| `cmd/xdev/tui.go` | 4,702 | `runTUI` alone is ~2,953 lines |
| `internal/tui/mermaid.go` | 1,714 | `parseMermaid`, no memo |
| `internal/tui/dock.go` | 1,460 | |
| `internal/tui/askoverlay.go` | 1,266 | `handleAskKey` + `drawAskCard` |
| `internal/tui/commands.go` | 1,165 | `dispatch` at line 774 |
| `internal/tui/markdown.go` | 964 | `renderMarkdown`, no memo |

Two structural gaps, both confirmed by reading Empryo source:

1. **Config hot-reload is theme-only.** Settings and models load once at
   startup, so editing a preset means a restart. The machinery already exists
   and is proven: `theme.Watch` (`theme/custom.go:486`) polls a theme file
   every 2s and re-applies on a successful parse, keeping the last-good theme
   when an edit is broken. Empryo does the same shape with fs-watch
   (`surface-host.ts`) plus stop-and-rebuild rather than apply-in-place.
2. **One synchronous dispatch.** `dispatch(app CommandAPI, input string) bool`
   (`commands.go:774`) runs input handling inline on the event loop, so no
   async result can outlive the state it was computed against. That safety is
   a side effect of the synchronous design, not a guard: Empryo's tab loop
   (`tab-loop.ts`) is async and pays for it explicitly with a generation
   counter that drops work scheduled before a rebuild. Any move toward async
   input handling, and WS-4, needs that counter.

## 3. Scope

In scope:

- Overlay routing (WS-1) and key handling (WS-2)
- Tool-call grouping (WS-3), parity R-CONV-1
- Markdown/mermaid render caches (WS-3), refactor Phase 8
- Config hot-reload with a generation-guarded rebuild (WS-4, WS-5)

Out of scope (already shipped, do not redo): renderer throughput, keyboard
protocol, settings overlay, transcript window, subagent panel, effort ladder,
Rosé Pine theme, headless serve, R-THEME-1.

Out of scope (deferred, still open in the parity backlog): R-CONV-2 inline
images, R-CHR-1 default context/cost segments, R-CHR-2 git state in the status
row, R-IN-1 per-session drafts, R-IN-2 fuzzy recall, R-IN-3 paste collapse,
R-IN-6 `$EDITOR` hand-off, R-OVL-1 command palette, R-OVL-2/3/4, R-TUI-1
flush idempotence, R-SCRL-1 scroll velocity. None of these depend on the
router; they are separate slices.

Out of scope (separate): `runTUI` in `cmd/xdev/tui.go` is ~2,953 lines of
wiring and is the same class of problem as `app.go`, but it is a different
file with a different owner. WS-4 will touch it (reload wiring); splitting
it is its own plan.

## 4. Workstreams

### WS-1 Overlay router

Today overlays are two hand-ordered chains, both written out longhand in
`app.go`:

- the key chain, `app.go:3315-3357`: `handleAskKey`, `handleHubRosterKey`,
  `handleSessionPickerKey`, `handlePickerKey`, `handleTreeKey`,
  `handleSettingsOverlayKey`, `handleTrajectoryKey`, `handleDiffOverlayKey`,
  `handleMsgMenuKey`, each guarded by `if a.handleXxxKey(key) { return }`
- the draw chain, `app.go:5206-5215`: the matching `drawXxx(composerTop)` calls

Each chain is ordered by hand and the comments carry the reasoning ("the ask
card is the topmost modal", "the message menu sits above the diff overlay").
That reasoning is real and worth keeping. It is just currently encoded as
position in two lists, so every new overlay is a two-place edit and the
correct position is only knowable by reading both chains.

Replace both chains with one router: overlays push on open, pop on close, and
the router walks top-down for both key and mouse. Precedence stops being
position in two lists and becomes open order.

One decision to settle before writing it: the router's order is either
open-order (LIFO, what the 2026-10-06 doc proposed) or a static priority
field on each overlay. LIFO is less to maintain but changes behaviour
wherever two overlays can be open at once — diff overlay plus message menu is
the live case (`app.go:3350-3355`), and today the message menu wins by being
listed later, not by being opened later. If the two can be open in either
order, LIFO silently reorders their keys. Decide by test, not by argument:
write the open-in-both-orders case first and see which order it should give.

Migration order follows the 2026-10-06 doc (ask, hub, session picker, picker,
tree, settings, trajectory, diff, msg menu, status popup) rather than
reversing it. One overlay per step, full suite green before the next, and the
overlay's existing `*_test.go` unchanged as the gate.

Target: adding an overlay means implementing it and pushing it, not editing
two chains.

### WS-2 Key-handler componentization

`handleKey` (`app.go:3191-3957`) is one 767-line function. With the router in
place it splits into three components with an explicit order: overlays (the
router), chrome chords (`app.go:3740-3764`, which today work by running after
every modal), then the transcript and composer fallthrough. Keep `handleKey`
as the single entry point so the event loop is untouched.

### WS-3 Tool grouping and render caches

Two separate costs, both in the draw path.

Grouping: consecutive tool calls render one row each today (`toolhl.go:256`
`toolOutputHL` is per-`Block`, with no run-length pass over consecutive
blocks). Group them under one collapsible entry (parity R-CONV-1). The parity
doc already specifies the shape: a `toolGroup` that owns an ordered run of
call/result pairs, breaking on a kind change, on an assistant text segment, or
on a non-adjacent `CallID`, and reusing `Sub []*SubActivity` (`blocks.go:67`)
rather than inventing a second child model. Worth doing on its own for
readability; the render win follows.

Caching: `renderMarkdown(src, w)` (`markdown.go:92`) and `parseMermaid(src)`
(`mermaid.go:164`) are pure functions of their input with no memo, so every
repaint re-parses unchanged content. Key a cache on `(content hash, width,
mdStyle)`. Style is part of the key because `mdStyle()` depends on the theme,
and the theme changes at runtime (Rosé Pine). This is refactor Phase 8's
prefix/tail cache, and it wants Phase 1's stamp: a stamp is how the painter
knows a cached render is still valid.

### WS-4 Config hot-reload

Watch the settings file and reload `config.LoadSettings` (and
`LoadModelsLayered`) on change. Today those run once at startup, so a preset
edit means a restart.

The poll-and-apply pattern already ships for themes: `theme.Watch`
(`theme/custom.go:486`) ticks every 2s, re-parses on an mtime change, and only
applies on a successful parse. Reuse that shape and that failure rule — a
broken edit keeps the last-good config rather than leaving the session
unconfigured. Empryo's fsnotify is the alternative and is not worth a new
dependency for a config file the user edits by hand.

The difference from themes is what "apply" means. A theme is a value the
painter reads; settings and models are inputs to running work. So follow
Empryo's order (`surface-host.ts`): stop the running work, rebuild from the
reloaded config, start again — do not mutate live config in place, which is
how a half-reloaded session happens.

Which reloads are safe is a separate question from the mechanism. Credentials
(`config.LoadCredentials`) and secrets are held by providers with their own
lifetime and are out of scope for the first cut; settings and models are the
two the user edits and expects to take effect. Emit a one-line status line on
reload so the change is visible, including when a reload is rejected.

### WS-5 Generation-guarded input loop

Add a generation counter, bumped on every rebuild. Async work and timers
capture the generation they were scheduled under and are dropped when it no
longer matches, as `tui-host.ts`'s bridge-lock steal protocol does when a new
generation supersedes the old one.

Today `dispatch(app CommandAPI, input string) bool` (`commands.go:774`) runs
synchronously on the event loop, so there is no window for a stale write to
land. WS-4 creates that window: a rebuild stops and restarts work, and an
in-flight result arriving afterwards must not be applied to the new
generation. Build the counter with WS-4, not after it.

## 5. Sequencing

| Step | Work | Depends on |
| --- | --- | --- |
| 1 | WS-1 overlay router | — |
| 2 | WS-2 key handlers | WS-1 |
| 3 | WS-5 generation guard | WS-1 (rebuild path must exist) |
| 4 | WS-4 config hot-reload | WS-5 |
| 5 | WS-3 grouping and caches | WS-1, WS-2 |

WS-5 before WS-4 is not optional: hot reload without the guard is the exact
class of bug Empryo's generation counter exists to prevent. Steps 1-4 are one
release; step 5 is independent and can be reordered if a correctness bug
jumps it.

## 6. Traceability

- WS-1 is refactor Phase 2 (overlay router). WS-3 grouping is refactor Phase 3
  and parity R-CONV-1; its cache half is refactor Phase 8. Phase 1 (`View`
  interface, stamp memoization) is a prerequisite the cache half wants, because
  a stamp is what tells the painter a cached render is still valid. Take Phase
  1 when WS-3 starts, not before.
- Phases 4-7 (per-session draft, context segments, paint throttle, stdout
  backpressure) are not in this plan. Paint throttle and backpressure look
  largely landed given the 40fps work (#663); the parity doc has no matching
  R-* item for backpressure, so re-verify against §3.3 of the refactor doc
  before re-proposing.
- WS-2 is not in the 2026-10-06 doc. It exists because `handleKey` grew past
  what Phase 2 alone addresses, and because the chrome chords depend on
  running after every modal, which is only true until something reorganises
  that chain.
- WS-4 extends a pattern that already ships (`theme.Watch`), so it is a
  smaller change than it reads. WS-5 is not in either doc; it comes from
  reading `tui-host.ts`, `surface-host.ts`, and `tab-loop.ts`, and it exists
  because WS-4 turns a value-apply into a work-restart.
- The `<2,000` line `app.go` target is aspirational, not a gate: `app.go` is
  6,512 lines today, WS-1 and WS-2 remove overlay and key code, but render and
  state still dominate. Measure the split after step 2 and restate the number
  rather than assuming it lands.

## 7. Success criteria

- The key chain and draw chain at `app.go:3315-3357` and `app.go:5206-5215`
  are gone; every overlay resolves through the router for both keys and
  mouse.
- A test overlay is usable from a test file with no production file outside
  the router touched.
- Opening two overlays in either order gives the same key owner both times,
  or the difference is a deliberate, tested one. This is the LIFO-vs-priority
  question in WS-1, pinned by a test.
- Editing config while a prompt is in flight reloads cleanly, and the
  in-flight result does not land on the rebuilt state.
- Consecutive tool calls render as one grouped, collapsible entry (R-CONV-1);
  `read read read edit bash read` paints as one group plus one group plus one
  lone call, and `CallID` pairing still holds when two same-name calls run
  concurrently.
- A repaint of unchanged content does not re-enter `renderMarkdown` or
  `parseMermaid`; a theme change does re-render.
- `go test ./internal/tui/... ./cmd/xdev/...` passes, including the existing
  `chrome_test.go` and `commands_test.go` coverage.

## 8. Risks

- **Overlay regression during migration.** Each overlay keeps working for
  every step; a full-suite pass per step is the gate, not the end.
- **Key precedence surprises.** WS-2 changes ordering behaviour, and the
  chrome chords only work because they run after every modal. Record the
  precedence explicitly in code before splitting, cover it with tests, and do
  not touch call sites until it is covered.
- **LIFO reorders keys.** A router that walks open-order can hand a key to a
  different overlay than the hand-written chain did. The tests that gate each
  migration step are what catch this; if one of them has to be edited rather
  than kept unchanged, stop and re-decide the router order.
- **Hot-reload data loss.** An in-flight prompt at reload time is the real
  risk, not a crash. WS-5 first, and state in the PR that the guard is what
  makes reload safe.
- **A reload that half-applies.** `theme.Watch` applies only on a successful
  parse; a settings reload must reject the same way rather than apply a
  partially-decoded Settings. Keep the last-good config and say so on the
  status line.
- **Cache staleness.** A cache that misses an input renders stale content,
  which reads as a bug in the theme rather than in the cache. Width and
  `mdStyle()` must be in the key. Cover both in tests, not by inspection.
