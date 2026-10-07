# TUI Refactor Research — omp / OpenCode / Grok → xdev

**Date:** 2026-10-06 · **Subject:** a comprehensive refactor plan for xdev's `internal/tui`, derived from deep primary-source analysis of three production coding-agent TUIs: **omp/pi-tui** (TypeScript, `@oh-my-pi/pi-tui@18.1.21`), **OpenCode** (`@opencode/tui@2.0.18`, OpenTUI + SolidJS), and **Grok CLI** (closed-source Rust, `grok-1.0.46-macos-aarch64`, ratatui-class).

**Question this answers:** what should xdev's TUI become, what should it stay, and what concrete refactor sequence gets there without breaking the hard-won properties (bounded render, terminal honesty, stall survival).

**Companions:** [PRD.md §3.5](../PRD.md) (xdev TUI contract), [2026-10-03-empryo-tui-parity.md](2026-10-03-empryo-tui-parity.md) (Empryo/SoulForge parity — the fourth reference), [QA-TUI-INTERACTIVE.md](../QA-TUI-INTERACTIVE.md) (real-pty audit), [2026-09-15-xdev-slow-session-rca.md](2026-09-15-xdev-slow-session-rca.md) (the stall chain this refactor must not reintroduce).

---

## 0. Verdict

**xdev's TUI is not behind on the hard parts.** On the three things a TUI cannot fake — bounded rendering, terminal capability honesty, and input plumbing — xdev is at or ahead of all four references. The row index (`internal/tui/rowindex.go`) is a real virtualization boundary; the stall watchdog + unlock-before-`Show()` + frame-drop `Sync()` self-heal is a survival stack none of the JS references match; the mouse depth (drag-select, edge auto-scroll, thumb drag, per-pill hit targets) exceeds every reference.

**The real gaps are structural, not feature gaps:**

1. **`app.go` is a 6,312-line god file.** Paint, input dispatch, agent mutators, HUD, overlay routing, and tool rendering all live on one `App` type. Every reference has a component boundary here; xdev has none.
2. **No component contract.** omp has `render(width) → []string` with reference-equality memoization; OpenCode has Solid memos; Grok has a pager/shell split. xdev has stamps on blocks and ad-hoc `if`-chains for overlay precedence.
3. **Overlay precedence is two parallel ladders** (key vs mouse) that can drift. Every reference has an explicit stack or router.
4. **Status pressure metrics are off by default.** Context and cost exist as segments but are not in `defaultStatusSegments` (`app.go:5835`).
5. **Draft leaks across sessions.** One `Editor` for all tabs; a draft typed in session one is still there after `C-x n` opens session two (Empryo R-IN-1, measured on the built binary).
6. **No tool grouping.** One `Block` per call; a 12-read turn paints 12 boxes. OpenCode and Empryo both fold these; xdev does not.
7. **cmd/TUI split is a second megafile.** `cmd/xdev/tui.go` is 4,589 lines of wiring with thinner unit coverage than `internal/tui`.

**The refactor is a carve, not a rewrite.** Split `app.go` along seams that already exist in the code, introduce an explicit overlay router, add a component interface for memoization boundaries, and port three specific UX patterns (tool grouping, per-session draft, default context/cost segments) that every reference ships and xdev does not.

---

## 1. Method and evidence standard

Four source-level passes, each producing a structured scout report with `file:line` on every claim:

| Reference | Source | Version | Pass |
|---|---|---|---|
| omp/pi-tui | `/Users/linh.doan/node_modules/@oh-my-pi/pi-tui/src/` + `pi-coding-agent/src/tui/` + `modes/components/` | 18.1.21 | Architecture, frame plan, component contract, overlay lifecycle, status segments, composer shapes, images, markdown caches |
| OpenCode | `/Users/linh.doan/work/opensources/opencode/packages/tui/` | 2.0.18 | Stack (OpenTUI + Solid), app shell, transcript virtualization, grouping engine, permission UI, tabs, mini mode |
| Grok CLI | `~/.grok/bin/grok` (closed binary) + `~/.grok/docs/user-guide/` + `~/.grok/bundled/skills/statusline/` | 1.0.46 | Pager/shell split, composer/textarea, scrollback model, scriptable status line, overlay stack, blocking-card protocol, dashboard |
| xdev (self) | `internal/tui/` (45 prod files, 26,669 LOC; 109 test files, 26,484 LOC) + `cmd/xdev/tui.go` (4,589 LOC) | origin/main @ 59038e8 | Architecture as shipped, strengths, structural pain points, file map |

