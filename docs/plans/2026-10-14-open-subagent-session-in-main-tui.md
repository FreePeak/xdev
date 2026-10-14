# Plan: open a subagent session inside the main xdev TUI

*Date: 2026-10-14 · Status: **plan only** (no code in this change) · Branch: `docs/plan-open-subagent-session`*
*Trigger: user could not open a long-running TaskTool subagent session from the same xdev TUI that spawned it; request is to learn Claude Code's shape and apply it to xdev.*

Related:

- `docs/decisions/live-subagent-view.md` (implemented — live child rows + click → panel transcript)
- `docs/decisions/subagent-chains-and-panel.md` (implemented — strip above composer + Alt+B)
- `docs/research/claude-code/cc-subagent-cowork-2026-09-16.md`
- `docs/research/claude-code-internals.md`
- `scripts/tui-subagent-transcript-drive.py`, `scripts/tui-subagent-panel-drive.py`

---

## 0. One-sentence outcome

Make "open this running subagent" a reliable, discoverable action in the **same** xdev TUI process that spawned it — matching Claude Code's *observe / open child / Esc back* contract — without inventing a second transcript surface or breaking yield-only isolation.

---

## 1. What Claude Code actually does (primary source)

Claude Code does **not** have one "subagent open" feature. CC 2.1.x ships four multi-agent products; only two matter for this plan:

| Mechanism | What the human can do while it runs | How "open" works |
| --- | --- | --- |
| **Subagent** (Agent tool / `.claude/agents/*.md`) | watch progress; result is a summary only ("Subagents can't talk to the user") | child has its own context window; parent never streams the child into the model |
| **Forked subagent** (`/subtask`) | panel below the prompt: **observe**, **steer (`f`)**, **`x` to open** | opens the child's conversation in-place; Esc/close returns to the parent |

Supporting surfaces (out of scope for this plan, already partially covered elsewhere):

- **Agent view** (`claude agents`) — supervisor for many background sessions (`attach` / `logs` / `stop`)
- **Agent teams** — separate instances + shared task list (explicitly not adopted; see `docs/parity/harness-cross-check-2026-09-14.md`)
- **Cross-session messaging** — `ListAgents` + `SendMessage` by name

Bundle evidence already captured in-repo (`docs/decisions/subagent-chains-and-panel.md` §1):

- Agent panel = background-tasks dialog with sections `active agents` / `Local agents` / …
- Key hints: *"Press ← again to open agents"*, `ctrl+o` → `app:toggleTranscript`
- Per-row status line via settings `subagentStatusLine` (JSON on stdin)
- Depth cap default 3 (`CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH`); concurrency cap via env

**The load-bearing CC UX contract for "open inside the main session":**

