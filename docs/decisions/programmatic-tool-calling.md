# Design: programmatic tool calling (the eval-kernel route)

*Reference: `internal/eval/kernel.go` (`Frame`, `readLoop`, `route`, `begin`, `ErrBusy`),
`internal/eval/runner.py` (`main`, `run_cell`, `emit`), `internal/tool/catalog.go`
(`Runner`, the bridge-tool refusal), `internal/agent/catalog.go` (`WireCatalog`,
`runCatalogCall`), `internal/agent/loop.go` (`runOneTool`, `runTools`, `MaxToolWorkers`),
`internal/agent/task.go` (`maxDepth`, `resolveAgentTools`) · Date: 2026-10-02 ·
Status: **implemented** (#268; `internal/agent/evalbridge.go`, `Kernel.SetRunner`,
`dispatchToolCall`, `reply`, `failPending`; tests `internal/eval/bridge_test.go`,
`cmd/xdev/wiremode_test.go`). §0.7 stays open — see "Still open" below.

Origin: `docs/research/dsh-internals.md` §11, which names this *"the single biggest
capability gap on xdev's side"* and sketches the route. This doc settles the three
questions that sketch does not answer. Peers shipping it: dsh `run_code`, opencode
`codemode`/`execute`, pi 1.0 Codemode.

## 0.1 The gap, in one paragraph

A fan-out of N tool calls costs N model round trips. Today the model must emit
every tool call as its own assistant message, and each one costs a full request.
DSH's framing is the benchmark to design against: *"a fan-out of N tool calls costs
one assistant message of arguments plus the printed result, instead of N model-call
round trips."* xdev cannot compose tool calls at all — the model can only issue
them.