Every claim below carries `file:line` on the side it is about. Where a pass did not read a file, the claim is marked *not verified*.

---

## 2. Reference architectures

### 2.1 omp/pi-tui — the component contract

**Stack:** TypeScript, no framework. Pure string-row composition.

**Component model** (`pi-tui/src/tui.ts:186-233`):
- `render(width): readonly string[]` — one physical row per string; must not exceed width.
- **Reference equality = byte-identical content** — same array ref when unchanged enables Container memo + engine stable-prefix (`tui.ts:189-198`, `:211-216`).
- Optional: `handleInput`, `invalidate`, `wantsKeyRelease`, `setIgnoreTight`.
- `Focusable` with `focused` + optional `setUseTerminalCursor`; emit `CURSOR_MARKER` for IME/hardware cursor park (`tui.ts:261-277`).
- Container memo (`tui.ts:417-511`): caches `#memoLines` / `#memoChildLines` / `#memoWidth`; rebuild only when child line refs change or width changes.

**Frame plan** (`tui.ts:1-13`, `:129-158`):
- Two channels: optional immutable `HistoryBatch` (`id`, `rows`, `kind?: "append"|"replay"`) + complete mutable `viewport: readonly string[]`.
- `TerminalFrameProvider` (`tui.ts:148-158`): `renderFrame`, `acknowledgeHistory`, optional `renderResizeFrame`, `beginHistoryReplay`, `beginHistoryFlush`.
- Engine never infers finality from row position — product owns retirement; writer anchors viewport under remaining history.
- Product owner: `pi-coding-agent/src/modes/composer.ts:198` `class Composer implements TerminalFrameProvider`; `setFrameProvider(this)` at `:276`.

**Paint cadence** (`tui.ts:753-769`, `:1958-2102`):
- `MIN_RENDER_INTERVAL_MS = 1000/30` (~30 fps floor).
- Adaptive backpressure: next frame waits ~`2 × lastFrameCostMs`, cap `MAX_ADAPTIVE_RENDER_MS = 200` (~5 fps floor).
- Stdout backlog gate: defer if `pendingOutputBytes > MAX_PENDING_OUTPUT_BYTES`.
- Diff path when geometry stable, no history, same top, prior window non-empty (`:2703-2724`): per-row absolute CUP + rewrite only changed prepared lines.
- Full path (`:2725-2761`): erase unfinished live viewport before scroll push; write history then viewport; never full-screen clear except destructive ED2+ED3.

**Overlay lifecycle** (`tui.ts:1026-1079`):
- `showOverlay(component, OverlayOptions?) → OverlayHandle`.
- Stack entry saves `preFocus`; focus if visible; `hide` / `setHidden` restore focus to top visible or `preFocus`.
- Options: width/maxHeight %, anchor, margin, `visible(w,h)`, `fullscreen` → alt screen 1049h, mouseTracking.
- Fullscreen paints only modal; normal buffer + history accounting untouched.

**Status line** (`modes/components/status-line/segments.ts:146-854`):
- ~40 segment IDs: `pi`, `status`, `model`, `mode`, `path`, `git`, `pr`, `subagents`, tokens, cost, context_*, time, session, hostname, cache_*, collab, vim, usage.
- Settings: left/right segments, separator, contextLine gauge, transparent bg, sessionAccent.

**Composer shapes** (`composer/types.ts:15-24`): box, band, claude, pi, field, rail, borderless, rule — each with `statusAttachment` + `bottomBar` so editor, settings preview, wizard cannot drift.

**Images** (`image.ts:82-128`): `ImageBudget` — cap concurrent inline images, transmit once, placement epochs, demotion to text.

**Markdown** (`markdown.ts:1655-1770`): streaming caches — frozen blank-line prefix tokens + tail row cache + highlight stream — O(N) streaming vs O(N²).

**Size:** `tui.ts` ~3,285 lines; `editor.ts` ~153 KB; `markdown.ts` ~150 KB; `terminal.ts` ~85 KB. Agent `modes/components/` has 97 entries. Total pi-tui `src/` ≈ 15–18k LOC TS.

### 2.2 OpenCode — the projection model

**Stack:** SolidJS + OpenTUI (`@opentui/core`, `@opentui/solid`, `@opentui/keymap`). Not Ink/blessed/tcell. Renderer is `createCliRenderer` + `render()` from `@opentui/*` (`app.tsx:1-19`, `:257-293`).

