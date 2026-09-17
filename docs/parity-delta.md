# xdev ↔ omp parity delta (2026-09-12)

Status: **launch surface implemented, CLI defect batch fixed; the remaining
residuals were re-verified 2026-09-13 and ticketed as #79–#112 (PRD §4).** Companion evidence:
`docs/parity/cli.md` (CLI/flag/subcommand findings), `docs/parity/tools.md`,
`docs/parity/agent-system.md`. (Earlier this line listed `knowledge-and-sessions.md`
and `tui-and-v2.md` as companions — those docs were never written; the 2026-09-13
reconciliation fan-out covers their scope with file:line evidence in the issues.)

Baseline: **omp v18.1.17** (`~/.bun/bin/omp` at audit time; installed since moved to
`~/.local/bin/omp`, v18.1.18 — 2026-09-13 drift check: same 131 `omp://` docs, no feature
drift) plus the authoritative `cli-reference.md`. Method: mechanical surface diff of
`omp --help` vs the installed `xdev -h`, then per-item semantic check against the omp reference.

## Already found and fixed by this pass

| Finding | Status |
| --- | --- |
| `xdev -h` printed a literal `@both` line (merge artifact leaked into the usage text) | **FIXED** (cmd/xdev/main.go) |
| `xdev -h` omitted 13 of 34 subcommands (acp, rpc, say, plugin, serve, stats, memory, share, update, setup, bench, login/logout, version) | **FIXED** (usage rewritten, grouped) |
| Stale 15 MB `./xdev` binary committed-risk artifact in the repo root (built Sep 11, predates the sweep) | **REMOVED** (`rm -f xdev`); ignored by gitignore |
| `xdev memory` (no args) panicked (`slice bounds out of range`, cmd/xdev/memorycmd.go) | **FIXED** — rest is empty when args is empty |
| `xdev print --help` ran a model turn instead of printing usage | **FIXED** — a lone `--help`/`-h` in print mode is usage (exit 0) |
| `xdev print -- "<prompt>"` stored the literal `--` and dropped the prompt | **FIXED** — a leading `--` is the separator |
| Extra positionals dropped (`xdev print "A" "B"` sent only A) | **FIXED** — positionals join into one prompt (omp's MESSAGES semantic) |
| `</dev/null` was misread as a TTY (ModeCharDevice) and opened the TUI | **FIXED** — `term.IsTerminal` instead of ModeCharDevice |
| `xdev logout <typo>` reported success for an unknown provider | **FIXED** — validated against models.yml (exit 2) |
| `xdev --models <a,b,c>` printed the catalog and dropped the patterns | **FIXED** — `--models` is the Ctrl+P pattern list; the catalog is the `models` subcommand |
| `--allow-home` chdir'd into a temp dir for every subcommand | **FIXED** — the switch is gated to the run modes (print/tui/rpc/acp/join) |
| Usage/dispatch drift and merge markers | **GUARDED** by cmd/xdev/usage_test.go (every subcommands entry must appear in the usage; no `@both`/`@ours`/`@theirs`) |

## Launch flags: implemented this pass

| omp flag | xdev | Consumer (where the value lands) |
| --- | --- | --- |
| `-p`, `--print` | **added** | forces print mode after mode resolution |
| `-c` | **added** | alias of `--continue` |
| `-r <id>`, `--session <id>` | **added** | alias of `--resume` |
| `-h`, `--version`, `-v` | **added** | `-h` exits 0; `--version`/`-v` print and exit before any run |
| `--yolo` | **added** | alias of `--auto-approve` |
| `--approval-mode <always-ask\|write\|yolo>` | **added** | validated override of `settings.ApprovalMode` (unknown value = settings stand) |
| `--smol <id>`, `--slow <id>`, `--plan-model <id>` | **removed (2026-09-17)** | they were per-run writes into `settings.ModelRoles`, which no longer exists. `-model <ref>` is the one per-run model override; use it with `--prewalk-into` (which takes a plain ref) where a second model is genuinely needed |
| `--models <a,b,c>` | **added (retyped)** | `settings.Models.Cycle` + Ctrl+P cycling (was a bool that printed the catalog; the catalog is the `models` subcommand, as in omp) |
| `--provider <name>` | **added** | forces the provider when the model ref names none; unknown provider fails fast |
| `--no-prewalk` | **added** | beats `--prewalk` and `settings.Prewalk.Enabled` |
| `--prewalk-into` + `prewalk.enabled`/`prewalk.into` | **added** | flag → `prewalk.into` → the session model; the flag default is now "" so the setting is reachable |
| `-retry-forever` + `retry.infinite` | **added** | the flag writes the setting for one run; `wireAgentMode` copies it into every mode, so once the whole fallback chain drains the ladder keeps re-running it — announcing each round — instead of ending the turn |
| `-e`, `--extension <path>` | **added** | explicit extension directory load; a non-directory (omp's single-file shape) fails **loudly** on stderr |
| `--plugin-dir <dir>` | **added** | extra plugin roots via `marketplace.SetExtraRoots` (commands/skills/agents/hooks inherit) |
| `--add-dir <dir>` | **added** | workspace roots: file-scan roots (`fscache`), context-file discovery (AGENTS.md per root), and named in the prompt |
| `--allow-home` | **added** | without it a RUN from `$HOME` switches to a temp dir (announced); utility subcommands keep the real cwd |
| `--no-pty` | **added (no-op by design)** | accepted for script parity: xdev's bash is pipe-based and never allocates a PTY — the flag restates the behavior |
| `--hook <path>` | **added** | a value with no `=` and no discovered-name match is parsed as a hook file (internal/hooks parseHookFile) |
| `--skills <globs>` | **already present** (delta previously misfiled it as missing) | skill advertisement filter |

## Launch flags: still missing (recorded, not implemented)

| omp flag | Why it is not implemented |
| --- | --- |
| `--provider-session-id <id>` | no adapter carries a provider-side session id; adding one needs a `StreamRequest` field plus per-adapter mapping |
| `--prompt-cache-key <key>` | same plumbing; xdev reuses the session id for cache affinity today |
| `--service-tier <tier>` | OpenAI-only field, not in `ai.StreamRequest`; needs an adapter-level decision (and a per-provider support matrix) |
| `--external-thinking` | private-scratchpad mode that omp itself documents as abuse-flagged; no honest implementation path |
| `--mode json`, `--mode rpc-ui` | xdev's structured surfaces are `rpc` (JSONL over stdio) and `acp`; a `--mode json` alias is cheap but redundant — recorded, not added |
| `--alias <name>` | **semantic divergence kept:** xdev's `--alias` binds a mailbox/agent identity (its own documented feature). omp's "create a shell shortcut for a profile and exit" is not implemented; `--profile` exists for relocation |

## Behavioral fixes from the audit wave (agents + user-reported)

| Finding | Source | Fix |
| --- | --- | --- |
| `--advisor` / `advisor: true` were inert in print runs (`buildAdvisor` had exactly one caller: the TUI) | T3 #20 | runPrint builds the reviewer, feeds it after each clean turn, and runs a bounded (30s) final review whose notes print at exit |
| Headless `-plan` auto-accepted the first proposal, so the plan was implemented despite the read-only flag | T3 #27 | `PlanMode.PlanOnly`: a headless `--plan` (without `--plan-yolo`) ends at the proposal and prints the plan; `--plan-yolo` keeps approve-and-build |
| `agent_end` hooks received a nil payload (literal `null` on stdin) | T3 #15 | the event carries `{model, turns, stopReason, error?}` |
| `/goal <verb>` dropped its arguments — `/goal create …` looked dead | user-reported | GoalOps verbs `create/resume/evidence/complete/drop` wired to the live GoalState; unknown verbs report the grammar |
| `/goal check` errored as an unknown verb, with no documented word for "what is my goal" | user-reported | the read verbs now take the synonyms (`check`/`show`/`status`/`get`/`list`/`info`/`view`, plus bare `/goal`, all one query — same rule as `/theme list`); `/help` and the README name the full grammar, and the empty-goal view states the slash-command remedy; a registry test rejects duplicate commands, which silently shadow the row after them |
| Up/Down never recalled the previous prompt (empty box scrolled; a typed draft blocked recall) | user-reported | readline-style: Up always recalls, the stashed draft returns on Down; scrolling stays on Shift+arrow/PgUp/PgDn/wheel |
| Esc / Ctrl+C could not cancel or terminate a running turn | user-reported | root cause in `withMaxTime`: with `--max-time` unset it returned the parent plus a **no-op cancel**, and the TUI's cancel handler calls exactly that. It now always returns a cancellable context |
| `grep` silently ignored omp's `case` field | T2 F8 | accepted (`case:false` = insensitive) on both the rg and Go paths |
| `bash` silently dropped omp's `cwd`/`env` and turned `timeout:0` into a 120s kill | T2 F1/F2 | `cwd` alias with a disagree-error, `env` map with key validation, `*int` timeout (0 = no deadline), `ApplyCallEnv` layering that preserves hardening |
| `glob` rejected omp's path-only shape; `read` said "file not found" for a selector-shaped path; malformed `ast_grep` patterns reported "no matches" | T2 F10/F4/F11 | all three fixed; the ast case documents its heuristic ceiling |
| `-export <session.jsonl>` overwrote the transcript with HTML | T4/T5 | refuses any existing non-HTML target; re-export over a previous export stays allowed |

Still open from T3 — corrected 2026-09-13 against HEAD: four of these no longer stand.
Fixed by the 4ed9920/6038f28 waves: hooks payload names (`toolName`/`input`/`content`/`isError`
now ship; only `toolCallId` remains, #92), hub processes orphaned (StopAll deferred in all
four modes), and the TTSR `condition` field on discovered rules (`ttsrConfig` merge, #95).
Reclassified as documented-by-design, not a defect: goal budget "lag" — spend is counted at
message end before the turn's tools, so the only drift is the crash window #40 records.
Still genuinely open: task batch wire shape `{context, tasks[]}` + `task.disabledAgents` +
no-`yield` child completion (#91), and the items in the tables below (#104, #105).

## Subcommands: missing in xdev

| omp command | Purpose | Note |
| --- | --- | --- |
| `agents` | manage bundled task agents | user-facing |
| `read` | show what the read tool returns for a path/URL/URI | user-facing, cheap |
| `shell` | interactive shell console | user-facing |
| `ssh` | manage SSH host configurations | user-facing |
| `ttsr` | inspect/test TTSR rules | pairs with the shipped TTSR engine |
| `grep` | test the grep tool from the CLI | dev-facing but cheap |
| `install` | alias of `plugin install` / `plugin link` | one-line alias |
| `auth-broker`, `auth-gateway`, `browser-relay` | top-level names | xdev ships them under `serve <svc>` — needs aliases |
| `dry-balance`, `if-bench`, `grievances`, `images` | dev/QA/bench internals | deliberate non-parity (recorded, not implemented) |
| `tiny-models` | download tiny local models | xdev decision #70 is API-only; the download command has nothing to fetch |
| `git` | fullscreen git UI (diff viewer, staging, commit composer) | xdev has `worktree`/`commit`; the fullscreen UI is a larger TUI feature |

## Subcommands with the SAME NAME but DIFFERENT semantics (worst class)

Decision (2026-09-12): all seven stay as xdev-native commands. They are
existing xdev features with their own users and help text; renaming them to
free the name would break xdev's own CLI contract, and an omp-following user
gets a coherent (if different) answer rather than an error. The mitigation is
this table plus the root usage naming each command's actual verb — recorded,
not renamed.

| Command | omp | xdev | Risk |
| --- | --- | --- | --- |
| `cleanse` | detect and fix project diagnostics with weighted parallel subagents | redact secrets from a transcript | a user gets secret-redaction when expecting a diagnostics fixer |
| `compress` | rewrite a **text file** into the dense prompt register | compact a **session** file | different input entirely |
| `search` | test **web search** providers | search **local sessions** | different data source |
| `gallery` | preview tool/composer/status **renderers** | list sessions + render one to HTML | different output |
| `token` | get the **API key or OAuth token for a provider** | list/rotate xdev's per-install **service tokens** | different secret |
| `worktree` | list/clear **agent-managed** worktrees under `~/.omp/wt` | list/add/remove **repo** git worktrees | different root |
| `usage` | provider limits + `usage clients` (token burn per client) + `usage invalidate` | provider accounts/limits + observed usage | close; missing `clients`/`invalidate` |

## Tools

xdev's registry vs the omp tool docs: see `docs/parity/tools.md` (generated by the
tool-surface tester). Known intentional differences: extensions run as subprocesses
(no in-process JS), browser is attach-only CDP, TTS/computer/DAP are shell-out
backends.

## Non-parity accepted (recorded)

`dry-balance`, `if-bench`, `grievances`, `images`, `tiny-models` (download),
`git` fullscreen UI, and the `--no-pty`/`--external-thinking` flags have no honest
Go implementation path in this architecture today; each is listed here rather than
silently absent.
