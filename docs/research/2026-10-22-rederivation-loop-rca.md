# Why one question took 22 turns and 28 tool calls — root-cause analysis (2026-10-22)

**Session:** `adf5029b-7445-455f-a7ba-2d708ff27c3e`
(`~/.xdev/agent/sessions/-Users-linh.doan-work-harvey-freepeak-xdev/2026-10-22T02-54-41.475Z_adf5029b-*.jsonl`)

**User-reported symptom:** one question — *"list me all pr urls opening xdev"* — took 22 assistant turns and 28 tool calls over 6 minutes 19 seconds, and the answer arrived only when the run-budget wrap-up forced it. The user had already been told the answer was delivered at turn 5.

**Measured shape of the run:**

| metric | value |
|---|---|
| wall clock, prompt → last message | **6 min 19 s** (02:54:53 → 03:01:12 UTC) |
| assistant turns | 22 |
| tool calls | 28 (all `bash`) |
| `gh` calls | 23 `gh api`, 3 `gh search`, 2 `gh pr`, 2 `gh repo`, 1 `gh auth` |
| keep-going nudges | **0** |
| run-budget wrap-ups | 1 (at 6 min) — the only thing that ended the run |
| answer delivered | only after the wrap-up |

**The nudge is not the cause here.** Zero `prompt-continuation` messages: the six classifier PRs (#719–#726) closed the gate. What is left is the model re-deriving one question in many spellings, and the harness having no way to see it.

## The model had the answer at turn 5

Its own reasoning, at the point where it had already run `gh pr list`, the REST `pulls` endpoint, GraphQL and the search API:

> "So: the answer is clear. There are ZERO open PRs in FreePeak/xdev. Let me double-check that the PR numbers against branch names to determine which branches might have unmerged work… But wait — the user asked 'list me all pr urls opening xdev'. Maybe they mean 'open PRs' in xdev. Since there are none, I should report that plainly and offer the useful adjacent info: the most recent merged PRs, and the unmerged remote branches that could be reopened as PRs."

So it answered, decided the answer was "too thin", and went looking for adjacent material for 15 more turns. Nothing in the harness pushed it.

## 1. Root cause #1 — the repeat guard cannot see a re-derivation

`internal/agent/repeat.go:76` — `canonicalArgs` renders a call's arguments in a form "two calls with the same meaning share". It does exactly one thing: it decodes the JSON and re-marshals it, which sorts the map keys. Numbers keep their literal form (deliberately — `UseNumber`). **It does not normalize a command's content at all.**

The key is `name + "\x00" + canonicalArgs(args)` (`repeat.go:63`). For a `bash` call the argument is one JSON object with a single `command` string, so the whole shell line is ONE opaque string. Any difference in it — a different flag value, a different page size, a different projection — is a different key.

Measured on this session's 28 calls: **28 distinct keys, 0 duplicates.** The thresholds (`repeatThresholds = []int{3, 5, 8}`, `repeat.go:42`) never came within two of firing.

The chain is also **consecutive** (`repeat.go:64`: a different key resets the count to 1). The model alternated between `gh` and `git`, so even exact repeats would have been reset.

## 2. Root cause #2 — the tool-result cache shares the blind key

`internal/agent/loop.go:2493` — `cacheKey := call.Name + "\x00" + canonicalArgs(call.Arguments)`. The same key. So the 28 differently-spelled calls were **28 real executions**: the model paid for the same question 10 times over.

The cache cannot simply be coarsened: it returns a RESULT, and a result for `gh api X --jq 'length'` is not the result for `gh api X`. A coarse key that merged `--repo A` with `--repo B` would hand the model the answer to a different question. The cache key must stay precise; the *notice* need not.

## 3. Root cause #3 — the run budget is the only backstop, and it is not authoritative

`internal/agent/loop.go:759` — the wall-clock budget fires at 6 minutes and sends `RunBudgetPrompt`: *"this run has reached its time budget — answer now, in prose, with what you have found so far, and do not call any more tools"*.

It worked: the model answered in prose immediately. **And then the keep-going gate fired**, because the gate's guard (`lastWasPromptCont`) knows about the gate's OWN nudge and about nothing else. The turn had run `git fetch`, which is a genuine mutation, so `mutated` was true, and the budget's STOP was overridden by a "take the next concrete step" nudge. The run continued for one more turn after the clock had ended it.

The budget is also **not configurable** — `RunBudget` is an `Agent` field with no settings wiring, so 6 minutes is fixed for every user and every task.

## 4. Root cause #4 — the prompt has no rule for "you have the answer"

`internal/agent/prompt.go:67` — `SystemPromptBase` already says *"Match the effort to the ask"* and *"A question gets an answer … do the reading, give the report and stop. Don't start implementing."* Both are about not OVER-delivering in the sense of starting to implement.

Neither says what this session needed: **the answer was already in hand, and the model kept going anyway.** There is no rule that says a result, once found, is to be reported rather than corroborated.

## What is NOT the cause

- **Not the keep-going gate.** 0 nudges.
- **Not the classifier.** The `gh` read forms, the `2>&1` split, the loop scaffolding and the tool caps all landed (#719–#726) and are in v0.4.365.
- **Not context elision.** 0 `… [N ctx lines]` markers: the gateway did not truncate any result in this run.
- **Not the micro-compaction tier.** The tier now runs only under pressure, and this run never came near the threshold.

## The fix

| # | change | file |
|---|---|---|
| 1 | A **coarse "same work" key** for the repeat NOTICE: the program's path, its flag NAMES, and the positional values, with numbers normalized and the projection flags dropped. The notice is text, not data, so a coarse key cannot hand the model a wrong answer — the tool-result cache keeps the precise key. | `internal/tool` (next to the quote-aware splitter) + `internal/agent/repeat.go` |
| 2 | The notice counts **non-consecutively** within a run, so a re-derivation later in the run still counts. A user interjection still resets it. | `internal/agent/repeat.go` |
| 3 | The notice says what the model must do: **report what you have**. | `internal/agent/repeat.go` |
| 4 | The run budget's stop is **authoritative**: no keep-going nudge after the budget wrap-up. | `internal/agent/loop.go` |
| 5 | The run budget is **configurable** (`runBudget`, seconds; negative is unbounded). | `internal/config/settings.go`, `cmd/xdev/print.go` |
| 6 | The prompt gains the missing rule: **once you have the answer, report it** — paid for by trimming prose, never by appending. | `internal/agent/prompt.go` |


## What was measured, and what was REJECTED

The first instinct was a harness-side detector for "the same work again". Three
were built and measured, and two of them were thrown away. The numbers are
here because the rejected ones are the useful result: they say what the
harness CANNOT see.

### REJECTED: a tool-only streak guard

The live shape is 21 consecutive turns with tool calls and no text at all.
A guard on that streak looks perfect. It is not:

| streak length | share of this repo's 7,941 tool-only streaks |
|---|---|
| ≥ 6 | 54.3% |
| ≥ 10 | 39.3% |
| ≥ 20 | 22.3% |
| ≥ 30 | 14.4% |

The median streak is 6 and the p90 is 40. A long silent streak is NORMAL
here — a model doing a real task does not narrate every call. The signal does
not discriminate, so a streak guard would nag on most legitimate runs.

### REJECTED: a program-level counter

"you have run `gh` 12 times" — the live run ran `gh` 30 times (23 `gh api`,
3 `gh search`, 2 `gh pr`, 2 `gh repo`). Over this repo's own 791 runs:

| one program called at least | share of runs |
|---|---|
| 6 times | 70.5% |
| 12 times | 60.7% |
| 20 times | 49.1% |
| 30 times | 38.9% |

Half of all runs call one program 20+ times. The threshold that would catch
the live run is above the median run. Rejected.

### KEPT: a canonical "same work" key, for the NOTICE only

The one thing the classifier could see: the SAME command in a different
spelling. The exact key normalizes nothing but JSON key order, so a model
that changes `--limit 100` to `--limit 2000`, or swaps `--jq 'length'` for
`--jq '.[] | .url'`, or pipes into `head -40` instead of `head -50`, pays
for the same work again.

The canonical form (internal/tool/canonical.go) is the program's path plus
its sorted targets, with a projection's value dropped and a pure-number
magnitude normalized. What it deliberately does NOT normalize: a flag value
that names the TARGET (`--repo A` vs `--repo B`), a range (`sed -n '1,50p'`
vs `sed -n '200,250p'`), and the program path (`gh pr` vs `gh api`).

Measured over the store's 134,251 bash calls:

| | within-run calls in a group of more than one |
|---|---|
| the exact key (today) | 4,634 |
| the canonical key | **6,515** (+41%) |

The key is used for the NOTICE only. The tool-result cache keeps the exact
key, because a coarse key that merged `--repo A` with `--repo B` would hand
the model the answer to a different question. The hard break at eight
identical calls stays on the exact chain for the same reason: a coarse count
of eight can be a model reading a file in chunks.

### What the live run's 28 calls actually are

The canonical key does NOT group them, and that is correct: they are 28
genuinely different commands. `gh pr list --repo linhdmn/xdev`,
`gh pr list --state open`, `gh api repos/FreePeak/xdev`,
`gh api "repos/FreePeak/xdev/pulls?state=open"`, `gh api graphql`,
`gh search prs`, `gh repo list` — the same QUESTION in six program shapes
and a dozen query shapes. Telling them apart needs to know that "the open
PRs of FreePeak/xdev" is one question, and no shell canonicalizer knows
that.

So the live case is not fixed by a duplicate detector, because there is no
duplicate: the model asked a new question every time, and the new question
was always "and what about X?". That is a MODEL behaviour, and the only two
levers that reach it are the PROMPT (it is told to report when it has the
answer) and the CLOCK (the budget ends the run). Both are now in place; the
budget is also configurable, so a person who wants a tighter bound can have
one.
