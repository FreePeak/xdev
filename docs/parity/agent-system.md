# Agent-system parity: xdev vs omp (v18.1.17 docs / v18.1.18 binary)

Tester: T3Agent, 2026-09-12. Scope: subagents/task, agent hub, hooks,
advisor/watchdog, plan mode + ask + goal, prewalk, TTSR.

Build pin: findings verified against a **clean worktree build of `48459fe`**
(`/tmp/xdev-test-clean`). An earlier battery ran on a build of `ac50d35`;
every headline finding was re-confirmed on `48459fe` unless marked
"code-verified". Known deltas in `docs/parity-delta.md` are not re-reported.

Method: cheap surface checks plus a **scripted mock OpenAI-compatible
provider** (SSE streaming, per-request rules keyed on request substrings) so
model-dependent paths (plan/goal/prewalk/TTSR/advisor/task/hub) run
deterministically; every provider request is logged and inspected. Hook
payloads were captured by pointing settings hooks at `cat >> file`. One live
one-shot run per model-dependent surface on the real `onegw/free` model
confirmed the same shapes the mock shows.

Repro harness (reusable): `/tmp/parity-t3/mock/mock.py` (mock provider on
127.0.0.1:18099), `/tmp/parity-t3-mock/` sandbox (`XDEV_AGENT_DIR`,
`models.yml` with `mock` provider + `onegw` fallback), `analyze.py` for the
request log. Config knobs are set per test via `/tmp/parity-t3-mock/config.yml`
and `/tmp/parity-t3/mock/script.json`.

Severity classes: **blocker / bug / divergence / nit**, each marked
xdev-broken vs xdev-intentional.

## Verdict table

