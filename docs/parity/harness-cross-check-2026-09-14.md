# Cross-harness check — OMP / Claude Code / DeepSeek dsh / OpenCode (2026-09-14)

Four topics, each as **HAVE** (with the file that proves it) and **MISSING**
(with the owning issue, or the decision that closes it). Baselines: omp 18.1.21
(`docs/research/omp-feature-inventory-2026-09-11.md`, `docs/parity-delta.md`),
Claude Code v2.1.263 (`docs/research/claude-code/`), DeepSeek Harness
0.1.5-rc.2 (`docs/research/tool-inventories/deepseek-harness.md`,
`docs/research/dsh-internals.md`), OpenCode v1.18.28
(`docs/research/opencode-internals.md`).

## 1. OMP features

**Have** (launch surface + the sweep waves): session JSONL tree store, blob
store, torn-tail write set (#122); compaction triggers incl. mid-turn + async +
handoff + the snapcompact/shake/soft ladder; retry/failover + partial-stream
retain; task agents + hub (14 ops: jobs/send/wait/cancel/result/park/list/
inbox/start/ps/logs/stop/restart/describe) + advisor + prewalk + plan mode +
magic keywords + goal mode; TTSR (regex + astCondition + interruptMode);
rulebook pipeline; context_notes/new_context; skills + memory
(local/sharpshooter/hindsight/mnemopi) + `memory://` + `skill://` + `learn`;
themes; browser (CDP) + debug/DAP + eval(py) + notebook + web_search (3-chain,
#110) + github + ast_grep/ast_edit + imagegen + tts + computer + security_scan +
checkpoint/rewind + deferred catalog (tool_search/describe/call); MCP client
(stdio/HTTP, claude/codex/gemini/cursor imports, `!command` secrets, gemini
manifests); marketplace plugin manager (list/search/install/remove/info +
`xdev install`) with all four roots consumed (#85); extensions as subprocess
JSONL (`docs/reference/extension-protocol.md`); collab + the serve trio
(auth-broker/auth-gateway/browser-relay) + share + join; ACP/RPC/print/TUI modes;
the launch-flag parity batch; 6-platform CI + macOS sign/notarize + `install.sh`.

**Missing vs omp:**

| gap | owner |
| --- | --- |
| compaction ladder tails (branch-summary arming, settings block, remote-v2, snapcompact vision gate) | #83 |
| `compaction.idleAfter` / `compaction.async` never reach `CompactionConfig` | #82 |
| `retry.fallbackChains` has no arming site | #84 |
| session picker depth, `/tree` reach, foreign-source import pickers | #103 #158 #209 |
| keybinding engine (`keybindings.yml` has no consumer) | #148 |
| rule scoping residuals (`Rule.Scope`/`Agents` parsed, unused), rule grammar depth, TTSR parity surface + injection persistence | #108 #160 #95 #113 |
| hooks IO contract + residuals (toolCallId, lifecycle events, CC exit-2) | #92 #145 |
| task-tool tails (batch wire shape, `disabledAgents`, no-yield nudge), ask `questions[]`, hub/advisor tails | #91 #94 #93 |
| goal autonomy: continuation + `/goal create` submitting nothing | #244 |
| memory: hindsight TUI feed, mnemopi vector lane decision | #86 #87 |
| plugin manager verbs (uninstall/link/doctor/features/config/enable/disable, lockfile, `marketplace.autoUpdate`) | #85 closed on its explicit "record a scoped divergence" branch; remainder named on #152 #153 #154 |
| omp subcommands `agents` `read` `shell` `ssh` `ttsr` `grep` | #104 |
| `git` (fullscreen diff/staging/composer) — missed by #104's list | **#266** |
| launch-flag decisions (adapter-plumbing flags, `--mode` aliases, `--alias` divergence) | #105 |
| collab producer wiring, ACP depth, install-id on requests, `/share` upload | #99 #100 #102 #101 |
| model-facing session search (omp's picker has `history.db` matches; xdev's search is CLI-only) | **#262** |
| diagnostics on the write path (omp/opencode run them post-edit) | **#263** |
| LSP op surface (type_definition/implementation/call-hierarchy/raw) | #96 (#reload/status) + **#264** |
| lsp-config schema depth (`settings`, `isLinter`, `warmupTimeoutMs`, plugin-contributed servers) | **#261** |
| omp-only diagnostics subcommands: `grievances` (auto-QA feed), `tiny-models`, `dry-balance`, `if-bench`, `images` | none — deliberately unfiled; no xdev feature depends on them |

## 2. Claude Code — workflow, subagents, workers

**Have**: `task` (fresh children, outputSchema, `yield`, depth cap 2, model
precedence, per-agent tool allowlists); the hub as the worker pool (spawn, steer
with `send`, park/revive, cancel, results, TUI roster); cross-session
`send_message`/`inbox` with push delivery (#90); `workflowz`/`orchestrate`/
`ultrathink` keywords; per-turn advisor review; plan mode + plan-yolo handoff;
prewalk.

**Missing** (all ticketed): `/subtask` full-transcript fork #204 · spawn-economy
prompt rules #190 · machine-readable cap refusals + spend gate #191 ·
advertising discovered agent types #193 · global subagent prompt append #222 ·
advisor pull-mode consult #194 · the workflow runner (plan/script file, per-run
transcript dir, `agentCap`/`tokenCap`, `/workflows` pause-resume-save) #169 ·
background-session fleet (`--bg`, attach/logs/stop/respawn, `/exit`
detach-not-stop) #131 · `--max-budget-usd` with subagent rollup #174 · denying
model writes to harness state paths #192 · a `SendUserMessage`-equivalent #227 ·
scheduler/cron + `/loop` #162 · the ultracode boolean #182.

**Reference contract for #169** (so the runner doesn't get invented twice): dsh's
`dsh-tool-workflow` is a plain-JS orchestration script plus a `meta` block
(`name`, `description`, `whenToUse`, `phases[]`) and global `args`, exposing
`agent(prompt, opts?)`, `pipeline(items, ...stages)`, `parallel(thunks)`,
`phase()`, `log()`; no fs/network/timers inside the script; misuse kills the
run; concurrency + total-agent caps. CC's shape is the same feature with
`scriptPath`/`transcriptDir` on the wire.

**Deliberately not ported**: Agent Teams (`spawn_teammate`, the shared CAS task
board, `wait_agent`) — dsh ships the package disabled in `dsh-base`, CC's are
flag-gated, and PRD §5.3 rejects the gating/telemetry surface. No issue; reopen
only alongside a persistent-identity story for children.

## 3. DeepSeek Harness — plugin system

dsh's premise is "everything is a plugin": ~57 renameable model-facing tools,
none privileged (even the loop and the LLM adapters are plugins), registered
through `ctx.tools`; the catalog is generated from live schemas at boot with a
completeness guard that fails the build when a shipped tool goes undocumented;
capability seams (filesystem/subprocess/LSP/terminal/web) are swappable as
groups; `dsh-tool-cordis` lets a running session define/activate/stop its own
tools behind an approval gate.

**Have — xdev's answer to the same problems**: the subprocess extension protocol
(hello/capabilities handshake, tools auto-namespaced `ext_<name>_<tool>` so
nothing can shadow `read`, slash commands, declarative renderers, fail-closed
`tool_call` policy with `revise`, advisory `tool_result` with `patch`, runtime
actions steer/followUp/aside/register_provider — `internal/ext`,
`docs/reference/extension-protocol.md`); the hooks bus in the same interceptor
chain; marketplace plugins contributing command/skill/agent/hook roots (#85);
custom subprocess tools; the deferred catalog as the mid-session *disclosure*
seam.

**Missing / decided**:

| runtime tool *registration* (cordis_define/run/stop; `/reload-skills` for roots) | **decided**: reject the in-process VM (`dsh-internals.md` §11). The xdev-shaped form is a mid-session re-scan of the existing roots: #205 `/reload-skills`, #152 `/reload-plugins`, #208/#213. No new issue. |
| tool-catalog completeness guard (CI fails on an undocumented registered tool / a drifted op enum) | commented onto **#119** as a fourth CI mechanic (xdev already has the sibling guard for subcommands: `cmd/xdev/usage_test.go`). The drift is real: `internal/lsp/tool.go:63` advertises 8 ops while #96 still says 5. |
| config-renameable tools (`tool-subagent.toolName`) | rejected-ish, no issue: xdev's tool names are the approval-rule keys, so renaming them moves policy with them. |
| the `dsh` tool schemas themselves | each is either shipped (`ask_user_question`→`ask`, `exit_plan_mode`→propose, `str_replace_editor`→hashline `edit`, `job_*`→`bash action=`, `subagent`→`task`, `skill`→`skill://`, `create_goal`→`goal`, `schedule_*`→#162, `terminal_*`/`pwsh`→out of scope) or ticketed; **`run_code` (PTC) decided SHIP via the kernel that already exists** — the xdev-shaped form is an NDJSON `tool/call` frame over `internal/eval` answered by the loop's existing `runOneTool` path (plan gate → approval → interceptor → hooks), per #268 and `dsh-internals.md` §11. |
| session-model contracts worth copying verbatim: required-vs-ignorable entries, a logged request envelope, a recorded partial/attempt for cancelled turns | **#265** (first two; the third rides #265's AC), cancel-semantics family #124/#126 |
| session-model contracts worth copying verbatim: required-vs-ignorable entries, a logged request envelope, a recorded partial/attempt for cancelled turns | **#265** (first two; the third rides #265's AC), cancel-semantics family #124/#126 |

## 4. OpenCode — LSP

**Have**: an `lsp` tool with 8 wired ops — `diagnostics|definition|references|
hover|symbols|rename|code_actions|capabilities` (`internal/lsp/tool.go:63`;
rename/code_actions/capabilities landed in `36d478d` + `69c6430`); a lazy stdio
client per language (`internal/lsp/client.go`) — real server binaries, not
OpenCode's embedded WASM tree-sitter, which is a deliberate difference and stays;
4 default servers with per-language user merge (`internal/lsp/server.go:29`,
`manager.go:34`); `lsp-config list|validate` as a setup gate
(`internal/lsp/configcmd.go`); `--no-lsp`; `didOpen`/`didChange` document sync;
a diagnostics cache with a first-publish wait. Against OpenCode's 10-op list
(`opencode-internals.md` §2.7) that is 5-of-10 named ops plus four
(`diagnostics`, `rename`, `code_actions`, `capabilities`) OpenCode doesn't have.

**Missing**:

| item | owner |
| --- | --- |
| `type_definition`, `goToImplementation`, `prepareCallHierarchy`, `incomingCalls`, `outgoingCalls`, raw `request` | **#264** (`reload`/`status` stay #96) |
| post-edit diagnostics: OpenCode attaches LSP diagnostics + a diff to every write/edit/apply_patch result; xdev's edit/write never touch `internal/lsp`, so a bad edit surfaces a turn late | **#263** |
| `lsp-config` schema depth: `settings` → `didChangeConfiguration`, `isLinter` (diagnostics-only servers must be refused by position ops), `warmupTimeoutMs`, plugin-contributed servers | **#261** |

## Rules for this kind of sweep

A filing that closes another must name its replacement issue in the close
comment. During this pass #24x–#25x filings were closed with "see PRD §5.4"
while no §5.4 exists in `docs/PRD.md` (§5 stops at §5.3), which would have
dropped four real gaps out of tracking entirely; the rows above are their
replacements (#261–#266). #247 was a stray probe issue the sweep had already
closed; no other residue was left open.
