# Design: live subagent activity in the transcript (Claude-Code shape)

*Reference: `internal/agent/task.go` (TaskTool.Execute), `internal/agent/subagent.go`
(SpawnChild), `internal/tui/app.go` (AddToolBlock / blockLines), `internal/tui/blocks.go`
(Block) · Milestone: M11 (subagents) · Date: 2026-09-28 · Status: **implemented** (see
`docs/PRD.md`'s dated entry for `feat/task-live-view`)

## 0.1 The problem, in one paragraph
The user-facing half of that rule is the part we are missing. Today a foreground `task`
renders as **one generic tool row** — a spinner, an elapsed timer, then a result box — and
nothing in between. The user cannot tell which child is running, what it is doing right now,
or, when it finishes, what it actually did. The repo says so in its own parity notes:
`docs/research/parity-omp-internals.md:534` records omp's "yield-only isolation" and its
cost — *"a subagent transcript never streams into the parent"*.

So the design is:

> **the child's activity is rendered to the human; the model's context still only ever sees
> the yield.**

The two audiences are separated by an existing boundary we do not have to cross: the child
writes to its **own** `SessionStore` (`internal/agent/subagent.go:183`, in-process, in a
goroutine, never merged into the parent's), and the parent only reads what the tool *returns*
(`internal/agent/task.go:523`, `Details: res`). Making the first audience-visible costs no
tokens and breaks no contract, as long as the second is untouched.

### What the user sees today vs. what Claude Code shows

```
today                                  after
─────────────────────────────────       ─────────────────────────────────
⠹ task {"context":"shared",…}  1m2s    ⠹ task · 4 children  1m2s
  ██████ result box ████████              │  ⎿ reader    · reading parser.go  12s
                                         │  ⎿ reviewer  · grep TODO in go…     31s
                                         │  ⎿ +2 more running
                                         ██████ result box ████████
                                           ⎿ reader   · 6 calls · 2m14s   ⌃O
```

## 1. Evidence: what already exists (reuse is most of the work)

| Piece | Where | Reused as-is |
| --- | --- | --- |
| Child's own session store, live-readable | `subagent.go:183` `session.OpenMem`; `Store.Entries()` returns a copy under the store lock (`internal/session/store.go:460`) | the activity feed, no new event bus |
| The precedent for reading a child's transcript incrementally | `hub.Transcript(id, fromSeq)` (`hub.go:342`) + `rosterActivity` (`hub.go:306`) reading `st.Entries()` | the exact read pattern we copy |
| The only live child hook | `SubagentSpec.OnRun func(*Agent)` (`subagent.go:52-54`), invoked at `subagent.go:229`; `TaskTool` leaves it nil (`task.go:490-499`); `attachChildAdvisor` already uses exactly this seam to mirror child messages (`task.go:653-702`) | the injection point |
| Tool-row render with spinner + live elapsed | `internal/tui/app.go:2584-2617` (bullet, bold name, ` · detail`, `humanDur`); the stamp ages in whole seconds (`rowindex.go:156-167`) | a `KindTool` block already re-renders per second while running |
| Result box with `Ctrl+O` expand + head/tail trim | `app.go:2900` `toolBoxLines`, `ToggleBoxExpand` (`app.go:717`), `rowindex.go:52-63` budgets | the finished-child summary lands here for free |
| Batch shape and the 8-way cap | `task.go:230` `maxBatchParallel`, `task.go:236` `executeBatch` | batch = N child rows, no new plumbing |

**Nothing in the TUI knows about the `task` tool today.** `grep '"task"' internal/tui` → one
comment (`hubroster.go:14`). There is no per-tool switch, no `RenderSpec` for it
(`internal/tui/render.go:18`), and no dock/overlay for foreground children. The three
surfaces that exist (`/hub` roster, dock `AGENTS` section, `/trajectory`) are all fed by
**background hub jobs only**.

## 2. The one structural gap, and why it is cheap

`TaskTool.Execute` builds `SubagentSpec` with **no `OnRun`** (`task.go:490-499`). So for a
foreground spawn, nothing outside `SpawnChild` ever gets a handle on the live child — its
store, its hooks, its agent. The hub solves this for background jobs by wrapping `OnRun` in
`launchLocked` (`hub.go:130-141`).

The fix is one field and one assignment: give `SubagentSpec` a typed event callback, and have
`TaskTool.Execute` set it for foreground spawns. `OnRun` already delivers the `*Agent` at the
right moment (just before `ag.Run`), and the child's store is reachable from it
(`ag.Store`). The hub's wrapper chains additively (`hub.go:138-140`), so children dispatched
to the hub and children spawned in the foreground both work, and nesting (a child that
spawns its own, `task.go:456-479`) inherits the seam for free via the same struct field.

### Contract

```go
// SubagentEvent is one observable moment in a child's run. It is a
// USER-FACING signal: nothing here is ever added to the parent's model
// context, which still sees only the yield (the contract pinned by
// TestSubagentYieldOnlyIsolation).
type SubagentEvent struct {
    Kind   SubagentEventKind // start | tool | result | end
    Label  string            // child name / batch label
    Agent  string            // resolved agent type
    Model  string
    Tool   string            // Kind==tool: the child's tool name
    Detail string            // Kind==tool: the child's naming argument
    Status string            // ok | error
    Dur    time.Duration     // settled wall time
}

// OnEvent (optional) receives a child's progress. Fires from the child's
// goroutine; must be cheap and must not block — the same contract as
// Hub.SetNotify (hub.go:194-197).
OnEvent func(SubagentEvent)
```

`OnRun` stays as it is. It is a different question ("hand me the live agent") and the hub
already depends on it.

## 3. Emitting the events: three lines in `SpawnChild`

The child's hooks are created in one place (`subagent.go:201-204`) and today only persist
messages. `attachChildAdvisor` (`task.go:684-702`) already shows the additive-wrap pattern
for extending them. So:

1. build a `TurnHooksFunc` that, besides persisting, fires
   `spec.OnEvent(SubagentEvent{Kind: Tool, ...})` for **tool results**
   (`OnToolResultMsgF` carries the tool name; the arguments live in the preceding assistant
   message, so the row shows the tool name and, for reads, the path the child reported);
2. fire `start` from the `OnRun` site and `end` from the `defer` in `SpawnChild`;
3. `SpawnChild` returns as today — the *return value is unchanged*, so the yield-isolation
   contract and every existing test keep passing.

**Deliberate simplification (`ponytail:` ceiling).** The live row shows the child's **tool
name + its newest activity line**, not a re-layout of the child's arguments. Pulling the
naming argument out of a child's tool call would mean re-implementing `tui.toolDetail`
(`internal/tui/blocks.go:190`) on the agent side, and the win is a second column of text on
a line that is already dim. Upgrade path when it is actually wanted: add `Args json.RawMessage`
to the event and let the TUI run the *existing* `toolDetail` on it — the field lands once,
the reuse is free.

## 4. The TUI: one new block field, one new render branch

### 4.1 The state

`Block` (`internal/tui/blocks.go:27`) gains **one** field:

```go
// Sub holds the live activity of a `task` child, newest last. The
// transcript renders it under the running call row and, once
// finished, under the result box. Populated only for the task tool,
// so every other block is unaffected.
Sub []*SubActivity // at most 4 kept: 3 visible + the "+N" count
```

with a package-local value type (the TUI imports nothing from `internal/agent` — verified:
`grep -rn "internal/agent" internal/tui` → no hits, and that boundary should stay), and one
constructor + one updater, both mirroring the shape of `AddToolBlock`/`FinishTool`
(`app.go:664`, `app.go:676`):

```go
func (a *App) AddTaskChild(callID, label, agent, model string)
func (a *App) UpdateTaskChild(callID, label, tool, rawArgs, status string)
func (a *App) FinishTaskChild(callID, label, status string, d time.Duration)
```

`AddTaskChild` has to bind to the **right** `task` block. The blocks slice is guarded by
`a.mu` and appended in hook order, so: find the last `KindTool` block with
`ToolName == "task" && Status == "running"` and a matching call id, and drop the event if
there is none. That is exactly the match `FinishTool` already does.

**Correction to the design below, from the implementation:** a `task` call *can* be called
twice concurrently from the same turn — `agent.MaxToolWorkers` (6) runs same-batch tool
calls in parallel, and a model that spawns two subagents in one turn gets two live `task`
rows. So the call id is the parameter on all three methods, and the host
(`taskChildSink` in `cmd/xdev/tui.go`) is what resolves it: the agent's child events carry
no call id, so a new child binds to the newest running `task` row (the call whose spawn is
blocking) and keeps that binding for its whole life.

**Batch discipline (the answer to the batch question).** Paint the first 3 children and
render `+N more running` for the rest. 3 fits inside the dock-less transcript column at any
sane width, 4 leaves a row for the indicator, and 8 concurrent children would otherwise push
the parent row itself off a normal terminal. Start order, not recency, so the rows do not
jump while you read them.

**As implemented, the block keeps ALL the children, not the last 4.** The cap is a *paint*
decision (`Block.subVisible`), and the state keeps everything: dropping a child would take a
late settle with it, because `findSubLocked` would have nowhere to land it. A batch is at
most 8 small structs, so "keep them all" is free and the cap stays a display concern.

### 4.2 The render

`blockLines` gets a fourth branch (`app.go:2584-2619` is where `KindTool` is handled today):
after the existing single-line call row, if `b.Sub` is non-empty, append up to 3 dim
continuation lines shaped

```
  ⎿ <label> · <newest activity>  <elapsed>
```

- glyph `⎿` (1 cell, verified against the repo's own `width()`), style `theme.GrayDim` —
  it must read as a child of the row above it, not as a sibling tool call;
- **no block, no `Kind`, no `ToolName`**: a child row is a *line of a block*, not a block.
  A new `BlockKind` would drag it through `stampTiers`, `rowChrome`, `trimTier`,
  `ToggleBoxExpand`, dock sections and `/trajectory` — none of which want it;
- the rows are appended to the call row's own `[]line`, so `rowIndex` needs no change: the
  stamp already ages per second for a running tool row (`rowindex.go:158-160`) and
  `tlen`/`status` change whenever a child moves, so `sync` re-renders the right blocks
  (`rowindex.go:243-249`).

**Finished (deliberately not built).** `FinishTool` already appends the `KindToolDone`
box, and `ToggleBoxExpand` already gives the user `Ctrl+O` on it. What the design proposed
here — a settled `⎿` summary line inside that box — is **not** in this change: the call row
keeps its child rows after `FinishTool` (it is still the newest block), so a settled child
is already visible with its terminal status; the summary would repeat the same three rows
inside a box, and the per-child wall time belongs there only once the report is expanded.
The "full transcript" is the child session's own JSONL, which the system already writes
(`subagent.go`) and the parent result already names (`session: <id>`). We are not
duplicating it; we are pointing at it.

## 5. What this does *not* do, and why each is deliberate

| Not doing | Why |
| --- | --- |
| Streaming child text into the parent's model context | Breaks `TestSubagentYieldOnlyIsolation` (`subagent_test.go:248`) and the CC parity decision (`cc-design-decisions.md:34`). The whole feature is on the other side of that line. |
| Reusing the `internal/tui` ⇄ `internal/agent` import that the hub already has | The hub is a *session* surface wired from `cmd/xdev`; a tool the TUI must observe mid-call needs a seam the **tool** carries. `OnEvent` is that seam. |
| Foreground children in the dock `AGENTS` section / `/hub` | That is the "Phase 1 + hub" option. It is a bigger diff (hub tracking for a call that blocks the turn, plus its own cancel semantics) and orthogonal to what the user sees in the transcript. Belongs in its own PR. |
| A new overlay, keybinding, or settings entry | Nothing to bind: the rows paint in the transcript, and `Ctrl+O` already exists on the result box. |
| Persisting the activity in the session JSONL | It is a live affordance; the durable record is the child session, which already exists. Persisting would mean a new entry type and a replay path. |

## 6. Failure modes and how they are handled

| Failure | Handling |
| --- | --- |
| Child tool panics | Unchanged: a panic inside the child's tool is converted to a tool result by `toolOutcome.unwrap` (`loop.go:1406-1411`); the row never gets its `ok` line and the parent reports the failure. |
| Turn cancelled mid-child | `SpawnChild` gets the cancelled ctx, the `defer` fires `end` with `canceled`; the row stops spinning because the block's `Status` flips when `FinishTool` runs. |
| A child never calls `yield` | Already visible in the parent's result text (`task.go:636-640`); the UI adds nothing and hides nothing. |
| Batch with 8 children | 3 rows + `+5 more running`; the parent result reports all 8 (`task.go:295-296`). |
| `a.mu` contention | The three new App methods are called from the child's goroutine and take `a.mu` only around the slice append/mutation, exactly like `AddToolBlock`. They never block on the paint path. |
| Non-TUI hosts (print/rpc/acp) | `OnEvent` is nil there — the field is set by the TUI host. Nothing changes for them; `print` and `acp` never set it. |

## 7. The change, as built

Worktree: `.worktrees/task-live-view`, branch `feat/task-live-view`, off `origin/main`
(`3c44ec5`). Five slices, red→green, in this order.

**Slice 1 — the seam (agent side).** `SubagentEvent`/`SubagentEventKind` +
`SubagentSpec.OnEvent` in `subagent.go`; the emissions in `SpawnChild`: `start` before
`ag.Run`, one `tool` per finished child call from the `OnToolResultMsgF` hook, and a
deferred `end` reporting the TERMINAL status (after the yield nudges and the schema repair,
which is the status the parent renders). `yield` is not reported — it is the handoff, not
work. Test: `TestSpawnChildEmitsProgress` (sequence, label, model, tool, args, status, and
`res` unchanged).

**Slice 2 — the tool sets it.** `TaskTool.OnEvent` → `spec.OnEvent` for foreground spawns.
The hub composes its own additively, so background jobs are untouched. A batch item with no
`name` is labelled through the new `childLabel`. Test:
`TestTaskToolBatchNamesEveryChild`.

**Slice 3 — the block.** `SubActivity` + `Block.Sub` + `AddTaskChild`/`UpdateTaskChild`/
`FinishTaskChild` + `Block.subVisible`. Tests: `internal/tui/task_live_test.go`, 7 cases.

**Slice 4 — the render.** The `KindTool` branch appends `a.subLines(b, w)`, which reuses
`toolDetail` for the naming argument. Test: `internal/tui/task_render_test.go`, 5 cases on
painted simulation-screen output.

**Slice 5 — the host.** `taskChildSink` in `cmd/xdev/tui.go` maps a child event to a call
row; wired at the same seam `ask` uses (`reg.Get(...)`). Test:
`cmd/xdev/task_child_sink_test.go`, 2 cases.

**What the tests do NOT prove, and the only way to prove it:** a human watching a real
spawn in a real terminal. The simulation screen is evidence about the paint path, not about
the experience. That is the remaining step before this is done.

## 8. What was decided, and what is still open

Answered by the user before the build:

1. **Row density** — one row per child, capped at 3 with a `+N more running` indicator.
2. **Scope** — the full Claude-Code shape: live rows during the call, and a settled view
   after it.

Decided during the build, with the reasoning kept in §4:

3. **Per-child elapsed** — omitted while the call is in flight. The call row right above
   already counts the wall time; two clocks on one screen is noise.
4. **The cap is a paint decision, not a state decision** — the block keeps every child, so
   a late settle has somewhere to land.
5. **The settled summary inside the result box** — dropped as a repeat of three rows the
   user can already see.

Still open, and deliberately not in this change:

6. **Foreground children in the dock / `/hub`.** This does not do it. Making the `AGENTS ·
   2 running` panel count foreground children means a hub-tracked child that blocks its own
   turn, and it needs its own decision on what cancelling one means. A separate PR.
7. **A human watching a real spawn.** The remaining step before this feature is honestly
   done.

## 9. Follow-up: opening the child (`feat/subagent-transcript-view`)

The rows above are narration. A user watching a child for two minutes still could not see
the child — and the reason was structural, not a missing feature: **a foreground child is not
a hub job.** `/hub`'s transcript view reads `Hub.Transcript` over `hubJob.stores`, a
synchronous spawn was never registered anywhere, and `SpawnChild`'s store dies with the
call. So the live view showed *what the child was doing* and nothing could show *what it
said*.

Decided there:

8. **The hub's record shape, not its job surface.** `Hub.TrackForeground` reuses `hubJob`
   for the store and terminal status and registers it in its own read-only map. Cancel, park
   and revive stay on jobs only: the tool call owns a foreground child's context, so those
   ops would have nothing honest to do to it, and one roster mixing both would offer
   controls that work on half its rows.
9. **The `/hub` panel, not a second renderer.** The click fills the existing transcript
   view, so Esc means one thing in this app no matter how a child's transcript was opened,
   and the rows are painted by code that already existed. `viewName` is the one new field:
   the roster resolves a name from its own rows, and a foreground child has none.
10. **No selection cursor.** `Alt+B` opens the newest child, exactly as `Ctrl+O` expands the
    newest box; a click reaches any child, including the ones past the 3-row paint cap,
    because the cap never touched the state. A cursor would have needed a second driver (the
    mouse) for no case the pair does not already cover.

Still open: full focus mode (Claude Code's Ctrl+B swaps the transcript for the child's). It
needs its own render path, scroll save/restore, and Esc ordering against the double-Esc
rewind — a separate decision, not a mode flag on this one. And a human watching a real
spawn, clicking the row mid-run, and pressing Esc.