The seam already exists. `internal/eval` is one persistent CPython subprocess per
session speaking NDJSON on stdio, with a namespace that survives between cells
(`runner.py`'s `ENV`, `DefaultBackgroundAfter = 30s`). What is missing is a route
from a cell back to the tool registry.

## 0.2 What the research sketch got right

It is accurate, and worth restating because the rest of this doc is only about
what it missed:

- The runtime needs no new dependency. The kernel is a subprocess xdev already
  owns; there is no JS engine to embed.
- The call must take the **same** guarded path as a direct one — plan-mode gate →
  approval → interceptor → hooks. `catalog.go` already states this as the contract
  and `agent/catalog.go:runCatalogCall` already implements it by calling
  `runOneTool`.
- The re-entrancy guard already exists. `catalog.go:22-27` refuses to let the
  bridge tools (`tool_search`, `tool_describe`, `tool_call`) enter the catalog,
  so a bridged call cannot re-enter the bridge. The same refusal has to cover `eval`.

## 0.3 The blocker the sketch missed: `route` holds the lock

This is the one finding that changes the design, so it is worth being exact.

```go
func (k *Kernel) readLoop(r io.Reader) {
    for scanner.Scan() { k.route(fr) }   // route takes and holds k.mu
}
```

`route` (`kernel.go:451`) takes `k.mu` and holds it across its whole switch. A
`tool/call` frame arrives **while a cell is running**, and the answer has to come
from the Go side. Answering it inline would mean calling `runOneTool` while
holding the kernel mutex — and `runOneTool` re-enters everything: it dispatches
tools, fires hooks, persists, and (for a nested tool) can take other locks the
loop holds. That is a lock-order inversion with the whole agent loop behind it.

**DSH's design is the answer, and it is the same answer for a different reason.**
Their cell is async TypeScript calling *host bindings* (`await tools.name(args)`),
so the Python side never blocks on the host — the call yields, the runtime
continues, and the host answers on its own goroutine. The kernel reader is never
holding a lock across a dispatch.

So the design is not "answer the frame in `route`". It is:

1. `runner.py` exposes a **`tools` host binding** — an async callable the cell
   awaits. The runner emits a `tool_call` frame and suspends on a per-call
   result slot; it does not read the answer off stdin.
2. `route` **never answers a call**. It hands the frame to a dispatcher and
   returns, releasing `k.mu`. The dispatcher runs `runOneTool` **outside** every
   kernel lock, then writes the result back to the cell's pending slot.
3. The pending slot is per-call-id and lives with the `cell`, so a cancel
   (`cancelCell`) can fail every outstanding call rather than leave a cell
   blocked on a host answer that will never come.

A cell that calls `tools.x(...)` and is interrupted mid-call must get a Python
exception, not a hang. That is the whole reason the slot has to be cancellable.

## 0.4 The second thing the sketch missed: state does not survive a crash

Pi's claim is that Codemode state lives in the **session transcript**. xdev's
kernel does not have a transcript — it has a CPython subprocess with a live
namespace:

| | pi Codemode | xdev eval kernel |
|---|---|---|
| State after a crash | replayed from the transcript | **gone** — the subprocess died with the parent |
| On resume | cell state intact | fresh `ENV` |
| Approval for a repeated call | recorded in the transcript | re-asked |

**Decision: this is accepted, not solved.** The harness does not *need* kernel
state to survive — a cell whose namespace was lost raises on its next reference,
and the model sees a normal Python `NameError` and adapts. Building a transcript
replay for the kernel would be a second feature wearing a disguise, and the
research sketch does not ask for it.

What must be true for this to be honest, and is the reason it is in this doc: the
resume path must not *imply* state survived. `internal/session/context.go` already
reports an interrupted tool call rather than pretending it completed (see
`docs/decisions/tool-replay-safety.md`), and a cell that lost its namespace has to
be describable in the same terms. **Concretely: on resume, a fresh kernel is
booted, and the first cell that references pre-crash state gets a Python error
naming that, not a silent empty namespace.**

The kernel already makes the boot free: `NewKernel` sets `dead: true` and
`begin` starts the interpreter on first use, so a resumed session pays nothing
until a cell runs.

## 0.5 The caps

The research sketch asks for "a per-cell dispatch cap and a depth-1 guard". Both
already have a home in this tree:

- **Concurrency.** `begin` returns `ErrBusy` when `k.active != nil` — the kernel
  is exclusive, one cell at a time. A cell that fans out is bounded by
  `MaxToolWorkers = 6` (`loop.go:254`), the same semaphore `runTools` uses. No
  new limiter is needed; the bridge reuses the one the direct path already uses.
- **Depth.** `catalog.go` refuses the bridge tools; the eval bridge needs the
  same refusal for `eval` itself, so a cell cannot call a tool that spawns
  another cell. `task.go:398` already holds `maxDepth = 2` for subagents — a
  bridged call is *not* a subagent spawn and must not be able to become one.

Both are refusals, not counters, wherever possible. A counter is a number someone
will raise; a refusal is the registry not having the name.

## 0.6 What is deliberately not here

- **No new `Frame` field for the answer.** The result goes back through the
  binding the cell is awaiting, not through a second frame type. Two frame types
  for one round trip is how a protocol grows a state machine nobody wrote down.
- **No MCP-in-Codemode claim.** Earendil is explicit that Codemode *does not* fix
  MCP composition; the servers are the limitation. This feature composes the
  tools xdev already has; it does not make a bad MCP server good.
- **No auto-approved sub-calls.** Every bridged call still goes through
  `runOneTool`, so plan mode and the approval policy decide per call. A bridge
  that ran calls on its own would be a policy bypass, which is the exact reason
  `catalog.go`'s `Runner` is a callback and not direct execution.

## 0.7 Open, and deliberately unresolved

1. **Does the cell's *output* need to be a transcript entry?** Today a cell's
   rendered text is the tool result, which the model sees once. If a cell fans
   out to N tools, does the model see one result or N lines of what each call
   did? DSH's spec says "only printed/returned values become output" — which
   points at one result. This doc takes that reading; it is a product decision,
   not a technical one, and it is the thing to argue about in review.
2. **Cell history on resume.** Related to §0.4 but not identical: even with state
   accepted as lost, should a resumed transcript show *that* a cell ran? The
   rebuild-time notice (`UnansweredToolCallNotice`) covers the crash case. This
   doc says yes via that path and leaves it there.

## 0.8 What shipped, and where the doc was right

The implementation is the three decisions above, unchanged: `route` never answers
a call (`dispatchToolCall` returns before taking `k.mu`, and the tool runs on its
own goroutine); the slot is per-call-id and cancellable (`reply` claims it with
`sync.Once`, and `cancelCell`/`markExited`/`stopLocked` all release it, so a
cancelled cell raises instead of waiting on an answer that is never coming); and
the cell state does NOT survive a crash — `newCell` starts a fresh namespace, so
a resumed session's first reference to a pre-crash name is a Python `NameError`
with the kernel's own wording, never a silently empty namespace.

Two things the doc left open, settled by the code rather than by argument:

- **`eval` and the catalog bridge tools are refused by name** (§0.5's "refusal,
  not a counter"), in `dispatchToolCall`. `eval` cannot come back through the
  bridge — a cell that spawns a cell is unbounded recursion — and
  `tool_search`/`tool_describe`/`tool_call` are refused so a cell cannot reach
  the registry twice over. `tool.IsBridgeTool` is the exported predicate.
- **One reader thread on stdin** (`runner.py:_stdin_loop`). The doc said the call
  yields and the host answers on its own goroutine; in a synchronous kernel that
  only works if stdin is drained by someone other than the running cell, so the
  reader thread answers `tool_result` frames in place and queues cell requests
  for `main`. The cell polls its slot rather than blocking on it, which is what
  turns a cell clock or an agent abort into a `KeyboardInterrupt` where the cell
  already knows how to report it.
