# CLI + subcommand surface parity: xdev vs omp (v18.1.17 baseline)

Owner: cli parity (this file). Companion: `docs/parity-delta.md` (known surface gaps —
_do not re-report_), `docs/parity/tools.md` (tool registry).

Test binary: `/tmp/xdev-test`, rebuilt from the repo with `go build -o /tmp/xdev-test ./cmd/xdev`.
Baseline: `~/.bun/bin/omp` v18.1.17 (`omp --help`, `omp://cli-reference.md`).
Sandbox for every xdev run: `XDEV_AGENT_DIR=/tmp/parity-cli`, `models.yml` symlinked from
`~/.xdev/agent/models.yml`, cwd `/tmp/parity-cli/work` (empty, non-git, so `commit`/`worktree`/`gc`
cannot false-positive). Model turns bounded with `-max-turns 1` and `timeout 45–60`; everything
else was a 10 s bounded, no-model check.

## 0. Commands used

| command | purpose |
| --- | --- |
| `xdev -h`, `xdev --help`, `xdev <sub> --help` | help surface (stdout+stderr captured) |
| `xdev <sub>` (no args, `</dev/null`, timeout 12 s) | dispatch sanity, sane exit code |
| `xdev <sub> <plausible-arg>` | one-arg dispatch |
| `xdev -- <text>`, `xdev <sub> -- <text>`, `xdev <sub> <a> <b>` | `--` and extra-arg handling |
| `xdev print @<file>`, `echo x \| xdev -max-turns 1` | attachment, stdin |
| `xdev --<flag> val` for every delta-listed and delta-unlisted omp flag | rejection + message + exit code |
| session JSONL + `xdev search --regex` | what prompt text the session actually recorded |
| `omp <cmd> --help` (17 commands), `omp --help` parsed by script | exit codes, flag list |

## 1. Subcommand dispatch (§1) — result: clean

All **33** entries of `subcommands` (`cmd/xdev/main.go:51-62`) dispatch to a real arm; none is
registered-but-unreachable. Each was exercised with `--help`, no args, and one plausible arg:
print, tui, rpc, acp, config, lsp-config, say, plugin, join, login, logout, version, serve, stats,
memory, share, update, setup, bench, models, search, commit, compress, cleanse, gallery, render,
gc, usage, ps, token, completions, worktree, wt. The root usage text names all 33 (two lines are
combined: `xdev gallery|render`, `xdev worktree|wt`). The only dispatch anomaly is `print --help`
(F2) — a help-handling defect, not a missing dispatch.

Behavior spread with no args (all sane, inconsistent exit codes for `--help` noted in F18):

| name | no-args | `--help` |
| --- | --- | --- |
| stats, search, models, gc, usage, say, plugin, ps, token, completions, cleanse, compress, commit, gallery, render, bench, update | runs (exit 0/1 as appropriate) | usage on stderr, exit 2 |
| worktree, wt, memory, share, version | usage/version, exit 0/2 | usage, exit 0 |
| print | falls to the stdin/pipe branch | sent to the model (F2) |
| rpc, acp | protocol server: emits `{"type":"ready",...}` on stdout | same — no help (F18) |
| config, lsp-config | usage | `unknown subcommand --help`, exit 2 |
| login, logout | usage, exit 2 | `--help` taken as a provider name (F15) |

## 2. Same-name / different-semantics commands (§2)

All seven delta-listed commands were run; each one's actual action is the wrong tool for a user
following omp docs. Details per command are rows F7–F12 and F14 in the table.

## 3. Flag surface (§3)
Script: `python3 docs/parity/cli-flagdiff.py` (reproduces the counts below; writes `/tmp/parity-cli/flags.json`).

Mechanical diff of `omp --help` (49 flags) vs `xdev -h` (45 flags): 12 omp flags absent from xdev,
**all 12 already in the delta** (`--add-dir`, `--allow-home`, `--approval-mode`, `--extension`,
`--external-thinking`, `--no-prewalk`, `--no-pty`, `--print`, `--provider`, `--service-tier`,
`--smol`, `--slow`). 37 shared, 8 xdev-only (`--fork`, `--handoff`, `--max-tokens`, `--max-turns`,
`--personality`, `--theme`, `--trusted-extension`, `--verbose`). No new flag-existence divergence.
Beyond the 12, the doc-level flags `-v`, `-c`, `-r`, `-p`, `-e`, `--yolo`, `--plugin-dir`,
`--provider-session-id`, `--prompt-cache-key` (delta-recorded) were also exercised; each rejects
with **exit 2 + `flag provided but not defined: -<name>` + root usage** (consistent, loud). One
delta mis-file: `--models` is present with the wrong type (F13).

