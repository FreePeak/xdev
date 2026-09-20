# Building xdev for Long-Lived Streaming Sessions

How to build the xdev binary and run it in its default long-lived
streaming mode (the TUI), where each prompt resumes from the same
on-disk session and the model call streams back to a live terminal
renderer.

---

## 1. Build

One command. No Makefile, no JS runtime, no install step.

```bash
cd /path/to/xdev
CGO_ENABLED=0 go build -o xdev ./cmd/xdev
```

Result: a single static binary at `./xdev` (~20 MB). `CGO_ENABLED=0`
cross-builds for linux / macOS / windows, amd64 and arm64, from one
machine. Prebuilt artifacts also ship in `dist/` if you don't want to
compile.

The Go version requirement is in `go.mod`:

```
module github.com/FreePeak/xdev
go 1.25.14
```

## 2. Run modes

Routing is decided in `cmd/xdev/main.go:737`:

```go
func startupIsInteractive(prompt string, forcePrint, tty bool) bool {
    return prompt == "" && !forcePrint && tty
}
```

| Command | Mode | Session |
|---|---|---|
| `./xdev` | **TUI** — long-lived streaming | persists to `~/.xdev/agent/sessions/` |
| `./xdev "prompt"` | print — one-shot, headless | persists, then exits |
| `./xdev -p "prompt"` | same, explicit | same |
| `./xdev --mode rpc` | JSONL-over-stdio RPC server | persists |
| `./xdev --mode acp` | ACP server for editors | persists |
| `./xdev --no-session` | any mode, ephemeral | nothing written to disk |

The TUI is the long-lived streaming session. It's not a separate build
target — it's the default interactive run of the same binary.

## 3. What streaming needs

The repo's `~/.xdev/agent/models.yml` points at a local gateway:

```yaml
providers:
  onegw:
    baseUrl: http://127.0.0.1:8080/v1
    apiKey: ${ONEGW_KEY}
    api: openai-completions
```

Streaming requires that provider (or your own) to be running and
reachable. Without a configured provider the TUI fails on the first
turn — it surfaces the error rather than hanging.

Override the provider or models file at runtime:

```bash
./xdev -model onegw/free     # pin a specific model id
./xdev -provider onegw      # pin a specific provider key
```

## 4. The long-lived lifecycle, inside the TUI

When you submit a prompt, `runTurn` (`cmd/xdev/tui.go:1683`) does the
work that makes the session persist across turns:

1. **Claim the turn** — `running.CompareAndSwap(false, true)`. Only one
   turn runs at a time; a second prompt while busy queues as a
   `followUp` for the next run.
2. **Persist the user message** — append a `MessageEntry` to the store.
3. **Build a fresh `agent.Agent`** against the store (`startTurn` at
   `cmd/xdev/tui.go:1559`). The agent is disposable — the `Store` is
   the single source of truth.
4. **Launch `ag.Run` on a goroutine** — the tcell renderer keeps
   running; streaming events flow back through `TurnHooks`. The model
   call streams token-by-token into the composer as it arrives.
5. **Interrupt** — Esc / Ctrl+C calls `liveTurn.abort()`, which
   cancels the **live turn's** context, never `baseCtx`. Cancelling
   `baseCtx` would leave every later turn born already-canceled (a
   fixed bug).
6. **Complete** — `Store.Close()` flushes the JSONL. The session stays
   on disk for `--continue`.

## 5. Resume a session

```bash
./xdev --continue          # resume the last session for this CWD
./xdev --resume <short-id> # resume a specific session by id prefix
```

The resume picker (`cmd/xdev/tui.go:637`) lists sessions from
`~/.xdev/agent/sessions/`, reading only the first 4 KiB + last 32 KiB
per file (stat-cached). Subagent sessions (`titleSource: "subagent"`)
are skipped — they're not user-resumable.

## 6. Memory discipline

A hard <100 MB RSS budget is enforced by `debug.SetMemoryLimit`
(`XDEV_MEMLIMIT` overrides it). A runaway turn degrades into "compact
now" instead of an OOM kill — the compaction ladder fires at
`memlimit.HighPressure` (`internal/agent/compact.go:394`).

## 7. Session cost discipline (turn budget vs token budget)

The agent loop has **two** session budgets, but only one of them
actually gates the run:

- **Turn budget** (`MaxTurns`, default 200) — this is the real gate.
  The run loop is `for turn := 0; limit == 0 || turn < limit; turn++`
  (`internal/agent/loop.go:461`). `limit = effectiveMaxTurns()`.
  Nothing else appears in that condition.
- **Token budget** (`SessionTokenBudget`, default 5M) — this is a
  *break clause*, not a gate. It is accumulated after each turn
  completes (`spentSoFar += msg.Usage.TotalTokens` at
  `loop.go:514`) and breaks the loop at `loop.go:516`.

Why this matters for a long-lived streaming session:

1. **The token budget can't shorten a turn.** The check fires *after*
   `oneTurnWithRecovery` returns, so a single turn that burns more than
   the remaining budget (e.g. a long thinking block or a large tool
   result) completes in full before the break fires. At minimum one
   turn's worth of tokens leaks past the cap.

2. **The token budget can't abort an in-flight stream.** There is no
   per-call or per-delta accounting mid-stream. If the model streams
   a 3M-token response and `SessionTokenBudget` is nearly exhausted,
   the session keeps streaming until that response lands, then breaks
   at the turn boundary.

3. **The post-run wrap-up names it "turn budget" regardless of cause.**
   When either budget is crossed, the run appends `TurnBudgetPrompt`
   (`"turn budget reached — wrap up the current step and report status…"`,
   `loop.go:131`) with `TurnBudgetAttribution` (`loop.go:138`) and emits
   `turn_end` with `{"turn": limit}` (`loop.go:619`) — the turn number,
   not the token position. A consumer of the event stream cannot
   distinguish "stopped at 200 turns" from "stopped at 5M tokens".

4. **The turn cap dominates in practice.** Setting `SessionTokenBudget =
   1_000_000` but leaving `MaxTurns = 0` (unbounded) still gives you
   the default 200 turns — the turn gate is the dominant bound for
   most real sessions because turns are short relative to 1M tokens.

This is intentional: the token budget was added as **RCA #1**
(`loop.go:450-452`) to stop sessions from burning millions of tokens
before compaction or the turn cap kicked in. It was bolted onto the
existing turn-gated loop as a break clause rather than replacing the
turn gate, and the naming was never updated.

If you need a hard token stop — abort mid-stream, report the budget as
the cause, and skip the wrap-up turn — the loop condition at
`loop.go:461` needs to consult `spentSoFar`, the provider call needs a
context that aborts when the budget is crossed during streaming, and
`turn_end` needs a `stopReason` that distinguishes `token_budget` from
`turn_limit`.

---

- This is NOT a single git repo monorepo — each subdirectory may be
  independently versioned. Build from the `xdev` root.
- Some projects have worktrees under `.worktrees/` — these are feature
  branches. Build from the main checkout, not a worktree, unless you
  intentionally want that branch.
- The repo's author is `linhdmn <mnhatlinh.doan@gmail.com>` and that is
  the only name that may appear in commit metadata. Never attach a
  `Co-Authored-By:` trailer naming another AI model to a commit here.