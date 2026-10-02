# Design: a tool declares its own replay safety

*Reference: `internal/tool/replay.go` (Replayer, Registry.ReplaySafe) · `internal/session/context.go`
(UnansweredToolCallNotice, ReplaySafety, unansweredToolCallText, neutralizeToolCalls) ·
`internal/tool/read.go`, `internal/tool/search.go` (the declarations) · Date: 2026-10-01 ·
Status: **implemented** (see `docs/PRD.md`'s dated entry for `feat/tool-replay-safety`)

## 0.1 The problem, in one paragraph

A turn can die with a tool call in flight — the process is killed, the stream
cuts mid-decode, the laptop sleeps. xdev already reports that honestly: the
rebuild neutralizes the dangling call and leaves
`UnansweredToolCallNotice` in its place, *"the previous turn ended before this
tool call returned; its result is unknown, so run it again if you still need
it."* So the model is told rather than lied to.

What it cannot be told is **whether re-running is free.** Told only that, the
model must guess whether the tool it just called was a read or a write. That
guess is load-bearing — it decides whether `bash` gets repeated — and it is
made from the tool's name, which is exactly the kind of judgement the tool
itself is in a position to answer correctly and the model is not.

## 0.2 What was already here (and why this is not a rewrite)

The durability story is not missing; the *replay decision* inside it is:

| Piece | Where | Status before this |
|---|---|---|
| Interrupted turns are classified | `session/status.go` | present |
| The crash is reported to the model | `session/context.go` notice | present |
| Empty-shell requests are healed | `EnsureToolOutput`, neutralization | present |
| A turn that died on a call is replayed | `agent/loop.go` (`orphanToolCalls`) | present |
| **Is this call free to repeat?** | — | **absent** |

The transcript is the state machine. This makes the transcript's crash-repair
text carry the tool's own answer, and nothing else moves.

## 0.3 The decision

A tool answers once, where it is defined:

```go
type Replayer interface {
    ReplaySafe() bool
}
```

Three choices carried the weight:

1. **An optional interface, not a field on `Tool`.** A field would make every
   implementation — and every external one — answer it, and the answer would
   default to a boolean zero that nobody re-reads. An interface the tool does
   not implement is a **visible** absence.
2. **Unsafe is the zero.** The failure this guards is a duplicated side
   effect, not a redundant read, so the default has to fail closed. `read`
   costs a call; `bash` costs the thing it ran.
3. **An unregistered name is unsafe.** A session can name a tool this process
   does not have: a `--tools` filter removed it, it is a remote transcript, it
   was renamed. Assuming an uninspectable tool is harmless is the mistake, so
   `Registry.ReplaySafe` returns false for a miss.

### The seam

`session` cannot import `tool` — the dependency runs the other way
(`tool/checkpoint.go`, `todo_session.go`, `imagegen.go` all import `session`).
The bridge is one package-level function var, `session.ReplaySafety`, wired
by `cmd/xdev` to the live registry at all four entry points (`print`, `tui`,
`rpc`, `acp`) *after* MCP and extensions have registered, so a registered
MCP or extension tool answers from its own declaration rather than from a
name list.

A nil `ReplaySafety` reproduces today's wording exactly, so a build that does
not wire it (a test, a library consumer) behaves as it always did.

### Where the guidance is withheld

The notice names the safe calls **only when every call the turn lost is safe.**
A turn holding `read` and `bash` gets the plain notice: a partial "this one is
free" reads, to the model, as permission to repeat the whole turn — which is
the duplication the design exists to prevent.

## 0.4 What is deliberately not here

- **No auto-replay.** The declaration does not make the harness re-run the
  call. It makes the harness able to *say* the call is free. Auto-replay would
  need a real decision about which calls fire without the model asking, and
  the transcript already puts a human or the model back in the loop.
- **No per-argument safety.** `ReplaySafe()` is per tool, not per call. A tool
  whose arguments decide the answer (`bash "ls"` vs `bash "rm"`) cannot
  express that here; it belongs in the approval tier
  (`tool/approval.go`), which already models exactly that distinction.
- **No new storage.** Nothing about the call's replayability is persisted. The
  tool registry is live in every process, and re-deriving it on resume is
  cheaper than trusting a stale verdict.