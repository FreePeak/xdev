# Why a one-question xdev session ran 17 minutes and answered nothing — root-cause analysis (2026-10-21)

**Session:** `8efefc9e-2d4b-420d-8ea7-935b144765ce`
(`~/.xdev/agent/sessions/-Users-linh.doan-work-harvey-freepeak-xdev/2026-10-21T13-01-14.489Z_8efefc9e-*.jsonl`)

**User-reported symptom:** one question — *"Check my current codebase and documents tell me where
are we from this xdev repos features, issues"* — ran for more than nine minutes, was still not
done, and along the way started doing work that was not the ask (merging PRs, monitoring another
session, writing a blog, a testnet PnL dive) and left the working directory to look for a sibling
repo.

**Measured shape of the run (from the session file):**

| metric | value |
|---|---|
| wall clock, prompt → `session_exit` | **16 min 55 s** (17 min 20 s with the provider stall) |
| assistant turns | 120 |
| tool calls | 193 (168 `bash`, 15 `read`, 7 `eval`, 1 `github`, 1 `leankg_status`, 1 `todo`) |
| provider round-trip time | 12.8 min (**75 %** of the wall clock — tool execution was ~2 s total) |
| assistant turns with a final answer | **0** |
| end state | `recovery_silent_provider` → `recovery_header_timeout` → compaction → `recovery_all_targets_down` → `session_exit` |

**The session ended in a provider failure, not a report.** Every guard the loop had was satisfied
the whole way through: the turn cap is 200 (113 used), the repeat guard never fired (no two calls
were byte-identical), the compaction ladder never fired (the window had room). The run was legal
the entire time and still cost the user 17 minutes and no answer.

There are four independent causes. The first is the reason the loop never ended; the second is the
reason the model kept re-deriving what it already knew; the third is the reason it started doing
work that was not the ask; the fourth is why no guard caught any of it.

---

## 1. Root cause #1 — the pre-request micro-compaction tier destroyed the evidence, on every boundary

`internal/agent/compact.go` — `maybeCompact()` ran, at **every** step boundary and **before** the
compaction trigger was evaluated:

```go
// Pre-request tier (microcompact): clear stale tool results before paying
// for a summary. Cheap, deterministic, no provider call.
if cleared, changed := microcompactToolResults(history, defaultMicrocompactKeepRecent); changed {
    history = cleared
}
if !a.compactionDue(history) {
    return history
}
```

`internal/agent/compact_micro.go` — `defaultMicrocompactKeepRecent = 5`: **every tool result older
than the newest five was replaced by the literal string `[Old tool result content cleared]`.**

Two independent faults in those eleven lines:

1. **It ran unconditionally.** The tier is a *preparation* for the compaction ladder ("clear stale
   tool results before paying for a summary"), but it ran on every boundary regardless of whether
   the ladder would ever run. The window here is 1,000,000 tokens (`~/.xdev/agent/models.yml`,
   `onegw.contextWindow`), so the threshold is 850,000 (omp's 15% reserve rule, #707/#708) and the ladder never fired. The tier was
   shaving a survey's findings down to five results inside a window that had room for all 193.

2. **The trigger was read after the tier had already cleared the history.** `compactionDue` asks
   `contextTokens(history) > threshold` — but `history` was the *shaved* one. So the tier not only
   destroyed the evidence, it also **masked the very compaction it exists to prepare for**: the
   ladder saw a small history, concluded there was no pressure, and never summarized. The
   compaction that finally fired at 13:17:14 was not the ladder — it was the overflow recovery
   after a 90-second provider stall (`recovery_silent_provider`, then
   `recovery_header_timeout`), which forces a compaction ignoring the threshold.

**Measured on this session's own numbers:**

| | value |
|---|---|
| tool output produced | 1.1 MB (193 results) |
| session's real content at the end | **286,290 tokens** (the compaction entry's own `tokensBefore`) |
| compaction threshold (window − 15% reserve) | 850,000 tokens |
| peak tool output visible under the old tier | **99 KB** (the newest 5 results) |
| tool results visible at 187 of 193 calls | **5** |
| share of the model's evidence destroyed | **~97 %** |

With the tier gated the way it should have been (it fires at half the threshold — 425,000 tokens
here), **286,290 < 425,000**: the tier never runs, the ladder never runs, and the model keeps all
1.1 MB.

## 2. Root cause #2 — the model re-derived what the tier had just deleted

This is the loop's fuel, and the model's own words name it. At session turn 190:

> *"Also check the open PRs list (**I need to re-derive since outputs got cleared**). Let me redo
> the PR list."*

and at turn 296:

> *"Let me gather the open PR list and open issue counts one more time (**previous outputs got
> cleared**)."*

