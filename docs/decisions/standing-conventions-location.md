# Decision: where a standing convention lives — context files, not recall

*Reference: `internal/agent/prompt.go:277-335` (`LoadContextFiles`), `internal/rules/rules.go:197-213` (`native`), `internal/memory/hindsight.go:1037-1106` (recall cap) · Milestone: M12 (memory) follow-on · Date: 2026-09-17 · Status: **decided***

**Decision in one line: a convention that must hold on every turn — a workflow rule, a repo invariant, a "never do X" — belongs in an always-loaded context file (`~/.xdev/agent/AGENTS.md`, `<repo>/AGENTS.md`, or a native `RULES.md`); auto-recall is a retrieval channel with a hard 1024-token budget and explicit "not authoritative" framing, so a rule that lives only there is dropped, reordered, or discounted.**

---

## 1. Context

xdev has two independent channels that put text into the system prompt, and they look interchangeable to a user writing a rule down. They are not.

| | Context files | Rules | Auto-recall (Hindsight) |
| --- | --- | --- | --- |
| Loader | `agent.LoadContextFiles` (`prompt.go:281`) | `rules.Discover` (`rules.go:133`) | `mem.GuidanceBlock()` (`print.go:969`) |
| Sources | `~/.xdev/agent/AGENTS.md`, then `AGENTS.md`/`CLAUDE.md` root→cwd | `RULES.md` (native, `AlwaysApply`), `.cursor/rules/*.mdc`, `.agent/rules`, plugins | one remote bank |
| Budget | `MaxContextBytes` = **32 KB** (`prompt.go:136`) | same block, per-rule | **1024 tokens = 4096 chars** (`DefaultHindsightInjectionTokenLimit`, `approxCharsPerToken = 4`) |
| Selection | **all of it, every turn** | all matching rules, every turn | top-N by relevance to **the last user turn only** (`RecallContextTurns = 1`), cached `RecallTTL = 60s` |
| Framing | authoritative context | authoritative rules | *"Heuristic context recalled from earlier sessions — not authoritative. When it changes your plan, read the source"* (`hindsight.go:588-590`) |
| Failure mode | file missing → absent | name shadowed by another root → absent | ranked low, truncated, or discounted |

### What was measured (2026-09-17)

A rule that only existed in the memory bank — *"when a prompt asks to build a feature or fix a bug, never work in the main checkout; create a worktree"* — did not reliably reach a fresh session:

1. **Truncation, per query.** Against the live server (`leankg serve --hindsight-compat`, bank `omp`, 62 rows), the recall query `"add a helper to internal/config"` returned **8 notes = 8756 chars**. `capRecall` (`hindsight.go:1083`) cuts at the last newline inside 4096 chars, keeping notes 1–2 and part of 3; the two worktree-rule notes sat at char offsets **4357–4829** and **4829–5456**, i.e. entirely in the dropped tail.
2. **Discounting, when it survives.** In a real checkout the block did arrive once, and the session's own reasoning shows what the framing costs: *"The memory guidance says: never work in the main checkout; create a worktree. … The memory heuristic is a recalled instruction from earlier sessions … That's heavy for a one-line helper."* It then did not create a worktree. A channel that labels itself non-authoritative gets weighed against the prompt, and loses to a one-line request.
3. **Query mismatch.** Recall is keyed on the newest user turn, so a task phrased without any of the rule's vocabulary retrieves by surface similarity, not by whether the rule applies. The same bank did return 6–7 hits for `"fix this bug in internal/config"` and `"git worktree branch main checkout convention"` — the rule is *retrievable*, not *reliably applied*.
4. **Scope.** `scoping: global` blanks the project tag (`hindsight.go:446-448`), and retained rows carry `cwd: ""`, so a single untagged bank serves every repository. Nothing in recall can be filtered to "this project" — a rule for repo A competes with rows about repos B and C for the same 4096 chars.

The same exercise found the products in place, unused: `~/.xdev/agent/AGENTS.md`, `~/.xdev/agent/RULES.md` and `~/.xdev/agent/rules/` all **absent** on the machine where the convention was needed, and the repo root of `xdev` has no `AGENTS.md`. `LoadContextFiles` reads the global file *first* (`prompt.go:284`), ahead of the root→cwd walk, and `xdev setup` scaffolds only `sessions`, `agents`, `commands`, `themes` (`internal/dist/setup.go:62`) — so a user has no path to discover that the always-loaded channel exists.

## 2. Decision

1. **Standing conventions go in an always-loaded file.** Cross-repo habits → `~/.xdev/agent/AGENTS.md`. Repository invariants → `<repo>/AGENTS.md` (or `CLAUDE.md`, its documented alias). A hard rule that must apply even when a same-named rule exists elsewhere → `<repo>/RULES.md`, because the native source is `AlwaysApply` and carries native priority (`rules.go:200-207`).
2. **Recall is for facts recall is good at**: what was tried before, project-specific pitfalls, prior measurements, decisions with context — things that *inform* a plan and are fine to miss. It is not a policy channel.
3. **A convention worth recalling must also be written down.** Recall may then surface it as a reminder; the file is what makes it binding.
4. **Never state a convention as if it already exists when it is only in the bank.** If a rule matters, the same PR that relies on it writes it to the file.
5. **`xdev setup` should scaffold the always-loaded surfaces** (`AGENTS.md`, `RULES.md`, `rules/`, `skills/`) or at least report them, so the channel is discoverable. Tracked as the follow-up below.

## 3. Consequences

- **Budget asymmetry is the point, not a workaround.** 32 KB always-on beats 4096 chars ranked-per-turn; the cost is prompt tokens on every turn, which is the correct place to pay for a rule that must hold on every turn.
- **Recall's ceiling stays documented where it can be seen.** `capRecall` appends *"… (recall truncated; read memory://root for the rest)"*, and `memory://root` renders at `SummaryCapChars` = 6000 — a model that sees the marker can ask for the rest. That is a recovery path, not a policy mechanism.
- **The two channels must not be duplicated blindly.** Text in both places costs tokens twice; keep the file authoritative and let recall carry only the surrounding detail.
- **Global files affect every repository** under the data dir. A global `AGENTS.md` is the right home for habits that genuinely cross projects (PRD-per-repo, issue-based trackers, session discipline); repo-specific workflow stays in the repo.

## 4. Out of scope

- **Raising `hindsight.injectionTokenLimit` as the fix.** It buys length, not authority, and every turn pays for it. The knob stays for users who genuinely want a bigger digest.
- **Tagging the bank per project to fix selection.** That is the scope contract (`leankg-memory-backend.md`, K1/K2/X1/X2) and it does not change the authority framing or the truncation.
- **Automatic promotion of a recalled row into a context file.** `learn` writes lessons; choosing which ones graduate to policy is a human edit, not a heuristic.

## 5. Follow-ups

| # | Item | Where |
| --- | --- | --- |
| C1 | `xdev setup` scaffolds or reports `AGENTS.md` / `RULES.md` / `rules/` / `skills/` alongside the four dirs it creates today | `internal/dist/setup.go:62` |
| C2 | README documents the always-loaded surfaces as a table (global vs project, what each is for) | `README.md` §Configuration |
| C3 | `<repo>/AGENTS.md` in every repo that carries a workflow convention — start with `xdev` itself | repo roots |
