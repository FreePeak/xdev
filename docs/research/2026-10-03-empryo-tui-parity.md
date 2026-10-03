# Empryo (SoulForge) → xdev TUI parity analysis + requirements

**Date:** 2026-10-03 · **Subject:** the terminal UI of [Empryo](https://empryo.com) (`npm @proxysoul/soulforge` v2.20.25, BUSL-1.1), read as a *reference implementation* for xdev's `internal/tui`.
**Question this answers:** which parts of Empryo's TUI are worth adopting into xdev, which xdev already exceeds, and which must be rejected.
**Companion:** [PRD.md §3.5](../PRD.md) (xdev's TUI contract), [QA-TUI-INTERACTIVE.md](../QA-TUI-INTERACTIVE.md) (the real-pty mouse/keyboard audit), [2026-09-28-deepseek-harness-gap.md](2026-09-28-deepseek-harness-gap.md) (the same gap-table shape, against a different harness).

## 0. Verdict

**xdev is not behind on the hard parts.** On the three things a TUI cannot fake — bounded rendering, terminal capability honesty, and input plumbing — xdev is *ahead*:

- **Rendering budget.** xdev's row index (`internal/tui/rowindex.go:12-38`) re-renders only blocks whose stamp moved and charges O(viewport), not O(session). Empryo has **no virtualization at all**: `MessageList` renders every message unconditionally (`src/components/chat/MessageList.tsx:1563-1587`) and bounds *render count* instead (`MAX_RENDERED = 40`, `src/components/layout/TabInstance.tsx:88`) behind hand-written per-row memo comparators. Both work; xdev's is the one that survives a 50k-entry session.
- **Terminal honesty.** xdev quantizes RGB → 256 → 16 at startup and honours `NO_COLOR` (`internal/theme/theme.go:548`, `:483`, `:501`, `:527`). Empryo's theme path emits raw 24-bit SGR with **no truecolor gate and no `NO_COLOR`/`TERM=dumb` degradation in the TUI at all** — `NO_COLOR` is set only on *child* processes (`src/hearth/service.ts:158`, `src/core/tools/auto-format.ts:45`).
- **Input plumbing.** xdev has a 250 ms frame-write deadline with a `Sync()` self-heal (`internal/tui/app.go:4350-4361`), a signal guard that restores the tty on SIGTSTP/SIGHUP/SIGTERM/SIGTTOU/SIGTTIN (`cmd/xdev/tui_signal_unix.go:36-70`), and a stall watchdog (`internal/tui/stall.go:19-40`). Every one of these exists in xdev because a real pane died. Empryo has the analogous fixes in `src/core/terminal/suspend.ts` and `src/index.tsx:51-81`, but they are TypeScript around a native renderer.
- **Mouse.** xdev: double/triple-click word/line select, drag-select with edge auto-scroll, click-to-open links, per-message menu, scrollbar-thumb drag, per-pill click targets, tab close `×` (`internal/tui/selection.go:234-482`, `internal/tui/app.go:2933-3004`). Empryo: three button codes (64/65/0), no double-click, no drag (`src/hooks/useMouse.ts:5-7`, `:47-49`).

**The real gaps are three and a half things, and they are all readability, not robustness:**

1. **Consecutive tool calls are not grouped** (R-CONV-1). xdev paints one box per call; Empryo folds a 12-read turn into one file tree. This is the single largest reading-experience gap in the comparison.
2. **No inline image rendering** (R-CONV-2). xdev pastes images and shows a `[Image 256x128 #3]` chip — the model gets the bytes, the human sees nothing. Stdlib `image/png` already decodes them.
3. **The composer draft crosses session boundaries** (R-IN-1). Measured on the built binary: a draft typed in session one is still in the composer after `C-x n` opens session two, so a prompt written for one context can be sent into another.
4. **Context occupancy and spend are not on the status row by default** (R-CHR-1), so context pressure and cost are one click away instead of one glance.

Everything else is Should/Could, plus a short reject list (§5). **All four Musts were then driven on the built binary in a real pty (§6b) — three held as written, and the fourth turned out to be a worse bug than the code reading suggested.**

## 1. Method and evidence standard

Eight source-level passes: five over Empryo (chrome/layout, conversation rendering, input+editor, overlays/modals, theme/rendering/platform) and three over xdev's `internal/tui` (chrome, conversation, input/overlays). Every claim below carries `file:line` on the side it is about. Where a pass did not read a file, the claim is marked *not verified* rather than repeated from the other side.

Two facts worth stating up front so no reader over-reads the comparison:

- **Empryo's shipped product string is "SoulForge"** (`src/components/App.tsx:1558`), the npm name is `@proxysoul/soulforge`; "Empryo" is the rebrand. Cited paths are to the repo as checked out.
- **Size is not the story.** The Empryo UI surface read here — `src/components/chat/**`, `src/components/layout/**`, `src/components/{App,ModalLayer}.tsx`, `src/stores/*.ts`, `src/hooks/*.ts` — is ~24.3k lines. xdev's `internal/tui` is 46,046 lines across 42 non-test files with 90 test files alongside. xdev's TUI is larger *and* holds a hard budget; Empryo's smaller *and* ships no test coverage claim for the TUI surface read here.

## 2. Surface map

| Region | Empryo | xdev |
|---|---|---|
| Root layout | one column pinned to terminal height, `App.tsx:1541-1547` | `paint()` composes per frame, `app.go:4369` |
| Header | 1 row: version+update chip / provider › model › branch+dirty › mode › ContextBar › TokenDisplay / BrandTag ≥80 cols, `App.tsx:1548-1613` | 1 row: git branch + running spinner, `welcome.go:180` |
| Tab bar | always when `tabCount > 1`, `App.tsx:1615-1640`; max 5, `useTabs.ts:9`; per-tab attention/compacting/loading/error/mode/`✎N`, `TabBar.tsx:52-106` | row 1, only when `len(tabs) >= 2`, `tabstrip.go:35`; ✦ running, unread, underline current, `×` close |
| Transcript | `scrollbox` sticky-bottom + viewportCulling, `TabInstance.tsx:725-741` | own row index, `rowindex.go:90-97`; sticky prompt header, `sticky.go:65` |
| Right panel | optional Neovim PTY (split 40/50/60/70) + per-tab 20% changes+terminals, `TabInstance.tsx:848-853`, `stores/ui.ts:151` | context dock, 42 cols, `auto\|show\|hide`, opens ≥120 cols, `dock.go:40-64`, `:273-281` |
| Composer | OpenTUI `<textarea>`, grows to `max(4, 40% rows)`, `InputBox.tsx:846`, `:997-1022` | `[]rune` + cursor, `editor.go:13-28`, grows with wrapped rows |
| Status | header center segments + a 3-tier shortcut footer, `Footer.tsx:21-32`, `:276-301` | one right-aligned segment row, `app.go:5204`, `:5239` |
| Overlays | 32-name boolean map + a `SimpleModalLayer` (`ModalLayer.tsx:23`) + ~28 App-level modals + a LIFO dialog store, `stores/ui.ts:7-42`, `:155-162`, `stores/dialog.ts:39-54` | 10-surface precedence ladder on one screen, `app.go:3012-3067` |

## 3. Gap tables by surface

Legend: **HAVE** = already there · **PART** = partially · **GAP** = absent · **AHEAD** = xdev exceeds it · **REJECT** = do not adopt.

### 3.1 Chrome & layout

| # | Capability | Empryo evidence | xdev today | Verdict |
|---|---|---|---|---|
| C1 | Context occupancy (bar + % + `used/window`) in the header | `ContextBar.tsx:9`, `:83-106` | `hudSegment` "context" exists (`app.go:5437`) but is **not** in `defaultStatusSegments` (`app.go:5349`) | **GAP** → R-CHR-1 |
| C2 | Cost in the header | `TokenDisplay.tsx:26-39`, pricing table `stores/statusbar.ts:44-119` | "cost" segment exists (`app.go:5445`), off by default | **GAP** → R-CHR-1 |
| C3 | Dirty/staged/untracked counts | branch + `*`, amber/success, `App.tsx:1586-1594`; 5 s poll `useGitStatus.ts:27-59` | branch on row 0 only, no dirty state; the four `status_line_git_*` tokens are *reserved, unconsumed* (`theme.go:170-176`) | **GAP** → R-CHR-2 |
| C4 | Context bar turns red above 80 % | `ContextBar.tsx:20-26`, `:101` | none | **PART** → folded into R-CHR-1 |
| C5 | Per-tab mode chip in the tab bar | `TabBar.tsx:89-97` | no mode per tab (mode is process state) | **GAP** → R-IN-4 (per-tab state) |
| C6 | Edited-file count per tab (`✎ N`) | `useTabs.ts:16-23`, `TabBar.tsx:98-106` | dock FILES section (`dock.go:68-77`) | **HAVE** (different place) |
| C7 | Footer shortcut bar, degrades by width across 3 tiers | `Footer.tsx:21-32`, `:276-301` | none | **GAP** → R-CHR-4 (Could) |
| C8 | Tab eviction: refuse at 5 | `useTabs.ts:9`, `:109-119` | cap 6, evict oldest **parked idle**, never a running tab (`cmd/xdev/tabset.go:33`, `:106-124`) | **AHEAD** |
| C9 | Welcome screen | logo + tagline + rune field + breathing divider, `LandingPage.tsx:16-118`, `:122-168` | 8-row wordmark with sheen sweep + 4-row menu naming only real controls (`welcome.go:197`, `:23-30`) | **HAVE** (xdev's is quieter and honest about its chords) |
| C10 | Clickable status pills | none anywhere (`App.tsx:1563-1611` has no handlers) | 2 pills, each with a breakdown popup (`statuspill.go:65-68`, `:244-300`) | **AHEAD** |

### 3.2 Conversation rendering

| # | Capability | Empryo evidence | xdev today | Verdict |
|---|---|---|---|---|
| V1 | **Group consecutive tool calls** (edits/reads/search into a collapsible file tree; meta tools into one line) | `tool-grouping.ts:3-89`; tree render `MessageList.tsx:980-1140`; tree connectors `ToolCallDisplay.tsx:488-513` | **one `Block` per call**, no grouping; only label suppression when the previous block is the same tool (`app.go:4181-4184`); kinds `blocks.go:14-23` | **GAP** → R-CONV-1 |
| V2 | Retry-hiding: a failed edit later retried on the same path is hidden | `MessageList.tsx` retry scan (chat pass) | both rows shown | **GAP** → R-CONV-4 (Could) |
| V3 | Denial as a first-class **neutral** state (skip glyph, grey, not red) | `MessageList.tsx:283-285`, `StaticToolRow.tsx:533-541` | `Status` is `running\|ok\|error` only (`blocks.go:31`) | **GAP** → R-CONV-5 (Could) |
| V4 | Text-drip / typewriter reveal with word-boundary snapping | `useTextDrip.ts:3-9` (32 ms, 0.8–16 chars/tick, eased), `:171-187` | deltas land at provider speed; no drip anywhere | **GAP** → R-CONV-8 |
| V5 | Split diff view | `DiffView.tsx:10`, `:150-159` | **HAVE** — `internal/tui/diff_split.go` (#553, on `origin/main`): split at ≥120 interior cols, `s` overrides, per-side numbering from the hunk header (`diff_split.go:31`, `:58`, `:179`) | **HAVE** (xdev's gate is width, not a stored preference) |
| V6 | Diffs >50 changed lines collapse to `N lines changed` | `DiffView.tsx:8`, `:78`, `:136-139` | box render window + `Ctrl+O` expand (`app.go:4212-4214`, `:4284`); `Expanded` opts out of the trim (`rowindex.go:50-52`) | **HAVE** (better: xdev trims by age tier, not only by size) |
| V7 | Inline images: Kitty graphics + half-block ANSI fallback + capability gate | `ImageDisplay.tsx:29-100`; `core/terminal/image.ts:95-114`, `:401-`, `:152-`, `:775-` | **none** — no image block kind, no cell path; paste yields a chip (`clipboard_image.go:30-55`, `paste.go:83`) | **GAP** → R-CONV-2 |
| V8 | Reasoning collapsed to one row, grown to a 12-row focus window | `ReasoningBlock.tsx:94-115`, 150 ms throttle + 4096-char tail `:35-53` | 1 row collapsed / 12 rows focused (`rowindex.go:64-65`, `app.go:4067-4070`), junk gate `junk.go:48` | **AHEAD** (xdev also judges junk) |
| V9 | Streaming reasoning shows total line count, not just the tail | `ReasoningBlock.tsx:75-93` | n/a | **REJECT** — xdev's tail window is the point |
| V10 | Structured plan view + "awaiting review" affordance | `MessageList.tsx:497-587` | plan mode surfaces in the dock (`dock.go:68`, `planOps` `app.go:1715`) | **HAVE** |
| V11 | Child/sub-call nesting under a parent row | `MessageList.tsx:909-930`, `:1028-1038` | `Sub []*SubActivity`, capped at 3 painted rows (`blocks.go:59-67`, `:102`) | **HAVE** |
| V12 | Markdown: native renderable, tree-sitter highlighting, GFM tables | `Markdown.tsx:60-81` (OpenTUI `<markdown>` + web-tree-sitter) | hand-written subset: fences, headings, quotes, lists, GFM tables, inline bold/italic/code/links (`markdown.go:89-221`); hand-rolled per-line lexer, byte-preserving by construction (`highlight.go:354`, `:446`) | **HAVE** — the tree-sitter route is a documented non-goal (PRD §3.6, CGO) |
| V13 | `<system-reminder>` stripping / verbose rewrite | `Markdown.tsx:25-39` | not present | **GAP** → R-CONV-6 (Could) |
| V14 | Checkpoint rail (3-col dot strip over the checkpoint list) | `CheckpointRail.tsx:19-38`, `:75-109` | message tree + per-message revert (`tree.go`, `msgmenu.go:253-266`), no rail | **GAP** → R-CONV-7 (Could; xdev's tree *is* the rewind model) |
| V15 | Mermaid | not in these files (relies on OpenTUI's renderable) | 1,714 lines: sequence + flowchart, bounded at 240 rows / 80 nodes / 200 messages, refuses a parse whole (`mermaid.go:33-44`) | **AHEAD** |
| V16 | Per-turn `✓ Completed in Xs` footer | `MessageList.tsx:1439-1446` | tool boxes carry `⟦Wall: … │ Exit: N│` (`app.go:4286-4304`) | **HAVE** (different unit) |

### 3.3 Composer, editor, paste

| # | Capability | Empryo evidence | xdev today | Verdict |
|---|---|---|---|---|
| I1 | Draft survives tab/session switch (restored + consumed on mount) | `InputBox.tsx:202-219` | **single `Editor`**; only the Esc stash (`app.go:126-132`, `:3080-3105`) | **GAP** → R-IN-1 |
| I2 | Draft stash on demand (`Alt+S`/`Alt+P`) | `InputBox.tsx:727-737`, `:740-755` | Esc stash only | **PART** → folded into R-IN-1 |
| I3 | Fuzzy history recall (`Ctrl+R`, full-input capture, 500 items) | `InputBox.tsx:655-712`, `:232` | `C-r` = `history-prev` only (`keymap.go:140`) | **GAP** → R-IN-2 |
| I4 | Large paste collapses to one placeholder line, expands at submit | ≥4 lines → `<pasted (first-30-chars… +N lines)>` (`InputBox.tsx:568-577`), expanded `InputBox.tsx:428-434` | no collapse; the paste lands whole and the box pages to the cursor past `composerBudget` (`paste.go:252-270`, `app.go:5106-5129`) | **GAP** → R-IN-3 |
| I5 | Code-looking paste wrapped in a fence | (n/a) | `looksLikeCode` → fenced (`paste.go:265-267`, `:377-`) | **AHEAD** |
| I6 | Rotating placeholder tips | `InputBox.tsx:164-173` | one dim literal, `"Type a message…"` (`app.go:5074-5083`) | **PART** → R-IN-5 (Could) |
| I7 | Ctrl+C clears-or-exits; never exits on Shift | `useGlobalKeyboard.ts:88-99`, `InputBox.tsx:714-722` | `quit` → `quitOrCancel(running)` (`app.go:3214-3216`) | **HAVE** |
| I8 | `Ctrl+M/1..9` direct tab jump, `Ctrl+[`/`]` prev/next, `Ctrl+B`/`Ctrl+F` checkpoint nav | `useGlobalKeyboard.ts:144-186` | `C-x` leader pairs + `A-]`/`A-[` + unread variants (`keymap.go:176-179`, `:198-203`) | **HAVE** (different chords; `/hotkeys` is the map) |
| I9 | Embedded Neovim / floating terminal | `core/editor/neovim.ts:102-146` + shipped LazyVim `init.lua`; `EditorPanel.tsx:23-52` | none; `go.mod` has no PTY dep | **REJECT** — see X4; the cheap half is R-IN-6 |
| I10 | `$EDITOR`/`$VISUAL` hand-off | (n/a — in-app editor instead) | none | **GAP** → R-IN-6 (Should) |

### 3.4 Overlays & modals

| # | Capability | Empryo evidence | xdev today | Verdict |
|---|---|---|---|---|
| O1 | Command palette (fuzzy, grouped, Tab-to-category, suggested group) | `CommandPalette.tsx:59-60`, `:141-165`; registry `core/commands/registry.ts:143` | slash dropdown + `/hotkeys` text dump (`suggest.go:69-96`, `keymap.go:382-396`) | **GAP** → R-OVL-1 (Should; see the lazy form) |
| O2 | Runtime command registration from tools/agents/extensions | **none** — 15 static `register*` modules only | `SetExtensionCommands` / `RunExtensionCommand` reach the dropdown (`commands.go:907-923`) | **AHEAD** |
| O3 | Cross-tab usage comparison + per-model cost breakdown + process tree | `StatusDashboard.tsx:58`, `:304-332`, `:777-805`, `:1118-1274` | 2 pills + `/usage` transcript report (`statuspill.go:244-300`, `usage.go:84-186`) | **GAP** → R-OVL-2 (Should) |
| O4 | In-TUI git actions (9-item menu, destructive behind confirm) + commit modal | `GitMenu.tsx:26-60`, `GitCommitModal.tsx:35-58` | none in `internal/tui` | **GAP** → R-OVL-3 (Should) |
| O5 | Memory browser with pin/hide/cleanup-batch over 4 tabs | `MemoryBrowser.tsx:35`, `:209-282`, `:491-502` | `/memory view\|stats\|clear\|queue\|sync\|enqueue\|diagnose` as a transcript report (`commands.go:285-330`) | **PART** → R-OVL-4 (Could) |
| O6 | First-run wizard with a real provider/key/model step | `wizard/data.ts:3-15`, `SetupStep.tsx:39-61` | `xdev login` / `xdev setup` are CLI subcommands (`cmd/xdev/main.go:537-568`) | **REJECT** — a TUI wizard for something `login` already does headlessly |
| O7 | Modal stacking / nesting | mostly ad-hoc; `dialog.ts` barely wired, `select` kind is a stub | one-panel precedence ladder (`app.go:3012-3067`) | **PARITY** — neither stacks cleanly; not worth work |
| O8 | Overlay exclusivity rule | `openModal` resets the map then sets one (`stores/ui.ts:155`) | ladder: first consumer wins | **PARITY** |

### 3.5 Theme & rendering engine

| # | Capability | Empryo evidence | xdev today | Verdict |
|---|---|---|---|---|
| T1 | Built-in palettes | **36** (`tokens.ts:1879-1916`), 45 tokens (`:19-70`), `_extends` inheritance (`loader.ts:87`) | **2** (`theme.go:418-423`) + custom JSON + 2 s-poll live reload (`custom.go:466-497`) | **GAP** → R-THEME-1 (Should; content is mechanical) |
| T2 | Per-element opacity + true terminal transparency | `loader.ts:134-171`, `"transparent"` as `bgApp` | transparency only via an explicit `""` slot (`theme.go:256-262`); opacity blending is a documented off-by-default non-goal (PRD §3.5) | **REJECT** — runtime colour generation is exactly what PRD §3.5 declined |
| T3 | `NO_COLOR` / non-truecolor degradation in the TUI | **absent** — 24-bit SGR regardless; `NO_COLOR` only set on child processes | honoured (`theme.go:548`) + quantization (`:483`, `:501`, `:527`) | **AHEAD** — do not regress |
| T4 | Frame budget / dirty tracking | 60 fps cap + memo comparators; `externalOutputMode:"passthrough"`, `useKittyKeyboard`, `targetFps:60` (`index.tsx:367-385`) | ~30 fps tick + coalescing `dirty` channel, frame-write deadline, `Sync()` self-heal (`app.go:2745`, `:2733-2738`, `:4350-4361`) | **PARITY** |
| T5 | Glyph/symbol preset + ASCII fallback | nerd-font detection, two-stage probe (`core/icons.ts:255-282`) | symbol presets in theme JSON (`internal/theme/symbols.go`), mermaid ASCII vocabulary (`mermaid.go:95-112`) | **HAVE** |

## 4. Requirement backlog

Effort: **S** ≤ 1 day · **M** 2–4 days · **L** 1–2 weeks. "Ceiling" lines mark a deliberate simplification with a named upgrade path.
| I1 | Draft survives tab/session switch (restored + consumed on mount) | `InputBox.tsx:202-219` | **single `Editor`**; only the Esc stash (`app.go:126-132`, `:3087-3096`) | **GAP** → R-IN-1 |
### Must

**R-CHR-1 — Context occupancy and spend are on the status row by default.**
*Why:* today the row ships `["sessions","command",pillTime,pillToken]` (`app.go:5349`); the `context` and `cost` segments are implemented (`app.go:5437`, `:5445`) but opt-in, so context pressure and spend cost a click. Both are computed and cached already.
*Do:* add `context` and `cost` to `defaultStatusSegments`; give `context` a ≥85 % warning and ≥95 % error tint; keep `statusKeepRank` (`app.go:5639-5651`) shedding them first on a narrow row.
*Acceptance:* at 120 cols a fresh session shows `ctx 4.1k/200k · $0.0012`; at 60 cols the row sheds cost before time; a 90 %-full context renders in the warning ink and a click on the tokens pill still opens the full breakdown.
*Effort:* **S**. *Test:* table-driven over `hudSegment` per segment name + one narrow-width degradation case.

**R-CONV-1 — Consecutive tool calls collapse into one grouped block.**
*Why:* a 12-read turn paints 12 boxes today. Empryo folds them into one file tree (`tool-grouping.ts:3-89`, `MessageList.tsx:980-1140`). This is the biggest reading gap in the comparison.
*Do:* add a `toolGroup` block kind (a name this doc proposes — nothing named that exists yet) that owns an ordered run of call/result pairs; group breaks on a kind change, on an assistant text segment, or on a non-adjacent `CallID` (`blocks.go:36` already exists for that reason); a collapsed group is one line with `✓ N calls · +A/−D`; expanded it lists each child under it with its own result window. **Reuse `Sub []*SubActivity` (`blocks.go:59-67`) for the shape rather than inventing a second child model** — it already carries `{Label, Tool, Status, Calls, Dur}` and already has a paint cap.
*Acceptance:* `read read read edit bash read` paints as one group + one group + one lone call (3 boxes, not 6); `Ctrl+O` on the group expands every child and each child still honours its own head/tail window; `CallID` pairing still holds when two same-name calls run concurrently.
*Effort:* **L**. *Test:* one synthetic transcript exercising the break conditions plus one concurrent-same-name pairing case.

**R-CONV-2 — Inline image rendering in the transcript.**
*Why:* xdev reads a clipboard image and shows `[Image 256x128 #3]` (`clipboard_image.go:30-55`, `paste.go:83`); the model sees the bytes, the human sees a chip. stdlib `image/png` is already imported for dimension probing (`paste.go:34-36`).
*Do:* a `KindImage` block (proposed name; `blocks.go:14-23` has no such kind today); render by capability — half-block ANSI art (universal) or Kitty graphics when the terminal is confirmed capable. Follow Empryo's documented gate shape (`core/terminal/image.ts:95-114`: iTerm2/Warp/WezTerm/Konsole excluded; Kitty ≤ 0.37 because 0.38+ grapheme clustering breaks placeholder placement) rather than probing optimistically.
*Ceiling:* Kitty only, plus half-block fallback — **no sixel** (Empryo has none either) and **no image *upload* path** beyond paste. Upgrade path: `show-image` tool results reuse the same block once that tool exists.
*Acceptance:* pasting a PNG shows the image under the composer turn in half-block art; on a Kitty-capable terminal it renders as a Kitty image; a corrupt or unsupported file falls back to the existing chip with an honest error, never a blank block; a 12 MB image is refused at the existing cap (`paste.go:57`).
*Effort:* **L**. *Test:* golden-cell comparison of half-block art against a fixed synthetic PNG; capability-gate table test.

**R-IN-1 — Composer draft is per session, and never leaks across one.**
*Why:* one `Editor` serves the whole app (`editor.go:13-28`); the only stash is Esc's (`app.go:126-132`). **Measured on the built binary (2026-10-03, §6b): a draft typed in session one is still sitting in the composer after `C-x n` opens session two.** Two consequences, and the second is the worse one:
  - switching sessions and back loses nothing, because the text never left — but
  - **a draft crosses a session boundary it was never meant to cross**, so a prompt written for one context can be sent into another. That is a correctness bug wearing a UX gap's clothes, and it is why this is a Must.
*Do:* a `map[sessionID][]rune` on `App`, saved on session switch and restored on arrival, with the Esc stash layered on top (Esc clears-and-stashes, a second Esc restores) rather than replaced. Switching to a session with no entry **clears** the composer — that half is what stops the leak.
*Ceiling:* in-memory only — a process restart still loses drafts. Upgrade path: persist to the session dir when a crash-recovery story exists.
*Acceptance:* type in session one, `C-x n`, type nothing — the composer is empty and the old text is restored verbatim when you switch back; the same holds across `/resume` and `/fork`; a send clears the entry for that session only; a draft from a deleted session cannot reappear.
*Effort:* **S**. *Test:* two-session round trip, a switch-to-clean-session assertion (the leak case above), and a send-clears-one-session case — all three fail against the current shape.

### Should

**R-CHR-2 — Dirty/staged/untracked in the status row.** Branch and `*` (Empryo `App.tsx:1586-1594`) plus the four already-declared-but-unused `status_line_git_*` tokens (`theme.go:170-176`). Do: one 5 s `git status --porcelain` poll behind a version stamp, the way the row index already stamps. Ceiling: counts, not per-file lists. **M.**

**R-IN-2 — Fuzzy history recall.** `Ctrl+R` opens a full-input capture over the last 500 entries with scored matching; `Enter` loads the selection, `Esc` cancels; the arrow keys keep today's behaviour. Reuse `fuzzyScore` (`suggest.go:127-157`) — it already scores prefix, word-start, consecutive and sparse-gap matches. **M.**

**R-IN-3 — Large paste collapses to one line.** ≥ N lines pastes render as `‹ pasted: 142 lines ›`, expanded at submit so the model still receives the full text (Empryo `InputBox.tsx:568-577`, `:428-434`). xdev's fence-wrapping (`paste.go:265-267`) stays orthogonal. Ceiling: threshold by line count, not bytes. **S.**

**R-IN-4 — Per-tab model, thinking level and mode.** xdev's model is a process-wide holder (`cmd/xdev/tui.go:96-97`); a tab is `{id,title,store,running,cancel,unread}` (`cmd/xdev/tabset.go:41-58`). Empryo gives every tab its own `ContextManager` and `useChat` (`TabInstance.tsx:139-142`, `:210-223`). This is the one **architectural** item on the list: it moves session state from process scope into `tabset`. Order it after the Musts — it is a refactor, not a feature. **L.**

**R-OVL-1 — A command palette, in the lazy form.** xdev's slash dropdown already enumerates builtins + markdown commands + skills + extension commands (`suggest.go:69-96`). Do: bind `Ctrl+K` (a free chord — nothing owns it) to *open that dropdown focused*, and add grouped headers + a suggested group when the query is empty. Do **not** build a second registry: xdev's dynamic command sources make Empryo's static `COMMAND_DEFS` (`core/commands/registry.ts:143`) the thing to move away from. **S.**

**R-OVL-2 — Per-tab and per-model usage comparison.** Extend the token pill (`statuspill.go:244-300`) with a per-tab row set and a per-model cost split, so "which tab spent this" and "which model spent this" are one click instead of a `/usage` scroll. **M.**

**R-OVL-3 — In-TUI git menu and commit flow.** A 9-item menu with destructive items behind an explicit confirm (Empryo `GitMenu.tsx:26-60`) plus a commit modal that shows staged/modified/untracked and a `N lines changed` summary (`GitCommitModal.tsx:35-58`). **Constraint, non-negotiable per this repo's `AGENTS.md`:** the Co-Authored-By toggle from `GitCommitModal.tsx:55-57` must **not** ship as-is — xdev's history carries only the human author, so the modal takes a free-text trailer field or none at all, and never writes an AI-agent name. **M.**

**R-IN-6 — `$EDITOR`/`$VISUAL` hand-off.** One chord suspends the screen, execs the editor on a path (current file / a scratch buffer for a draft), waits, reloads. This is the cheap half of the embedded-editor idea (X4) and the part a daily driver actually feels. **M.**

**R-THEME-1 — More built-in themes, and a picker.** The file format, `Slot()`/Defaults transparency and live reload all exist (`custom.go:69-82`, `:466-497`); only the palettes and a picker UI are missing. 36 is Empryo's number; ship whatever can be verified against their palettes' licence provenance header (`tokens.ts:1-17`). Keep xdev's two Grok-derived defaults as the shipped default. **M** (content), **S** (picker: a new `picker` view over `LoadNamed`).

### Could

**R-CONV-8 — Streaming text-drip.** *Careful:* xdev's tick is deliberately a clock, not a repaint loop — `animate` is set for exactly six things (`app.go:2818-2869`), and a drip adds a seventh that runs for the whole turn. The lazy version that gets 90 % of the value: **paint streaming assistant text at most every N ms** (xdev already throttles live tool boxes to 100 ms, `livePaint` `app.go:1029`) and snap the reveal to a word boundary. Do not port the velocity easing (`useTextDrip.ts:3-9`). Ceiling: a rate limit, not an animation. **S**, opt-in.

**R-CONV-4 — Hide a failure that was later retried on the same path.** **S.**

**R-CONV-5 — Denial as a neutral state.** Add a `denied` `Status` value alongside `running|ok|error` (`blocks.go:31`) with a skip glyph in the neutral ink. **S.**

**R-CONV-6 — Strip `<system-reminder>` from rendered assistant text.** Providers echo them and they are noise in a transcript. **S.**

**R-CONV-7 — Checkpoint rail.** A 3-column dot strip naming rewound/viewing/live points (Empryo `CheckpointRail.tsx:19-38`). Only worth it if xdev grows a first-class checkpoint list; today the message tree *is* the rewind model (`tree.go`, `msgmenu.go:253-266`) and a rail would be a second way to say the same thing. **M.**

**R-IN-5 — Rotating composer tips.** **S.**

**R-CHR-4 — Footer shortcut bar.** 3 tiers, width-degraded (Empryo `Footer.tsx:276-301`). Costs a transcript row; xdev's status row is the grok-parity target and PRD §3.5 has already dropped a permanent row spent on one word (`tabstrip.go:33-34` says the same). **S** if wanted, **skip** if the row budget is sacred.

**R-OVL-4 — Memory browser surface.** Pin/unhide/restore + the four batch-cleanup classes (dupe/dead/stale/similar — `MemoryBrowser.tsx:209-282`), replacing `/memory view` as a transcript report. **M.**

## 5. Reject list — do not adopt

| # | Empryo behaviour | Why xdev says no |
|---|---|---|
| X1 | No transcript virtualization; a 40-message render cap plus hand-written per-row memo comparators (`MessageList.tsx:1563-1587`, `TabInstance.tsx:88`) | xdev's row index charges O(viewport) and is what makes a 50k-entry session interactive (`rowindex.go:12-38`). A cap would be a regression. |
| X2 | Raw 24-bit SGR with no truecolor gate; `NO_COLOR` honoured only on child processes | xdev quantizes and honours `NO_COLOR` (`theme.go:483-548`). Keep it. |
| X3 | Runtime colour generation (`blendBgOpacity`, `brighten`, opacity knobs — `loader.ts:100-117`) | PRD §3.5 turned runtime background blending off deliberately (`bg_blend` is the Grok-CLI knob's name, not an xdev identifier): every colour comes from a declared slot. |
| X4 | Embedded Neovim + a floating ghostty terminal | Needs a PTY dep (absent from `go.mod`) and a native terminal emulator. Empryo itself disables its ghostty renderer on Windows for dlopen segfaults (`core/platform/index.ts:126-130`) — a native surface xdev's single-binary CGO-free promise cannot carry. Take `$EDITOR` (R-IN-6) instead. |
| X5 | Unconditional animation, no reduced-motion path (wordmark recolour at 10 Hz, cross-fades, glitch) | xdev animates only while something is actually moving (`app.go:2818-2869`). |
| X6 | A first-run wizard that collects an API key | `xdev login` / `xdev setup` already do it headlessly (`cmd/xdev/main.go:537-568`). |
| X7 | A static `COMMAND_DEFS` table as the palette's source of truth (`core/commands/registry.ts:143`) | xdev discovers markdown commands, skills and extension commands at runtime (`discovery.go:32-70`, `suggest.go:69-96`). Copying a static table would lose that. |
| X8 | Keyboard shortcut *set* wholesale (`useGlobalKeyboard.ts:70-186`) | xdev's chords are opencode/omp/grok-derived and already carry the vendor muscle memory; `/hotkeys` (`keymap.go:382-396`) plus `keybindings.yml` (`keymap.go:208-210`) is the remap story. Adopt individual chords only where a row here names one (`Ctrl+K`). |

## 6. Not verified

- **Overlay internals beyond their call sites.** For xdev, `drawSessionPicker`, `drawAskCard`, `drawSettingsOverlay`, `drawHubRoster`, `drawTrajectory`, `drawTreeSelector`, `drawQueue`, `drawToasts` were read by signature and call site only; their internal geometry is unverified.
- **Empryo's renderer internals.** `node_modules/@opentui/core` is not installed in that checkout, so every renderer claim here is either Empryo app code, its own comments, or the dependency shape in `package.json` — never the renderer source.
- **Mouse and every keyboard claim** still rest on code reading plus the earlier pty audit in [QA-TUI-INTERACTIVE.md](../QA-TUI-INTERACTIVE.md); they were not re-driven for this document. Any row that changes input or rendering should be verified the same way before it is called done.

## 6b. Live verification of the four Musts (2026-10-03, added after this doc's first draft)

The four Musts are claims about what a user sees, so they were driven on the **built binary** through a forked pty with `pyte` rendering the grid — the method [QA-TUI-INTERACTIVE.md](../QA-TUI-INTERACTIVE.md) used. Setup: the branch binary, an isolated `XDEV_AGENT_DIR`, a scripted chat-completions SSE mock, 140×44, `groknight`, and a **synthetic fixture** (a fictional `Widget` API; the files the mock asks for do not exist, so the reads fail and the transcript is cheap and deterministic). The harness lives under `/tmp/xdev-556/` and is not committed.

| Must | Measured | Verdict |
|---|---|---|
| **R-CHR-1** | the status row paints `0s · 1t·3g`; no `ctx`, no `$` | **confirmed** — the segments exist and ship off |
| **R-CONV-1** | one assistant message with 4 `read` calls → **4 separate `╭─ read ─╮` boxes**, each naming its own file, no group row | **confirmed** — and worse than the doc said: a 4-call turn is 4 call boxes *plus* 4 result boxes |
| **R-IN-3** | a 40-line bracketed paste → **39 lines painted in the composer**, no collapsed placeholder | **confirmed** |
| **R-IN-1** | type in session one, `C-x n` → **the draft is still in the composer on session two** | **confirmed, and the finding changed** |

**R-IN-1 is not the gap this document first described.** Reading the code said "a draft dies on a session switch". Driving the binary says the opposite: nothing is lost, because the single `Editor` is never cleared — **so the draft leaks into the next session.** A prompt written for one context sits in the composer of another, waiting for `Enter`. That is a correctness bug, not a comfort gap, and §4's requirement was rewritten around it: drafts are per session, and switching to a session with no entry must **clear** the composer, which is the half that stops the leak.

The other three held exactly as written. The value of the run was the one row it overturned.
- **No live pty run of either binary was done for this document.** The xdev-side statements about mouse and keys rest on the code plus the earlier pty audit in [QA-TUI-INTERACTIVE.md](../QA-TUI-INTERACTIVE.md); any row that changes input or rendering should be verified the same way before it is called done.

---
*Last updated: 2026-10-03 (Empryo/SoulForge TUI parity pass):* first full source-level comparison of Empryo's terminal UI against `internal/tui`. Eight recon passes over both trees, every claim carrying `file:line`. Verdict: xdev leads on rendering budget, terminal-capability honesty and mouse/input plumbing; the actionable gaps are tool-call grouping (R-CONV-1), inline images (R-CONV-2), per-tab composer drafts (R-IN-1), and default context/cost segments (R-CHR-1) — four Musts, ten Shoulds, eight Coulds, and an eight-row reject list. Nothing is implemented by this document.

---

*Last updated: 2026-10-03 (same day, second pass — an audit of this document against its own claims).* The existence/range check I ran covered only that a cited line **exists**, not that it **says what the sentence claims**. A second script matched every backticked identifier in a citation cell against the cited file (±8 lines, then whole-file fallback), and 6 rows failed it:

- **I4 named a function that does not exist.** I wrote that a big paste "inflates the box to its `maxRows`". There is no `maxRows` in xdev — the box pages to the cursor past `composerBudget` (`app.go:5106-5129`), which is a different mechanism and a better one. Corrected.
- **The overlays row credited `ModalLayer`** where the export is `SimpleModalLayer` (`ModalLayer.tsx:23`). Corrected, and now cited to the line.
- **T1 attributed `_extends` to `tokens.ts`**; it is parsed in `loader.ts:87`. Corrected with the cite.
- **Three rows backticked names this document proposes** (`toolGroup`, `KindImage`, `denied`) as if they were existing code, and one (`bg_blend`) named a Grok-CLI knob as if it were an xdev identifier. All four are now marked as proposed/external. A reader grepping `toolGroup` in xdev should get nothing and now the document says so.

Re-run: 90 citation-bearing rows, **85 fully anchored**; the 5 remaining flags are all the deliberately-labelled proposed names above. Range check: **197/197** in range. No requirement, verdict, priority or effort rating changed — every edit was a citation or a naming correction. The audit is a throwaway script (`/tmp`), not a committed tool: the value was finding six wrong claims, not leaving a linter behind.

---

*Last updated: 2026-10-03 (same day, third pass — §6b: the four Musts driven on the built binary).* The four Must claims are claims about pixels, so they were run through a forked pty with `pyte` rendering the grid, against the branch binary, an isolated `XDEV_AGENT_DIR` and a scripted SSE mock (synthetic fixture: a fictional `Widget` API whose files do not exist, so the reads fail cheaply and deterministically). Harness under `/tmp/xdev-556/`, not committed.

**Three held as written.** The status row ships without `ctx`/`cost`; a 4-`read` turn paints four call boxes *plus* four result boxes with no grouping; a 40-line bracketed paste paints 39 composer lines with no collapsed placeholder.

**The fourth was wrong, and the way it was wrong is the point.** Code reading said the draft "dies on a tab switch". It does not — nothing is lost, because the single `Editor` is never cleared, so **the draft leaks into the next session** and waits there for `Enter`. That is a correctness bug (a prompt written for one context can be sent into another) wearing a UX gap's clothes. R-IN-1 is rewritten: drafts become per-session, and a switch to a session with no entry must CLEAR the composer, which is the half that stops the leak. The §0 summary and the acceptance criteria were updated with it.

Re-verified after the edits: 200/200 `file:line` refs in range, 87/93 citation rows content-anchored (the 6 flags are the deliberately-labelled proposed names), no broken relative links. §6's blanket "no live pty run was done" line is narrowed to what is actually still unverified (mouse and the keyboard set).