**App shell** (`app.tsx:1327-1404`):
```
box (full terminal size, column)
├── box row (flexGrow)
│   ├── [optional] SessionTabs vertical + resize handle
│   └── column
│       ├── [optional] SessionTabs horizontal
│       ├── Switch route: home | session→SessionFrame | plugin→PluginRoute
│       └── Slot path="app"   (plugin chrome)
├── DevToolsBar? / StartupLoading? / Reconnecting? / MigrationOverlay
└── Toast
```

**Transcript virtualization** (`session/index.tsx:136-137`, `:400-453`):
- `TRANSCRIPT_TAIL_ROWS = 40`, `TRANSCRIPT_BACKFILL_CHUNK = 60`.
- Only `visibleRows = rows.slice(hidden, visibleEnd)` mount.
- Weights by collapsed group size (`mount-budget.ts:4-21`, `rowWeight` / `rowsBefore` / `rowsAfter`).
- Scroll up near top → reveal older / `loadMore` history; sticky bottom when following.

**Grouping engine** (`grouping/session.ts:21`, `:78-91`):
- Kinds: `activity | reasoning | exploration | instructions`.
- Exploration tools = read/glob/grep/webfetch/websearch.
- Verbosity: low/medium/high.
- Questions never group. Instruction synthetic messages group by loaded paths.

**Permission UI** (`routes/session/permission.tsx` ~622 LOC):
- Stages, option labels, inline diff scroll, keymap.
- Same diff renderer as review, not a yes/no prompt.

**Tabs** (`session-tabs.tsx:39-78`, `:104-145`):
- Persisted store `tabs` (global or per-cwd).
- Retention: keep current + tab list, limit 3 offline sessions.
- Prefetch delay 300ms after connect.
- Scroll anchors per tab.

**Mini mode** (`mini/index.ts:8-10`, `runtime.ts:1-10`):
- Separate retained scrollback surface: stream commits rows into `ScrollbackSurface` (`mini/scrollback.surface.ts:1-64`).
- Footer-first paint, then stream transport + prompt queue.

**Size:** `routes/session/index.tsx` ~3,610 LOC; `app.tsx` ~1,406 LOC; `mini/runtime.ts` ~1,100 LOC; `permission.tsx` ~622 LOC. ~30 bundled theme JSONs.

### 2.3 Grok CLI — the interaction contract

**Stack:** Closed-source Rust, ratatui-class. Crate paths from binary strings: `xai-ratatui-inline`, `xai-ratatui-textarea`, `xai-grok-pager`, `xai-grok-shell`, `xai-grok-login`.

**Pager/shell split:**
- **Pager (UI):** `xai-grok-pager` — scrollback, overlays, slash pager-builtins.
- **Shell (agent):** `xai-grok-shell` — agent/session/tool builtins.
- **Login pre-TUI:** `xai-grok-login` / `pre_tui`.

**Two render modes:** `ui.screen_mode = fullscreen | minimal` (`26-config-reference.md:660`).

**Composer/textarea:**
- Custom textarea (ratatui-textarea fork): multiline toggle `Ctrl+M` when prompt focused.
- Modes on empty prompt: `!` shell, `#` remember; Esc exits mode.
- Chips: path-free image chips `[Image #N]`; path only in hover/preview overlay.
- Draft stash git-stash style `Ctrl+S` / `Alt+S`; border shows `Stashed`; in-memory only, single slot.
- Double-Esc 800ms: clear+stash (non-empty) or rewind picker (empty+history).
- Queue pane for mid-turn follow-ups; `follow_up_behavior = queue | steer`.

**Scrollback/pager:**
- Entry-oriented list (not raw line buffer): select entry, fold/expand, turn jumps, sticky headers.
- Follow mode with overscroll-to-follow, manual-fold pins (`respect_manual_folds`), auto-scroll stop on expand while following.
- Fullscreen block viewer Enter/Ctrl+F; copy y / metadata Y.
- Optional timeline tick rail instead of scrollbar: `ui.show_timeline`.
- Tool UX densification: `group_tool_verbs` folds consecutive read/search/list (+ finished thoughts); `collapsed_edit_blocks` +N/-M.

**Status line (scriptable):**
- Optional row above shortcuts bar (full) / under prompt info (minimal has no row) — default disabled.
- Types: `builtin | command | disabled`.
- Builtin items: `cwd`, `model`, `context`, `cost`, `turn-timer`, `session-name`.
- Command: JSON on stdin → stdout (≤5 lines × 1024 chars, ANSI kept, OSC8 http(s)/mailto kept); 10s timeout.
- Security: only user/`requirements` config — not repo `.grok/config.toml`.