## 4–6. `@path`, stdin, `--`

- `@path`: unsupported; the literal string reaches the model (F5).
- stdin: a real pipe is read as the prompt (`echo 'hi stdin' | xdev -max-turns 1` → rc=0, prompt
  "hi stdin"); `</dev/null` is misdetected as a TTY (F4).
- `--`: correct at top level (`xdev -- --help` sends "--help"), broken after a subcommand (F3).

## Findings

**Counts:** 1 blocker (F1) · 10 bugs (F2, F3, F4, F7–F13) · 8 divergences (F5, F6, F14–F18, F20)
· 1 nit (F19) · 2 positive checks (F21, F22). Highest-value repros, all with
`XDEV_AGENT_DIR=/tmp/parity-cli` and the repo-built `/tmp/xdev-test`:

```sh
xdev memory                                        # F1 blocker: panic
xdev -max-turns 1 print --help                     # F2: model turn, no help
xdev print -- "marker-three question"              # F3: sends "--", drops prompt
xdev -max-turns 1 </dev/null                       # F4: TUI error, no usage
xdev print "@dot/prompt.md"                        # F5: literal @path to the model
xdev -max-turns 1 launch                           # F6: prompt "launch"
xdev cleanse / compress --dry-run / search hello / gallery / token list / worktree list  # F7-F12: wrong tool
xdev --models claude,haiku                         # F13: dumps catalog, exit 0
xdev usage clients                                 # F14: unexpected argument
xdev logout --help                                 # F15: "logged out of --help"
xdev print "alpha" "beta"                          # F17: "beta" dropped
```

