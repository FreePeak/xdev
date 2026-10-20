# Plan: the gateway must not wedge, and an answer must not be lost

*Date: 2026-10-20 · Status: **in progress** · Branch: `fix/gateway-stuck` · Worktree: `.worktrees/fix-gateway-stuck`*
*Scope: the Telegram bridge (`internal/gateway`) — a running turn may not stop the bridge from answering, and the answer may not be dropped on a flaky network.*

Related:

- `internal/gateway/gateway.go:251` — `d.handle(ctx, u)`, called **synchronously inside the poll loop**
- `internal/gateway/gateway.go:464` — `res := w.wait()`, the blocking wait for the worker
- `internal/gateway/gateway.go:501` — the final answer, sent **once**
- `internal/gateway/telegram.go:94` — `&http.Client{Timeout: 2 * time.Minute}`, one timeout for every call
- `internal/gateway/chats.go:188` — `Chats.Acquire`, the per-chat lease (currently unreachable)
- `internal/gateway/modelcmd.go:203` — `modelReport`, the `/model` list (already shipped, #639)

---

## 0. One-sentence outcome

A Telegram turn that is slow, wedged, or interrupted by a flaky network **still ends with the answer in the chat**, and no single chat can ever freeze the bridge for the others.

---

## 1. The problem, precisely

The report is "the gateway gets stuck and cannot return the telegram message". Four distinct defects sit behind that one sentence, all in the same file, all reachable from the shipped binary (`v0.4.323`, running as pid 65717).

### 1.1 A turn blocks the poll loop (the wedge)

`Run` polls, then handles each update **on the poll goroutine**:

```go
for _, u := range updates {
    ...
    d.handle(ctx, u)      // gateway.go:251
}
```

and `handle` ends in `d.turn(...)` → `res := w.wait()` (`gateway.go:464`), which blocks until the worker process exits. While it blocks, the daemon does not call `getUpdates` again. Consequences, all visible to the user:

- **`/stop` cannot be delivered.** The cancel command is read by the same loop that is blocked in the turn. A turn that hangs cannot be stopped — the one command that exists to stop it is unreachable exactly when it is needed.
- **No other chat is served.** One slow chat freezes every chat on the bot.
- **The next message gets no answer** for the whole duration of the running turn, and no typing bubble either.

The lease that was built to prevent two workers on one chat (`chats.go:188`) can therefore never fire: the loop is serial, so the lease is always free by the time the next message is read. The guard exists; nothing can reach it.

### 1.2 Every Telegram call may block for two minutes (the second wedge)

`Telegram.client()` returns `&http.Client{Timeout: 2 * time.Minute}` — the right budget for a long poll, and the wrong budget for everything else. `sendMessage`, `editMessageText` and `sendChatAction` inherit it, and those calls are made **on the poll goroutine** (every command reply, `d.reply` at `gateway.go:287`/`292`/`294`/`303`).

On a network that is having a moment, one command reply therefore stalls the poll loop for up to two minutes. The log shows that network is exactly that kind: 411 poll errors and 19 typing failures, with `no such host`, `read: can't assign requested address` and `context deadline exceeded` in the same window.

### 1.3 The answer is sent once, so one failure loses it (the lost reply)

```go
if _, err := d.replySeen(ctx, chat, res.answer, w.stream.previewed()); err != nil {
    d.logFn("gateway: reply to chat %d: %v", chat, err)   // gateway.go:501
}
```

One attempt. The answer exists — the worker's stdout is right there in `res.answer` — and a single transient failure at the moment it is ready means the user gets **nothing**. The log has zero `reply to chat` lines, so this has not fired in the current window, but the failure mode is one network hiccup away and it is the difference between "slow" and "no reply at all".

### 1.4 A restart mid-turn loses the answer silently

The turn's context is the daemon's context. When the daemon stops, the context is cancelled, the in-flight `sendMessage` fails with `context canceled`, and the answer is gone. The log shows this happening:

```
gateway: chat 798213991 turn started (pid 94197, session (new))
gateway: typing for chat 798213991: telegram sendChatAction: ... context canceled
gateway v0.4.323 starting
```

The turn was killed by a restart and the chat was never told. The detached worker (`Setsid: true`, `sysprocattr_unix.go:9`) survives its parent and keeps burning CPU with nobody collecting its output.

---

## 2. What changes

| # | Change | File | Closes |
|---|--------|------|--------|
| A | A turn runs off the poll goroutine; a per-chat FIFO keeps the arrival order | `internal/gateway/gateway.go`, new `internal/gateway/queue.go` | 1.1 |
| B | Every non-long-poll Telegram call gets its own, short timeout | `internal/gateway/telegram.go` | 1.2 |
| C | A transient send failure is retried; a permanent refusal is not | `internal/gateway/gateway.go` | 1.3 |
| D | A running turn is recorded, so a restart reports the lost answer instead of hiding it | `internal/gateway/gateway.go`, `internal/gateway/runstate.go` | 1.4 |
| E | The worker gets a hang ceiling, so a wedged provider cannot hold a chat forever | `internal/gateway/gateway.go` | 1.1 (backstop) |
| F | `/model` gains a picker: the catalog as buttons, one tap to choose | `internal/gateway/modelcmd.go`, `internal/gateway/telegram.go` | the second half of the request |

### A — the turn leaves the poll loop

One runner goroutine per chat, fed by an unbounded FIFO. The properties that matter:

- **Order is preserved per chat.** Two messages in one poll batch must not race for the session; the second one has to be the second turn. The FIFO gives that without a lock around the whole daemon.
- **One turn per chat at a time.** The lease stays the guard (and now it is reachable: a second message that arrives while a chat is busy is answered with the existing "still working on the previous message — /stop cancels it" instead of silently queueing behind a turn that may never end).
- **No chat can freeze another.** The runner is per chat, so a wedged worker is one chat's problem.

### B — a short timeout on everything but the long poll

`getUpdates` keeps the two-minute budget (it holds the connection for `pollTimeout` + slack). Every other method gets a per-call deadline. A send that cannot complete in 20 seconds is not going to complete; the turn should learn that and try again, not hold the poll loop for two minutes.

### C — the answer is retried, not dropped

Retryable: a transport error, a 429, a 5xx. Permanent: a 400/401/403 (Telegram's own refusals — a bad token, a chat the bot cannot write to). The retry is bounded (a few attempts with a backoff) so a dead network costs seconds, not minutes, and it stops immediately when the daemon's context is cancelled.

### D — a lost turn is reported

The turn is recorded before the worker starts and cleared when the turn ends. A turn that ends because the **daemon's context was cancelled** leaves the record behind: the next daemon start finds it, tells the chat "the gateway restarted while your turn was running, so that answer was lost — send it again", and clears it. Silent loss becomes visible loss.

### E — a hang ceiling on the worker

A plain `xdev --print` run has no wall-clock ceiling (`cmd/xdev/main.go:801` applies the 2h default to `--bg` children only), so a gateway worker that hangs holds its chat forever. The gateway passes `--max-time` explicitly, using the same 2h ceiling `xdev --bg` documents.

---

## 3. The `/model` list is already there — and now it is a picker

The second half of the request — "choose the model from the list in the Telegram gateway" — is **already shipped as a list**: `internal/gateway/modelcmd.go:203` answers a bare `/model` with the model in force and the catalog to pick from (up to 20 refs, `send /model <ref>` to switch), the ref is persisted per chat in `gateway/sessions.json`, and it is resolved at spawn time so a ref the operator later removes cannot wedge a chat. It is in the running binary (`strings ~/.local/bin/xdev | grep "available (send /model"` → present) and the bot menu advertises it (`gateway.go:27`).

What it did **not** have is a way to *choose* without typing a ref — and typing `onegw/opencode/minimax-m3.1-flash-free` on a phone is exactly the friction the request names.

So the list gains a **picker**: the same catalog as inline buttons under the message, one tap to switch. The tap arrives as a `callback_query` update; the bot closes it with a toast and re-marks the list in place, so the choice is visible where the list already is instead of a second list arriving.

- `getUpdates` now asks for `callback_query` as well as `message`.
- The button payload is the model ref, prefixed `m:` so a button of another kind can never be mistaken for one.
- A tap is refused the same way a message is: a chat outside the allowlist changes nothing, and a ref the catalog no longer has is refused at the tap, not one turn later inside the worker.
- The text list stays — it is the full catalog, and it is what a copy-paste of a ref looks like.

---

## 4. Tests

Every change is a test first (the repo's rule for an existing codebase).

- `TestTurnDoesNotBlockThePollLoop` — a worker that blocks on a gate file; `/status` must be answered while the turn is still running.
- `TestReplyRetriesATransientFailure` — the fake bot refuses the first send with a 429; the answer must still arrive.
- `TestReplyRetriesAnEdgeFailure` — a 502 whose body is not a Bot API envelope; the answer must still arrive.
- `TestReplyDoesNotRetryAPermanentRefusal` — a 403 must not be retried (one request, not three).
- `TestSendHasItsOwnTimeout` — a hanging endpoint must not hold a call for the long-poll budget.
- `TestLongPollKeepsItsOwnBudget` — the send timeout must not cut the poll short.
- `TestWorkerArgsHasAHangCeiling` — the argv carries `--max-time`.
- `TestRestartReportsTheLostTurn` — a pending record left by a dead run must produce one notice on the next start.
- `TestPendingSurvivesACancelledDaemon` / `TestPendingIsClearedAfterAGoodTurn` — the record's life.
- `TestQueueKeepsAChatInOrder` / `TestQueueDoesNotLetOneChatBlockAnother` / `TestQueueRunsEveryQueuedTurn` — the FIFO.
- `TestModelCommandOffersAPicker` / `TestModelCallbackPicksTheModel` / `TestModelCallbackRefusesAnUnknownProvider` / `TestModelCallbackRefusesAChatOutsideTheAllowlist` — the picker.