**Overlay stack:**
- Command palette Ctrl+P / `?`; slash / @ / completion dropdowns; model picker Ctrl+M; session picker Ctrl+R; settings F2 / Ctrl+,; theme picker `/theme`; extensions Ctrl+L; history search ↑ empty / Ctrl+R.
- Blocking cards: question, MCP elicit, permission, cancel-turn.
- **Blocking-card contract:** shared Tab wrap inside card; Esc clears then parks to scrollback (card stays visible) for question/permission; priority order permission > cancel-turn > question > elicit; bar always shows focused surface's keys.

**Dashboard:** first-class multi-session roster (Ctrl+\): peek, reply, dispatch, pin, worktree toggle.

**Theme:** groknight (default), grokday, tokyonight, rosepine, oscura, terminal (feature-gated), auto/OS. Truecolor/256/16 + NO_COLOR; `/doctor` for terminal health.

**Size:** 144 MB Mach-O arm64 binary.

---

## 3. xdev as shipped

### 3.1 Architecture

**App struct** (`internal/tui/app.go:116-374`):
- The entire interactive surface: tcell screen, theme, `blocks`, scroll model, editor, keymap, status, tabs, dock, overlays, selection, row index, stall atoms.
- Comment contract: model thread mutates via exported mutators (mutex); UI loop redraws ~30fps and on keys.
- Core lock: `mu sync.Mutex` (`app.go:124`). Separate `cmdMu` for active-command HUD. Atomic: `loopBeat`, `wedged`, `frameDropped`.
- `rowIdx rowIndex` is the frame-plan buffer.

**Event loop / paint path:**
```
PollEvent goroutine → keyq
Run(): select quit | keyq→handleKey+drainKeys+draw | dirty→draw | 33ms tick
draw() = paint() [holds mu] → markComposerDirty → Show() [NO mu] → optional Sync if frameDropped
```
- `Run` `app.go:2947-3013`; tick 33ms (~30fps); poller `:2955-2993`.
- `draw` deliberately unlocks before `Show()` (`app.go:4709-4730`).
- `poke` non-blocking dirty signal; agent path uses mutators + poke.
- Burst coalesce: one draw after `drainKeys` — fix for scroll lag.

**Stall watchdog** (`stall.go:11-102`):
- UI loop is single-goroutine for handleKey+draw. Watchdog never takes `App.mu`.
- Thresholds: stall 5s, check 1s, slow-log 10s, poll-skip 30s sleep false-alarm.
- Dump stacks to `stallDir`; `SetStallExitAfter` ends wedged session. cmd wires 90s + `scr.Fini` restore.
- Wedged quit served off-loop in poller. Bounded restore 2s then `os.Exit`.

**Frame-plan under tcell (row index)** (`rowindex.go:11-101`):
- Not omp native scrollback retirement: own scrollback, prebuilt per-block lines.
- `blockRend` stamped; cumulative `start[]`; aged tool trim tiers.
- `paint`: `dockBuild` → `sync(contentW)` → `sm.NewContent` → viewport slice + sticky.
- Cost model: O(viewport) paint after O(dirty blocks) re-render.

**Block model** (`blocks.go:16-23`):
- Kinds: User/Assistant/Thinking/Tool/ToolDone/System.
- Tool pairing by `CallID` under concurrency. Live streaming boxes, Sub children capped (`subRowsMax=3`).
- No multi-tool group kind — one block per call.

**Editor/composer** (`editor.go:13-69`):
- Runes, history ≤500, wantCol sticky.
- Esc draft stash on App. One editor for all tabs — draft not per-session.

**Keymap** (`keymap.go:20-79`):
- YAML-remappable `KeyMap`, leader 2s, `<leader>` token expands at load.
- Dispatch: overlay chain then `keyMap.Resolve`.

**Overlay precedence (ad-hoc ladder, not a stack):**
- Keys (`app.go:3215-3269`): ask → hub → session picker → picker → tree → settings → trajectory → diff → msg menu → status popup Esc → Esc draft/tree → keymap.
- Mouse (`app.go:3153-3181`): ask → picker → hub → settings → trajectory → msg menu → status popup → tab strip → queue → else selection/dock.
- Diff/settings/ask are separate `*State` fields on App.

**Dock/sidebar** (`dock.go:15-84`):
- 42-col chrome, auto@≥120, versioned rebuild via `dockBumpSeq` atom.
- Sections plan/tasks/files/agents/mcp/trajectory. Drawn under full-width overlays.

**Tabs/status:**
- Tabs owned by cmd `tabset`; App paints `[]TabInfo`. Policy auto|on|off.
- Default HUD segments: `sessions`, `command`, time pill, token pill only — not context/cost (`app.go:5835`).