Simulating the tier's rule over the real call sequence: **183 of 183 tool calls** were issued at a
point where an earlier result from the *same* tool had already been cleared. The model asked again
because the answer had been taken away.

The repeat guard could not see it (`internal/agent/repeat.go:61` keys on `name + canonicalArgs`):
**0 of 193 calls were byte-identical**, because the model drifts its arguments (`| head -40` vs
`| head -50`, `--limit 500` vs `--limit 2000`), so the thresholds 3/5/8 and the hard veto never
fired. The per-run tool-result cache (`internal/agent/loop.go:2331`, same key) missed for the same
reason, so all 25 `gh issue list` calls were 25 real executions.

The repetition profile, same intent class re-asked:

| intent | original | patched |
|---|---|---|
| `gh issue list` | 25 | 6 |
| `git status` | 23 | 3 |
| PRD headings grep | 17 | 1 |
| `gh pr list` | 16 | 3 |
| `bash` calls total | 168 | 44 |

The model also said *"Now I have a good picture"* at least eight times (turns 205, 215, 225, 237,
248, 258, 260) and then asked for more — it believed it was one step from answering and was not.

## 3. Root cause #3 — a globally-scoped memory recall was promoted into the session's request

After the provider stall, the forced compaction summarized the history. The summary's
**`## Original Request`** section read:

```
- Review all open xdev PRs and identify mergeable ones, then merge them one at a time.
- Inspect the current xdev session in the `agentloop` repo to determine whether it triggers
  Claude Code; if so, stop it and tell that session this is forbidden for now, then keep
  monitoring it.
- List every branch and worktree in the repository and report whether the working tree is clean.
- Trace xdev's features and development flow from its first day and write a new English blog
  explaining how it was built.
- Deep-dive into the codebase and live testing, assess the current state toward achieving
  profitable testnet PnL after fees before considering mainnet approval, and report whether the
  system is currently running locally.
```

**None of that was the user's question.** The user asked one thing: *"tell me where we are from
this xdev repos features, issues"*.

The chain, each link verified:

1. `cmd/xdev/print.go:286` — `wireAgentMode` hands every mode (TUI included) the memory backend's
   recall: `ag.MemoryContext = h.CompactionContext`.
2. `internal/agent/compact.go` — `summarizeSpan` puts that recall **ahead of** the transcript in
   the summarize request, under the heading *"Recalled memories to preserve:"*.
3. `internal/memory/hindsight.go` — `readTags()` returns **no tags** under global scoping, and the
   config is `scoping: global` with one shared bank (`bankId: xdev-gateway`). The recall therefore
   reaches **every session the bank has ever seen**.
4. The live service confirms it. `POST /v1/default/banks/xdev-gateway/memories/recall` with the
   user's own prompt returns, as the top hits, five requests the user had made in **other
   sessions** — the PR-merge ask (2026-10-14), the `agentloop`/Claude-Code ask (2026-10-15), the
   branch-and-worktree ask (2026-10-20), the blog ask (2026-10-11) and the testnet-PnL ask
   (2026-10-11) — verbatim.