| # | finding | severity | repro | observed | expected | verdict |
| --- | --- | --- | --- | --- | --- | --- |
| F1 | `xdev memory` (no args) panics | **blocker** | `XDEV_AGENT_DIR=/tmp/parity-cli /tmp/xdev-test memory` (run twice, identical; also reproduces with the default `~/.xdev/agent` config, so it is not sandbox-caused) | `panic: runtime error: slice bounds out of range [1:0]` + goroutine stack naming `cmd/xdev/memorycmd.go:42` (`main.memoryCmd`); exit 2 | `usage: xdev memory <subcommand> ...` + exit 2 (what `xdev memory --help` prints, and what every other no-args subcommand does) | xdev-broken. `rest := args[1:]` (memorycmd.go:42) runs before the `case "", "help", "-h", "--help"` branch, so zero args panics on a zero-length slice. One-line fix: move it below the first switch (or guard `len(args) > 1`). |
| F2 | `xdev print --help` runs a model turn instead of printing help | bug | `XDEV_AGENT_DIR=/tmp/parity-cli /tmp/xdev-test -max-turns 1 print --help` | Session JSONL records `user: --help`; the model replies about "--help"; no usage printed; a plain `print --help` ran >60 s until killed by timeout | `--help` on a subcommand prints that command's usage and exits — true for all 32 other xdev subcommands and for every `omp <cmd> --help` | xdev-broken. `print` is the one subcommand whose `--help` becomes the prompt (and costs a provider request). Guard `-`-prefixed `args[0]` in the print arm. |
| F3 | `xdev print -- "<prompt>"` sends `--` and silently drops the prompt | bug | `XDEV_AGENT_DIR=/tmp/parity-cli /tmp/xdev-test print -- "marker-three question"` then `xdev search --regex 'marker-three'` | Stored user message is literally `--`; `marker-three` → 0 sessions; `search --regex '^--$'` → 4 sessions whose user message is `--`; the model's own thinking: "The user message is \"--\" which is essentially empty." | `cli-reference.md` argument handling: "`--` ends flag parsing; everything after it is literal message text, even if it looks like a flag" | xdev-broken. Go's `flag` stops at the first positional (`print`), so the `--` is never stripped and the print arm takes it as `args[0]`. Top-level `xdev -- --help` is correct; only the subcommand form breaks. |
| F4 | `</dev/null` (char-device stdin) opens the TUI and errors | bug | `XDEV_AGENT_DIR=/tmp/parity-cli /tmp/xdev-test -max-turns 1 </dev/null` (same for bare `xdev </dev/null`, `xdev print </dev/null`) | `xdev: tui: screen: terminal not cursor addressable`, exit 2 — no prompt read, no usage | non-TTY stdin is read as the initial prompt; empty stdin falls back to usage. Real pipe works: `echo 'hi stdin' \| xdev -max-turns 1` → rc=0, prompt "hi stdin" | xdev-broken. `main.stdinIsTerminal()` uses `fi.Mode()&os.ModeCharDevice != 0`; `/dev/null` is a char device. Replace with a real isatty check. (omp with `</dev/null` exits 129 silently — also poor, so not a strict parity regression.) |
| F5 | `@path` attachment unsupported; literal `@path` reaches the model | divergence | `XDEV_AGENT_DIR=/tmp/parity-cli /tmp/xdev-test print "@dot/prompt.md"` (file exists, contains `ATTACHED FILE CONTENT`) | Model receives the string "@dot/prompt.md" and starts globbing for the file ("The user sent \"@dot/prompt.md\" — ... Let me check if this file exists") | `cli-reference.md`: `omp @prompt.md @image.png "What color is the sky?"` — `@`-prefixed args attach file/image contents to the initial message | xdev-intentionally-different (not implemented), but silent misdirection: expand `@path` (minimally: read and prepend) or reject with a clear "attachments unsupported". |
| F6 | `launch` is not a subcommand | divergence | `XDEV_AGENT_DIR=/tmp/parity-cli /tmp/xdev-test -max-turns 1 launch` | Prompt "launch" reaches the model: "Need more context. \"launch\" by itself doesn't map to an action I can take..." | `omp launch "fix it"` is omp's default command; `omp "fix it"` ≡ `omp launch "fix it"` | xdev-intentionally-different, but confusing for docs-following users. Recommend registering `launch` as an alias of the default mode. Low priority. |
| F7 | `cleanse`: secret redaction vs diagnostics fixer | bug | `XDEV_AGENT_DIR=/tmp/parity-cli /tmp/xdev-test cleanse` and `... cleanse --dry-run` | `xdev cleanse: <session>.jsonl was written 24s ago — that looks like a live session — pass --force to modify it anyway` (exit 1); dry-run: `would redact <id>: 0 of 7 lines` | delta + `cli-reference.md`: omp `cleanse` = "Detect and fix project diagnostics with weighted parallel subagents" (`-n/--agents`, `-m/--model`, `-t/--tests`, `-a/--all`) | xdev-broken (name collision; delta-recorded, confirmed live). Opposite tool under the same name. |
| F8 | `compress`: session compaction vs text-file rewrite | bug | `XDEV_AGENT_DIR=/tmp/parity-cli /tmp/xdev-test compress --dry-run` | `no droppable prefix: the whole history fits in the 20000-token kept tail` (exit 1); a live session gets `pass --force to modify it anyway` | `cli-reference.md`: omp `compress` = "Rewrite a **text file** into the dense prompt register, reporting what it drops" | xdev-broken (name collision; delta-recorded, confirmed live). Different input entirely. |
| F9 | `search`: local sessions vs web-search providers | bug | `XDEV_AGENT_DIR=/tmp/parity-cli /tmp/xdev-test search hello` | `SEARCH "hello" — 1 sessions, 2 matches (scanned 1 of 2)` + session ids/messages | `cli-reference.md`: omp `search` = "Test web search providers" (`--provider`, `--recency`, `-l/--limit`); `omp search --help` prints "Test web search providers" | xdev-broken (name collision; delta-recorded, confirmed live). Wrong data source. |
| F10 | `gallery`: session list vs renderer preview | bug | `XDEV_AGENT_DIR=/tmp/parity-cli /tmp/xdev-test gallery`; `gallery <id> --html` | Table of local sessions (ID, TITLE, CWD, SIZE); `--html` renders one transcript to HTML | `cli-reference.md`: omp `gallery` = "Preview tool, composer, and status-line renderers in a deterministic visual gallery" | xdev-broken (name collision; delta-recorded, confirmed live). |
| F11 | `token`: xdev service tokens vs provider API key/OAuth token | bug | `XDEV_AGENT_DIR=/tmp/parity-cli /tmp/xdev-test token list` / `token rotate` / `token show` | Table of xdev's per-install service tokens: auth-broker, auth-gateway, browser-relay, "not minted"; rotate mints, show prints in full | `cli-reference.md`: omp `token` = "Get the API key or OAuth token for a provider" | xdev-broken (name collision; delta-recorded, confirmed live). Different secret entirely. |
| F12 | `worktree`: repo git worktrees vs agent-managed worktrees | bug | `XDEV_AGENT_DIR=/tmp/parity-cli /tmp/xdev-test worktree list` / `add` / `remove` / `prune` (in a non-repo: `git worktree list: fatal: not a git repository`); `wt` is a full alias | xdev operates on the **current repo's** worktrees; actions list/add/remove/prune; flags `-b`, `--force`, `--json` | `cli-reference.md`: omp `worktree, wt` = "Add, list, or clear git worktrees (clone-first when enabled)", root `~/.omp/wt`, actions list/clear/add, flags `--all`, `-n/--dry-run`, `-j/--json`, `-C/--cwd`, `-b/--branch`, `-B/--force-branch`, `-d/--detach`, `-q/--quiet` | xdev-broken (name collision; delta-recorded, confirmed live). Different root and different action set. |
| F13 | `--models <a,b,c>` silently prints the catalog instead of setting Ctrl+P cycling | bug | `XDEV_AGENT_DIR=/tmp/parity-cli /tmp/xdev-test --models claude,haiku` | rc=0, prints `default: onegw/free` + provider/model table, exits; no session, no error, no usage — looks successful | `cli-reference.md`: `--models <a,b,c>` = "Comma-separated model patterns for Ctrl+P cycling" | xdev-broken (silent wrong tool). xdev defines `-models` as a **bool** ("print the resolved model catalog and exit"); the value is dropped. The delta files this under "missing" — it is present with the wrong type. Rename the bool (e.g. `--list-models`) and add a real string `--models`. |
| F14 | `usage` missing `clients` / `invalidate` actions | divergence | `XDEV_AGENT_DIR=/tmp/parity-cli /tmp/xdev-test usage clients` | `xdev usage: unexpected argument clients`, exit 2 (`usage` itself works; `usage --provider onegw` works) | `cli-reference.md`: "`usage clients` breaks token burn down per client (with `--days`); `usage invalidate` drops cached reports" | xdev-broken (partial parity; delta-recorded). Same tool, subset of semantics — a gap, not a wrong-tool collision. |
| F15 | `logout` accepts any argument, including `--help`, and reports success | divergence | `XDEV_AGENT_DIR=/tmp/parity-cli /tmp/xdev-test logout --help`; `... logout nosuchprovider` | rc=0, stdout `logged out of --help` / `logged out of nosuchprovider` | `--help` prints help; an unknown provider errors against models.yml | xdev-broken (minor). Silent no-op reported as success. |
| F16 | `--hook <path>` (omp semantics) is silently ignored | divergence | `XDEV_AGENT_DIR=/tmp/parity-cli /tmp/xdev-test -hook /tmp/x.sh -max-turns 1 "hi"`; also `-hook nonsense ...` | rc=0, run proceeds, model answers; no error/warning that no hook spec matched | `cli-reference.md`: `--hook <path>` = "Load a hook/extension **file**" | xdev-intentionally-different (delta records the semantic difference), plus a defect: a bogus spec is silently ignored rather than rejected. |
| F17 | Extra positional args silently dropped | divergence | `XDEV_AGENT_DIR=/tmp/parity-cli /tmp/xdev-test print "alpha" "beta"` (verified via session JSONL + `xdev search`) | Stored user message is "alpha" only; "beta" never sent. omp contrast: `omp -p "ALPHA_MSG_ONE" "BETA_MSG_TWO"` records **both** tokens in `~/.omp/agent/sessions/...jsonl` | `cli-reference.md`: positional args are MESSAGES (plural) — all are sent as the initial prompt | xdev-broken (silent data loss): the print arm uses only `args[0]`. Join all positionals. |
| F18 | `--help` exit code/stream differs; 9 subcommands have no `--help`; `-v`/`--version` missing | divergence | `XDEV_AGENT_DIR=/tmp/parity-cli /tmp/xdev-test --help`; `xdev search --help`; `xdev worktree --help`; `xdev rpc --help`; `xdev acp --help`; `xdev config --help`; `xdev login --help`; `xdev -v` | `xdev --help` and most `<sub> --help` → usage on **stderr, exit 2**; `worktree`/`wt`/`memory --help` → exit 0; `rpc --help` → emits a ready frame and exits 0; `acp --help` → no output, exit 0; `config`/`lsp-config --help` → `unknown subcommand --help`; `completions --help` → `unknown shell "--help"`; `-v`/`--version` → `flag provided but not defined`, exit 2 | `cli-reference.md`: "`--help`, `-h` — Show help for omp or a subcommand and exit"; omp: stdout + exit 0 for all 17 commands checked; `--version`/`-v` print the version | xdev-intentionally-different (delta records `-v`/`--version` as missing) — but the stderr/exit-2 convention and the 9 commands with no help are internal inconsistencies worth a pass. |
| F19 | `completions` with no shell defaults to bash instead of erroring | nit | `XDEV_AGENT_DIR=/tmp/parity-cli /tmp/xdev-test completions` | rc=0, emits a bash completion script | `omp completions` → exit 1, `Usage: omp completions <bash\|zsh\|fish>` | xdev-intentionally-different. Harmless but inconsistent. |
| F20 | No short aliases at all (`-h` is help, `-p`/`-c`/`-r`/`-e`/`-v` missing) | divergence | `xdev -p`, `-c`, `-r`, `-e`, `-v` | each: `flag provided but not defined: -<x>` + root usage, exit 2 | `cli-reference.md`: `-p`=`--print`, `-c`=`--continue`, `-r`=`--resume`, `-e`=`--extension`, `-v`=`--version` | xdev-broken but **already in the delta** — recorded here only as the exhaustive confirmation; no new alias exists. |
| F21 | All 33 `subcommands` entries dispatch (positive) | — | exhaustive sweep described in §1 | No entry is registered-but-unreachable; no subcommand name is swallowed as a prompt | every registered subcommand dispatches | No finding (positive). |
| F22 | Flag diff confirms the delta; no new flag divergence (positive) | — | `python3 docs/parity/cli-flagdiff.py` + rejection tests for 19 flags | 12 omp-only flags, all delta-recorded; every tested flag rejects with exit 2 + `flag provided but not defined` + usage | — | No finding beyond F13 (`--models` mis-filed in the delta). |

