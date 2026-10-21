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

Routing is decided in `cmd/xdev/main.go:808` (called at `main.go:693`):

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
   `cmd/xdev/tui.go:2268`). The agent is disposable — the `Store` is
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

The resume picker (`cmd/xdev/tui.go:3823`) lists sessions from
`~/.xdev/agent/sessions/`, reading only the first 4 KiB + last 32 KiB
per file (stat-cached). Subagent sessions (`titleSource: "subagent"`)
are skipped — they're not user-resumable.

## 6. Memory discipline

A hard <100 MB RSS budget is enforced by `debug.SetMemoryLimit`
(`XDEV_MEMLIMIT` overrides it). A runaway turn degrades into "compact
now" instead of an OOM kill — the compaction ladder fires at
`memlimit.HighPressure` (`internal/agent/compact.go:585,610`).

## 7. Session cost discipline

Read 2026-10-12 against `origin/main`. A session has **three** bounds, and
which ones you get depends on the mode — so "what stops a runaway session"
has a different answer in the TUI than in `xdev --print`.

### 7.1 The turn cap is the only turn-gated bound

`Agent.Run` is `for turn := 0; limit == 0 || turn < limit; turn++`
(`internal/agent/loop.go:614`), with `limit = a.effectiveMaxTurns()` and
`effectiveMaxTurns()` returning `a.MaxTurns` verbatim
(`loop.go:492-494`). Nothing else is in that condition.

- `--max-turns 0` (the flag default) means **"use the configured
  `maxTurns`"** — 200 by default — NOT unbounded: the CLI substitutes
  `settings.MaxTurns` before the agent ever sees the number, and the
  settings layer refuses a configured 0, so 0 can never reach the agent
  from the command line. The agent layer itself treats a 0 as unbounded
  (`effectiveMaxTurns()` returns `a.MaxTurns` verbatim), and the constant
  `DefaultMaxTurns = 200` that used to name that default is **deleted** —
  the value lives in the settings layer now
  (`internal/config/settings.go`, `MaxTurns: 200`).
- When the cap bites, the run does NOT error: it injects one wrap-up turn
  ("turn limit reached — wrap up the current step and report status",
  attributed `turn-limit`, `loop.go:909`) and returns normally.

### 7.2 There is no session token budget — it was deliberately removed

`SessionTokenBudget` **no longer exists**. `b282a39` (#605, "remove
per-turn token budget limit from agent loop") deleted it, after the same
change had already turned the session budget into a *per-turn* one. So all
three findings this section used to record — "the token budget can't
shorten a turn", "the token budget can't abort an in-flight stream",
"`turn_end`'s `{"turn": limit}` can't distinguish token from turn cause" —
are now moot for the budget itself:

- **The stream can't be cut short by any budget.** A turn runs to
  completion (`oneTurnWithRecovery`); the only in-flight stop is user
  cancellation.
- **The per-turn token cap is also gone**, so a 3M-token response would
  stream to completion.
- The one remaining cost bound that can still spend unbounded money is a
  goal's `TokenBudget` (`internal/agent/goal.go:60-61`), which emits a
  reminder ("token budget: N of M remaining", `goal.go:182`) but does not
  terminate the run.

### 7.3 What still bounds a runaway session

| Bound | Where | Effect |
|---|---|---|
| `--max-turns N` | `flag` → `loop.go:614` | the only run-ending gate |
| `--max-tokens N` | provider-side output cap | bounds one response, not the run |
| `debug.SetMemoryLimit` 100 MB default | `internal/memlimit` | GC pressure, then compaction |
| `memory-limit` config key | `memlimit.ApplyFrom` (main.go:334) | same, higher or lower |
| goal token budget | `internal/agent/goal.go` | reminder only, non-terminal |

The practical consequence for a long-lived streaming session: **the turn
cap is the whole story.** A TUI session that you let run with
`--max-turns 0` will keep turning as long as the model keeps calling tools,
and compaction (not a budget) is what keeps the context — and therefore the
per-request cost — from growing without bound. The compaction ladder is
also the path that reacts to memory pressure (`HighPressure`,
`internal/agent/compact.go:585,610`), which is why a memory limit and the
turn cap together bound the two things a session can run out of.

If you need a hard token stop, the loop condition itself has to consult a
running spend and the provider call has to take a context that aborts when
that spend crosses the ceiling. That is a real gap since #605, and it is
the shape of change to make deliberately rather than discover in
production.

---

- This is NOT a single git repo monorepo — each subdirectory may be
  independently versioned. Build from the `xdev` root.
- Some projects have worktrees under `.worktrees/` — these are feature
  branches. Build from the main checkout, not a worktree, unless you
  intentionally want that branch.
- The repo's author is `linhdmn <mnhatlinh.doan@gmail.com>` and that is
  the only name that may appear in commit metadata. Never attach a
  `Co-Authored-By:` trailer naming another AI model to a commit here.