**Agent events → UI:**
- `tuiHooks` in cmd, not package tui (`cmd/xdev/tui.go:3037-3157`).
- `OnEvent` switches stream events → `h.ts.paint(func(){ app.Begin/Append/... })`.
- Tools: `OnToolStart/End`, task children via `taskChildSink`.

**Concurrency model:**
| Actor | Role |
|---|---|
| UI goroutine | `handleKey`, `paint` under `mu`, never holds `mu` across `Show` |
| Poller | `PollEvent` → `keyq`; wedged quit |
| Watchdog | beat atom only |
| Agent / tools | call App mutators (lock `mu`), `poke` |
| Dock writers | `dockBumpSeq` atom, no App lock |
| Deadline tty | write timeout 250ms; macOS often no deadline |

### 3.2 Strengths (keep)

| Strength | Evidence |
|---|---|
| Bounded render / long-session | `rowindex.go:12-37`; aged tool trim |
| `mu` not across tty write | `app.go:4765-4766`, `draw` `:4709-4722` |
| Frame drop repair | `frameDropped` + `Sync` `app.go:4723-4730` |
| Stall self-diagnosis + exit | `stall.go` whole; 90s `tui.go:418` |
| Mouse depth | selection drag/edge/thumb, double-click, docks, tabs |
| Sticky user prompts | `sticky.go:1-35` |
| Mermaid bounded ASCII | `mermaid.go:3-26`, max 240 rows/80 nodes |
| Split diff | `diff_split.go:1-31`, gate 120 cols |
| Thinking box + junk gate | `rowindex.go:58-64`; `junk.go` |
| Terminal honesty | theme quantize; composer whole-row dirty for reattach |
| Key burst coalesce | `drainKeys` + one draw |
| Dock cheap rebuild | version bump, no I/O per frame |
| Test density on freezes/locks | many `*_lock_test`, `stall_*`, `draw_hang_*` |

### 3.3 Structural pain points (refactor targets)

1. **Monolith `app.go` — 6,312 lines.** God surfaces: `Run`, `handleKey` (~hundreds of lines of ladder), `paint`, HUD, tool render, mutators. Hardest file to review/bisect.

2. **Coupling: paint + input + agent API on one type.** Hundreds of `func (a *App)` mutators sit beside pixel paint. No `Component`/`View` contract. cmd hooks reach deep into App.

3. **Missing memoization / component boundaries.** Row index is the only real memo boundary. Overlays, dock, composer, status each ad-hoc.

4. **Overlay precedence ad-hoc.** Parallel if-chains for key vs mouse. No LIFO stack; exclusivity is "first handler returns true".

5. **Status segments off-by-default for pressure metrics.** `defaultStatusSegments` omits context/cost. Segments exist in map/hudSegment switch but need config.

6. **Draft/session isolation.** Single `ed Editor`; Esc stash only. Tab switch keeps draft.

7. **No tool grouping.** One `KindTool`/`KindToolDone` per call; only same-name label suppression.

8. **Stall/sync residual risk.** macOS: write deadline often unavailable → unbounded write again; watchdog 90s backstop. Agent still sync-calls App under mu via hooks. `paint` still holds `mu` for full layout.

9. **Test surface vs production.** Dense unit tests in-package; interactive QA required real PTY. cmd wiring thinner unit coverage.

10. **`ponytail:` ceilings (named).** `tty_deadline.go:33-37` deadline not writer queue; macOS `:99-103`. `dock.go:61-63` long title eats panel. `pathindex.go:105` one traversal never refreshed. `blocks.go:108-110` max 3 sub rows. `diff.go:250` word-diff quadratic ceiling. `askoverlay.go:1195` heuristic window. `app.go:1595`, `:4210`, `:4585` lexer full body. `selection.go:636` missing button-up. `usage.go:147` bang-mode invisible.

11. **cmd/TUI split weight.** Session tabs, hooks, MCP notices, send-now waits live in `cmd/xdev/tui.go` — App is "view+controller", cmd is "application service", but both are megafiles.

### 3.4 File map

| LOC | File |
|---|---|
| 6,312 | `app.go` |
| 1,714 | `mermaid.go` |
| 1,438 | `dock.go` |
| 1,266 | `askoverlay.go` |
| 1,144 | `commands.go` |
| 959 | `selection.go` |
| 933 | `markdown.go` |
| 685 | `settings.go` |
| 677 | `keymap.go` |
| 625 | `highlight.go` |
| 593 | `hubroster.go` |
| 510 | `diff.go` |
| 379 | `rowindex.go` |
| 365 | `diff_split.go` |
| 358 | `blocks.go` |