1. Child work is **visible while running** (panel / status), not only after yield.
2. One gesture **opens the child's own transcript** without leaving the parent process.
3. **Esc (or close) returns** to the parent transcript; the parent viewport is not destroyed.
4. Model isolation holds: the parent model still sees **only the yield**, never the child's stream.
5. Optional later: **steer** into the child (`f` in CC forked subagents) and **full focus** (swap the main transcript for the child's).

xdev already decided (and largely shipped) 1–4. Item 5 is explicitly still open.

---

## 2. What xdev already ships (source-verified at HEAD)

### 2.1 Data path (agent side) — complete

```
TaskTool.Execute (foreground)
  → Hub.TrackForeground(spec)           // id = "fg-N"
  → spec.TranscriptID = fgID
  → spec.OnEvent = withForegroundID(t.OnEvent, fgID)
  → SpawnChild
       OnEvent(start/tool/end) carries TranscriptID
  → Hub.SettleForeground(fgID, res, err)
```

- `Hub.Transcript(id, fromSeq)` resolves **jobs and foreground** from one map walk (`hub.go`).
- `Hub.Foreground()` feeds the roster list so `/hub` is not "empty" over a live child.
- Yield-only isolation is unchanged: parent model context still only sees the tool result.

### 2.2 Host wiring (cmd) — complete on paper

```
reg has HubTool{Hub} + TaskTool{Hub: same hub}     // print.go wire path
TUI:
  sessionHub ← reg.Get(HubToolName)
  app.SetHubOps{ Roster, Transcript, Park, Kill, Revive }
  app.SetAgentPanelOps{ OpenChild: app.OpenSubChild }
  taskTool.OnEvent = taskChildSink{app}.onEvent
       → Add/Update/FinishTaskChild
       → AgentPanelStart/Update/Settle
       → SetTaskChildTranscript + AgentPanelSetTranscript(label, TranscriptID)
```

### 2.3 TUI surfaces that already mean "open this child"

| Gesture | Binding / hit | Activation |
| --- | --- | --- |
| Click child row under a `task` call | `subChildAt` → arm `subID` on press, open on no-motion release | `openSubChildTranscript` |
| Click live strip row above composer | `agentHits` → `runPendingAgentOpen` | same |
| Keyboard | **Alt+B** → `app.subagent.transcript` | `openNewestSubChildTranscript` |
| Roster | **Alt+A** / `/hub`, Enter on a row | same panel transcript view |
| Back | **Esc** once → roster (if opened via hub) / leave child view; Esc again closes panel | parent transcript never moved |

Single renderer on purpose: the `/hub` panel's transcript view is the only child surface. That is the Esc contract.

### 2.4 Tests and drives already written

- Unit: `internal/tui/subagent_transcript_test.go` (click, drag-still-selects, empty roster, Esc, Alt+B, batch newest).
- Unit: `internal/tui/agentspanel_test.go`, `internal/agent/hub_foreground_test.go`.
- Real binary: `scripts/tui-subagent-transcript-drive.py`, `scripts/tui-subagent-panel-drive.py` (pty + pyte; mock provider branches on conversation, not request count).

### 2.5 Explicitly still open (from the decisions themselves)

From `live-subagent-view.md` §9:

> **Full focus mode** (Claude Code's Ctrl+B swaps the transcript for the child's). Needs its own render path, scroll save/restore, and Esc ordering against double-Esc rewind — a separate decision, not a mode flag on the panel view.

From `subagent-chains-and-panel.md`:

> Background hub jobs stay out of the live strip (different lifetime / different controls).

Ctrl+B in xdev today is **scroll / dock**, not "focus child" — do not steal it without a decision.

---

## 3. Root-cause analysis: why "I cannot open that subagent" still happens

The feature is **implemented end-to-end in unit tests**. Failures in a real main session almost always come from one of these seams. Ranked by how often the drives already caught them:

### R1 — Open path got a blank `TranscriptID` (row not clickable)

**Symptom:** strip paints the child name, click does nothing; Alt+B is silent; `subChildAt` returns `""`.

**Mechanism:**

- Strip only adds an `agentHits` entry when `r.TranscriptID != ""` (`agentspanel.go`).
- Transcript child rows only become openable after `SetTaskChildTranscript` / hit table fill.
- `withForegroundID` is a no-op when `t.OnEvent == nil`.
- `TrackForeground` only runs when `t.Hub != nil`.

**Likely causes in a live session:**

1. `TaskTool.Hub` not the same hub instance `SetHubOps` reads (two hubs → Transcript(id) → not ok → "no transcript for …").
2. `taskTool.OnEvent` never assigned (host path skipped) → no strip/transcript rows at all, or rows without ids.
3. Race: click before the start event's `AgentPanelSetTranscript` lands (rare; start event carries id immediately when hub is set).
4. Child settled and panel row removed (`AgentPanelSettle`) before the user clicked the strip — strip is *running-only*; settled children remain openable only via the transcript `⎿` row (if the call block still has the hit).

**Probe:**

```text
while child running:
  Alt+B
  expect hub panel title "transcript · <label>" and viewID fg-N
if silent → log whether HubOps.Transcript(fg-N) returns ok
```

### R2 — Panel opened but painted "no background agents yet" over the child

**Symptom:** click "worked" (panel open) but screen reads as empty / ignored.

**Mechanism (already fixed once, still the failure mode if regressed):** panel branch keys off `len(ui.rows)`; open path must refresh roster **and** fold the foreground child into `ui.rows` before paint (`openSubChildTranscript`).

**Probe:** `scripts/tui-subagent-transcript-drive.py` case "panel shows the child's transcript".

### R3 — Hit offset pointed at the call row, not the child row

**Symptom:** click on what looks like the child does nothing or opens the wrong thing.

**Mechanism (already fixed once):** hit `offset` must be the child line inside the block, not 0 (the call row). Drive found this when a simulation-screen test had the call as the first on-screen row.

**Probe:** `TestSubagentHitIsTheChildRowNotTheCallRow` + transcript drive.

### R4 — User expectation mismatch: "open session" ≠ "open transcript panel"

**Symptom:** panel shows the child's messages, user still says "I cannot open the subagent session."

**Two different products:**

| User means | xdev today | CC analogue |
| --- | --- | --- |
| A. Read what the child said / is saying | ✅ panel transcript view | observe + open |
| B. Type into the child / take over its turn | ❌ not built | forked subagent steer + open |
| C. Swap the whole TUI to the child (focus mode) | ❌ explicitly deferred | ctrl+o / focus |
| D. Attach a background job like `xdev attach` | hub job only; fg child is not a job | agent view attach |

If the user wants **B or C**, the current code is working as designed and the gap is product, not a bug. This plan separates **fix reliability of A** from **design of B/C**.

### R5 — Foreground child lifetime

Foreground `task` blocks the parent turn. When the child yields, `SettleForeground` marks it done and the strip drops the row. The transcript `⎿` row remains on the call block for a while; after scrollaway / compaction the click target can vanish even though `Hub.Transcript(fg-N)` may still serve stores until the hub drops them.

**Not a bug by itself**, but it feels like "I can't open it" if the only discoverable entry was the strip.

### R6 — Discoverability

Bindings exist but are easy to miss in a busy TUI:

- Alt+B (open newest child transcript)
- Alt+A (hub roster)
- Alt+P (toggle strip)
- Click on `⎿` row or strip row

No on-screen hint on the child row that it is openable (CC shows panel chrome + key hints). A user who only tries "enter the tab" or "resume session id" will correctly fail.

---

## 4. Target behavior (apply CC to xdev)

### 4.1 Non-negotiables (keep)

1. **One Esc contract, one child renderer** — `/hub` panel transcript view only.
2. **Yield-only isolation** — parent model context never receives child stream.
3. **Foreground ≠ job** — no kill/park/revive on `fg-N` rows; tool call owns ctx.
4. **Strip = running children only**; hub roster lists jobs + foreground for inspection.

### 4.2 Must-fix (reliability of path A)

| ID | Fix | Done when |
| --- | --- | --- |
| F1 | **Assert single hub identity** at TUI wire time: the `Hub` on `TaskTool` and the `Hub` on `HubTool` / `SetHubOps` are the same pointer. Fail loud in dev / log once in prod if not. | unit test on the wire helper; drive still passes |
| F2 | **Always bind OnEvent + Hub before first turn** on every TUI host path (including resume, `/new`, multi-tab session switch). Re-bind when `store`/`reg` swaps. | grep-able invariant test; no path registers TaskTool without Hub when HubTool exists |
| F3 | **Open feedback when id missing**: Alt+B / click on an untracked row sets a one-line notice (`no transcript for …` already exists on panel; chord currently returns false silently). | notice visible; test |
| F4 | **Re-run both pty drives on HEAD** and treat failures as blockers, not "known flaky". | drive exit 0 in CI or documented manual gate |
| F5 | **Settled child still openable from call row** after strip settle, as long as hub still has stores; document lifetime. | test: FinishTaskChild + strip empty + click `⎿` still opens |

### 4.3 Discoverability (small, CC-shaped)

| ID | Change | Notes |
| --- | --- | --- |
| D1 | Dim suffix on openable child rows: `⏎` or `open` hint when `TranscriptID != ""` | paint only; no new mode |
| D2 | Status / footer chip while any fg child running: `subagent · Alt+B` | one place, disappear when none |
| D3 | Hub roster row for foreground already folded in — ensure label is the child name, not only `fg-N` | already partially done via `viewName` |

### 4.4 Product gaps (CC parity — separate slices, decide before coding)

| ID | CC behavior | xdev proposal | Priority |
| --- | --- | --- | --- |
| P1 | **Full focus mode** — swap main transcript for child; save/restore scroll; Esc ordering vs double-Esc rewind | New decision doc; **do not reuse Ctrl+B** (scroll/dock). Candidate: **Alt+Shift+B** or `app.subagent.focus` unbound by default until decided | deferred unless user confirms C |
| P2 | **Steer into child** (`f`) while viewing | Reuse hub `Send` only for jobs today; fg child would need a steer channel into the child's `Agent` (OnRun already captures `*Agent`) | deferred; needs design |
| P3 | **Type-as-user into child session** | True session takeover — out of scope; conflicts with tool-owned ctx | reject unless new milestone |
| P4 | Agent view attach across process death | collab / `xdev serve` + hub jobs; fg children die with the turn by design | out of scope |

**Default reading for this plan:** deliver **F1–F5 + D1–D3** (make A rock-solid and obvious). Do **not** build P1–P3 until the user confirms they need B/C after A works.

---

## 5. Work plan (implementation order)

Worktree discipline: one branch off `origin/main`, e.g. `fix/open-subagent-session`, all edits absolute-path in that worktree. PRD status stamp in the same PR.

### Slice 0 — Confirm the failure class (½ day, no product code)

1. Run on the broken machine / this repo:
   - `go test ./internal/tui/ ./internal/agent/ -count=1`
   - `python3 scripts/tui-subagent-panel-drive.py`
   - `python3 scripts/tui-subagent-transcript-drive.py`
2. Manual: spawn `task` with a long child (or the mock), try in order: strip click → transcript `⎿` click → Alt+B → Alt+A+Enter.
3. Record which of R1–R6 matched. **If only R4**, stop implementing fixes and decide P1/P2 with the user.

### Slice 1 — Wire invariants (F1, F2)

- Centralize "bind hub + task OnEvent + HubOps + AgentPanelOps" in one function called from every session enter path.
- Test: constructing the TUI registry yields `taskTool.Hub == hubTool.Hub` and `OnEvent != nil`.

### Slice 2 — Open feedback + settled open (F3, F5)

- Chord/click soft-fail → `setNotice`.
- Test settled child still opens from call row.

### Slice 3 — Discoverability (D1–D3)

- Paint hint + footer chip.
- Simulation-screen tests for the hint string.

### Slice 4 — Drive gate (F4)

- Document how to run the two scripts in `docs/QA-TUI-INTERACTIVE.md` (or extend it).
- Optional: CI job only if mock+pty is stable in GH runners; otherwise make it a release manual check.

### Slice 5 — (Optional, gated) Focus mode decision + build (P1)

Only after Slice 0–4 and an explicit "I need the child to own the whole screen" confirmation.

---

## 6. Files likely touched (when implementing — not in this docs PR)

| File | Why |
| --- | --- |
| `cmd/xdev/tui.go` | single bind helper; session switch re-bind; F1/F2 |
| `internal/tui/hubroster.go` | notice paths; title/label polish |
| `internal/tui/agentspanel.go` | openable hint; hit feedback |
| `internal/tui/app.go` / `keymap.go` | notice on silent chord; footer chip |
| `internal/tui/*_test.go` | F3/F5/D1 |
| `cmd/xdev/task_child_sink_test.go` | wire invariants if extracted |
| `docs/PRD.md` | status stamp |
| `docs/QA-TUI-INTERACTIVE.md` | drive gate |
| `docs/decisions/live-subagent-view.md` | append "reliability pass" note if behavior changes |

No new dependency. No second transcript renderer. No change to `SpawnChild` yield contract.

---

## 7. Verification checklist (definition of done for the fix PR)

- [ ] `go test ./internal/tui/ ./internal/agent/ ./cmd/xdev/ -count=1` green
- [ ] `go build -o /tmp/xdev ./cmd/xdev` succeeds
- [ ] `python3 scripts/tui-subagent-panel-drive.py` → strip visible, click opens child marker, Esc restores parent
- [ ] `python3 scripts/tui-subagent-transcript-drive.py` → child answer visible in panel
- [ ] Manual long spawn: Alt+B opens while running; after settle, call-row click still opens until hub drops stores
- [ ] Two hubs cannot silently diverge (F1 test)
- [ ] PRD footer stamped
- [ ] No `Co-Authored-By` trailers; pre-commit hooks pass

---

## 8. Out of scope (do not sneak in)

- Agent teams / tmux split panes
- Cloud agents / GrowthBook
- Changing Ctrl+B meaning without a new decision
- Streaming child tokens into parent model context
- Kill/park/revive on foreground children
- Making strip list background hub jobs
- True multi-session tab that *is* the child store (session takeover)

---

## 9. Recommendation to the user

1. **Treat this first as a reliability + discoverability fix of path A** (open child transcript in the existing panel), not as a new "subagent IDE".
2. Run Slice 0 before any code: the pty drives either prove HEAD is already correct (then the bug is environment / binary age / R4 expectation) or reproduce R1–R3.
3. Only after A is boring, decide whether CC-style **focus (P1)** or **steer (P2)** is worth the Esc/scroll complexity.

---

## 10. Mapping: user report → this plan

| User said | Plan reading |
| --- | --- |
| "spawn 1 subagent" / "make it running longer" | foreground `task` + long child (panel drive scenario) |
| "cannot open that TUI subagent session in this same xdev TUI main session" | path A broken or undiscoverable (R1–R6), or path B/C expected (R4) |
| "explore how Claude Code lets user open the subagent session … then apply to xdev" | §1 CC contract → §4 target → §5 slices |
| "expected result is plan document" | this file |

---

*Last updated: 2026-10-14 (initial plan from CC research + xdev source trace; no code changes).*