## Worth fixing now (shortlist, ordered by blast radius)

1. **F1 — `xdev memory` panic.** One-line fix (`rest := args[1:]` ordering) for a hard crash on the
   user-facing memory entry point.
2. **F2 — `xdev print --help` runs a model turn.** Cheap trap that also bills a provider request.
3. **F13 — `--models` silently dumps the catalog.** A scripted `xdev --models a,b` never errors and
   never does what the docs say; retype/rename the flag.
4. **F3 — `xdev <sub> -- <prompt>` sends `--`.** Fixing this also removes the worst case of F17.
5. **F17 — extra positional args dropped.** Join positionals (omp semantic, no cost).
6. **F4 — `</dev/null` char-device stdin crashes the TUI path.** Replace `ModeCharDevice` with an
   isatty check; removes a confusing hard exit from scripts.
7. **F7–F12 — the six wrong-tool name collisions** (cleanse, compress, search, gallery, token,
   worktree). They are delta-recorded; the cheapest mitigation is an alias/redirect or an explicit
   "xdev has no <X>; did you mean <Y>?" error rather than silently running the wrong tool.
8. **F15 — `logout` accepts anything and reports success.** Validate against models.yml.

## Notes

- Re-verified the delta's "already fixed" claims on the fresh binary: `xdev -h` names all 33 map
  entries and the leaked `@both` line is gone.
- Sandboxes: xdev session store `/tmp/parity-cli/sessions`; `~/.omp/agent/sessions` was read only
  as evidence for F17 (the `omp -p "ALPHA_MSG_ONE" "BETA_MSG_TWO"` probe).
- `xdev say <text>` really does speak through macOS `say`; `xdev ps` lists host xdev processes;
  `xdev gc` is dry-run by default. All as designed, not parity issues.