Package dependency: `cmd/xdev/tui.go` → `tui.App` + many `Set*Ops` / handlers → agent, ai, session, tool, config, theme, collab. `internal/tui` (single package) → tcell/v2, internal/theme, internal/config, internal/logx, internal/tool. NO import of agent/ai/session inside tui core (ops interfaces / closures injected from cmd).

---

## 4. Gap analysis — adopt / reject / ahead

### 4.1 Adopt (with evidence)

| # | Pattern | Source | xdev today | Verdict |
|---|---|---|---|---|
| A1 | **Component contract** — `render(width) → []Row` with stamp-based memoization | omp `tui.ts:186-233`; OpenCode Solid memos | Row index only; overlays/dock/composer ad-hoc | **ADOPT** — the single highest-leverage refactor |
| A2 | **Explicit overlay router** — ordered table, not parallel if-chains | omp `tui.ts:1026-1079`; Grok blocking-card protocol | Two ladders (key/mouse) that can drift | **ADOPT** — replace with one `[]overlay` slice + `handleKey`/`handleMouse` methods |
| A3 | **Tool grouping** — fold consecutive read/search/list into one collapsible group | OpenCode `grouping/session.ts:21-91`; Empryo `tool-grouping.ts:3-89` | One `Block` per call | **ADOPT** — pure view over consecutive blocks; no agent change |
| A4 | **Per-session draft** — draft lives in tab state, not global App | Grok `Ctrl+S` stash; Empryo `InputBox.tsx:202-219` | Single `Editor`; draft leaks across tabs | **ADOPT** — move `Editor` into tab state; Esc stash per-tab |
| A5 | **Default context/cost segments** — pressure metrics on the status row by default | OpenCode `ContextBar.tsx:9-106`; Empryo R-CHR-1 | Exist but off by default | **ADOPT** — add to `defaultStatusSegments` |
| A6 | **Adaptive paint throttle** — cost-aware duty cycle, not fixed 30fps | omp `tui.ts:753-769` | Fixed 33ms tick | **ADOPT** — measure frame cost, skip frames under load |
| A7 | **Stdout backpressure gate** — defer frame if tty buffer full | omp `tui.ts:2094-2102` | Frame drop + Sync self-heal | **ADOPT** — check `tcell.Screen` pending bytes before compose |
| A8 | **Streaming markdown prefix/tail cache** — O(N) not O(N²) | omp `markdown.ts:1655-1770` | Hand-rolled per-line lexer | **ADOPT** — cache frozen prefix rows, only re-render tail |
| A9 | **Blocking-card protocol** — Tab stays inside card; Esc parks to scrollback | Grok `03-keyboard-shortcuts.md:102-126` | Ask overlay owns keyboard | **ADOPT** — formalize the contract in the overlay router |
| A10 | **Scriptable status line** — external command with JSON contract | Grok `25-status-line.md:52-60` | Builtin segments only | **DEFER** — builtin first; script status as M15 tail |
| A11 | **Mini mode** — footer + retained scrollback, no full chrome | OpenCode `mini/index.ts:8-10` | No mini mode | **DEFER** — only if headless-ish daily use demands it |
| A12 | **Dashboard** — multi-session roster | Grok `23-dashboard.md:1-43` | Hub roster exists | **DEFER** — hub already covers this differently |

### 4.2 Reject (with evidence)

| # | Pattern | Source | Why reject |
|---|---|---|---|
| R1 | **Full vim mode** | omp `vim.ts:25.8 KB`; Grok `simple_mode` | xdev has simple mode + opt-in vim; full vim is a maintenance sink |
| R2 | **Kitty inline images** | omp `image.ts:24.1 KB`; Grok chips | xdev pastes images and shows a chip; stdlib `image/png` can decode but Kitty protocol is a large surface |
| R3 | **LaTeX → unicode** | omp `latex-to-unicode.ts` | Niche; no evidence of demand |
| R4 | **DECCARA rectangular SGR fill** | omp `deccara.ts` | Optimization for a problem xdev doesn't have (tcell handles this) |
| R5 | **OSC66 text sizing** | omp `tui.ts` | Kitty-specific; no demand |
| R6 | **30fps accent-wave animation** | Grok `[animation] fps = 30` | Fights tcell's low-alloc model; static accent rail is the default |
| R7 | **Background blending / `bg_blend`** | Grok `pager.toml` | Runtime color generation fights the theme quantization model |
| R8 | **Embedded Neovim / floating terminal** | Empryo `core/editor/neovim.ts` | No PTY dep in go.mod; `$EDITOR` hand-off is the cheap half |
| R9 | **Full ComposerStyle gallery (8 shapes)** | omp `composer/types.ts:15-24` | xdev has one composer shape; the gallery is over-engineering |
| R10 | **Marketplace / plugin slots** | OpenCode `plugin/context.tsx:53-80` | xdev's ext subprocess protocol covers this differently |

