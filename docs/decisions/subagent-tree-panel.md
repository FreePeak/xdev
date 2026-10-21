# Design: the subagent strip is a tree (Crush's shape), and its row stopped lying

*Reference: `internal/tui/agentspanel.go`, `internal/tui/app.go` (`subLines`),
`internal/agent/{subagent,task}.go`, `cmd/xdev/tui.go` (`taskChildSink`) ·
Date: 2026-10-21 · Status: **implemented**

## 0. What was asked

"Update the TUI subagents display like Crush (Bubble Tea), in my tcell."

Crush's subagent display is one thing, and it is a **tree**. Read from the
source (`charmbracelet/crush` @ `main`):

| What | Where | What it says |
| --- | --- | --- |
| the Agent tool call renders as a tree | `internal/ui/chat/agent.go` (`AgentToolMessageItem.RenderTool`) | the child's own tool calls hang under the `Agent` header as `tree.Root(header).Child(...)` |
| the connectors are rounded | `internal/ui/chat/tools.go` (`roundedEnumerator`, `roundedIndenter`) | `├` / `╰` + `─`, and `│` continues a branch down a wrapped child |
| there is no other subagent surface | `internal/ui/` tree | no bottom panel, no roster — the tree in the transcript IS the subagent view |

So "like Crush" for xdev means: **the strip draws a tree, not a list.**

xdev has two subagent surfaces, and only one of them can be a tree:

- the transcript's rows under a `task` call (`subLines`, `app.go:1380`) hang
  under the call that spawned the child. A grandchild has no call row of its
  own — its parent's call lives inside the child's own session — so this
  surface can only ever show ONE level. Treeing it would be a glyph change
  with nothing to nest.
- the always-on strip above the composer (`agentspanel.go`) shows every child
  at every depth. It is the only surface where a chain is visible, and it
  was flat — so a chain read as a list of names with no way to say which
  child spawned which.

**The strip is the one that becomes the tree.**

## 1. Why the strip was flat: the link did not exist

The strip could not draw a tree because nothing told it the shape. A child's
events (`agent.SubagentEvent`) carried its label, agent, model, tool and
duration — and no parent. A grandchild therefore arrived as an anonymous
label, indistinguishable from a top-level child.

The link was missing at the only place that knows it: **the spawn**. A child
is spawned by a `task` call; a grandchild is spawned by a `task` call that
lives inside a child. The nested task tool is built in the parent's
`TaskTool.Execute` (`task.go:559`), where the child's own label (`a.Name`)
is in hand — that is the parent's label for everything the child spawns.

Three fields carry the edge out:

```go
// internal/agent/task.go
ParentLabel string  // the child that owns THIS task tool

// internal/agent/subagent.go
SubagentSpec.Parent  string  // who spawned this child
SubagentEvent.Parent string  // who spawned this child, on every event

// internal/agent/task.go, the nested tool
ParentLabel: a.Name,
```

`SpawnChild` stamps `spec.Parent` on the start, tool and end events; the
host (`cmd/xdev/tui.go`'s `taskChildSink`) passes it to the strip, which
stores it on the row.

**A root has no parent.** The top-level session's children are spawned by a
`task` tool with no `ParentLabel`, so their events carry `""` and the strip
draws them as roots. A child whose parent the strip never saw (a background
job, a parent whose start event was dropped) is an **orphan**: it draws as a
root rather than vanishing, because it is still work the user is watching.

## 2. What the strip paints

```
 ├─ ⠹ ⎿ reviewer · grep · TODO                                   12s
 ╰─ ⠹ ⎿ scout · read · internal/tui/app.go                   1m02s free
   ╰─ ⠹ ⎿ leaf · grep · TODO                                    8s
```

- **the tree column** — the ancestor bars and the row's own branch (`├─` /
  `╰─` / `│`), one level of indent per depth. Crush's vocabulary, drawn in
  tcell;
- **the status glyph** — the shared spinner (`spinFrame`, so the row moves in
  step with the composer divider and the call row) while the child works, and
  a static `✓` once it settled;
- **the openable tick** — `⎿`, next to the label, for a child a host tracked;
- **the label and its agent type** — bold, `·`-separated;
- **the activity** — the child's newest tool and its naming argument, with
  the same `toolDetail` precedence the transcript's own rows use;
- **the elapsed clock** — the child's own time (a settled row shows
  `End - Start`, not a clock that keeps running);
- **the model** — dim, at the far right, only when the child runs on a
  different model than the session.

## 3. Three defects the rewrite fixed

Found by reading the painter against what it claimed to do; all three are
visible in the real-terminal capture below.

1. **The label and the activity ran together.** The painter advanced
   `x += width(name) + 1` — a single space — so the row read
   `reviewer grep · TODO`, one word. The `·` separator existed only between
   the label and the agent type.
2. **The elapsed clock was never painted.** `elapsed := humanDur(...)` was
   computed and *reserved* in the row's budget
   (`budget := w - 2 - rightW - width(elapsed) - 2`) but no `drawText` ever
   drew it. Every row said what the child was doing and never said for how
   long.
