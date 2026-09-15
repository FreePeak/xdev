# Why xdev sessions feel slower than omp — root-cause analysis (2026-09-15)

**User-reported symptom:** in this repo, xdev sessions *take more turns* and *run longer* than omp
sessions doing the same work. This doc finds the root causes by measuring both tools' real session
JSONL stores (166 xdev + 52 omp files, all on the same onegw `free`/`fast` lanes, same usage/ttft/
duration schema), reading xdev's loop/TUI source, and reading the 13 UI-stall dumps in
`~/.xdev/agent/dumps`.

## 0. The baseline that has to be stated first

Steady state is **not** the problem. Full-step latency (event → assistant completion, which includes
gateway queue + prefill + generation, measured identically for both tools from wall timestamps):

| metric (big sessions, ≥40 steps) | xdev (43 sess) | omp (41 sess) |
|---|---|---|
| full-step latency p50 / p75 / p90 | 17.0 / 34.4 / 62.6 s | 17.0 / 36.4 / 73.8 s |
| recorded model `duration` p50 | 7.3 s | 17.0 s |
| tool calls per assistant step | 1.11 | 1.04 |
| failed tool results (bash exit≠0 / error text) | 2.0 % of steps | 6.6 % of steps |

The onegw `free` lane costs every request ~10 s of queue+prefill **for both harnesses**; model choice
and gateway routing are not differentiators. Cold start favors xdev heavily (paired `/tmp/xdev-vs-omp`
A/B, 2026-09-11, identical single-step prompts: xdev 1–4 s wall, omp 7–136 s). So the perception is
caused by episodic and structural issues around the model call, not by the model call.

## 1. Root cause #1 (the big one): UI-loop stalls kill healthy streams, which the loop recovers as *extra turns*

xdev's TUI is single-goroutine by design (`internal/tui/stall.go:11-16`): `Run` calls `handleKey`
and `draw` inline. The chain that turns a terminal hiccup into a lost turn:

1. `draw()` takes `a.mu` and **holds it across `s.Show()`** (`app.go:2291-2293`; the stack dumps
   show `tScreen.Show → draw → Buffer.WriteTo → syscall.write` blocked for **12 m 9 s** in
   `tui-stall-20260915-133325.txt`). tcell writes to the tty synchronously; a full tty buffer
   (suspended terminal, laptop sleep, huge frame flush) stops the UI loop.
2. Provider deltas reach the app **synchronously on the agent goroutine**: `tuiHooks.OnEvent` →
   `app.AppendAssistant` (`cmd/xdev/tui.go:1760`, `app.go:389`) — which takes the same `a.mu`.
   A frozen UI therefore stalls the agent's stream mid-turn.
3. The stall stops event delivery through the watchdog relay, so the **idle timer (90 s) fires on a
   stream that is healthy at the socket** and cancels it (`internal/ai/watchdog.go:17-19, 42-90`,
   `ErrWatchdogAborted`).
4. `ErrWatchdogAborted` classifies transient → **retain-and-continue**: the partial assistant message
   is persisted, a **synthetic user turn is injected** —
   `"your previous message was cut off by a provider error — continue exactly where it stopped"`
   (`loop.go:566-583`, `failover.go:21`) — plus a backoff sleep before the retry
   (0.5→1→2→4→8 s ladder, 4 attempts, then failover chain + escalation rounds, `retry.go:26-38`).

Measured fingerprints in the session stores:

- 27 auto-injected "cut off by a provider error" user turns across 12 xdev sessions.
- Follow-up user messages that are nudges ("continue", "continue on this", "?", retries):
  **xdev 57.5 % (73/127) vs omp 16.7 % (17/102)** of all follow-ups on big sessions. During a UI
  stall keystrokes never reach the loop, so the user retypes; each retry costs a round trip.
- 13 stall dumps over 2026-09-13→15 (9 on the 15th), beat-lags 5.6 s → 15 m 43 s, blocked in
  tty write (×2), tcell paint (×4), `app.go` loop sites (×5), and one **mutex wait** (`sema.go:95`).
  Dumps only exist for stalls ≥5 s; sub-5 s hiccups are free but invisible.