### 4.3 xdev ahead (keep the lead)

| # | Strength | Evidence |
|---|---|---|
| H1 | **Bounded render** — row index is a real virtualization boundary | `rowindex.go:12-37`; Empryo has no virtualization at all |
| H2 | **Terminal honesty** — quantize RGB → 256 → 16, honor `NO_COLOR` | `internal/theme/theme.go:548`, `:483`, `:501`, `:527` |
| H3 | **Stall survival** — watchdog + unlock-before-Show + frame-drop Sync | `stall.go` whole; `app.go:4709-4730` |
| H4 | **Mouse depth** — drag-select, edge auto-scroll, thumb drag, per-pill hit targets | `selection.go:234-482`; `app.go:2933-3004` |
| H5 | **Mermaid bounded ASCII** | `mermaid.go:3-26`, max 240 rows/80 nodes |
| H6 | **Split diff** | `diff_split.go:1-31`, gate 120 cols |
| H7 | **Thinking box + junk gate** | `rowindex.go:58-64`; `junk.go` |
| H8 | **Key burst coalesce** | `drainKeys` + one draw |
| H9 | **Dock cheap rebuild** | version bump, no I/O per frame |
| H10 | **Test density on freezes/locks** | many `*_lock_test`, `stall_*`, `draw_hang_*` |

---

## 5. Refactor sequence

### Phase 1 — Component contract (no behavior change)

**Goal:** introduce a `View` interface and stamp-based memoization without changing what paints.

```go
// internal/tui/view.go
type View interface {
    // Render returns the rows this view paints at the given width.
    // Implementations must return the same slice ref when content
    // is unchanged (stamp-based memoization).
    Render(width int) []Row
    // Stamp increments when content changes; the painter skips
    // re-render when the stamp matches the last painted frame.
    Stamp() uint64
}

type Row struct {
    Cells []Cell
    Style tcell.Style
}
```

**Carve from `app.go`:**
- `chrome.go` — status row, tab strip, composer chrome (border, placeholder, model caption)
- `transcript.go` — the row-index painter (already exists as `rowindex.go`; wrap it)
- `dock.go` — already a separate file; add `Render(width)` method
- `overlay.go` — the overlay router (see Phase 2)

**Tests:** every existing `*_test.go` in `internal/tui` must pass unchanged. New tests assert stamp stability (same ref → same stamp → no re-render).

### Phase 2 — Overlay router

**Goal:** replace the two parallel if-chains with one ordered slice.

```go
// internal/tui/overlay.go
type overlay struct {
    name string
    handleKey func(ev tcell.Event) bool   // true = consumed
    handleMouse func(ev *tcell.EventMouse) bool
    paint func(screen tcell.Screen, x, y, w, h int)
}

type overlayRouter struct {
    stack []overlay  // LIFO; [0] is bottom
}

func (r *overlayRouter) push(o overlay) { r.stack = append(r.stack, o) }
func (r *overlayRouter) pop() { r.stack = r.stack[:len(r.stack)-1] }
func (r *overlayRouter) handleKey(ev tcell.Event) bool {
    for i := len(r.stack) - 1; i >= 0; i-- {
        if r.stack[i].handleKey(ev) { return true }
    }
    return false
}
func (r *overlayRouter) handleMouse(ev *tcell.EventMouse) bool {
    for i := len(r.stack) - 1; i >= 0; i-- {
        if r.stack[i].handleMouse(ev) { return true }
    }
    return false
}
```

**Migration order:** ask → hub → session picker → picker → tree → settings → trajectory → diff → msg menu → status popup. Each becomes an `overlay` value. The existing `*State` fields on App (`ask`, `diffOv`, `msgm`, `statusPop`) become overlay implementations.

**Tests:** `overlay_pane_width_test.go`, `diff_overlay_*_test.go`, `msgmenu_test.go` must pass unchanged. New tests assert LIFO ordering and exclusivity.

### Phase 3 — Tool grouping

**Goal:** fold consecutive read/search/list calls into one collapsible group.