3. **The openable tick was a stray glyph.** It was painted at
   `w - 2 - rightW` — the far right, immediately left of the model column —
   where it read as debris rather than as "this row opens something". It now
   sits next to the label, where it means what it says.

A fourth, found while rewriting: the spinner was painted for **every** row,
including settled ones, so a finished child kept spinning — the opposite of
what the row's own comment promised ("Done rows paint dim and still").

## 4. Two decisions that are not obvious

**The paint budget counts the newest rows, and a kept row drags its
ancestors in.** `agentTreeCut` picks the newest rows by start time, then
pulls in each one's ancestors — a child without its parent on screen is a
branch with nothing above it. So a chain can paint a row or two more than
the 3-row budget. That is the price of showing a chain whole, and the
chain's depth is bounded (`taskMaxDepth`, default 3). The alternative —
cutting the tree at the budget — put the newest link of a chain below the
cut, which is the one row the user was looking for.

**The keyboard focus lands on a VISIBLE row.** `focusNewest` picks the newest
row among the rows the strip paints, and `focusMove` walks that same list. A
focus on a cut row would open a transcript the user cannot see on screen.

Roots are ordered by the newest row **under** them, not by their own start:
a chain's newest link is the one the eye wants, and ordering the roots by
their own start pushed that link below the budget. A flat batch is
unaffected — every root's newest row is its own.

## 5. Files touched

| File | Change |
| --- | --- |
| `internal/agent/subagent.go` | `SubagentSpec.Parent`, `SubagentEvent.Parent`; `SpawnChild` stamps the parent on every event |
| `internal/agent/task.go` | `TaskTool.ParentLabel`; the nested tool carries the child's label; `Execute` sets `spec.Parent` |
| `internal/tui/agentspanel.go` | the tree (`treeLocked`, `agentTreeCut`, `visibleTreeLocked`), the rewritten painter, the focus over the painted rows |
| `cmd/xdev/tui.go` | the sink passes `ev.Parent` through |

## 6. Verification

**Tests.** `internal/agent/subagent_parent_test.go` — the parent link on a
child's events, the empty link on a session's own child, and a chain driven
END TO END: a real `task` spawn whose child calls `task` again, asserting the
grandchild's event names the child that spawned it. `internal/tui/agentspanel_test.go`
— the tree shape (the child sits under its parent, indented, with a branch),
the orphan, the whole chain, the focus on a visible row, the separator, the
clock, the settled tick, the openable tick next to the label, and a cyclic
parent link (which cannot come from a real spawn, but a hang in the paint
path is the worst failure a strip can have — it did hang, and the guard is
what the test bought).
`cmd/xdev/task_child_sink_test.go` — the host's pass-through.

`go test ./internal/tui/ ./internal/agent/ ./cmd/xdev/` green; `go build ./...`
clean. The full `go test ./...` has one failure, `internal/ai`'s
`TestIsNetworkOutage` — it dials an unroutable IPv6 address and expects a
"no route" error, and this machine answers "operation timed out" instead. It
fails identically on a clean `HEAD` worktree, so it is not a regression.

**A real terminal.** `scripts/tui-subagent-panel-drive.py` drives the built
binary in a pty against a mock provider whose child never yields. The strip
row, before and after:

```
before   ⠙ scout read · internal/tui/agentspanel.go                                       ⎿ mock-1
after    ╰─ ✓ ⎿ scout · read · internal/tui/agentspanel.go                    0s mock-1
```

The before line is the flat list with the three defects visible in one row:
the label and the activity run together, there is no clock, and the `⎿` tick
is stranded at the far right next to the model. The after line is the tree
with the settled tick, the tick next to the label, the separator, and the
child's own clock.

The click path is unchanged: clicking the strip row opens that child's
transcript (`transcript · scout (fg-1)`) in the same panel surface as
`/hub`'s Enter, on both binaries.

## 7. What was deliberately not done

| Not doing | Why |
| --- | --- |
| treeing the transcript's `⎿` rows under the `task` call | that surface shows one level only — a grandchild has no call row of its own — so there is nothing to nest |
| a new overlay, keybinding or settings entry | the strip already has its chord (`Alt+F`), its toggle (`Alt+P`) and its click path |
| a per-child elapsed in the transcript rows | the call row right above already counts the wall time; two clocks on one row is noise (decided in `live-subagent-view.md` §8) |
| `+N more running` on the strip | the strip keeps settled rows, so the hidden ones are not all working; the line says `+N more` |

## 8. Still open

1. **A human watching a real chain.** The tree is asserted on a simulation
   screen and painted in a real terminal for a single child; a real
   three-level chain, watched, is the step that is still missing.
2. **The strip's height.** A chain can paint `budget + depth` rows. The
   depth is bounded by `taskMaxDepth`, so the worst case is bounded too —
   but a batch of three children that each spawn two would paint nine rows
   over the transcript's tail. If that shows up in practice, the fix is a
   depth-aware budget, not a bigger strip.
3. **`agentPanelRowText`** (`agentspanel.go`) is dead code: nothing calls it.
   It was already dead before this change and is left alone rather than
   silently removed.
