# xdev ↔ omp tool parity — agent-tool testing

Test date: 2026-09-12. Baseline: **omp v18.1.17** (`~/.bun/bin/omp`, docs `omp://`).
xdev under test: built fresh `go build -o /tmp/xdev-test ./cmd/xdev` (2026-09-12T18:05),
then `XDEV_AGENT_DIR=/tmp/parity-tools/<run> /tmp/xdev-test -max-turns <N> "…"`.
Each run got its own sandbox: `mkdir -p /tmp/parity-tools/<run> && ln -sf ~/.xdev/agent/models.yml /tmp/parity-tools/<run>/models.yml`,
cwd `/tmp/parity-tools/work` (shared fixtures: `go.mod`, `big.txt` 800 lines, `editme.txt`, `caseme.txt`, `main.go`, `probe.py`, `shift.sh`, `out/w.txt`).
`NO_COLOR=1` is exported in the user shell everywhere; the agent runs against the local onegw gateway, model `free`.

Two verification passes for every finding: code inspection (structural) + at least one live session run.
Raw transcript evidence for the run-based findings is in the `t*` sandbox session JSONLs under each sandbox's `sessions/`.

## Inventories

**xdev registered tools** (from `cmd/xdev/print.go` buildRegistry + deferred catalog block, 2026-09-12):

Eager: `read`, `write`, `edit`, `bash`, `grep`, `glob`, `ast_grep`, `ast_edit`, `eval`, `github`, `ask`, `todo`, `web_search`, `generate_image`, `computer`, `security_scan`, `task`, `hub`, `send_message`, `inbox`, `goal`, `checkpoint`, `rewind`, `learn`, `memory_edit` (Mnemopi backend; `recall`/`retain`/`reflect` only with that backend), `lsp`, `tts`, `debug`, `context_notes`, `new_context`, `browser`.

Deferred (shown via `tool_search`/`tool_describe`/`tool_call`): `ast_grep`, `ast_edit`, `github`, `hub`, `send_message`, `inbox`, `checkpoint`, `rewind`.

**omp doc tools** (`omp://` index → `tools/*.md`): `ask`, `ast-edit`, `ast-grep`, `bash`, `browser`, `checkpoint`, `computer`, `context-notes`, `debug`, `edit`, `eval`, `generate-image`, `github`, `glob`, `grep`, `hub`, `learn`, `lsp`, `manage-skill`, `memory-edit`, `new-context`, `read`, `recall`, `reflect`, `retain`, `rewind`, `security-scan`, `task`, `todo`, `tts`, `web-search`, `write`.

**Set difference** (dashes rename the same concept: `ast-edit` → `ast_edit`, `web-search` → `web_search`, …):

- **In omp docs but NOT in xdev**: `manage-skill`. Matters? Mostly no — xdev ships the `learn` tool which can create managed skills, so the workflow is reachable; but a model following the omp doc cannot `manage_skill` as a tool.
- **In xdev but NOT in omp docs**: `goal`, `send_message`, `inbox`, `tool_search`, `tool_describe`, `tool_call`, `propose`, `advise`, `vibe_status`, and all `ext_*`/`mcp_*` dynamic tools. All intentional additions (extension/MCP/agent/memory machinery); none mislead an omp-trained model because they are discoverable by name only.

## Findings table