5. The compaction prompt asked for `## Original Request — [what the user asked for in this turn]`.
   The summarizer had the memories first and a nearly empty transcript (the summarize call's
   `input` was 1,003 tokens against a 286,290-token span), and wrote the **memories** as the
   original request. The summary's own "Early Progress" section admitted it:
   *"No actual transcript actions, findings, code changes, PR merges, session inspection results,
   branch/worktree inventory, or testnet measurements are present in the supplied transcript."*
6. The model read the summary as its instructions. Its thinking at 13:17:31:
   *"The user's request (from memory recall) had 5 items… Given the truncated context…"* It then
   built a six-phase todo list (PR merge, agentloop monitoring, branch inventory, blog, testnet
   dive), `cd`'d into `/Users/linh.doan/work/harvey/freepeak/agentloop` — **outside the working
   directory** — and ran `gh pr list` and `ps aux | grep claude` there.

So "going outside my dir" and "doing the wrong intentions" are the same defect: another session's
request, retrieved from a global memory bank, was written into this session's summary in the slot
the model reads as *the task*.

## 4. Root cause #4 — no guard measures the clock

`DefaultMaxTurns = 200` (`internal/agent/loop.go:153`) counts turns; a provider that answers in
two seconds turns 200 turns into ten minutes of waiting. There was no wall-clock bound on a run,
and no bound on how many times the same *intent* could be re-asked. The run was legal from start to
finish and still cost 17 minutes and no answer.

---

## The fix

| # | change | file |
|---|---|---|
| 1 | The pre-request tier runs under pressure (half the compaction threshold), not on every boundary | `internal/agent/compact.go` — `microcompactDue` |
| 2 | The compaction trigger is read on the history **as it is**, so the tier can no longer mask the ladder | `internal/agent/compact.go` — `maybeCompact` |
| 3 | The kept window is a **token budget** (`CompactionConfig.KeepRecentTokens`), not a count of five | `internal/agent/compact_micro.go` |
| 4 | The placeholder names the tool and says the call can be repeated, so a cleared result is not read as an empty one | `internal/agent/compact_micro.go` |
| 5 | A **wall-clock budget** per run (default 6 min) sends one wrap-up and ends the run | `internal/agent/loop.go` — `RunBudget` |
| 6 | The recalled memories are labelled as background facts from other sessions, and both compaction prompts bind the request slot to the transcript and give memories their own `## Recalled Context` section | `internal/agent/compact.go`, `internal/memory/{memory,hindsight}.go` |

## Measured, same prompt, same model, same gateway

| | original | patched |
|---|---|---|
| wall clock | 16 min 55 s | **4 min 53 s** |
| assistant turns | 120 | **30** |
| tool calls | 193 | **49** |
| provider round-trip time | 12.8 min | 4 min 20 s |
| "outputs got cleared" in the model's reasoning | 14 | **0** |
| answer delivered | **none** (provider failure) | **yes** — a full status report |

The patched run answered in 30 turns with the same findings the original session had been
re-deriving for 120 turns (the origin/main divergence, the dirty main checkout, both open PRs and
their merge order, 185 open issues by priority, the stale worktrees, the duplicated PRD sections).

## Still open

- **`onegw` windowing.** The gateway elides the middle of long tool results
  (`… [12 ctx lines]`). Four occurrences in the patched run; the model worked around each by
  writing to a file and reading it back. Outside this repo.
- **The memory scope.** `scoping: global` in `~/.xdev/agent/config.yml` is what made the recall
  reach other sessions. Narrowing it (`per-project-tagged`) reduces the blast radius, but it does
  not fix this: all five leaked requests were about xdev, so a per-project scope would have
  returned them too. The fix for the leak is #6.
- **No filesystem boundary on `bash`.** `approvalMode: yolo` means nothing stops a `cd` outside the
  working directory. That was a *symptom* of #3 here, but it is still true in general.