```go
// internal/tui/toolgroup.go
type toolGroup struct {
    toolName string        // "read", "grep", "glob"
    blocks   []*Block      // consecutive blocks of the same tool
    expanded bool
}

func (g *toolGroup) Render(width int) []Row {
    if !g.expanded {
        return []Row{headerRow(g.toolName, len(g.blocks))}
    }
    // render all blocks with tree connectors
}
```

**Integration:** in `rowindex.go`, detect runs of `KindTool` with the same tool name and wrap them in a `toolGroup`. The group is a view; it participates in the row index like any other block.

**Tests:** new `toolgroup_test.go` — grouping detection, expand/collapse, row count. Existing `blocks_test.go` must pass unchanged.

### Phase 4 — Per-session draft

**Goal:** move `Editor` from App into tab state.

```go
// cmd/xdev/tabset.go
type Tab struct {
    // ...
    draft *tui.Editor  // per-tab draft
}
```

**Migration:** `app.go`'s `ed Editor` field moves to `Tab`. The Esc stash becomes per-tab. `switchSession` swaps the editor reference.

**Tests:** new `tabset_test.go` — draft isolation across tabs. Existing `editor_test.go` must pass unchanged.

### Phase 5 — Default context/cost segments

**Goal:** add `context` and `cost` to `defaultStatusSegments`.

```go
// internal/tui/app.go:5835
var defaultStatusSegments = []string{"sessions", "command", "context", "cost", pillTime, pillToken}
```

**Tests:** `statussegment_case_test.go` must pass unchanged. New test asserts the default set.

### Phase 6 — Adaptive paint throttle

**Goal:** measure frame cost and skip frames under load.

```go
// internal/tui/app.go
func (a *App) draw() {
    start := time.Now()
    a.paint()
    cost := time.Since(start)
    a.lastFrameCost = cost
    // next tick waits min(33ms, 2 * cost), capped at 200ms
}
```

**Tests:** new `frame_cost_test.go` — throttle behavior under simulated load. Existing `draw_hang_test.go` must pass unchanged.

### Phase 7 — Stdout backpressure gate

**Goal:** check tty buffer before composing a frame.

```go
// internal/tui/app.go
func (a *App) draw() {
    if a.screen.PendingBytes() > maxPendingBytes {
        a.frameDropped.Store(true)
        return
    }
    // ...
}
```

**Tests:** new `backpressure_test.go` — gate behavior. Existing `draw_hang_test.go` must pass unchanged.

### Phase 8 — Streaming markdown prefix/tail cache

**Goal:** cache frozen prefix rows, only re-render tail.

```go
// internal/tui/markdown.go
type mdCache struct {
    frozenPrefix []Row  // rows that won't change
    tailStart    int    // index where tail begins
}
```

**Tests:** new `markdown_cache_test.go` — O(N) streaming. Existing `markdown_test.go` must pass unchanged.

---

## 6. Risks and mitigations

| Risk | Mitigation |
|---|---|
| Carve introduces a regression in paint | Every phase is behavior-preserving; existing tests must pass before merge |
| Overlay router changes key dispatch order | Migration order is the current order; tests assert LIFO |
| Tool grouping breaks row index | Groups are views; they participate in the row index like any block |
| Per-session draft breaks cmd wiring | `tabset.go` owns the editor; App reads from active tab |
| Adaptive throttle skips frames under load | Cap at 200ms; watchdog still fires at 5s |
| Backpressure gate drops frames | Frame drop + Sync self-heal already exists |

---

## 7. What this refactor is NOT

- **Not a rewrite.** The row index, stall watchdog, mouse depth, and terminal honesty are kept.
- **Not a port of OpenTUI.** The component contract is the idea; the implementation is Go/tcell.
- **Not a feature grab.** Tool grouping, per-session draft, and default segments are the only UX changes; everything else is structural.
- **Not a milestone.** This is a refactor sequence that can land in small PRs, each independently testable.

---

## 8. Success criteria

1. `app.go` drops below 2,000 lines (from 6,312).
2. Every overlay is an `overlay` value in the router; no parallel if-chains.
3. Tool grouping folds a 12-read turn into one collapsible group.
4. Draft typed in session one is NOT in session two after `C-x n`.
5. Context and cost are on the status row by default.
6. Frame cost is measured and throttled; tty backpressure is gated.
7. All existing `internal/tui` tests pass unchanged.
8. New tests cover: stamp stability, LIFO ordering, tool grouping, draft isolation, throttle, backpressure, markdown cache.

---

*Last updated: 2026-10-06 (initial research pass — four-source analysis, refactor sequence, adopt/reject/ahead tables).*