| # | Feature | Finding | Severity | Class |
|---|---------|---------|----------|-------|
| 1 | task tool | omp batch wire shape `{context, tasks[]}` rejected: `task: prompt is required` | bug | xdev-broken |
| 2 | task tool | default child gets no `task` tool → can never nest; omp bundled `task` agent spawns `*` to depth 2 | divergence | xdev-intentional? |
| 3 | task tool | `task.disabledAgents` unimplemented (no setting, no enforcement) | divergence | xdev-broken |
| 4 | task tool | discovery roots `.xdev/agents` + `<dataDir>/agents` only; frontmatter `advisor:` not parsed; `thinking-level`/`thinking` keys not accepted (only `thinkingLevel`) | divergence | mixed |
| 5 | task tool | `PI_BLOCKED_AGENT`, job manager, `agent://`/`history://`, idle-TTL parking absent (session-local `hub-N` ids, park/revive-by-send instead) | divergence | xdev-intentional |
| 6 | task tool | child that never calls `yield` silently hands back prose, status `completed` (omp: 3 reminders + `SYSTEM WARNING`) | divergence | xdev-broken |
| 7 | hub | tool op surface diverges: `send` takes `id`/`text` (job steering) vs omp `to`/`message` (+broadcast `all`, `await`, `replyTo`, `peek`); `start` lacks `env/pty/restart/persist/detached`, flat `ready_log`/`ready_port` vs nested `ready{}`; `logs` lacks `grep/head/follow/cursor`; extra `park`/`result` ops; peer messaging split into separate `send_message`/`inbox` mailbox tools | divergence | mostly xdev-intentional |
| 8 | hub | hub-started processes are **orphaned at session exit** (verified pid alive after xdev exited); omp broker stops non-`persist` processes when the last client exits | bug | xdev-broken |
| 9 | hub | process table is per-process; no cross-instance broker (omp: names/logs/state shared by every omp instance in the project dir) | divergence | xdev-intentional |
| 10 | hub | roster statuses `running\|idle\|parked\|done\|failed` vs omp `running\|idle\|parked\|aborted`; no Main; job-scoped roster | divergence | xdev-intentional |
| 11 | hub | duplicated error prefix `hub: hub: job "hub-1" already finished…` | nit | xdev-broken |
| 12 | hub | roster opens only via `/hub`; no `Alt+A`/`Ctrl+S`/double-← binding (BuiltinActions has no `app.agents.hub`/`app.session.observe`); empty roster refuses to open (`hub: no background agents`) vs omp opens even when empty | divergence | xdev-broken (cheap fix) |
| 13 | hub | verified good: `jobs`/`wait`/`cancel`/`send`-steer/`park`→`send`-revive/`result` lifecycle end-to-end; process `start`/`ps`/`logs`/`stop` verified live, `restart`/`describe` code-verified | ok | — |
| 14 | hooks | payload field names diverge: `tool_call` → `{"tool","input"}` (omp `toolName`,`toolCallId`,`input`); `tool_result` → `{"tool","args","text"}` (omp `toolName`,`toolCallId`,`isError`,`content`) | bug | xdev-broken |
| 15 | hooks | `agent_end` emitted with **nil payload** (`internal/agent/loop.go:278`) → agent_end hooks run with stdin `null` and can never match; observed `null` in the dump file | bug | xdev-broken |
| 16 | hooks | `session_switch`/`session_before_switch` fire only on the TUI resume/switch path (`cmd/xdev/tui.go:357,373`); `/new` (`swapStore`) emits nothing; print/rpc/acp never emit. Verified: no `ev-session_switch.jsonl` after `/new`, populated after `/resume` pick | divergence | xdev-broken (cheap fix) |
| 17 | hooks | event surface: 13 events (`internal/hooks/hooks.go:40`) vs omp ~20; missing `session_before_branch/branch`, `session_before_compact`, `session.compacting`, `session_before_tree/tree`, `session_shutdown`, `auto_compaction_*`, `auto_retry_*`, `todo_reminder` | divergence | xdev-intentional (subset) |
| 18 | hooks | shell-command hooks (event→command, JSON on stdin) vs omp JS/TS `pi.on` modules | divergence | xdev-intentional (no in-process JS) |
| 19 | hooks | verified good: 10s timeout (measured 10.002s, `signal: killed`, fail-closed), non-zero exit denies the tool (`tool call blocked: hook failed (fail-closed): exit status 1`), stdout `{"input":…}` mutation applied to the executed call (bash ran `MUTATED-FROM-HOOK`), settings `hooks` record + `.xdev/hooks` discovery + `--hook event=cmd` | ok | — |
| 20 | advisor | **print mode never runs the advisor**: `--advisor` is consumed only by the TUI (`cmd/xdev/tui.go:96`); `print.go`'s `buildAdvisor` (line 76) is never called → `omp -p --advisor` has no equivalent; all headless advisor semantics (steering, late notes, 10-min drain / 30s error drain) are unreachable | bug | xdev-broken |
| 21 | advisor | steering injection is plain text `advisor (concern): …` / `advisor aside (sev): …` via the steer queue vs omp `<advisory advisor="…" severity="…" guidance="weigh, don't blindly obey">` | divergence | xdev-broken (format) |
| 22 | advisor | child-advisor note delivered after the child finished is **silently dropped** (observed: `advise delivered` in the advisor transcript, nothing ever reaches the child/parent); omp preserves late notes as visible cards | divergence | xdev-broken |
| 23 | advisor | no `__advisor*.jsonl` transcript persistence → no Agent Hub advisor rows, no `omp stats`-style usage attribution, dump is memory-only | divergence | xdev-broken |
| 24 | advisor | emission guard: FIFO 256 + note cap 400 vs omp 4096; content-free phrase list `{ok,lgtm,looks good,all good,fine,nothing to add}` missing omp's `stop/done/complete/no issue continue` | nit | xdev-broken |
| 25 | advisor | `/advisor` lacks `configure` (WATCHDOG.yml editor; non-TUI hosts report TUI-only in omp) | nit | xdev-intentional |
| 26 | advisor | verified good: `modelRoles.advisor` resolution (unresolved → warn + disarm), `advisorImmuneTurns` default 3, WATCHDOG.md injected as `Especially pay attention to:\n<attention>…</attention>` (8KB cap) **including child advisors**, `taskAgentAdvisor: on/off/<model>` (advisor provider round-trip observed against the child's delta), WATCHDOG.yml roster plumbing (code-verified; roster fan-out not behaviorally exercised) | ok | — |
| 27 | plan mode | headless `-plan` **auto-accepts the first proposal**: `propose` → `plan accepted (no reviewer wired) — plan mode off; implement it now` (planmode.go:86-90). The read-only guarantee is void in print runs and `--plan-yolo` becomes meaningless (both paths auto-approve) | bug | xdev-broken |
| 28 | plan mode | full toolset (incl. `write`/`edit`/`bash`/`task`) still advertised while planning; denial happens per call. omp plan mode restricts the toolset (subagents: `read/grep/glob/web_search` + declared `ast_grep`) | divergence | xdev-broken (wasted turns) |
| 29 | plan mode | exit tool named `propose` (+ `xd://propose`/`xd://resolve` devices) vs omp `resolve` | divergence | xdev-intentional (naming) |
| 30 | ask | schema diverges: xdev `{question, options[label,description], multi, recommended: labels}` vs omp `{questions:[{id, question, options[label,description,preview], multi, recommended: index}]}`. An omp-shaped call fails `ask: question is required` (verified) | bug | xdev-broken |
| 31 | ask | registered and **blocking in headless runs** (omp registers ask only when `hasUI`): a model ask stalls the run for the full timeout — default 120s at `48459fe` (measured: run did not finish in 120s), 30s in an in-flight sibling edit | bug | xdev-broken |
| 32 | ask | verified good: `ask.timeout` → recommended auto-select (`[ok, 2.001s] → {"selected":["B"]}`); no recommendation → `no answer within 3s — proceed with your best judgment and state the assumption` | ok | — |
| 33 | goal | budget accounting lags one turn: request N's reminder/budget shows spend as of request N-1 (mock usages 1/10: rem(1)=100000 then rem(2)=99990; tool views 0→10 in the same turns). A 1-token budget flips to `budget_exhausted` a turn late | bug | xdev-broken |
| 34 | goal | ops `create\|get\|resume\|evidence\|complete\|drop`; no `remind`, no `pause`/`budget`; statuses `active/completed/dropped/budget_exhausted`; no goal-mode toggle (tool always present, session-scoped). Continuation landed 2026-09-14: a yield with no tool calls re-enters the run with the hidden `goal-continuation` prompt and `/goal create\|resume` starts the first turn — TUI-only, matching omp's `goal.continuationModes: [interactive]` default, so print/RPC/ACP still end at the yield | divergence | xdev-intentional |
| 35 | goal | verified good: create → evidence-gated completion (no-evidence complete refused), `goal_updated` persistence, per-turn reminder injection (observed in the system prompt of the next request), and the goal continuation (live 2026-09-14: a plan-only assistant message with no tool calls is followed by `message user attribution=goal-continuation` in the session JSONL, the next turn continues the work, and the run ends at `goal_updated completed`) | ok | — |
| 36 | prewalk | verified good: todo gate (write before any plan → held, no switch; plan todo with ≥1 task → switch after the first successful write), one-shot disarm, `-prewalk`/`--prewalk-into`/`--no-prewalk`, target rides the failover machinery (model field flips m1→m2 in consecutive requests) | ok | — |
| 37 | prewalk | "holding until a plan exists" diagnostics invisible in print mode (`logx` off in print; `cmd/xdev/print.go:325`) | nit | xdev-broken |
| 38 | ttsr | verified good: mid-stream abort + retry, `contextMode discard` drops the partial (request had 3 messages: user, user `<system-interrupt>`), per-turn repeat gap (gap=1 fires every turn; default 3 silences turns 1–2), quiet after 3 interrupts/turn, non-interrupting tool match folds into the tool result, `ttsr_triggered` hook event emitted | ok | — |
| 39 | ttsr | discovered rule files' `condition` field is parsed but **never consumed** (`internal/rules/rules.go:44-46` stores it; no consumer outside settings): only the inline `ttsr.rules` settings group feeds the engine. Repro: `.omp/rules/secret.md` with `condition: "SK-SECRET"` + streaming `SK-SECRET-123` → no interrupt; the rule body was still injected as prompt context | bug | xdev-broken |
| 40 | ttsr | tag shapes lack omp attributes: `<system-interrupt>rule "secret-leak": msg</system-interrupt>` vs `<system-interrupt reason="rule_violation" rule="…" path="…">`; non-interrupting reminder **appended** to the tool result text vs omp **prepended** leading text block (`<system-reminder reason="rule_violation" …>`) | divergence | xdev-broken (format) |
| 41 | ttsr | no `scope`/`globs` gating: text + thinking + tool args all monitored by default (omp default excludes thinking); no `disabledRules`/`builtinRules`; `astCondition` shells out to an `ast-grep` binary (omp: in-process native matcher, per-file digest projection) | divergence | mixed |

## Repro index (all verified twice unless noted)

Sandbox: `XDEV_AGENT_DIR=/tmp/parity-t3-mock`, cwd `/tmp/parity-t3-mock/ws`,
binary `/tmp/xdev-test-clean` (build: `git worktree add --detach /tmp/wt-48459fe
48459fe && cd /tmp/wt-48459fe && go build -o /tmp/xdev-test-clean ./cmd/xdev`).
Real-model checks used `XDEV_AGENT_DIR=/tmp/parity-t3` (models.yml symlink to
`~/.xdev/agent/models.yml`, `onegw/free`).

```sh
# 1  task batch shape rejected
#    script.json: rule → tool call task {"context":"shared","tasks":[{"name":"Kid","task":"…"}]}
xdev -max-turns 2 "batch shape"          # → task: prompt is required

# 2  default child has no task tool / 6  no-yield child silent
#    script.json: task {"prompt":"…","name":"K"} ; child reply text only
xdev -max-turns 3 "…" ; python3 analyze.py summary
#    → child request tools == [read, yield] (no task); result shows child prose, no warning

# 3  task.disabledAgents: `xdev config list` has no task.* group; grep finds no consumer
xdev config list

# 8  hub process orphaned at exit
#    script.json: hub start {"name":"probe2","application":"sh","args":["-c","echo READY; sleep 120"]}
xdev -max-turns 4 "…" ; pgrep -fl "sleep 120"    # → pid still alive after xdev exited

# 12 hub roster keybinding
grep -n "BuiltinActions" -A 12 internal/tui/keymap.go   # no app.agents.hub / app.session.observe

# 14/15 hooks payloads (config.yml hooks: tool_call/tool_result/agent_end → cat >> file)
xdev -max-turns 3 "Run: echo hello" ; cat /tmp/parity-t3/ev-*.jsonl
#    tool_call: {"input":{…},"tool":"bash"}   agent_end file: null

# 16 /new emits no session_switch (TUI in tmux; /resume does)
tmux … send-keys "/new" Enter ; cat ev-session_switch.jsonl     # → missing

# 20 print advisor inert (static, re-checked at 48459fe)
grep -n "buildAdvisor" cmd/xdev/print.go        # defined, never called; only tui.go:96 calls it

# 22 child-advisor note dropped
#    config: modelRoles.advisor + taskAgentAdvisor: "on"; script: task spawn →
#    advisor rule answers advise(concern) after the child yielded
#    → "advise delivered" in advisor turn; no steering text in any later request

# 27 headless plan auto-accept
xdev -plan -max-turns 3 "headless plan"  # propose → "plan accepted (no reviewer wired) — plan mode off"

# 30/31 ask headless (config: {} → default timeout)
#    script.json: tool call ask {"questions":[…]} → "ask: question is required" (omp shape)
#    valid shape, no ask.timeout: run does not finish within 120s (default 2-minute wait)
# 32 config ask.timeout: 2 → [ok, 2.001s] {"selected":["B"]}

# 33 goal accounting lag (mock usage 1 then 10 tokens, budget 100000)
xdev -max-turns 3 "probe goal" ; analyze requests
#    req1 reminder "100000 of 100000 remaining" while the turn already spent 1; req2 shows 99990
# 33b/35 goal continuation (TUI): /goal create "<objective whose first reply must be plan-only, no tool calls>"
#    session JSONL: message user(attr=user) objective → assistant plan (no tool calls) → message user(attr=goal-continuation)
#    → assistant tool calls → goal_updated completed → run idle (no user prompt in between); same file pins the negative:
#    a print/RPC/ACP run (GoalContinuation off) ends at the plan-only yield

# 36 prewalk gate (config: modelRoles.smol: mock/m2; -prewalk)
#    todo init P1/[task] then write → request models: m1, m1, m2 (switch after the write)
#    write without any todo → all requests m1

# 38/39/40 ttsr (config: ttsr.rules[...] / .omp/rules/secret.md)
#    prose "SK-SECRET-123" + rule condition SK-SECRET → <system-interrupt> injection, partial discarded
#    same condition in a discovered .omp/rules file → no interrupt (dead field)
#    interruptMode: never + tool-args match → <system-reminder> appended to the tool result
```

## Shortlist — real bugs worth fixing now

1. **#20 print-mode advisor inert** — documented omp flag (`-p --advisor`)
   silently no-ops; every headless advisor behavior is unreachable. Wire
   `buildAdvisor` into print mode + the disposal drain.
2. **#27 headless plan auto-accept** — `-plan` must stay read-only in print
   runs; auto-accept only under `--plan-yolo` (and surface the proposal for
   `xd://resolve` instead of accepting).
3. **#8 hub process orphaning** — stop non-persist hub processes when the
   session/run tears down (mirror the omp broker's last-client rule).
4. **#15 `agent_end` nil payload** — one-line fix; agent_end hooks are
   currently unfireable.
5. **#39 rule-file TTSR conditions dead** — feed `rules.Rule.Condition` into
   `agent.NewTTSR` bucketing or drop the field from the parser.
6. **#33 goal budget off-by-one** — the reminder/budget view trails a turn;
   with small budgets `budget_exhausted` fires late.
7. **#31 headless ask stall** — deeper fix beyond the in-flight 2min→30s
   change: omp never registers `ask` without a UI; xdev should either do the
   same in print mode or default `ask.timeout` to a small value.
8. **#1 task batch shape** — accept omp's default `{context, tasks[]}` shape
   (or at least reject with a model-actionable message).
9. **#14 hooks payload names** — rename `tool`→`toolName`, add `toolCallId`,
   align `tool_result` (`content`/`isError`); cheap and unblocks hook porting.
10. **#12 hub roster keybinding** — add `app.agents.hub` (Alt+A) to
    `BuiltinActions` and open the roster even when empty.

## Build-pin note

A binary built from the **live shared tree** mid-session (dirty
`internal/tool/{ask,ast,bash}.go` from a parallel stream) failed at stream
start with `json: error calling MarshalJSON for type json.RawMessage` — the
same script ran clean on the pinned worktree build. Verify agent-system
behavior from a clean pinned worktree, not the live tree.