| # | Tool | Severity | Repro command | Observed | Expected (omp doc) | Verdict |
|---|---|---|---|---|---|---|
| F1 | bash | **bug** | `bash {"command":"echo PWD=$PWD; echo KEY=$KEY","cwd":"/tmp","env":{"KEY":"fromenv"}}` (see details) | `cwd`/`env` fields **silently dropped**: ran in `/tmp/parity-tools/work`, `KEY=` empty, exit 0, no warning | `cwd` resolves under session cwd (must exist); `env` keys validated and passed as process env | xdev-broken: contract mismatch, no error |
| F2 | bash | divergence | `bash {"command":"true","timeout":0}` | `timeout:0` → 120s default deadline (`DefaultTimeoutSecs`), capped 600s | deadline disabled (`timeout:0` means no deadline) | xdev-broken: hidden deadline |
| F3 | bash | divergence | `bash` with `run_in_background` on a long command | hands off to the process registry (`[timed out after Ns — still running as background job …; output: /tmp/xdev-bg-*.log]`); the orphaned process outlives the session | background job with automatic result delivery via the job manager | intentional (parity-delta notes "shell-out backends"), but no delivery + process leak |
| F4 | read | divergence | `read {"path":"go.mod:1"}` | `file not found: go.mod:1` (the file exists at `go.mod`) | `:1` selector returns line 1; selectors like `:50-100`, `:raw`, `:img`, `:conflicts`, multi-range `:5-16,960-973` work; archive/SQLite/URL/internal-URL reads work | xdev-intentionally-different (offset/limit schema), but an omp-shaped call yields a wrong error |
| F5 | read | nit | `read {"path":"big.txt"}` (800 lines, default limit) | returns all 800 lines, `truncated:false`; continuation footer only when the window is truncated; default window 2000 lines | structural summary with `…NNln elided; re-read needed ranges, e.g. …` footer; default `read.defaultLimit = 300` | xdev-intentionally-different (no summarizer in the Go rewrite); default 2000 vs 300 |
| F6 | edit | divergence | `edit {"input":"…"}` | `edit: path is required` (schema: `{path, ops:[{op, range:{start,end|line}, lines, dest}]}`) | `{input: "<hashline patch sections with [PATH#TAG] lines>"}` registers, anchors, CUT/MV in one patch | xdev-intentionally-different (code comment: "hashline UX ported to JSON ops"), not in parity-delta |
| F7 | edit | nit | fresh-tag edit without a prior read | rejected with a re-read advisory (snapshot/hash freshness guard + exact-text re-anchor recovery; `edit_freshness_test.go`) | stale-tag recovery with safe re-anchoring | parity (works) |
| F8 | grep | **bug** | `grep {"pattern":"beta","path":"caseme.txt","case":false}` (raw args verified in t18 transcript) | **case-sensitive** search → `no matches`; `case` is silently dropped | `case:false` ⇒ case-insensitive search | xdev-broken: wrong results, no signal |
| F9 | grep | divergence | `grep` schema | uses `ignore_case`, `max_matches`; no `case`, `gitignore`, `skip`, semicolon multi-path, line-range selectors, per-file grouping/hashline anchors, 512-col cap | `case`, `gitignore`, `skip`, multi-file caps 20/200/2000, `:ln` selectors in path | xdev-intentionally-different (flat output); `case` drop is a hazard (F8) |
| F10 | glob | **bug** | `glob {"path":".","max_results":5}` | `glob: pattern is required`; `path`-only input rejected | `path` is the only documented field (glob string incl. `**`), plus `hidden`/`gitignore`/`limit` | xdev schema requires `pattern`, so omp-shaped calls fail; also no cap clamp |
| F11 | ast_grep / ast_edit | **bug** (misleading) | `ast_grep {"pattern":"[","path":"."}` | `no matches` — a malformed AST pattern is reported as absence, not a parse issue (CLI exits 1 with empty stderr; xdev maps `total==0` → "no matches") | `No matches found. Parse issues mean the query may be mis-scoped; …` + `details.parseErrors` | xdev-broken: misleading failure |
| F12 | eval | divergence | `eval {"language":"js","code":"1+1"}` | `eval: language "js" is not supported (only "py")` | py and js runtimes, persistent Bun worker VM | xdev-intentionally-different (only Python in the Go rewrite); matters for parity |
| F13 | eval | nit | `eval {"code":"…","timeout":1}` | `timed out after 1s; the cell was interrupted`; kernel stays usable; `timeout:0` ⇒ unlimited | same semantics; clamps 1..3600 | parity (semantics match) |
| F14 | ask | divergence | `ask {"question":"…","options":[{"label":"A"},{"label":"B"}],"recommended":["B"]}` in print mode | blocked **2 min** then fell back to the recommended label; empty options ⇒ `ask: at least one option is required` | tool is **never registered** in headless sessions (`createIf` requires `session.hasUI`) | xdev-broken-ish: stalls print runs; `recommended` is a list of labels vs a zero-based **index**; single question vs `questions[]` |
| F15 | tts | divergence | `tts {"text":"hi","dry_run":true}` | `dry-run: planned 1 chunk(s) via say` — speaks through the OS synthesizer; no `output_path`, no file output | writes an audio file (`output_path` **required**; `voice_id`, `language`, `sample_rate`, `bit_rate`) | intentional backend (parity-delta: "TTS shell-out"), but `output_path` missing → an omp-shaped call plays audio on the user's machine |
| F16 | checkpoint / rewind | divergence | `checkpoint {"op":"create","name":"cp1","note":"…"}` then `rewind {"name":"cp1","report":"…"}` | checkpoint records a named bookmark + entry id; rewind branches and records the report; refusal while a turn runs: `rewind refused: a turn is running — …` | checkpoint `{goal}` → "Checkpoint created."; rewind `{report}` against an active checkpoint; rewind applies at turn end | intentional design (named bookmarks, turn-boundary guard), not in parity-delta |
| F17 | computer | nit (refusal OK) | `computer` call while `computer.enabled=false` | `computer is disabled: desktop control is off by default — enable it with xdev config set computer.enabled true` | refused unless enabled | parity (refusal works); interface differs (omp: Eval prelude helper chains) |
| F18 | debug | divergence | `debug {"op":"sessions"}` | `unknown op sessions; valid ops: launch\|attach\|status\|…\|custom` | field `action` with 28 ops incl. set_breakpoint/read+write_memory/disassemble/modules/loaded_sources/sessions | xdev op subset + `op` vs `action` field name |
| F19 | security_scan | divergence | `security_scan {"path":".","timeout_seconds":10}` | `security_scan: no findings in … scanners: vet error: … \| govulncheck ok \| semgrep skipped: semgrep is not installed \| gitleaks ok` | OMP-native/Codex-Cloud review pipeline with `action`, `plan_id`, `credential_id`, `validate`, `cloud_*` | xdev-intentionally-different (local scanner merge); same name, very different semantics |
| F20 | task | divergence | `task {"context":"…","tasks":[…]}` (omp batch shape) | `task: prompt is required` — omp-shaped batch rejected; flat `{prompt,…}` works | `{context, tasks[]}` batch, `task` per item, `outputSchema`/`schemaMode` | xdev-intentionally-different (flat shape) |
| F21 | hub | nit | `hub {"op":"jobs"}` | `no background jobs` | jobs list + wait/cancel etc. | parity (+`result`/`park` ops) |
| F22 | github | not-a-bug | `github {"op":"search_repos","query":"language:go stars:>1000","limit":2}` | `gh: Invalid search query …` — caused by shell quoting of the `>` token, not by xdev (`gh search repos language:go stars:"->1000"` works; plain `language:go` works from xdev too) | search works | xdev is correct: it passes the query as a single argv entry |
| F23 | lsp | nit | `lsp {"action":"hover","file":"probe.py"}` | `lsp: no language server for .py files — install it, or point lsp.servers.python.command at another binary` (pyright/tss not on PATH; `lsp-config list` reports `not found on PATH`) | a missing server ⇒ explicit missing-binary error | parity (clear error) |
| F24 | todo | nit | `todo op done task a` after it was already done; `rm` twice | auto-promote invariant holds; `rm` of a completed task works; a second `rm` → `todo: Task "a" not found` (matches omp's non-idempotent `rm`) | per-omp state transitions | parity |
| F25 | write | nit | `write {"path":"out/w.txt","content":"…"}` | parent dirs created, write succeeds, read-back matches; byte count = UTF-16 length | same | parity |
| F26 | read (URI schemes) | nit | `read {"path":"agent://…"}` / `ssh://…` | only `skill`, `rule`, `memory`, `xd`, `history`, `pr`, `issue` internal schemes; `agent://`, `artifact://`, `ssh://`, `mcp://`, `omp://`, `local://` are not routed | all of `agent://`, `artifact://`, `ssh://`, `mcp://`, `omp://`, `local://` handled | xdev-intentionally-different (smaller scheme set) |

## Details of the findings worth fixing now

**F1 (highest severity) + F2: bash accepts an omp-shaped call and does the wrong thing silently.**
The `bashArgs` struct (internal/tool/bash.go:34) carries only `command`/`timeout`/`workdir`/`run_in_background`. Go's `json.Unmarshal` drops unknown fields with no error. So `cwd` (must exist and be used — omp `resolveToCwd`) and `env` (validated against `^[A-Za-z_][A-Za-z0-9_]*$`, passed as process environment) silently vanish. With `timeout <= 0` mapped to the 120s default instead of "no deadline", a fully omp-shaped bash call silently runs in the session cwd, without the env, under a 120s kill.
Concrete repro:
```
cd /tmp/parity-tools/work
XDEV_AGENT_DIR=/tmp/parity-tools/<run> /tmp/xdev-test -max-turns 2 \
 "Call the bash tool with EXACTLY these JSON arguments, byte for byte: {\"command\":\"echo PWD=\$PWD; echo KEY=\$KEY; pwd\", \"cwd\":\"/tmp\", \"env\":{\"KEY\":\"fromenv\"}}"
```
Observed (t7 transcript): `PWD=/private/tmp/parity-tools/work` / `KEY=` — silent wrong dir + env, exit 0.
Fix path: accept `cwd`/`env` (alias `cwd` → `workdir`, fold `env` into the child environment), reject or warn on unknown top-level fields so a mis-trained call cannot look successful, and make `timeout:0` mean "no deadline".

**F8: grep silently ignores `case`** (internal/tool/search.go:71 — `grepArgs` has `ignore_case`, not `case`). Same silent-semantics class as F1: `case:false` is treated as case-*sensitive* (the default), returning no matches for a pattern that should have matched case-insensitively. Fix: accept/alias `case` → `ignore_case` (or reject unknown fields).

**F11: malformed ast-grep patterns are reported as "no matches"** (internal/tool/ast.go Execute: `if total == 0 { return Result{Text: "no matches"} }`). A pattern like `[` makes ast-grep exit 1 with empty stderr, which xdev maps to "no matches". The omp contract surfaces parse issues (`details.parseErrors`, `…Parse issues mean the query may be mis-scoped…`). Fix: distinguish a real CLI failure from "no matches" (e.g. surface ast-grep's own diagnostic output, and keep the "no matches" text only for the empty-stderr case with a parse caveat).

**F4: read has no selector grammar** (internal/tool/read.go:75 — `readArgs{Path, Offset, Limit}` only). A call like `read {"path":"go.mod:1"}` yields `file not found: go.mod:1` for a file that exists. Cheap mitigation: when a path contains a `:digit` suffix and nothing resolves, say so and point at `offset`/`limit`. xdev read also lacks archive/SQLite/URL reads; parity-delta is silent on that.

**F14: ask stalls print-mode sessions for 2 minutes** (internal/tool/ask.go: `DefaultAskTimeout = 2 * time.Minute`). Headless runs block until the timeout fires, then guess the recommended label; the same call in a TUI session stays interactive. Print/one-shot sessions should either omit `ask` (as omp does) or use a short timeout; the stall is not documented anywhere.

## Missing omp tools (no xdev equivalent)

| omp tool | xdev equivalent | Does the omission matter? |
| --- | --- | --- |
| `manage-skill` | none (the `learn` tool can create/update a managed skill as a side effect, so the workflow is reachable, but there is no tool surface for it) | Mild — only if the model manages skills directly; no data loss or wrong results. |

Everything else from the omp docs index exists in xdev, either under the same name or as an intentional extension (`goal`, `send_message`/`inbox`, `tool_search`/`tool_describe`/`tool_call`, `mnemopi_recall`/`retain`/`reflect`).

## Shortlist worth fixing now (in priority order)

1. **F1 + F2** — bash must not silently ignore `cwd`/`env`/`timeout:0`. Highest severity: a mis-trained run silently succeeds while doing the wrong thing, including running destructive commands in the wrong directory.
2. **F8** — grep `case` alias (same silent-wrong-results class).
3. **F11** — ast_grep must not mask parse failures as "no matches" (misleading; the model refactors against a query that never ran).
4. **F14** — ask timeout for headless sessions (2-minute stall in one-shot/CI runs).
5. **F4** — read should detect selector-shaped paths and point at `offset`/`limit` instead of `file not found`.
6. **F12** — eval `js` (documented but not implemented; a parity gap, not a silent failure).
7. **F3/F5/F6/F7/F9/F10/F15/F16/F17/F18/F19/F20/F21/F23/F24/F25/F26** — intentional or benign divergences; record them in `docs/parity-delta.md` so the gap is not rediscovered, but no code change is required.