Every stall episode = dead UI + killed stream + partial in context + injected "fake user" turn +
backoff + full re-request: tens of seconds per episode, one extra conversational turn per episode.
That is precisely "costs more turns and runs longer".

## 2. Root cause #2: the prompt-cache gap (xdev wrote nothing to the cache until today)

Before #133 (landed 2026-09-15, `d589778`, in the installed binary the same evening), xdev emitted
**no `cache_control` markers and no `prompt_cache_key`** on any request
([omp-prompt-cache-2026-09-15.md](omp-prompt-cache-2026-09-15.md) §1). Consequences observed in the data:

- On the OpenAI-compat free lanes, upstream auto-prefix-caching still saved most of the context
  (84.7 % hit ratio, `cacheWrite` 0 everywhere), but without a stable per-session affinity key
  xdev's requests carry no routing stickiness for the gateway's per-account steering — each cold
  prefix re-pays full prefill: xdev non-cached input is p50 2,094 tok/step (omp: 983) with a p90 of
  45 k. On any non-auto-caching endpoint (native Anthropic) the whole prefix was re-paid every turn.
- The fix exists now; its effect is not yet measurable in the session stores (binary installed
  22:21 the same day). Re-check after real use: non-cached input p50 should collapse toward ~1 k
  and the p90 cold-prefill tail should thin.

## 3. Root cause #3: xdev's own latency numbers hide #1/#2 (why it wasn't caught)

All four adapters set `start := time.Now()` **after** `wirePost` returns — i.e. after response
headers (`openai_completions.go:238-244`, `anthropic.go:312-318` and siblings). So xdev's `ttft` and
`duration` exclude connect + gateway queue + prefill, while omp measures from fetch start
(omp `ttft` p50 10.7 s vs xdev's 43 ms on the same lane — different measurement windows, not
different speed). Effect: xdev's HUD/stats report ~7 s steps while users wait ~17 s, and the queue
time that dominates everything is invisible in every xdev-side metric.

## 4. Root cause #4: heavier context per step (no artifact offload + bash-heavy profile)

- Tool results kept verbatim in context: median **952 B (xdev) vs 392 B (omp)**; xdev's sink cap
  is a 32+32 KiB head/tail window with **no artifact store** (PRD §2 "Tools", retention open as
  #115), so the 1.2 % of results >16 KB (15 % of all tool bytes, max 150 KB) sit in context.
- The model's tool profile diverges: bash:read = 8726:563 (**15:1**) for xdev vs 7686:1991 (3.9:1)
  for omp — xdev agents `cat`/pipe through bash instead of structured `read`/`grep`, which is how
  the raw bytes get in. Mid-session effective context (83 k vs omp's 154 k) still favors xdev, but
  per-step uncached growth (2,094 vs 983 tok) is the visible cost.

## 5. Ruled out

- Raw model/gateway latency (identical p50 17.0 s), cold start (xdev wins by seconds-to-minutes),
  tool parallelism (calls/step 1.11 vs 1.04), error rates (xdev lower), session persistence
  (dead-air between events ≈ 0 beyond model time in non-stalled turns), memory (RSS budget met).

## 6. Fix directions (ranked by measured cost)

1. **Cut the UI→stream coupling.** Never hold `a.mu` across `s.Show()`; copy deltas under a short
   lock and paint on the UI tick; ideally render on a dedicated goroutine with a bounded,
   drop-coalescing writer. A frozen terminal must not be able to touch an in-flight request.
2. **Make the watchdog measure stream liveness, not consumer liveness**: arm the idle timer from
   the raw SSE read side (before the bounded `out` channel) so a backed-up consumer cannot kill a
   healthy stream.
3. **Count injected continuation turns out of "user" in metrics/UI** and show them distinctly —
   right now they masquerade as user turns, inflating both the transcript and the felt turn count.
4. **Verify #133's effect post-fix** (uncached-input p50, cold-prefill p90 tail) and keep
   `promptCacheKey = sessionId` sticky through `/switch`/`/fork` as omp does.
5. Fix the `ttft`/`duration` origin to fetch-start so xdev's own numbers match omp's and reality;
   otherwise the next latency regression hides the same way.
6. Artifact-offload seam for results >16 KB (#115) and nudge the model back to structured
   `read`/`grep` (the 400-char tool-description cap clipped `edit` once already — bug.md §2).
